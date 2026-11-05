// Package androidphys is the physical Android adapter: it attaches
// to devices that exist independently of the manager (USB/network
// phones and tablets) and never creates, destroys or resets them.
// Attach verifies identity (adb state "device" and ro.serialno) and
// takes the exclusive attachment core's lease represents; release
// disconnects only — removing this attachment's adb forwards and a
// frida-server it started. Device access over the serial is
// internal/androiddev, shared with the emulator adapter.
//
// Exclusivity is single-host: core's lease row is the authority
// within this manager (one manager per state dir, file-locked), and
// the adapter additionally refuses a second concurrent Attach of the
// same serial in this process. No cross-host leasing is attempted.
package androidphys

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/androiddev"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// ParamSerial names the spec param carrying the adb serial to attach.
const ParamSerial = "adb.serial"

// Adapter is the physical-device adapter.
type Adapter struct {
	ADB *adb.Client
	// Transport overrides the adb transport (tests). Nil means ADB.
	Transport androiddev.Transport
	// FridaServer locates frida-server binaries (optional).
	FridaServer *androiddev.FridaServer
	// ProxyCAPEM is the proxy CA certificate (PEM) to install.
	ProxyCAPEM string
	// CaptureFn pins evidence before repairs.
	CaptureFn func(envID string, f androiddev.FailureInfo) string

	mu         sync.Mutex
	attached   map[string]string // serial → envID, this process
	components *componentSink
}

type componentSink struct {
	fn func(envID string, c adapter.ComponentChange)
}

// New builds the adapter over the local adb server.
func New() *Adapter {
	return &Adapter{ADB: adb.New(), attached: map[string]string{}}
}

func (a *Adapter) transport() androiddev.Transport {
	if a.Transport != nil {
		return a.Transport
	}
	if a.ADB == nil {
		a.ADB = adb.New()
	}
	return androiddev.ClientTransport{C: a.ADB}
}

// Describe implements adapter.Adapter.
func (a *Adapter) Describe() adapter.Description {
	return adapter.Description{Name: "android-phys", Kinds: []envspec.Kind{envspec.Android},
		Relationship: adapter.Attached, Version: "1"}
}

// WatchComponents implements adapter.ComponentWatcher.
func (a *Adapter) WatchComponents(fn func(envID string, c adapter.ComponentChange)) (stop func()) {
	sink := &componentSink{fn: fn}
	a.mu.Lock()
	a.components = sink
	a.mu.Unlock()
	return func() {
		a.mu.Lock()
		if a.components == sink {
			a.components = nil
		}
		a.mu.Unlock()
	}
}

func (a *Adapter) componentChanged(envID string, c adapter.ComponentChange) {
	a.mu.Lock()
	sink := a.components
	a.mu.Unlock()
	if sink != nil {
		sink.fn(envID, c)
	}
}

func isEmulatorSerial(serial string) bool { return strings.HasPrefix(serial, "emulator-") }

// Check reports whether the adb server answers, without starting
// anything on any device.
func (a *Adapter) Check(ctx context.Context) error {
	if _, err := a.transport().Version(ctx); err != nil {
		return adapter.BackendDown(err)
	}
	return nil
}

// Validate requires the serial param and rejects emulator serials
// and creation-flavoured Android options that mean nothing here.
func (a *Adapter) Validate(ctx context.Context, op adapter.OpContext, s envspec.Spec) error {
	serial := strings.TrimSpace(s.Params[ParamSerial])
	if serial == "" {
		return fmt.Errorf("%w: physical attach needs params {%q: \"<adb serial>\"}", envspec.ErrInvalid, ParamSerial)
	}
	if isEmulatorSerial(serial) {
		return fmt.Errorf("%w: %s is an emulator serial; use the emulator adapter", envspec.ErrInvalid, serial)
	}
	if o := s.Android; o != nil {
		if o.ProxyCA && (o.Root == envspec.RootNone || o.Root == "") {
			return fmt.Errorf("%w: ProxyCA installs a system CA and needs root", envspec.ErrInvalid)
		}
		if o.Frida != nil && (o.Root == envspec.RootNone || o.Root == "") {
			return fmt.Errorf("%w: frida-server needs root; set Android.Root to how this device is rooted", envspec.ErrInvalid)
		}
	}
	return nil
}

// Cost: a physical device takes no host RAM worth admitting against
// beyond its adb traffic; count only the spec's declared resources.
func (a *Adapter) Cost(s envspec.Spec) adapter.Cost {
	return adapter.Cost{Pool: envspec.PoolHost, CPUs: s.Resources.CPUs, MemoryMB: s.Resources.MemoryMB, DiskMB: s.Resources.DiskMB}
}

// Discover lists non-emulator serials adb reports as devices.
// Devices already attached by this adapter carry their env; the rest
// are candidates for Attach (Foreign when some other state than
// "device" — visible but not attachable now).
func (a *Adapter) Discover(ctx context.Context, scope adapter.Scope) ([]adapter.Candidate, error) {
	devs, err := a.transport().Devices(ctx)
	if err != nil {
		return nil, adapter.BackendDown(err)
	}
	a.mu.Lock()
	attached := make(map[string]string, len(a.attached))
	for k, v := range a.attached {
		attached[k] = v
	}
	a.mu.Unlock()
	var out []adapter.Candidate
	for _, dev := range devs {
		if isEmulatorSerial(dev.Serial) {
			continue
		}
		envID := attached[dev.Serial]
		if scope.EnvID != "" && envID != scope.EnvID {
			continue
		}
		c := adapter.Candidate{Ref: adapter.Ref{EnvID: envID, External: dev.Serial},
			Foreign: dev.State != "device", Detail: "adb state " + dev.State}
		if envID != "" {
			c.Ref.Name = adapter.NameFor(envID)
			c.Foreign = false
		}
		out = append(out, c)
	}
	return out, nil
}

// Observe reports attachment presence: the device present and in
// state "device" is Present (Attached while this process holds it);
// a definitive absence from adb's list is Absent; an unreachable adb
// server or a mid-transition state is Unknown.
func (a *Adapter) Observe(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Observation, error) {
	obs := adapter.Observation{Ref: ref, Workspace: "/data/local/tmp/work"}
	serial := ref.External
	if serial == "" {
		obs.Presence = adapter.Unknown
		obs.Detail = "no serial recorded for env"
		return obs, nil
	}
	obs.Endpoints = map[string]string{"adb": serial}
	a.mu.Lock()
	_, held := a.attached[serial]
	a.mu.Unlock()
	obs.Attached = held
	devs, err := a.transport().Devices(ctx)
	if err != nil {
		obs.Presence = adapter.Unknown
		obs.Detail = adapter.BackendDown(err).Error()
		return obs, nil
	}
	for _, dev := range devs {
		if dev.Serial != serial {
			continue
		}
		if dev.State == "device" {
			obs.Presence = adapter.Present
		} else {
			obs.Presence = adapter.Unknown
			obs.Detail = "adb state " + dev.State
		}
		return obs, nil
	}
	obs.Presence = adapter.Absent
	obs.Attached = false
	return obs, nil
}

// verifyIdentity checks the device behind serial is the one meant:
// adb state must be "device" and ro.serialno must match the serial.
// Network serials (host:port) cannot equal ro.serialno, so for those
// the property must at least be readable and is recorded in the
// returned identity for the caller to cite.
func (a *Adapter) verifyIdentity(ctx context.Context, serial string) (string, error) {
	tr := a.transport()
	devs, err := tr.Devices(ctx)
	if err != nil {
		return "", adapter.BackendDown(err)
	}
	state := ""
	for _, dev := range devs {
		if dev.Serial == serial {
			state = dev.State
			break
		}
	}
	if state == "" {
		return "", fmt.Errorf("%w: device %s is not attached to adb", envspec.ErrNotFound, serial)
	}
	if state != "device" {
		return "", fmt.Errorf("%w: device %s is in adb state %q, not \"device\" (authorise USB debugging on the device)", envspec.ErrNotReady, serial, state)
	}
	out, code, err := tr.Run(ctx, serial, "getprop ro.serialno")
	if err != nil || code != 0 {
		return "", fmt.Errorf("android: read ro.serialno on %s (exit %d): %w", serial, code, err)
	}
	roSerial := strings.TrimSpace(out)
	if roSerial == "" {
		return "", fmt.Errorf("android: device %s reports an empty ro.serialno; refusing to attach to an unidentified device", serial)
	}
	if !strings.Contains(serial, ":") && roSerial != serial {
		return "", fmt.Errorf("%w: adb serial %s but ro.serialno is %s; refusing to attach to the wrong device", envspec.ErrConflict, serial, roSerial)
	}
	return roSerial, nil
}

func (a *Adapter) claim(serial, envID string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.attached == nil {
		a.attached = map[string]string{}
	}
	if other, held := a.attached[serial]; held && other != envID {
		return fmt.Errorf("%w: device %s is already attached to %s (exclusive lease)", envspec.ErrConflict, serial, other)
	}
	a.attached[serial] = envID
	return nil
}

func (a *Adapter) releaseClaim(serial, envID string) {
	a.mu.Lock()
	if a.attached[serial] == envID {
		delete(a.attached, serial)
	}
	a.mu.Unlock()
}

// Attach verifies identity, takes the exclusive attachment and wires
// the device. It creates nothing on the device beyond the workspace
// directory; managed components verify best-effort (degraded, not
// fatal), as on emulators.
func (a *Adapter) Attach(ctx context.Context, op adapter.OpContext, s envspec.Spec, progress func(envspec.Progress)) (adapter.Instance, error) {
	if err := a.Validate(ctx, op, s); err != nil {
		return nil, err
	}
	serial := strings.TrimSpace(s.Params[ParamSerial])
	roSerial, err := a.verifyIdentity(ctx, serial)
	if err != nil {
		return nil, err
	}
	if err := a.claim(serial, op.EnvID); err != nil {
		return nil, err
	}
	inst := &Instance{ad: a, envID: op.EnvID, serial: serial, roSerial: roSerial, spec: s}
	inst.wire()
	if progress != nil {
		progress(envspec.Progress{Op: "attach", Stage: "components", EnvID: op.EnvID, Message: "verifying managed components"})
	}
	_, _, _ = a.transport().Run(ctx, serial, "mkdir -p /data/local/tmp/work")
	for _, name := range []string{"adb", "root", "frida", "proxy-ca"} {
		_ = inst.EnsureComponent(ctx, name) // degraded is recorded, not fatal
	}
	return inst, nil
}

// Open re-materialises an attachment from its recorded Ref after a
// restart. The device still exists independently; Open re-takes the
// in-process claim without re-verifying identity (Observe and the
// component probes report the truth from there).
func (a *Adapter) Open(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Instance, error) {
	if ref.External == "" {
		return nil, fmt.Errorf("android: no serial recorded for %s", ref.EnvID)
	}
	if err := a.claim(ref.External, ref.EnvID); err != nil {
		return nil, err
	}
	inst := &Instance{ad: a, envID: ref.EnvID, serial: ref.External}
	inst.wire()
	return inst, nil
}

// Instance is one attached physical device.
type Instance struct {
	ad       *Adapter
	envID    string
	serial   string
	roSerial string
	spec     envspec.Spec
	dev      *androiddev.Device

	mu       sync.Mutex
	detached bool
}

// Device returns the device-over-serial for the facade.
func (i *Instance) Device() *androiddev.Device { return i.dev }

// Serial returns the adb serial.
func (i *Instance) Serial() string { return i.serial }

func (i *Instance) wire() {
	a := i.ad
	envID := i.envID
	cfg := androiddev.Config{
		Serial: i.serial, NameSeed: i.serial,
		FridaServer: a.FridaServer, ProxyCAPEM: a.ProxyCAPEM,
		OnComponent: func(c adapter.ComponentChange) { a.componentChanged(envID, c) },
	}
	if a.CaptureFn != nil {
		fn := a.CaptureFn
		cfg.CaptureFn = func(f androiddev.FailureInfo) string { return fn(envID, f) }
	}
	if o := i.spec.Android; o != nil {
		cfg.Root, cfg.Frida, cfg.ProxyCA, cfg.Repair, cfg.ABI = o.Root, o.Frida, o.ProxyCA, o.Repair, o.ABI
	}
	i.dev = androiddev.New(a.transport(), cfg)
}

// Ref implements adapter.Instance.
func (i *Instance) Ref() adapter.Ref {
	return adapter.Ref{EnvID: i.envID, External: i.serial, Name: adapter.NameFor(i.envID)}
}

// Dispose disconnects only: it removes this attachment's forwards
// and a frida-server this attachment started, and releases the
// exclusive claim. The device, its apps and its data are untouched —
// physical devices are never destroyed or reset.
func (i *Instance) Dispose(ctx context.Context, op adapter.OpContext, disp adapter.Disposition) error {
	if disp == adapter.Destroy || disp == adapter.ResetToPool {
		return fmt.Errorf("%w: physical devices are disconnected, never %s", adapter.ErrUnsupported, disp)
	}
	i.mu.Lock()
	if i.detached {
		i.mu.Unlock()
		return nil
	}
	i.detached = true
	i.mu.Unlock()
	err := i.dev.DetachCleanup(ctx)
	i.ad.releaseClaim(i.serial, i.envID)
	return err
}

// Exec implements adapter.Executor.
func (i *Instance) Exec(ctx context.Context, op adapter.OpContext, cmd adapter.ExecCmd) (*adapter.Process, error) {
	return i.dev.ExecProcess(ctx, cmd)
}

// Arch implements adapter.Arched.
func (i *Instance) Arch() string { return i.dev.Arch() }

// Collectors implements adapter.CollectorProvider.
func (i *Instance) Collectors() []signalcore.Collector { return i.dev.Collectors() }

// CollectorEnvInfo implements adapter.CollectorProvider.
func (i *Instance) CollectorEnvInfo() signalcore.EnvInfo {
	return i.dev.CollectorEnvInfo(i.envID, i.spec.Profile)
}

// Probes implements adapter.ProbeProvider.
func (i *Instance) Probes() []adapter.Probe { return i.dev.Probes() }

// Components implements adapter.ComponentReporter.
func (i *Instance) Components() []envspec.ComponentState { return i.dev.Components() }

// EnsureComponent implements adapter.ComponentReporter.
func (i *Instance) EnsureComponent(ctx context.Context, name string) error {
	return i.dev.EnsureComponent(ctx, name)
}

// InjectEnv: no per-exec env delivery on Android (by design).
func (i *Instance) InjectEnv(name, value string) {}

// InjectFile pushes a credential file under /data/local/tmp (or an
// absolute path) after ensuring adb health.
func (i *Instance) InjectFile(ctx context.Context, path string, value []byte) error {
	if err := i.EnsureComponent(ctx, "adb"); err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/") {
		path = "/data/local/tmp/" + path
	}
	return i.ad.transport().SyncSend(ctx, i.serial, path, 0o100400, strings.NewReader(string(value)))
}

var (
	_ adapter.Adapter           = (*Adapter)(nil)
	_ adapter.Attacher          = (*Adapter)(nil)
	_ adapter.Checker           = (*Adapter)(nil)
	_ adapter.Coster            = (*Adapter)(nil)
	_ adapter.ComponentWatcher  = (*Adapter)(nil)
	_ adapter.Instance          = (*Instance)(nil)
	_ adapter.Executor          = (*Instance)(nil)
	_ adapter.CredInjector      = (*Instance)(nil)
	_ adapter.Arched            = (*Instance)(nil)
	_ adapter.CollectorProvider = (*Instance)(nil)
	_ adapter.ProbeProvider     = (*Instance)(nil)
	_ adapter.ComponentReporter = (*Instance)(nil)
)
