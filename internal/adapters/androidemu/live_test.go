package androidemu

// Live emulator test: provisions one real emulator through the
// adapter (Create -> exec -> snapshot -> reset -> destroy) against
// the locally installed Android SDK. Gated: CRAGGY_TEST_ANDROID_EMU=1
// (and a prepared or downloadable system image; first run prepares).

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

func TestLiveEmulatorLifecycle(t *testing.T) {
	if os.Getenv("CRAGGY_TEST_ANDROID_EMU") != "1" {
		t.Skip("set CRAGGY_TEST_ANDROID_EMU=1 to run against a real Android SDK")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	d := New(t.TempDir())
	if err := d.Check(ctx); err != nil {
		t.Skipf("android SDK unavailable: %v", err)
	}
	op := adapter.OpContext{EnvID: "env_live", Generation: 1}
	spec := envspec.Spec{Kind: envspec.Android, Profile: "android",
		Android: &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB}}
	inst, err := d.Create(ctx, op, spec, func(p envspec.Progress) { t.Logf("%s: %s", p.Stage, p.Message) })
	if err != nil {
		t.Fatal(err)
	}
	emu := inst.(*Instance)
	defer func() { _ = inst.Dispose(context.Background(), op, adapter.Destroy) }()

	obs, err := d.Observe(ctx, op, inst.Ref())
	if err != nil || obs.Presence != adapter.Present {
		t.Fatalf("observe = %+v err=%v", obs, err)
	}
	dev := emu.Device()
	if out, code, err := dev.Shell(ctx, "getprop ro.build.version.sdk"); err != nil || code != 0 || !strings.Contains(out, "34") {
		t.Fatalf("sdk prop = %q code=%d err=%v", out, code, err)
	}
	if out, _, err := dev.RootShell(ctx, "id"); err != nil || !strings.Contains(out, "uid=0") {
		t.Fatalf("root = %q err=%v", out, err)
	}
	if png, err := dev.Screenshot(ctx); err != nil || len(png) < 1024 {
		t.Fatalf("screenshot = %d bytes err=%v", len(png), err)
	}
	// Reset to the clean snapshot and confirm the device answers.
	if err := emu.Reset(ctx, op); err != nil {
		t.Fatal(err)
	}
	if err := dev.Health(ctx); err != nil {
		t.Fatalf("health after reset: %v", err)
	}
	names, err := emu.Snapshots(ctx, op)
	if err != nil || len(names) == 0 {
		t.Fatalf("snapshots = %v err=%v", names, err)
	}
	// Destroy verifies Absent through Observe.
	if err := inst.Dispose(ctx, op, adapter.Destroy); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if obs, err := d.Observe(ctx, op, inst.Ref()); err == nil && obs.Presence == adapter.Absent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("emulator still present after destroy")
		}
		time.Sleep(2 * time.Second)
	}
}
