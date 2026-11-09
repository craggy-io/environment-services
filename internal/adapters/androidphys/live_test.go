package androidphys

// Live physical test: attaches to a real device named by
// CRAGGY_TEST_ANDROID_PHYS_SERIAL, checks identity and exec, then
// disconnects. The device is otherwise untouched.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/core"
	"github.com/craggy-io/environment-services/internal/signalcore"
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

// TestLivePhysicalTransferThroughCore attaches the real device through
// the core manager and moves a tree with Handle.Push/Pull (the
// Transferrer path), then disconnects. Same gate as TestLivePhysicalAttach.
func TestLivePhysicalTransferThroughCore(t *testing.T) {
	serial := os.Getenv("CRAGGY_TEST_ANDROID_PHYS_SERIAL")
	if serial == "" {
		t.Skip("set CRAGGY_TEST_ANDROID_PHYS_SERIAL=<adb serial> to run against a real device")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	m, err := core.Open(ctx, core.Config{
		Dir: t.TempDir(), ManagerID: "mgr-phys-live",
		Adapters: []adapter.Adapter{New()},
		Signals:  signalcore.NewHub(),
		Profiles: []envspec.Profile{{Name: "phys-live", Kind: envspec.Android,
			Resources: envspec.Resources{CPUs: 1, MemoryMB: 512}, TTL: time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close(context.Background(), core.ShutdownDisconnect) }()
	h, err := m.Attach(ctx, envspec.Owner{ID: "agent:live", Session: "s1"},
		envspec.Spec{Profile: "phys-live", Params: map[string]string{ParamSerial: serial}})
	if err != nil {
		t.Fatal(err)
	}
	dst := fmt.Sprintf("/data/local/tmp/craggy-core-xfer-%d", time.Now().UnixNano())
	// Clean up over adb directly: the handle is released before this runs.
	t.Cleanup(func() { _, _, _ = adb.New().Run(context.Background(), serial, "rm -rf "+dst) })

	want := map[string][]byte{"in.txt": []byte("through the core\n"), "nested/x.bin": {0, 0xff, 7}}
	if err := h.Push(ctx, envspec.MemSource(want), dst); err != nil {
		t.Fatalf("push: %v", err)
	}
	sink := envspec.NewMemSink()
	if err := h.Pull(ctx, dst, sink); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(sink.Files) != len(want) {
		t.Fatalf("pulled %v, want %v", sink.Files, want)
	}
	for p, w := range want {
		if string(sink.Files[p]) != string(w) {
			t.Fatalf("%s = %q, want %q", p, sink.Files[p], w)
		}
	}
	if err := h.Pull(ctx, dst+"/missing", envspec.NewMemSink()); err == nil {
		t.Fatal("pulling a missing path through the core must fail")
	}
	if err := h.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
