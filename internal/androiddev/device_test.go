package androiddev

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

type installClient struct {
	mu               sync.Mutex
	files            map[string]string
	paths            []string
	ready            chan struct{}
	pushErr          error
	installErr       error
	output           string
	code             int
	cancel           context.CancelFunc
	cleanupCancelled bool
}

func (c *installClient) SyncSend(_ context.Context, _, path string, _ uint32, r io.Reader) error {
	b, _ := io.ReadAll(r)
	c.mu.Lock()
	c.files[path] = string(b)
	c.paths = append(c.paths, path)
	if c.ready != nil && len(c.paths) == 2 {
		close(c.ready)
	}
	c.mu.Unlock()
	return c.pushErr
}

func (c *installClient) Run(ctx context.Context, _, cmd string) (string, int, error) {
	if strings.HasPrefix(cmd, "rm -f ") {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.cleanupCancelled = ctx.Err() != nil
		delete(c.files, strings.TrimPrefix(cmd, "rm -f "))
		return "", 0, nil
	}
	if c.ready != nil {
		<-c.ready
	}
	if !strings.HasSuffix(cmd, " 2>&1") {
		return "", 0, errors.New("stderr was not retained")
	}
	if c.cancel != nil {
		c.cancel()
	}
	return c.output, c.code, c.installErr
}

func TestInstallIsolatesConcurrentStaging(t *testing.T) {
	c := &installClient{files: map[string]string{}, ready: make(chan struct{}), output: "Success"}
	results := make(chan error, 2)
	for _, apk := range []string{"first APK", "second APK"} {
		go func() { results <- installAPK(context.Background(), c, "device", strings.NewReader(apk), true) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if len(c.paths) != 2 || c.paths[0] == c.paths[1] {
		t.Fatalf("staging paths collided: %v", c.paths)
	}
	if len(c.files) != 0 {
		t.Fatalf("staging leaked: %v", c.files)
	}
}

func TestInstallFailureDiagnosticsAndCleanup(t *testing.T) {
	const detail = "Exception occurred while executing 'install':\njava.lang.IllegalArgumentException: invalid APK\n\tat PackageManager.install(PackageManager.java:42)"
	for _, stage := range []string{"push", "install", "cancel", "zero-exit-failure"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &installClient{files: map[string]string{}, output: detail, code: 1}
			switch stage {
			case "push":
				c.pushErr = errors.New("broken sync")
			case "cancel":
				c.cancel, c.installErr = cancel, context.Canceled
			case "zero-exit-failure":
				c.code = 0
			}
			err := installAPK(ctx, c, "device", strings.NewReader("APK"), false)
			if err == nil {
				t.Fatal("failure accepted")
			}
			if stage != "push" && !strings.Contains(err.Error(), detail) {
				t.Fatalf("lost diagnostics: %v", err)
			}
			if stage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if len(c.files) != 0 || c.cleanupCancelled {
				t.Fatalf("cleanup failed: %+v", c)
			}
		})
	}
}
