package androidphys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/androiddev"
)

func physSpec() envspec.Spec {
	return envspec.Spec{Kind: envspec.Android, Params: map[string]string{ParamSerial: "PIXEL1"},
		Android: &envspec.AndroidOptions{Root: envspec.RootADB}}
}

func TestValidateRequiresPhysicalSerial(t *testing.T) {
	a := New()
	ctx := context.Background()
	if err := a.Validate(ctx, adapter.OpContext{}, envspec.Spec{}); !errors.Is(err, envspec.ErrInvalid) {
		t.Fatalf("missing serial = %v", err)
	}
	emu := envspec.Spec{Params: map[string]string{ParamSerial: "emulator-5554"}}
	if err := a.Validate(ctx, adapter.OpContext{}, emu); !errors.Is(err, envspec.ErrInvalid) {
		t.Fatalf("emulator serial = %v", err)
	}
	fridaNoRoot := physSpec()
	fridaNoRoot.Android = &envspec.AndroidOptions{Frida: &envspec.FridaOptions{}}
	if err := a.Validate(ctx, adapter.OpContext{}, fridaNoRoot); err == nil {
		t.Fatal("frida without root accepted")
	}
	if err := a.Validate(ctx, adapter.OpContext{}, physSpec()); err != nil {
		t.Fatal(err)
	}
	if a.Describe().Relationship != adapter.Attached {
		t.Fatalf("describe = %+v", a.Describe())
	}
}

func TestDiscoverListsNonEmulatorDevices(t *testing.T) {
	f := newFakePhys()
	f.devices = append(f.devices, adb.Device{Serial: "TABLET9", State: "unauthorized"})
	a := New()
	a.Transport = f
	got, err := a.Discover(context.Background(), adapter.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want PIXEL1 + TABLET9 (no emulator)", got)
	}
	for _, c := range got {
		if c.Ref.External == "TABLET9" && !c.Foreign {
			t.Fatalf("unauthorized device must be Foreign: %+v", c)
		}
		if c.Ref.External == "PIXEL1" && c.Foreign {
			t.Fatalf("attachable device marked Foreign: %+v", c)
		}
	}
}

func TestAttachVerifiesIdentity(t *testing.T) {
	ctx := context.Background()
	op := adapter.OpContext{EnvID: "env_1"}

	// Happy path.
	f := newFakePhys()
	a := New()
	a.Transport = f
	inst, err := a.Attach(ctx, op, physSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref().External != "PIXEL1" || inst.Ref().Name != "craggy-env_1" {
		t.Fatalf("ref = %+v", inst.Ref())
	}
	pi := inst.(*Instance)
	if pi.Device() == nil || pi.Device().Serial() != "PIXEL1" {
		t.Fatalf("device = %v", pi.Device())
	}
	// Exclusive: second env cannot attach the same serial.
	if _, err := a.Attach(ctx, adapter.OpContext{EnvID: "env_2"}, physSpec(), nil); !errors.Is(err, envspec.ErrConflict) {
		t.Fatalf("double attach = %v, want conflict", err)
	}
	// Observe while held: Present + Attached.
	obs, _ := a.Observe(ctx, op, inst.Ref())
	if obs.Presence != adapter.Present || !obs.Attached {
		t.Fatalf("observe = %+v", obs)
	}
	// Disconnect releases the claim; the device remains present.
	if err := inst.Dispose(ctx, op, adapter.Disconnect); err != nil {
		t.Fatal(err)
	}
	obs, _ = a.Observe(ctx, op, inst.Ref())
	if obs.Presence != adapter.Present || obs.Attached {
		t.Fatalf("after disconnect = %+v, want Present unattached", obs)
	}
	inst2, err := a.Attach(ctx, adapter.OpContext{EnvID: "env_2"}, physSpec(), nil)
	if err != nil {
		t.Fatalf("re-attach after disconnect: %v", err)
	}
	// Destroy/Reset are never offered on physical devices.
	if err := inst2.Dispose(ctx, op, adapter.Destroy); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("destroy disposition = %v", err)
	}

	// Wrong ro.serialno: refuse (wrong device behind the serial).
	f2 := newFakePhys()
	f2.roSerial = "OTHER"
	a2 := New()
	a2.Transport = f2
	if _, err := a2.Attach(ctx, op, physSpec(), nil); !errors.Is(err, envspec.ErrConflict) {
		t.Fatalf("serial mismatch = %v", err)
	}
	// Unauthorized state.
	f3 := newFakePhys()
	f3.devices = []adb.Device{{Serial: "PIXEL1", State: "unauthorized"}}
	a3 := New()
	a3.Transport = f3
	if _, err := a3.Attach(ctx, op, physSpec(), nil); !errors.Is(err, envspec.ErrNotReady) {
		t.Fatalf("unauthorized = %v", err)
	}
	// Missing device.
	f4 := newFakePhys()
	f4.devices = nil
	a4 := New()
	a4.Transport = f4
	if _, err := a4.Attach(ctx, op, physSpec(), nil); !errors.Is(err, envspec.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	// Network serial: ro.serialno need not equal host:port.
	f5 := newFakePhys()
	f5.devices = []adb.Device{{Serial: "192.168.1.5:5555", State: "device"}}
	a5 := New()
	a5.Transport = f5
	spec := physSpec()
	spec.Params[ParamSerial] = "192.168.1.5:5555"
	if _, err := a5.Attach(ctx, op, spec, nil); err != nil {
		t.Fatalf("network serial attach: %v", err)
	}
}

func TestAttachDetectsPhysicalABI(t *testing.T) {
	f := newFakePhys()
	f.cpuABI = "arm64-v8a"
	a := New()
	a.Transport = f
	inst, err := a.Attach(context.Background(), adapter.OpContext{EnvID: "env_arm64"}, physSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := inst.(*Instance)
	if got.spec.Android.ABI != "arm64-v8a" {
		t.Fatalf("physical ABI not propagated: spec=%q", got.spec.Android.ABI)
	}
}

func TestDetachRemovesOnlyOwnedFrida(t *testing.T) {
	ctx := context.Background()
	// Fake frida-server binary for repair to push.
	dir := t.TempDir()
	bin := filepath.Join(dir, "frida-server-"+fridaTestVersion()+"-android-x86_64")
	if err := os.WriteFile(bin, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakePhys()
	a := New()
	a.Transport = f
	a.FridaServer = &androiddev.FridaServer{ServerDir: dir}
	spec := physSpec()
	spec.Android.Frida = &envspec.FridaOptions{Version: fridaTestVersion()}
	inst, err := a.Attach(ctx, adapter.OpContext{EnvID: "env_1"}, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Attach ensured frida: pushed + started (fake pidof flips on setsid).
	if f.syncCount() == 0 {
		t.Fatal("frida binary was not pushed during ensure")
	}
	if err := inst.Dispose(ctx, adapter.OpContext{}, adapter.Disconnect); err != nil {
		t.Fatal(err)
	}
	if !f.saw("pkill -f") {
		t.Fatalf("detach did not stop the frida-server it started; cmds=%v", f.commands)
	}
	// Nothing destructive ran against the device itself.
	for _, bad := range []string{"pm uninstall", "rm -rf /data", "reboot"} {
		if f.saw(bad) {
			t.Fatalf("detach ran destructive command containing %q", bad)
		}
	}
}

func TestObserveUnknownWhenBackendDown(t *testing.T) {
	f := newFakePhys()
	f.devErr = errors.New("connection refused")
	a := New()
	a.Transport = f
	obs, err := a.Observe(context.Background(), adapter.OpContext{}, adapter.Ref{EnvID: "env_1", External: "PIXEL1"})
	if err != nil || obs.Presence != adapter.Unknown {
		t.Fatalf("observe = %+v err=%v", obs, err)
	}
	if _, err := a.Discover(context.Background(), adapter.Scope{}); !errors.Is(err, adapter.ErrBackendDown) {
		t.Fatalf("discover = %v", err)
	}
}
