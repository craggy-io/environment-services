package androidphys

import (
	"context"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/androiddev"
)

// fakePhysTransport scripts one physical device PIXEL1.
type fakePhysTransport struct {
	mu        sync.Mutex
	devices   []adb.Device
	devErr    error
	roSerial  string
	cpuABI    string
	commands  []string
	syncSent  []string
	forwards  [][2]int
	fridaUp   atomic.Bool // pidof answers once true
	execOuts  atomic.Int32
	logcatOut func() (io.ReadCloser, error)
}

func newFakePhys() *fakePhysTransport {
	return &fakePhysTransport{
		devices:  []adb.Device{{Serial: "PIXEL1", State: "device"}, {Serial: "emulator-5554", State: "device"}},
		roSerial: "PIXEL1",
		cpuABI:   "x86_64",
	}
}

func (f *fakePhysTransport) Run(ctx context.Context, serial, cmd string) (string, int, error) {
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	f.mu.Unlock()
	switch {
	case strings.Contains(cmd, "getprop ro.serialno"):
		return f.roSerial + "\n", 0, nil
	case strings.Contains(cmd, "getprop ro.product.cpu.abi"):
		return f.cpuABI + "\n", 0, nil
	case strings.Contains(cmd, "pidof"):
		if f.fridaUp.Load() {
			return "4321\n", 0, nil
		}
		return "", 0, nil
	case cmd == "echo ok" || strings.Contains(cmd, "echo ok"):
		return "ok", 0, nil
	case strings.Contains(cmd, " id") || strings.HasSuffix(cmd, "id"):
		return "uid=0(root) gid=0", 0, nil
	case strings.Contains(cmd, "date +%s"):
		return "1700000000", 0, nil
	case strings.Contains(cmd, "setsid"):
		f.fridaUp.Store(true)
		return "", 0, nil
	default:
		return "", 0, nil
	}
}

func (f *fakePhysTransport) Shell(ctx context.Context, serial, cmd string) (*androiddev.ShellSession, error) {
	out, code, err := f.Run(ctx, serial, cmd)
	if err != nil {
		return nil, err
	}
	return &androiddev.ShellSession{Stdout: strings.NewReader(out), Stderr: strings.NewReader(""),
		Stdin: nopWC{}, Wait: func() (int, error) { return code, nil }, Kill: func() error { return nil }}, nil
}

type nopWC struct{}

func (nopWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopWC) Close() error                { return nil }

func (f *fakePhysTransport) ExecOut(ctx context.Context, serial, cmd string, stdin io.Reader) (io.ReadCloser, func() error, error) {
	f.execOuts.Add(1)
	f.mu.Lock()
	f.commands = append(f.commands, "exec:"+cmd)
	fn := f.logcatOut
	f.mu.Unlock()
	if fn != nil && strings.Contains(cmd, "logcat") {
		rc, err := fn()
		return rc, func() error { return nil }, err
	}
	out, _, err := f.Run(ctx, serial, cmd)
	return io.NopCloser(strings.NewReader(out)), func() error { return nil }, err
}

func (f *fakePhysTransport) SyncSend(ctx context.Context, serial, p string, m uint32, r io.Reader) error {
	_, _ = io.Copy(io.Discard, r)
	f.mu.Lock()
	f.syncSent = append(f.syncSent, p)
	f.mu.Unlock()
	return nil
}

func (f *fakePhysTransport) Forward(ctx context.Context, serial string, l, r int) (int, error) {
	f.mu.Lock()
	f.forwards = append(f.forwards, [2]int{l, r})
	f.mu.Unlock()
	if l == 0 {
		return 47000, nil
	}
	return l, nil
}

func (f *fakePhysTransport) Devices(ctx context.Context) ([]adb.Device, error) {
	return f.devices, f.devErr
}
func (f *fakePhysTransport) Root(ctx context.Context, serial string) (string, error) {
	return "restarting adbd as root", nil
}
func (f *fakePhysTransport) KillServer(ctx context.Context) error { return nil }
func (f *fakePhysTransport) Version(ctx context.Context) (string, error) {
	return "0029", f.devErr
}

// fridaTestVersion is the version test frida options must request:
// the probe (correctly) reports a mismatch when the requested
// version differs from the host frida client, so tests follow it.
func fridaTestVersion() string {
	if bin, err := exec.LookPath("frida"); err == nil {
		if out, err := exec.Command(bin, "--version").Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
	}
	return "9.9.9"
}

func (f *fakePhysTransport) saw(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.commands {
		if strings.Contains(c, substr) {
			return true
		}
	}
	return false
}

func (f *fakePhysTransport) syncCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.syncSent)
}

var _ androiddev.Transport = (*fakePhysTransport)(nil)
