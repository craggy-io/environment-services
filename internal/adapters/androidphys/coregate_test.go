package androidphys

// Core-driven §10 gates against the physical adapter with a fake adb
// transport: #8 a required Android collector dies after readiness ->
// its signal condition goes False and the activity is restarted; #9
// an experiment hold (MutationPolicy denial) stops the frida
// component's autonomous repair (activity held, no repair runs), and
// lifting the hold lets the repair through.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/androiddev"
	"github.com/craggy-io/environment-services/internal/core"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/store"
)

var physProfile = envspec.Profile{
	Name: "phys", Kind: envspec.Android,
	Resources: envspec.Resources{CPUs: 1, MemoryMB: 512}, TTL: time.Hour,
	Android: &envspec.AndroidOptions{Root: envspec.RootADB, Frida: &envspec.FridaOptions{Version: fridaTestVersion()}},
}

func physManager(t *testing.T, a *Adapter, pol envspec.MutationPolicy) *core.Manager {
	return physManagerAt(t, t.TempDir(), a, pol)
}

func physManagerAt(t *testing.T, dir string, a *Adapter, pol envspec.MutationPolicy) *core.Manager {
	t.Helper()
	m, err := core.Open(context.Background(), core.Config{
		Dir: dir, ManagerID: "mgr-phys",
		Adapters:       []adapter.Adapter{a},
		Signals:        signalcore.NewHub(),
		Profiles:       []envspec.Profile{physProfile},
		MutationPolicy: pol,
		HealthInterval: 25 * time.Millisecond, ReapInterval: 20 * time.Millisecond,
		BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		ActivityStopWait: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background(), core.ShutdownDisconnect) })
	return m
}

func TestCoreReopensPhysicalDeviceWithPersistedInstrumentation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := newFakePhys()
	f.cpuABI = "arm64-v8a"
	a1 := New()
	a1.Transport = f
	m1 := physManagerAt(t, dir, a1, nil)
	spec := envspec.Spec{Profile: "phys", Params: map[string]string{ParamSerial: "PIXEL1"},
		Android: &envspec.AndroidOptions{Root: envspec.RootMagisk, Frida: &envspec.FridaOptions{Version: fridaTestVersion()}}}
	h, err := m1.Attach(ctx, envspec.Owner{ID: "agent:restart", Session: "s1"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	envID := h.ID()
	if err := m1.Close(ctx, core.ShutdownDisconnect); err != nil {
		t.Fatal(err)
	}
	a2 := New()
	a2.Transport = f
	m2 := physManagerAt(t, dir, a2, nil)
	eventually(t, "physical attachment reopened", func() bool {
		h2, err := m2.Get(envID)
		if err != nil {
			return false
		}
		_, err = m2.Instance(h2)
		return err == nil
	})
	h2, err := m2.Get(envID)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := m2.Instance(h2)
	if err != nil {
		t.Fatal(err)
	}
	got := inst.(*Instance)
	if got.spec.Android == nil || got.spec.Android.Root != envspec.RootMagisk || got.spec.Android.Frida == nil ||
		got.spec.Android.ABI != "arm64-v8a" || got.Device().RootMode() != envspec.RootMagisk {
		t.Fatalf("reopened physical config = %+v", got.spec.Android)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func condOf(m *core.Manager, envID string, gen int64, name string) (envspec.Condition, bool) {
	conds, err := m.Store().Conditions(envID, gen)
	if err != nil {
		return envspec.Condition{}, false
	}
	for _, c := range conds {
		if c.Name == name {
			return c, true
		}
	}
	return envspec.Condition{}, false
}

func TestCoreRequiredAndroidCollectorDiesAndRestarts(t *testing.T) {
	f := newFakePhys()
	// Logcat streams through a pipe the test controls: alive during
	// the readiness ack, killed after.
	pr, pw := io.Pipe()
	var killed atomic.Bool
	f.logcatOut = func() (io.ReadCloser, error) {
		if !killed.Load() {
			return pr, nil
		}
		// Restarted runs get a fresh blocking pipe.
		r, _ := io.Pipe()
		return r, nil
	}
	a := New()
	a.Transport = f
	m := physManager(t, a, nil)
	ctx := context.Background()
	spec := envspec.Spec{Profile: "phys", Params: map[string]string{ParamSerial: "PIXEL1"},
		Signals: []envspec.SignalRequest{{Stream: envspec.StreamAndroidLogcat, Required: true}}}
	h, err := m.Attach(ctx, envspec.Owner{ID: "agent:test", Session: "s1"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	envID := h.ID()
	e, err := m.Store().GetEnv(envID)
	if err != nil {
		t.Fatal(err)
	}
	gen := e.Generation
	actName := "signal:" + envspec.StreamAndroidLogcat
	eventually(t, "required collector running", func() bool {
		act, err := m.Store().GetActivity(envID, gen, actName)
		return err == nil && act.State == store.ActivityRunning
	})
	// Spec-less frida assertions aside: the physical device instance
	// is reachable for AndroidDevice extraction.
	inst, err := m.Instance(h)
	if err != nil {
		t.Fatal(err)
	}
	if pi, ok := inst.(*Instance); !ok || pi.Device() == nil || pi.Device().Serial() != "PIXEL1" {
		t.Fatalf("instance = %T %+v", inst, inst)
	}

	// Kill the collector's stream: the death must be recorded
	// (collector.failed, from the condition going False) and the
	// activity restarted. The live condition flips back to True on
	// restart within milliseconds at test backoffs, so the event +
	// attempts are the durable evidence; the False write itself is
	// what produces the event.
	killed.Store(true)
	_ = pw.Close()
	eventually(t, "collector death recorded and restarted", func() bool {
		act, err := m.Store().GetActivity(envID, gen, actName)
		return err == nil && act.Attempts >= 1 && f.execOuts.Load() >= 2
	})
	evs, err := m.EventsSince(ctx, 0, 300)
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for _, ev := range evs {
		if ev.Kind == "collector.failed" {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("no collector.failed event after required collector death (%d events)", len(evs))
	}
	// And the condition is healthy again only because it restarted.
	eventually(t, "signal condition true again after restart", func() bool {
		c, ok := condOf(m, envID, gen, actName)
		return ok && c.Status == envspec.ConditionTrue
	})
	if err := m.Release(ctx, envID); err != nil {
		t.Fatal(err)
	}
	e, _ = m.Store().GetEnv(envID)
	if e.Phase != envspec.StateReleased {
		t.Fatalf("after release phase=%s, want released (disconnect)", e.Phase)
	}
}

type flipPolicy struct{ allow atomic.Bool }

func (p *flipPolicy) AllowMutation(envID, kind string) (bool, string) {
	if p.allow.Load() {
		return true, ""
	}
	return false, "experiment_hold"
}

func TestCoreExperimentHoldBlocksFridaRepair(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "frida-server-"+fridaTestVersion()+"-android-x86_64")
	if err := os.WriteFile(bin, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := newFakePhys() // fridaUp=false: pidof reports the daemon down
	a := New()
	a.Transport = f
	a.FridaServer = &androiddev.FridaServer{ServerDir: dir}
	pol := &flipPolicy{}
	m := physManager(t, a, pol)
	ctx := context.Background()
	spec := envspec.Spec{Profile: "phys", Params: map[string]string{ParamSerial: "PIXEL1"}}
	h, err := m.Attach(ctx, envspec.Owner{ID: "agent:test", Session: "s1"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	envID := h.ID()
	e, _ := m.Store().GetEnv(envID)
	gen := e.Generation
	// Attach-time Ensure raced the policy? No: the device-level gate
	// is only installed when environments wires it; core's probe gate
	// is what this test exercises. Reset the counters after attach so
	// only core-driven repairs count.
	f.mu.Lock()
	f.syncSent = nil
	f.mu.Unlock()
	f.fridaUp.Store(false)

	eventually(t, "frida probe activity held", func() bool {
		act, err := m.Store().GetActivity(envID, gen, "probe:frida")
		return err == nil && act.State == store.ActivityHeld
	})
	if c, ok := condOf(m, envID, gen, "frida"); !ok || c.Status != envspec.ConditionFalse {
		t.Fatalf("frida condition = %+v ok=%v, want False", c, ok)
	}
	if got := f.syncCount(); got != 0 {
		t.Fatalf("repair pushed frida-server %d times under experiment hold", got)
	}
	// An activity.held event names the hold.
	eventually(t, "activity.held event", func() bool {
		evs, err := m.EventsSince(ctx, 0, 200)
		if err != nil {
			return false
		}
		for _, ev := range evs {
			if ev.Kind == "activity.held" {
				return true
			}
		}
		return false
	})

	// Lift the hold: the repair runs (push + start), the daemon
	// comes up in the fake, and the condition recovers.
	pol.allow.Store(true)
	eventually(t, "frida repair after hold lifted", func() bool {
		return f.syncCount() > 0
	})
	eventually(t, "frida condition true", func() bool {
		c, ok := condOf(m, envID, gen, "frida")
		return ok && c.Status == envspec.ConditionTrue
	})
	if err := m.Release(ctx, envID); err != nil {
		t.Fatal(err)
	}
}
