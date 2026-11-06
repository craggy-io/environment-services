// Package androiddev is device access over one adb serial: shell and
// root shell, package operations and their receipt protocols, input,
// screenshots, the managed instrumentation components (adb, root,
// frida-server, proxy CA) supervised with evidence-before-repair, and
// the Android signal collectors bound to the device. It knows nothing
// about how the device came to exist: the emulator adapter
// (internal/adapters/androidemu) and the physical adapter
// (internal/adapters/androidphys) both build a Device over their
// serial and differ only in acquisition and release.
//
// Identity strings created here are craggy.* only (staged files under
// /data/local/tmp/craggy-*, package-action results under
// files/craggy-actions/). Nothing legacy is recognised.
package androiddev

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adb"
)

// ShellSession is one shell-v2 session over a Transport. It is the
// fakeable seam: tests script sessions without an adb server.
type ShellSession struct {
	Stdout io.Reader
	Stderr io.Reader
	Stdin  io.WriteCloser
	Wait   func() (int, error)
	Kill   func() error
}

// Transport is the adb subset a Device needs. ClientTransport
// adapts *adb.Client; tests substitute a scripted fake.
type Transport interface {
	Run(ctx context.Context, serial, cmd string) (string, int, error)
	Shell(ctx context.Context, serial, cmd string) (*ShellSession, error)
	ExecOut(ctx context.Context, serial, cmd string, stdin io.Reader) (io.ReadCloser, func() error, error)
	SyncSend(ctx context.Context, serial, remotePath string, mode uint32, r io.Reader) error
	Forward(ctx context.Context, serial string, localPort, remotePort int) (int, error)
	Devices(ctx context.Context) ([]adb.Device, error)
	Root(ctx context.Context, serial string) (string, error)
	KillServer(ctx context.Context) error
	Version(ctx context.Context) (string, error)
}

// ClientTransport adapts an *adb.Client to Transport (and forwards
// the emulator console for adapters that assert it).
type ClientTransport struct{ C *adb.Client }

func (t ClientTransport) Run(ctx context.Context, serial, cmd string) (string, int, error) {
	return t.C.Run(ctx, serial, cmd)
}
func (t ClientTransport) Shell(ctx context.Context, serial, cmd string) (*ShellSession, error) {
	p, err := t.C.Shell(ctx, serial, cmd)
	if err != nil {
		return nil, err
	}
	return &ShellSession{Stdout: p.Stdout, Stderr: p.Stderr, Stdin: p.Stdin, Wait: p.Wait, Kill: p.Kill}, nil
}
func (t ClientTransport) ExecOut(ctx context.Context, serial, cmd string, stdin io.Reader) (io.ReadCloser, func() error, error) {
	return t.C.ExecOut(ctx, serial, cmd, stdin)
}
func (t ClientTransport) SyncSend(ctx context.Context, serial, remotePath string, mode uint32, r io.Reader) error {
	return t.C.SyncSend(ctx, serial, remotePath, mode, r)
}
func (t ClientTransport) Forward(ctx context.Context, serial string, localPort, remotePort int) (int, error) {
	return t.C.Forward(ctx, serial, localPort, remotePort)
}
func (t ClientTransport) Devices(ctx context.Context) ([]adb.Device, error) { return t.C.Devices(ctx) }
func (t ClientTransport) Root(ctx context.Context, serial string) (string, error) {
	return t.C.Root(ctx, serial)
}
func (t ClientTransport) KillServer(ctx context.Context) error { return t.C.KillServer(ctx) }
func (t ClientTransport) Version(ctx context.Context) (string, error) {
	return t.C.Version(ctx)
}

// Emu forwards the emulator console (androidemu asserts this).
func (t ClientTransport) Emu(ctx context.Context, serial, cmd string) (string, error) {
	return t.C.Emu(ctx, serial, cmd)
}

// Config parameterises one Device.
type Config struct {
	Serial string
	// Root selects root-shell wrapping: RootMagisk prefixes `su -c`,
	// RootADB runs plainly (adbd already root), RootNone has no root.
	Root     envspec.RootMode
	Frida    *envspec.FridaOptions
	ProxyCA  bool
	Repair   envspec.RepairPolicy
	ABI      string // guest ABI ("x86_64", "arm64-v8a"); default x86_64
	NameSeed string // frida on-device name seed (AVD name or serial)

	// FridaServer locates/downloads frida-server binaries; nil leaves
	// the frida component degraded with guidance.
	FridaServer *FridaServer
	// ProxyCAPEM is the CA certificate (PEM) the proxy-ca component
	// installs; empty leaves that component a no-op probe.
	ProxyCAPEM string
	// CaptureFn pins evidence before a repair and returns a capture ID
	// (the adapters wire the recorder through it). The device also
	// drops a guest-side bundle under the workspace.
	CaptureFn func(f FailureInfo) string
	// OnComponent receives every component transition (the adapters
	// forward to core's ComponentWatcher sink).
	OnComponent func(c adapter.ComponentChange)
	// WaitDevice waits for the serial to report state=device (the
	// emulator adapter and physical adapter supply their own; the
	// default polls Transport.Devices).
	WaitDevice func(ctx context.Context, serial string, timeout time.Duration) error
}

// Device is Android access over one serial. It is the value behind
// the environments facade's AndroidDevice.
type Device struct {
	tr  Transport
	cfg Config
	sup *Supervisor

	mu         sync.Mutex
	pkgStarts  map[string]time.Time
	forwards   []int                  // local ports this device forwarded (owned)
	fridaOwned bool                   // this device (re)started frida-server
	challenges map[io.Closer]struct{} // live challenge servers (owned)
}

// New builds a Device and its component supervisor. Components are
// registered immediately; probing is driven by Probes() (core's
// activity reconciler) and by EnsureComponent before dependent ops.
func New(tr Transport, cfg Config) *Device {
	d := &Device{tr: tr, cfg: cfg, pkgStarts: map[string]time.Time{}}
	d.sup = newSupervisor(SupervisorHooks{
		Capture:    d.captureBeforeRepair,
		Transition: cfg.OnComponent,
	})
	if cfg.Repair.MaxAttempts > 0 {
		d.sup.SetLimits(cfg.Repair.MaxAttempts, 5*time.Minute)
	}
	d.sup.Foreground = func() string {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return d.ForegroundPackage(c)
	}
	d.sup.PackageStarts = d.packageStarts
	d.registerComponents()
	return d
}

// Serial returns the adb serial (library-internal; envcap never shows it).
func (d *Device) Serial() string { return d.cfg.Serial }

// Transport exposes the underlying adb transport for adapter-level
// operations (emulator console lives on *adb.Client, not here).
func (d *Device) Transport() Transport { return d.tr }

// Rooted reports whether this device has a root mode.
func (d *Device) Rooted() bool {
	return d.cfg.Root != envspec.RootNone && d.cfg.Root != ""
}

// RootMode returns the configured root mode.
func (d *Device) RootMode() envspec.RootMode { return d.cfg.Root }

// Arch returns the guest GOARCH for helper-binary selection.
func (d *Device) Arch() string {
	switch d.cfg.ABI {
	case "arm64-v8a", "arm64":
		return "arm64"
	default:
		return "amd64"
	}
}

// Workspace is the guest working directory.
func (d *Device) Workspace() string { return "/data/local/tmp/work" }

// Shell runs one command string and returns stdout + exit.
func (d *Device) Shell(ctx context.Context, cmd string) (string, int, error) {
	return d.tr.Run(ctx, d.cfg.Serial, cmd)
}

// RootShell runs a command as root (su or adbd-as-root, per mode).
func (d *Device) RootShell(ctx context.Context, cmd string) (string, int, error) {
	return d.tr.Run(ctx, d.cfg.Serial, d.RootCmd(cmd))
}

// RootCmd wraps a command line for root execution. RootADB variants
// run adbd as root already; Magisk needs su.
func (d *Device) RootCmd(cmd string) string {
	if d.cfg.Root == envspec.RootMagisk {
		return "su -c " + adbQuote(cmd)
	}
	return cmd
}

func adbQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// guestProc is one running guest command (collector/exec plumbing);
// it is a ShellSession.
type guestProc = ShellSession

// ExecArgv runs argv in the guest. user "root" wraps for root; dir is
// resolved against the workspace when relative; env entries prefix
// the command. This is the one exec path: adapter Executor impls and
// collectors both go through it.
func (d *Device) ExecArgv(ctx context.Context, argv []string, user, dir string, env map[string]string, stdin io.Reader, timeout time.Duration) (*guestProc, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("android: empty argv")
	}
	cmdline := d.commandLine(argv, user, dir, env)
	execCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, timeout)
	}
	p, err := d.tr.Shell(execCtx, d.cfg.Serial, cmdline)
	if err != nil {
		if cancel != nil {
			cancel()
		}
		return nil, err
	}
	if stdin != nil {
		go func() {
			_, _ = io.Copy(p.Stdin, stdin)
			_ = p.Stdin.Close()
		}()
	}
	wait := func() (int, error) {
		code, err := p.Wait()
		if cancel != nil {
			cancel()
		}
		return code, err
	}
	return &guestProc{Stdout: p.Stdout, Stderr: p.Stderr, Stdin: p.Stdin, Wait: wait, Kill: p.Kill}, nil
}

func (d *Device) commandLine(argv []string, user, dir string, env map[string]string) string {
	var b strings.Builder
	if dir != "" {
		if !strings.HasPrefix(dir, "/") {
			dir = d.Workspace() + "/" + dir
		}
		b.WriteString("cd " + adb.Quote([]string{dir}) + " && ")
	}
	for k, v := range env {
		b.WriteString(k + "=" + adb.Quote([]string{v}) + " ")
	}
	b.WriteString(adb.Quote(argv))
	line := b.String()
	if user == "root" {
		return d.RootCmd(line)
	}
	return line
}

// ExecProcess adapts ExecArgv to adapter.Executor for the adapters'
// Instance implementations.
func (d *Device) ExecProcess(ctx context.Context, cmd adapter.ExecCmd) (*adapter.Process, error) {
	p, err := d.ExecArgv(ctx, cmd.Argv, cmd.User, cmd.Dir, cmd.Env, cmd.Stdin, cmd.Timeout)
	if err != nil {
		return nil, err
	}
	return adapter.NewProcess(p.Stdout, p.Stderr, p.Stdin, p.Wait, func(string) error { return p.Kill() }), nil
}

// Health is a one-shot adb echo round-trip.
func (d *Device) Health(ctx context.Context) error {
	out, code, err := d.Shell(ctx, "echo ok")
	if err != nil {
		return err
	}
	if code != 0 || !strings.Contains(out, "ok") {
		return fmt.Errorf("android: health echo failed (%d)", code)
	}
	return nil
}

// NotePackageStart records a package start for suspect correlation.
// The android.proc collector feeds it.
func (d *Device) NotePackageStart(pkg string, at time.Time) {
	d.mu.Lock()
	d.pkgStarts[pkg] = at
	d.mu.Unlock()
}

func (d *Device) packageStarts() map[string]time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]time.Time, len(d.pkgStarts))
	for k, v := range d.pkgStarts {
		out[k] = v
	}
	return out
}

// DetachCleanup removes only what this device attached: its challenge
// servers, the adb forwards it created and a frida-server it
// (re)started. Physical detach and emulator disconnect use it;
// nothing else on the device is touched.
func (d *Device) DetachCleanup(ctx context.Context) error {
	firstErr := d.CloseChallenges()
	d.mu.Lock()
	fwds := append([]int(nil), d.forwards...)
	d.forwards = nil
	owned := d.fridaOwned
	d.fridaOwned = false
	d.mu.Unlock()
	for _, p := range fwds {
		if _, err := d.tr.Forward(ctx, d.cfg.Serial, p, 0); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if owned {
		name := fridaName(d.cfg.Frida, d.nameSeed())
		if _, _, err := d.Shell(ctx, d.RootCmd("pkill -f "+name)); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (d *Device) nameSeed() string {
	if d.cfg.NameSeed != "" {
		return d.cfg.NameSeed
	}
	return d.cfg.Serial
}

func (d *Device) noteForward(local int) {
	d.mu.Lock()
	d.forwards = append(d.forwards, local)
	d.mu.Unlock()
}

func (d *Device) dropForward(local int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, p := range d.forwards {
		if p == local {
			d.forwards = append(d.forwards[:i], d.forwards[i+1:]...)
			return
		}
	}
}
