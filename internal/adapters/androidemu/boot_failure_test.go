package androidemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/androiddev"
)

func TestBootFailureRetainsEmulatorAndADBDiagnostics(t *testing.T) {
	d := newBootFakeAdapter(t, &bootFake{devices: []adb.Device{{Serial: "emulator-5556", State: "offline"}}})
	inst := &Instance{ad: d, avd: "craggy-google-apis-35-x86_64-adb", serial: "emulator-5556", pid: 57212}
	var output syncBuffer
	_, _ = output.Write([]byte("loading snapshot clean\nqemu: guest transport still offline\n"))
	cause := &androiddev.DeviceWaitError{Serial: inst.serial, State: "offline"}
	err := d.bootFailure(inst, []string{"-snapshot", "clean", "-read-only", "-port", "5556"}, &output, cause)
	var wait *androiddev.DeviceWaitError
	if !errors.As(err, &wait) || wait != cause || !strings.Contains(err.Error(), "guest transport still offline") {
		t.Fatalf("boot error lost cause or tail: %v", err)
	}
	dir := filepath.Join(d.StateDir, "android", "boot-logs")
	entries, readErr := os.ReadDir(dir)
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("boot logs: %v err=%v", entries, readErr)
	}
	path := filepath.Join(dir, entries[0].Name())
	data, readErr := os.ReadFile(path)
	if readErr != nil || !strings.Contains(string(data), "last adb state: offline") || !strings.Contains(string(data), "-snapshot clean -read-only -port 5556") || !strings.Contains(string(data), "guest transport still offline") || !strings.Contains(err.Error(), path) {
		t.Fatalf("retained diagnostic = %q err=%v readErr=%v", data, err, readErr)
	}
}

func TestEmulatorOutputTailIsBounded(t *testing.T) {
	var output syncBuffer
	large := strings.Repeat("x", 300<<10) + "END"
	if n, err := output.Write([]byte(large)); err != nil || n != len(large) {
		t.Fatalf("write=%d err=%v", n, err)
	}
	if got := output.String(); len(got) != 256<<10 || !strings.HasSuffix(got, "END") {
		t.Fatalf("tail length=%d ends correctly=%t", len(got), strings.HasSuffix(got, "END"))
	}
}

func TestQuickbootRetryIsBoundedToOfflineSnapshot(t *testing.T) {
	ctx := context.Background()
	offline := &androiddev.DeviceWaitError{Serial: "emulator-5556", State: "offline"}
	if deviceWaitLimit(bootOptions{snapshot: "clean"}) != quickbootDeviceWait || deviceWaitLimit(bootOptions{writable: true, noSnapshot: true}) != 3*time.Minute {
		t.Fatal("device wait limits differ")
	}
	if !shouldRetryQuickboot(ctx, bootOptions{snapshot: "clean"}, fmt.Errorf("diagnostic: %w", offline)) {
		t.Fatal("offline read-only snapshot quickboot should retry")
	}
	for _, opt := range []bootOptions{{writable: true, snapshot: "clean"}, {noSnapshot: true}, {snapshot: "clean"}} {
		err := error(offline)
		if !opt.writable && !opt.noSnapshot {
			err = &androiddev.DeviceWaitError{Serial: "emulator-5556", State: "absent"}
		}
		if shouldRetryQuickboot(ctx, opt, err) {
			t.Fatalf("unexpected retry for %+v err=%v", opt, err)
		}
	}
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if shouldRetryQuickboot(short, bootOptions{snapshot: "clean"}, offline) {
		t.Fatal("retry with no time left")
	}
}

func TestBootWithRetryLaunchesAtMostTwice(t *testing.T) {
	offline := &androiddev.DeviceWaitError{Serial: "emulator-5556", State: "offline"}
	launches, retries := 0, 0
	want := &Instance{serial: "emulator-5554"}
	got, err := bootWithRetry(context.Background(), bootOptions{snapshot: "clean"}, func(stage, msg string) {
		if stage == "boot-retry" {
			retries++
		}
	}, func() (*Instance, error) {
		launches++
		if launches == 1 {
			return nil, offline
		}
		return want, nil
	})
	if err != nil || got != want || launches != 2 || retries != 1 {
		t.Fatalf("got=%v err=%v launches=%d retries=%d", got, err, launches, retries)
	}
	launches = 0
	_, err = bootWithRetry(context.Background(), bootOptions{snapshot: "clean"}, nil, func() (*Instance, error) {
		launches++
		return nil, offline
	})
	if launches != 2 || !strings.Contains(err.Error(), "failed twice") || !errors.As(err, &offline) {
		t.Fatalf("failed retry err=%v launches=%d", err, launches)
	}
}
