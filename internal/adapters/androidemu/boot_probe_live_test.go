package androidemu

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

// TestLiveReadOnlySnapshotOnSecondPort exercises the failure shape seen in
// production: an API 35 clean snapshot on emulator-5556 with -read-only.
// The state directory is explicit and persistent so failed-boot logs survive
// a test process exit. No emulator is launched unless explicitly enabled.
func TestLiveReadOnlySnapshotOnSecondPort(t *testing.T) {
	if os.Getenv("CRAGGY_BOOT_PROBE") != "1" {
		t.Skip("set CRAGGY_BOOT_PROBE=1")
	}
	stateDir := os.Getenv("CRAGGY_BOOT_PROBE_DIR")
	if stateDir == "" {
		t.Fatal("CRAGGY_BOOT_PROBE_DIR must name a persistent diagnostic directory")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
	d := New(stateDir)
	if sdk := os.Getenv("ANDROID_SDK_ROOT"); sdk != "" {
		d.SDK = sdk
	}
	if err := d.Check(ctx); err != nil {
		t.Fatal(err)
	}
	options := &envspec.AndroidOptions{API: 35, Image: envspec.GoogleAPIs, Root: envspec.RootADB}
	profile := envspec.Profile{Name: "android-boot-probe", Kind: envspec.Android, Android: options}
	progress := func(p envspec.Progress) { t.Logf("%s: %s", p.Stage, p.Message) }
	if err := d.PrepareProfile(ctx, profile, progress); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.ports[5554] = "probe-reserved"
	d.mu.Unlock()
	defer d.freePort(5554)
	op := adapter.OpContext{EnvID: "env_boot_probe", Generation: 1}
	spec := envspec.Spec{Kind: envspec.Android, Profile: profile.Name, Android: options, Resources: envspec.Resources{MemoryMB: 3072, CPUs: 2}}
	start := time.Now()
	inst, err := d.Create(ctx, op, spec, progress)
	if err != nil {
		t.Fatalf("read-only quickboot after %s: %v", time.Since(start).Round(time.Second), err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = inst.Dispose(cleanup, op, adapter.Destroy)
	}()
	serial := inst.(*Instance).Serial()
	if serial != "emulator-5556" {
		t.Fatalf("serial=%s, want emulator-5556", serial)
	}
	out, code, err := inst.(*Instance).Device().Shell(ctx, "getprop sys.boot_completed")
	if err != nil || code != 0 || strings.TrimSpace(out) != "1" {
		t.Fatalf("boot completed=%q code=%d err=%v", out, code, err)
	}
	t.Logf("read-only clean snapshot booted %s in %s", serial, time.Since(start).Round(time.Second))
}
