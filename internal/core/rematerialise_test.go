package core

import (
	"context"
	"errors"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/envtestfake"
	"github.com/craggy-io/environment-services/internal/store"
)

// Recovery gate, harder half: the backend is down at manager restart so
// recovery cannot even Open (re-materialise) a present env. The env
// must stay present/unknown — and must become usable again, without
// another manager restart, once the backend returns.
func TestBackendDownAtOpenRematerialisesOnReturn(t *testing.T) {
	dir := t.TempDir()
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m1 := openMgr(t, testConfig(dir, created))
	h, err := m1.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	m1.simulateCrash()
	created.Script.Set(func(s *envtestfake.Script) {
		s.DiscoverErr = errors.New("daemon gone")
		s.OpenErr = errors.New("daemon gone")
	})

	m2 := openMgr(t, testConfig(dir, created))
	e, err := m2.store.GetEnv(h.EnvID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Phase != envspec.StatePresent || e.Presence != store.PresenceUnknown {
		t.Fatalf("after backend-down recovery: phase=%s presence=%s, want present/unknown", e.Phase, e.Presence)
	}
	if m2.refreshRuntimeEnv(h.EnvID) != nil {
		t.Fatal("runtime materialised while backend down")
	}
	h2, err := m2.Get(h.EnvID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h2.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err == nil {
		t.Fatal("exec succeeded with no runtime")
	}

	// Backend returns: reconcile must re-materialise the runtime.
	created.Script.Set(func(s *envtestfake.Script) { s.DiscoverErr = nil; s.OpenErr = nil })
	eventually(t, "runtime re-materialised and exec works", func() bool {
		_, _, _, err := h2.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}})
		return err == nil
	})
	e, err = m2.store.GetEnv(h.EnvID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Phase != envspec.StatePresent {
		t.Fatalf("phase=%s, want present", e.Phase)
	}
}
