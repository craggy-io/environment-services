package androidemu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/androiddev"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// Instance is one running emulator. It implements the adapter
// instance contract by delegating device work to androiddev.Device.
type Instance struct {
	ad       *Adapter
	envID    string
	serial   string
	port     int // console port (even)
	grpc     int
	pid      int
	avd      string
	spec     envspec.Spec
	variant  *envspec.AndroidOptions
	dev      *androiddev.Device
	writable bool // read-only multi-instance boots cannot save snapshots

	mu        sync.Mutex
	destroyed bool
}

// Device returns the device-over-serial (the environments facade
// extracts it through core.Manager.Instance for AndroidDevice).
func (i *Instance) Device() *androiddev.Device { return i.dev }

// Serial returns the adb serial.
func (i *Instance) Serial() string { return i.serial }

// Ref implements adapter.Instance.
func (i *Instance) Ref() adapter.Ref {
	return adapter.Ref{EnvID: i.envID, External: i.serial, Name: adapter.NameFor(i.envID)}
}

// wire builds the androiddev.Device for this instance (components
// and collectors bind to it). Construction probes nothing.
func (i *Instance) wire() {
	d := i.ad
	envID := i.envID
	cfg := androiddev.Config{
		Serial: i.serial, NameSeed: i.avd,
		FridaServer: d.FridaServer, ProxyCAPEM: d.ProxyCAPEM,
		OnComponent: func(c adapter.ComponentChange) { d.componentChanged(envID, c) },
	}
	if d.CaptureFn != nil {
		fn := d.CaptureFn
		cfg.CaptureFn = func(f androiddev.FailureInfo) string { return fn(envID, f) }
	}
	if i.variant != nil {
		cfg.Root, cfg.Frida, cfg.ProxyCA = i.variant.Root, i.variant.Frida, i.variant.ProxyCA
		cfg.Repair, cfg.ABI = i.variant.Repair, i.variant.ABI
	}
	i.dev = androiddev.New(d.transport(), cfg)
}

func (i *Instance) saveCache() {
	_ = os.MkdirAll(i.ad.instancesDir(), 0o755)
	c := instanceCache{Env: i.envID, PID: i.pid, Port: i.port, GRPC: i.grpc,
		Serial: i.serial, AVD: i.avd, Spec: i.spec, Variant: i.variant, Writable: i.writable}
	b, _ := json.MarshalIndent(c, "", "  ")
	_ = os.WriteFile(i.ad.cachePath(i.envID), b, 0o644)
}

func (i *Instance) dropCache() { _ = os.Remove(i.ad.cachePath(i.envID)) }

// Dispose implements adapter.Instance. Destroy kills the emulator
// and frees the port; Disconnect removes only our attachments
// (forwards/frida) and leaves the emulator running for recovery;
// ResetToPool is normally driven through Reset (core keeps the
// instance), and here is the same detach as Disconnect.
func (i *Instance) Dispose(ctx context.Context, op adapter.OpContext, disp adapter.Disposition) error {
	switch disp {
	case adapter.Destroy:
		return i.destroy(ctx)
	case adapter.Disconnect, adapter.ResetToPool, adapter.RetainWorkspace:
		return i.dev.DetachCleanup(ctx)
	default:
		return fmt.Errorf("%w: disposition %s", adapter.ErrUnsupported, disp)
	}
}

// destroy kills the emulator and frees the port.
func (i *Instance) destroy(ctx context.Context) error {
	i.mu.Lock()
	if i.destroyed {
		i.mu.Unlock()
		return nil
	}
	i.destroyed = true
	i.mu.Unlock()
	_, _ = i.ad.emu(ctx, i.serial, "kill")
	if p := emulatorProcess(i.pid, i.port); p != nil {
		killTree(p)
	}
	i.ad.freePort(i.port)
	i.dropCache()
	return nil
}

// Exec implements adapter.Executor via the device shell.
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

// Probes implements adapter.ProbeProvider (the managed components as
// component activities; core gates Repair on MutationPolicy).
func (i *Instance) Probes() []adapter.Probe { return i.dev.Probes() }

// Components implements adapter.ComponentReporter.
func (i *Instance) Components() []envspec.ComponentState { return i.dev.Components() }

// EnsureComponent implements adapter.ComponentReporter.
func (i *Instance) EnsureComponent(ctx context.Context, name string) error {
	return i.dev.EnsureComponent(ctx, name)
}

// --- Snapshotter / Resetter via emulator console ---

// Save implements adapter.Snapshotter.
func (i *Instance) Save(ctx context.Context, op adapter.OpContext, name string) error {
	if !i.writable {
		return withHint(fmt.Errorf("snapshot save is disabled on read-only multi-instance boots"),
			"provision with params {\"avd.writable\": \"true\"} when you need new named snapshots (loading and clean-reset work on read-only boots)")
	}
	_, err := i.ad.emu(ctx, i.serial, "avd snapshot save "+name)
	return err
}

// Load implements adapter.Snapshotter.
func (i *Instance) Load(ctx context.Context, op adapter.OpContext, name string) error {
	_, err := i.ad.emu(ctx, i.serial, "avd snapshot load "+name)
	return err
}

// Snapshots implements adapter.Snapshotter.
func (i *Instance) Snapshots(ctx context.Context, op adapter.OpContext) ([]string, error) {
	out, err := i.ad.emu(ctx, i.serial, "avd snapshot list")
	if err != nil {
		return nil, err
	}
	return parseSnapshotList(out), nil
}

func parseSnapshotList(out string) []string {
	var names []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		// Data rows look like "--  <tag>  <size> ..." or "<num> <tag> ...".
		if len(fields) < 3 {
			continue
		}
		isRow := fields[0] == "--"
		if !isRow {
			if _, err := strconv.Atoi(fields[0]); err == nil {
				isRow = true
			}
		}
		if isRow {
			names = append(names, fields[1])
		}
	}
	return names
}

// Reset restores the clean snapshot (warm-pool reuse).
func (i *Instance) Reset(ctx context.Context, op adapter.OpContext) error {
	return i.Load(ctx, op, "clean")
}

// --- CredInjector ---

// InjectEnv: Android has no per-exec env delivery in the design;
// this is a no-op by design.
func (i *Instance) InjectEnv(name, value string) {}

// InjectFile root-pushes a credential file to /data/local/tmp (or an
// absolute path when rooted).
func (i *Instance) InjectFile(ctx context.Context, path string, value []byte) error {
	if err := i.EnsureComponent(ctx, "adb"); err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/") {
		path = "/data/local/tmp/" + path
	}
	return i.ad.transport().SyncSend(ctx, i.serial, path, 0o100400, bytes.NewReader(value))
}

var (
	_ adapter.Instance          = (*Instance)(nil)
	_ adapter.Executor          = (*Instance)(nil)
	_ adapter.Snapshotter       = (*Instance)(nil)
	_ adapter.Resetter          = (*Instance)(nil)
	_ adapter.CredInjector      = (*Instance)(nil)
	_ adapter.Arched            = (*Instance)(nil)
	_ adapter.CollectorProvider = (*Instance)(nil)
	_ adapter.ProbeProvider     = (*Instance)(nil)
	_ adapter.ComponentReporter = (*Instance)(nil)
)
