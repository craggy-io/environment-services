package docker

// Core-driven gates (§10 #3/#5/#6) against the docker adapter with
// a fake engine: the durable core provisions, crashes (Close with
// Disconnect = process death without cleanup), recovers by
// deterministic name and label discovery, tolerates a down backend
// as Unknown, and verifies destroys through this adapter's Observe.

import (
	"context"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/core"
)

func coreWithFake(t *testing.T, dir string, f *fakeEngine) *core.Manager {
	t.Helper()
	a := &Adapter{cli: f, ManagerID: "mgr-1", SidecarImage: "mitmproxy/mitmproxy:latest"}
	m, err := core.Open(context.Background(), core.Config{
		Dir: dir, ManagerID: "mgr-1", Adapters: []adapter.Adapter{a},
		HealthInterval: 50 * time.Millisecond, ReapInterval: 50 * time.Millisecond,
		Profiles: []envspec.Profile{{Name: "tiny", Kind: envspec.Container, Image: "busybox:latest",
			AllowImages: []string{"busybox:*"}, Resources: envspec.Resources{CPUs: 1, MemoryMB: 256},
			MaxResources: envspec.Resources{CPUs: 2, MemoryMB: 1024}, Network: envspec.NetworkNone, TTL: time.Hour}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCoreCrashRecoveryAndBackendDown(t *testing.T) {
	f := newFakeEngine()
	dir := t.TempDir()
	ctx := context.Background()

	m1 := coreWithFake(t, dir, f)
	h, err := m1.Provision(ctx, envspec.Owner{ID: "agent:test"}, envspec.Spec{Profile: "tiny"})
	if err != nil {
		t.Fatal(err)
	}
	envID := h.ID()
	if out, _, code, err := h.Exec(ctx, envspec.Cmd{Argv: []string{"echo", "before-crash"}}); err != nil || code != 0 || string(out) != "before-crash\n" {
		t.Fatalf("exec before crash: %q code=%d err=%v", out, code, err)
	}
	if f.creates != 1 { // the keepalive container; execs are not containers
		t.Fatalf("containers created = %d, want 1", f.creates)
	}
	if err := h.Push(ctx, envspec.MemSource(map[string][]byte{"a.txt": []byte("alpha")}), "/work"); err != nil {
		t.Fatal(err)
	}

	// Crash: disconnect without releasing (§10 #3 shape: recovery
	// must re-observe and re-materialise, not recreate).
	if err := m1.Close(ctx, core.ShutdownDisconnect); err != nil {
		t.Fatal(err)
	}

	// Backend down at recovery: presence must stay Unknown, env
	// must not be declared lost (§10 #5).
	f.down = true
	m2 := coreWithFake(t, dir, f)
	info, err := m2.Info(envID)
	if err != nil {
		t.Fatal(err)
	}
	if info.State == envspec.StateFailed || info.State == envspec.StateReleased {
		t.Fatalf("backend-down recovery marked env %s; want it kept (Unknown presence)", info.State)
	}
	if err := m2.Close(ctx, core.ShutdownDisconnect); err != nil {
		t.Fatal(err)
	}
	// Backend back: a fresh manager recovers and re-materialises by
	// identity. (The S3 core materialises runtimes only at Open, so
	// an env recovered while the backend is down needs a restart
	// once it returns — named as an S4 finding.)
	f.down = false
	m2 = coreWithFake(t, dir, f)
	m2.ReconcileOnce(ctx)

	h2, err := m2.Get(envID)
	if err != nil {
		t.Fatalf("Get after recovery: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var out []byte
	var code int
	for {
		out, _, code, err = h2.Exec(ctx, envspec.Cmd{Argv: []string{"cat", "/work/a.txt"}})
		if err == nil && code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exec after recovery never worked: code=%d err=%v", code, err)
		}
		m2.ReconcileOnce(ctx)
		time.Sleep(100 * time.Millisecond)
	}
	if string(out) != "alpha" {
		t.Fatalf("recovered container content = %q, want the pre-crash file", out)
	}
	if f.creates != 1 {
		t.Fatalf("recovery created a second container (creates=%d); rediscovery by name must win", f.creates)
	}

	// Destroy through the core verifies Absent via Observe (§10 #6).
	if err := m2.Release(ctx, envID); err != nil {
		t.Fatal(err)
	}
	if len(f.containers) != 0 {
		t.Fatalf("containers left after release: %d", len(f.containers))
	}
	info, err = m2.Info(envID)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != envspec.StateReleased {
		t.Fatalf("state after release = %s, want released", info.State)
	}
	if err := m2.Close(ctx, core.ShutdownDrain); err != nil {
		t.Fatal(err)
	}
}

func TestCoreDestroyFailureStaysPending(t *testing.T) {
	f := newFakeEngine()
	dir := t.TempDir()
	ctx := context.Background()
	m := coreWithFake(t, dir, f)
	defer func() { _ = m.Close(ctx, core.ShutdownDrain) }()
	h, err := m.Provision(ctx, envspec.Owner{ID: "agent:test"}, envspec.Spec{Profile: "tiny"})
	if err != nil {
		t.Fatal(err)
	}
	f.removeErr = errString("container is restarting")
	// Release records the durable obligation; the drive fails and
	// the env must remain non-terminal until destroy verifies.
	_ = m.Release(ctx, h.ID())
	info, err := m.Info(h.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.State == envspec.StateReleased {
		t.Fatal("env declared released while destroy is failing (§10 #6)")
	}
	f.removeErr = nil
	deadline := time.Now().Add(15 * time.Second)
	for {
		m.ReconcileOnce(ctx)
		info, err = m.Info(h.ID())
		if err == nil && info.State == envspec.StateReleased {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("release never completed after destroy recovered: state=%s err=%v", info.State, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(f.containers) != 0 {
		t.Fatalf("containers left: %d", len(f.containers))
	}
}

type errString string

func (e errString) Error() string { return string(e) }
