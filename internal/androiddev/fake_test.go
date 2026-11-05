package androiddev

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/craggy-io/environment-services/internal/adb"
)

// fakeTransport scripts adb answers by command substring. Shell and
// Run share the script (Run is Shell in the real client too).
type fakeTransport struct {
	mu       sync.Mutex
	devices  []adb.Device
	devErr   error
	answer   func(cmd string) (string, int, error) // nil = "",0,nil
	execOut  func(cmd string) (io.ReadCloser, error)
	commands []string
	blockOn  string // Shell commands containing this block until ctx ends
	syncSent []string
	forwards [][3]int // serial-less: local, remote, _
	emuAns   map[string]string
}

func (f *fakeTransport) run(cmd string) (string, int, error) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	fn := f.answer
	f.mu.Unlock()
	if fn == nil {
		return "", 0, nil
	}
	return fn(cmd)
}

func (f *fakeTransport) Run(ctx context.Context, serial, cmd string) (string, int, error) {
	return f.run(cmd)
}

func (f *fakeTransport) Shell(ctx context.Context, serial, cmd string) (*ShellSession, error) {
	if f.blockOn != "" && strings.Contains(cmd, f.blockOn) {
		f.mu.Lock()
		f.commands = append(f.commands, cmd)
		f.mu.Unlock()
		pr, pw := io.Pipe()
		go func() { <-ctx.Done(); pw.Close() }()
		return &ShellSession{Stdout: pr, Stderr: strings.NewReader(""), Stdin: nopWriteCloser{},
			Wait: func() (int, error) { <-ctx.Done(); return 0, nil },
			Kill: func() error { return pw.Close() }}, nil
	}
	out, code, err := f.run(cmd)
	if err != nil {
		return nil, err
	}
	return &ShellSession{
		Stdout: strings.NewReader(out),
		Stderr: strings.NewReader(""),
		Stdin:  nopWriteCloser{},
		Wait:   func() (int, error) { return code, nil },
		Kill:   func() error { return nil },
	}, nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

func (f *fakeTransport) ExecOut(ctx context.Context, serial, cmd string, stdin io.Reader) (io.ReadCloser, func() error, error) {
	f.mu.Lock()
	f.commands = append(f.commands, "exec:"+cmd)
	fn := f.execOut
	f.mu.Unlock()
	if fn != nil {
		rc, err := fn(cmd)
		return rc, func() error { return nil }, err
	}
	out, _, err := f.run(cmd)
	if err != nil {
		return nil, nil, err
	}
	return io.NopCloser(strings.NewReader(out)), func() error { return nil }, nil
}

func (f *fakeTransport) SyncSend(ctx context.Context, serial, remotePath string, mode uint32, r io.Reader) error {
	_, _ = io.Copy(io.Discard, r)
	f.mu.Lock()
	f.syncSent = append(f.syncSent, remotePath)
	f.mu.Unlock()
	return nil
}

func (f *fakeTransport) Forward(ctx context.Context, serial string, localPort, remotePort int) (int, error) {
	f.mu.Lock()
	f.forwards = append(f.forwards, [3]int{localPort, remotePort, 0})
	f.mu.Unlock()
	if localPort == 0 {
		return 47000, nil
	}
	return localPort, nil
}

func (f *fakeTransport) Devices(ctx context.Context) ([]adb.Device, error) {
	return f.devices, f.devErr
}

func (f *fakeTransport) Root(ctx context.Context, serial string) (string, error) {
	return "restarting adbd as root", nil
}
func (f *fakeTransport) KillServer(ctx context.Context) error { return nil }
func (f *fakeTransport) Version(ctx context.Context) (string, error) {
	return "0029", f.devErr
}
func (f *fakeTransport) Emu(ctx context.Context, serial, cmd string) (string, error) {
	if f.emuAns != nil {
		if v, ok := f.emuAns[cmd]; ok {
			return v, nil
		}
	}
	return "OK", nil
}

var _ Transport = (*fakeTransport)(nil)
var _ = fmt.Sprintf
