package androidphys

// Live physical test: attaches to a real device named by
// CRAGGY_TEST_ANDROID_PHYS_SERIAL, checks identity and exec, then
// disconnects. The device is otherwise untouched.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

func TestLivePhysicalAttach(t *testing.T) {
	serial := os.Getenv("CRAGGY_TEST_ANDROID_PHYS_SERIAL")
	if serial == "" {
		t.Skip("set CRAGGY_TEST_ANDROID_PHYS_SERIAL=<adb serial> to run against a real device")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	a := New()
	op := adapter.OpContext{EnvID: "env_live_phys", Generation: 1}
	spec := envspec.Spec{Kind: envspec.Android, Params: map[string]string{ParamSerial: serial}}
	inst, err := a.Attach(ctx, op, spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	pi := inst.(*Instance)
	if pi.roSerial == "" {
		t.Fatal("ro.serialno not recorded")
	}
	dev := pi.Device()
	if out, code, err := dev.Shell(ctx, "echo phys-ok"); err != nil || code != 0 || !strings.Contains(out, "phys-ok") {
		t.Fatalf("shell = %q code=%d err=%v", out, code, err)
	}
	if pkgs, err := dev.Packages(ctx, "android"); err != nil || len(pkgs) == 0 {
		t.Fatalf("packages = %d err=%v", len(pkgs), err)
	}
	if err := inst.Dispose(ctx, op, adapter.Disconnect); err != nil {
		t.Fatal(err)
	}
	obs, _ := a.Observe(ctx, op, inst.Ref())
	if obs.Attached {
		t.Fatal("still attached after disconnect")
	}
}
