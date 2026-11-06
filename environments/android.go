package environments

import (
	"context"
	"fmt"
	"io"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/androiddev"
	"github.com/craggy-io/environment-services/internal/core"
)

// AndroidDevice is the device surface of one Android environment
// (emulator or attached physical device), obtained from
// Manager.Android. Shell commands are strings run by the device's
// own shell — unlike Handle.Exec, which is argv-only.
type AndroidDevice interface {
	// Serial is the adb serial of the underlying device.
	Serial() string
	// Shell runs a shell command as the shell user.
	Shell(ctx context.Context, cmd string) (output string, exit int, err error)
	// RootShell runs a shell command as root (requires a rooted image).
	RootShell(ctx context.Context, cmd string) (output string, exit int, err error)
	// Install installs an APK from r; replace permits downgrade-free reinstall.
	Install(ctx context.Context, apk io.Reader, replace bool) error
	Uninstall(ctx context.Context, pkg string) error
	StartApp(ctx context.Context, pkg string) error
	ForceStop(ctx context.Context, pkg string) error
	Tap(ctx context.Context, x, y int) error
	Swipe(ctx context.Context, x1, y1, x2, y2, durationMS int) error
	KeyEvent(ctx context.Context, code int) error
	Text(ctx context.Context, text string) error
	// PackageSnapshot describes one installed package.
	PackageSnapshot(ctx context.Context, pkg string) (envspec.PackageSnapshot, error)
	// ApplyPackageSequenceAndAction applies package mutations in
	// order, then performs action, under one receipted protocol.
	ApplyPackageSequenceAndAction(ctx context.Context, steps []envspec.PackageMutation, action envspec.PackageAction) (envspec.PackageSequenceActionReceipt, error)
	// BackupToLocalTransport runs a local-transport backup of pkg.
	BackupToLocalTransport(ctx context.Context, pkg string) (envspec.BackupReceipt, error)
	// Logcat streams logcat output; call the returned func to stop.
	Logcat(ctx context.Context, args string) (io.ReadCloser, func() error, error)
	// Screenshot returns a PNG of the current display.
	Screenshot(ctx context.Context) ([]byte, error)
	// StartTLSChallenge starts a host-owned TLS peer presenting a leaf for
	// opts.ExpectedHost from an untrusted ephemeral CA; completed
	// handshakes are receipted.
	StartTLSChallenge(ctx context.Context, opts envspec.TLSChallengeOptions) (Challenge[envspec.TLSChallengeReceipt], error)
	// StartCleartextChallenge starts a host-owned HTTP-only peer on an
	// unguessable path; every request is receipted by path digest.
	StartCleartextChallenge(ctx context.Context, opts envspec.CleartextChallengeOptions) (Challenge[envspec.CleartextChallengeReceipt], error)
}

// Challenge is a host-owned network peer for the app under test. The
// device reaches it at URL (by default through the emulator's host alias
// envspec.ChallengeHostAlias; a physical device needs an explicit
// AdvertiseHost). The environment's lease owns it: release, expiry,
// reset or destroy closes it, as do Close and cancelling the ctx it was
// started with. Receipt can still be read after it closes.
type Challenge[R any] interface {
	URL() string
	Receipt() R
	Close() error
}

// deviceInstance is implemented by Android adapter instances.
type deviceInstance interface{ Device() *androiddev.Device }

// androidDevice resolves the device through the checked handle on
// every call: a released or reassigned environment fails the lease
// check and the device stops answering.
type androidDevice struct {
	m *Manager
	h core.Handle
}

var _ AndroidDevice = (*androidDevice)(nil)

func (d *androidDevice) device() (*androiddev.Device, error) {
	inst, err := d.m.core.Instance(d.h)
	if err != nil {
		return nil, err
	}
	di, ok := inst.(deviceInstance)
	if !ok || di.Device() == nil {
		return nil, fmt.Errorf("%w: environment %s is not an Android device", envspec.ErrInvalid, d.h.ID())
	}
	return di.Device(), nil
}

// Serial cannot fail in the interface; a stale device reports "".
func (d *androidDevice) Serial() string {
	dev, err := d.device()
	if err != nil {
		return ""
	}
	return dev.Serial()
}

func (d *androidDevice) Shell(ctx context.Context, cmd string) (string, int, error) {
	dev, err := d.device()
	if err != nil {
		return "", 0, err
	}
	return dev.Shell(ctx, cmd)
}

func (d *androidDevice) RootShell(ctx context.Context, cmd string) (string, int, error) {
	dev, err := d.device()
	if err != nil {
		return "", 0, err
	}
	return dev.RootShell(ctx, cmd)
}

func (d *androidDevice) Install(ctx context.Context, apk io.Reader, replace bool) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.Install(ctx, apk, replace)
}

func (d *androidDevice) Uninstall(ctx context.Context, pkg string) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.Uninstall(ctx, pkg)
}

func (d *androidDevice) StartApp(ctx context.Context, pkg string) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.StartApp(ctx, pkg)
}

func (d *androidDevice) ForceStop(ctx context.Context, pkg string) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.ForceStop(ctx, pkg)
}

func (d *androidDevice) Tap(ctx context.Context, x, y int) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.Tap(ctx, x, y)
}

func (d *androidDevice) Swipe(ctx context.Context, x1, y1, x2, y2, durationMS int) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.Swipe(ctx, x1, y1, x2, y2, durationMS)
}

func (d *androidDevice) KeyEvent(ctx context.Context, code int) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.KeyEvent(ctx, code)
}

func (d *androidDevice) Text(ctx context.Context, text string) error {
	dev, err := d.device()
	if err != nil {
		return err
	}
	return dev.Text(ctx, text)
}

func (d *androidDevice) PackageSnapshot(ctx context.Context, pkg string) (envspec.PackageSnapshot, error) {
	dev, err := d.device()
	if err != nil {
		return envspec.PackageSnapshot{}, err
	}
	return dev.PackageSnapshot(ctx, pkg)
}

func (d *androidDevice) ApplyPackageSequenceAndAction(ctx context.Context, steps []envspec.PackageMutation, action envspec.PackageAction) (envspec.PackageSequenceActionReceipt, error) {
	dev, err := d.device()
	if err != nil {
		return envspec.PackageSequenceActionReceipt{}, err
	}
	return dev.ApplyPackageSequenceAndAction(ctx, steps, action)
}

func (d *androidDevice) BackupToLocalTransport(ctx context.Context, pkg string) (envspec.BackupReceipt, error) {
	dev, err := d.device()
	if err != nil {
		return envspec.BackupReceipt{}, err
	}
	return dev.BackupToLocalTransport(ctx, pkg)
}

func (d *androidDevice) Logcat(ctx context.Context, args string) (io.ReadCloser, func() error, error) {
	dev, err := d.device()
	if err != nil {
		return nil, nil, err
	}
	return dev.Logcat(ctx, args)
}

func (d *androidDevice) Screenshot(ctx context.Context) ([]byte, error) {
	dev, err := d.device()
	if err != nil {
		return nil, err
	}
	return dev.Screenshot(ctx)
}

func (d *androidDevice) StartTLSChallenge(ctx context.Context, opts envspec.TLSChallengeOptions) (Challenge[envspec.TLSChallengeReceipt], error) {
	dev, err := d.device()
	if err != nil {
		return nil, err
	}
	c, err := dev.StartTLSChallenge(ctx, opts)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (d *androidDevice) StartCleartextChallenge(ctx context.Context, opts envspec.CleartextChallengeOptions) (Challenge[envspec.CleartextChallengeReceipt], error) {
	dev, err := d.device()
	if err != nil {
		return nil, err
	}
	c, err := dev.StartCleartextChallenge(ctx, opts)
	if err != nil {
		return nil, err
	}
	return c, nil
}
