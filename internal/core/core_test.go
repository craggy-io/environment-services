package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/envtestfake"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/store"
)

var testProfiles = []envspec.Profile{
	{Name: "fake", Kind: envspec.Container, Resources: envspec.Resources{CPUs: 1, MemoryMB: 512},
		AllowImages: []string{"*"}, TTL: time.Hour, MaxTTL: 4 * time.Hour},
	{Name: "fake-reuse", Kind: envspec.Container, Resources: envspec.Resources{CPUs: 1, MemoryMB: 512},
		AllowImages: []string{"*"}, TTL: time.Hour, Reuse: true},
	{Name: "fake-android", Kind: envspec.Android, Resources: envspec.Resources{CPUs: 1, MemoryMB: 512}, TTL: time.Hour},
}

var warmProfile = envspec.Profile{Name: "fake-warm", Kind: envspec.Container,
	Resources: envspec.Resources{CPUs: 1, MemoryMB: 512}, AllowImages: []string{"*"},
	TTL: time.Hour, Reuse: true, Warm: 1}

func testConfig(dir string, adapters ...adapter.Adapter) Config {
	return Config{
		Dir: dir, Adapters: adapters, Profiles: testProfiles,
		HealthInterval: 25 * time.Millisecond, ReapInterval: 20 * time.Millisecond,
		BackoffBase: time.Millisecond, BackoffMax: 2 * time.Millisecond,
		ActivityStopWait: 300 * time.Millisecond,
	}
}

func testConfigWarm(dir string, adapters ...adapter.Adapter) Config {
	cfg := testConfig(dir, adapters...)
	cfg.Profiles = append(append([]envspec.Profile{}, testProfiles...), warmProfile)
	return cfg
}

func openMgr(t *testing.T, cfg Config) *Manager {
	t.Helper()
	m, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close(context.Background(), ShutdownDisconnect) })
	return m
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

var ownerA = envspec.Owner{ID: "agent:test", Session: "s1"}

// Gate: concurrent identical provisioning → one op, one resource.
func TestConcurrentIdenticalProvisionOneResource(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m := openMgr(t, testConfig(t.TempDir(), created))
	spec := envspec.Spec{Profile: "fake", Key: "req-1"}
	var wg sync.WaitGroup
	handles := make([]Handle, 4)
	errs := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handles[i], errs[i] = m.Provision(context.Background(), ownerA, spec)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("provision %d: %v", i, err)
		}
		if handles[i].EnvID != handles[0].EnvID {
			t.Fatalf("handle %d got %s, want %s", i, handles[i].EnvID, handles[0].EnvID)
		}
	}
	if got := world.Creates(); got != 1 {
		t.Fatalf("creates during concurrent provision = %d, want 1", got)
	}
	rows, _ := m.store.ListEnvs()
	live := 0
	for _, e := range rows {
		if !e.Phase.Terminal() {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("live env rows = %d, want 1", live)
	}
	// Same key, different request: conflict, not a second env.
	other := spec
	other.Resources.MemoryMB = 1024
	if _, err := m.Provision(context.Background(), ownerA, other); !errors.Is(err, envspec.ErrConflict) {
		t.Fatalf("digest conflict: %v", err)
	}
	// Handle works.
	out, _, code, err := handles[0].Exec(context.Background(), envspec.Cmd{Argv: []string{"echo", "hi"}})
	if err != nil || code != 0 || string(out) != "hi\n" {
		t.Fatalf("exec: out=%q code=%d err=%v", out, code, err)
	}
}

// Gate: concurrent warm claims → one wins the warm instance.
func TestConcurrentWarmClaimsOneWinner(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m := openMgr(t, testConfigWarm(t.TempDir(), created))
	var warmID string
	eventually(t, "warm env present", func() bool {
		rows, _ := m.store.ListEnvs()
		for _, e := range rows {
			if e.Warm && e.Phase == envspec.StatePresent {
				warmID = e.ID
				return true
			}
		}
		return false
	})
	var wg sync.WaitGroup
	handles := make([]Handle, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			handles[i], errs[i] = m.Provision(context.Background(),
				envspec.Owner{ID: fmt.Sprintf("agent:c%d", i)}, envspec.Spec{Profile: "fake-warm"})
		}()
	}
	wg.Wait()
	wins := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("provision %d: %v", i, err)
		}
		if handles[i].EnvID == warmID {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("warm winners = %d, want 1 (warm=%s)", wins, warmID)
	}
	if handles[0].EnvID == handles[1].EnvID {
		t.Fatal("both claimants share one env")
	}
}

// Gate: crash after Create, before the result is persisted →
// recovery rediscovers by deterministic name and finishes the op.
func TestCrashAfterCreateRecovers(t *testing.T) {
	dir := t.TempDir()
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m1 := openMgr(t, testConfig(dir, created))

	// Seed the durable intent (tx1) and the external effect (Create)
	// with no tx2 between: exactly the crash window.
	envID := envspec.NewID("env_")
	merged, err := mergeSpec(testProfiles[0], envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	now := m1.now()
	e := store.Env{ID: envID, Kind: envspec.Container, Profile: "fake", Spec: merged, Owner: ownerA,
		Generation: 1, Revision: 1, Desired: store.DesiredPresent, Phase: envspec.StateAcquiring,
		Presence: store.PresenceUnknown, CostPool: envspec.PoolDocker, CostMemMB: 512, Created: now}
	op := store.Operation{ID: envspec.NewID("op_"), Env: envID, Generation: 1, Kind: store.OpProvision,
		OwnerScope: ownerA.ID, Outcome: store.OutcomePending, Created: now, Updated: now}
	lease := store.Lease{Env: envID, LeaseID: envspec.NewID("lease_"), Epoch: 1, Holder: ownerA.ID,
		Expires: now.Add(time.Hour), State: store.LeaseHeld}
	if _, _, _, err := m1.store.InsertProvision(e, op, lease, now); err != nil {
		t.Fatal(err)
	}
	if _, err := created.Create(context.Background(), adapter.OpContext{EnvID: envID, Generation: 1}, merged, nil); err != nil {
		t.Fatal(err)
	}
	createsAtCrash := world.Creates()
	m1.simulateCrash()

	m2 := openMgr(t, testConfig(dir, created))
	eventually(t, "recovered env present", func() bool {
		row, err := m2.store.GetEnv(envID)
		return err == nil && row.Phase == envspec.StatePresent && row.Instance != ""
	})
	if got := world.Creates(); got != createsAtCrash {
		t.Fatalf("creates = %d, want %d (recovery must not recreate)", got, createsAtCrash)
	}
	h, err := m2.Get(envID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, code, err := h.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil || code != 0 {
		t.Fatalf("exec after recovery: code=%d err=%v", code, err)
	}
}

// Gate: release during provisioning, late success → still released.
func TestReleaseDuringProvisionLateSuccess(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	gate := make(chan struct{})
	created.Script.CreateGate = gate
	m := openMgr(t, testConfig(t.TempDir(), created))

	provErr := make(chan error, 1)
	go func() {
		_, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake", Key: "k"})
		provErr <- err
	}()
	var envID string
	eventually(t, "acquiring row", func() bool {
		rows, _ := m.store.ListEnvs()
		for _, e := range rows {
			if e.Phase == envspec.StateAcquiring {
				envID = e.ID
				return true
			}
		}
		return false
	})
	relErr := make(chan error, 1)
	go func() { relErr <- m.Release(context.Background(), envID) }()
	// Desired flips first, then Create completes (late success).
	eventually(t, "desired released", func() bool {
		e, err := m.store.GetEnv(envID)
		return err == nil && e.Desired == store.DesiredReleased
	})
	close(gate)
	if err := <-relErr; err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-provErr; err == nil {
		t.Fatal("provision should fail once released mid-flight")
	}
	eventually(t, "env released", func() bool {
		e, err := m.store.GetEnv(envID)
		return err == nil && e.Phase == envspec.StateReleased
	})
	if world.Count() != 0 {
		t.Fatalf("world still holds %d resources", world.Count())
	}
}

// Gate: backend down during recovery → unknown, never lost.
func TestBackendDownRecoveryStaysUnknown(t *testing.T) {
	dir := t.TempDir()
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m1 := openMgr(t, testConfig(dir, created))
	h, err := m1.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	m1.simulateCrash()
	created.Script.Set(func(s *envtestfake.Script) { s.DiscoverErr = errors.New("daemon gone") })

	m2 := openMgr(t, testConfig(dir, created))
	e, err := m2.store.GetEnv(h.EnvID)
	if err != nil {
		t.Fatal(err)
	}
	if e.Phase != envspec.StatePresent || e.Presence != store.PresenceUnknown {
		t.Fatalf("after backend-down recovery: phase=%s presence=%s, want present/unknown", e.Phase, e.Presence)
	}
	lease, err := m2.store.GetLease(h.EnvID)
	if err != nil || lease.State != store.LeaseHeld {
		t.Fatalf("lease must survive unknown recovery: %+v %v", lease, err)
	}
	// Backend returns: presence resolves, handle works again.
	created.Script.Set(func(s *envtestfake.Script) { s.DiscoverErr = nil })
	eventually(t, "presence present again", func() bool {
		e, err := m2.store.GetEnv(h.EnvID)
		return err == nil && e.Presence == store.PresencePresent
	})
	h2, err := m2.Get(h.EnvID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h2.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// Gate: destroy fails → op pending, visible, retried to completion.
func TestDestroyFailureStaysPendingAndRetries(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m := openMgr(t, testConfig(t.TempDir(), created))
	h, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	created.Script.Set(func(s *envtestfake.Script) { s.DestroyErr = errors.New("daemon refuses") })
	if err := m.Release(context.Background(), h.EnvID); err == nil {
		t.Fatal("release should report the destroy failure")
	}
	e, _ := m.store.GetEnv(h.EnvID)
	op, oerr := m.store.OpenOperation(h.EnvID, store.OpRelease)
	if e.Phase != envspec.StateReleasing || oerr != nil || op.Outcome != store.OutcomePending {
		t.Fatalf("phase=%s openOp=%+v err=%v", e.Phase, op, oerr)
	}
	if world.Count() != 1 {
		t.Fatal("resource should still exist while destroy fails")
	}
	created.Script.Set(func(s *envtestfake.Script) { s.DestroyErr = nil })
	m.ReconcileOnce(context.Background())
	eventually(t, "released after retry", func() bool {
		e, err := m.store.GetEnv(h.EnvID)
		return err == nil && e.Phase == envspec.StateReleased && world.Count() == 0
	})
}

// Destroy that claims success but leaves the resource: verification
// keeps the op pending until absence is real.
func TestDestroyLeavesPresentStaysPending(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m := openMgr(t, testConfig(t.TempDir(), created))
	h, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	created.Script.Set(func(s *envtestfake.Script) { s.LeavesPresent = true })
	if err := m.Release(context.Background(), h.EnvID); err == nil {
		t.Fatal("release must not succeed while the resource is still present")
	}
	e, _ := m.store.GetEnv(h.EnvID)
	if e.Phase != envspec.StateReleasing {
		t.Fatalf("phase = %s, want releasing", e.Phase)
	}
	created.Script.Set(func(s *envtestfake.Script) { s.LeavesPresent = false })
	m.ReconcileOnce(context.Background())
	eventually(t, "released once really gone", func() bool {
		e, err := m.store.GetEnv(h.EnvID)
		return err == nil && e.Phase == envspec.StateReleased
	})
}

// Gate: stale handle after warm reassignment → epoch rejection.
func TestStaleHandleAfterWarmReassignment(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m := openMgr(t, testConfig(t.TempDir(), created))
	h1, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake-reuse"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h1.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.Release(context.Background(), h1.EnvID); err != nil {
		t.Fatal(err)
	}
	// Reuse profile: reset to pool, same env ID, new generation.
	row, _ := m.store.GetEnv(h1.EnvID)
	if !row.Warm || row.Phase != envspec.StatePresent {
		t.Fatalf("after reuse release: warm=%v phase=%s", row.Warm, row.Phase)
	}
	if res := world.Get(adapter.NameFor(h1.EnvID)); res == nil || res.Resets() != 1 {
		t.Fatal("instance should have been reset once, not destroyed")
	}
	h2, err := m.Provision(context.Background(), envspec.Owner{ID: "agent:other"}, envspec.Spec{Profile: "fake-reuse"})
	if err != nil {
		t.Fatal(err)
	}
	if h2.EnvID != h1.EnvID {
		t.Fatalf("expected warm claim of %s, got %s", h1.EnvID, h2.EnvID)
	}
	if _, _, _, err := h1.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); !errors.Is(err, envspec.ErrConflict) {
		t.Fatalf("stale handle exec: %v", err)
	}
	if err := h1.RenewLease(); !errors.Is(err, envspec.ErrConflict) {
		t.Fatalf("stale handle renew: %v", err)
	}
	if _, _, _, err := h2.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatalf("new holder exec: %v", err)
	}
}

// Gate: recovered env released → reservation SUM returns to zero.
func TestRecoveredReleaseReturnsReservation(t *testing.T) {
	dir := t.TempDir()
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m1 := openMgr(t, testConfig(dir, created))
	h, err := m1.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if _, mem, _ := m1.store.ReservedCost(envspec.PoolDocker); mem != 512 {
		t.Fatalf("reserved before crash = %d", mem)
	}
	m1.simulateCrash()

	m2 := openMgr(t, testConfig(dir, created))
	if _, mem, _ := m2.store.ReservedCost(envspec.PoolDocker); mem != 512 {
		t.Fatalf("reserved after recovery = %d, want 512 (recomputed at Open)", mem)
	}
	if err := m2.Release(context.Background(), h.EnvID); err != nil {
		t.Fatal(err)
	}
	if _, mem, _ := m2.store.ReservedCost(envspec.PoolDocker); mem != 0 {
		t.Fatalf("reserved after release = %d, want 0", mem)
	}
}

// Attached fake: attach verifies identity, release disconnects only.
func TestAttachDisconnectLifecycle(t *testing.T) {
	world := envtestfake.NewWorld()
	attached := envtestfake.NewAttached(world, envspec.Android)
	dev := attached.AddDevice("serial-1")
	m := openMgr(t, testConfig(t.TempDir(), attached))
	spec := envspec.Spec{Profile: "fake-android", Params: map[string]string{"serial": "serial-1"}, Key: "dev1"}
	h, err := m.Attach(context.Background(), ownerA, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.Exec(context.Background(), envspec.Cmd{Argv: []string{"id"}}); err != nil {
		t.Fatalf("exec on attached: %v", err)
	}
	// A second attach of the same device conflicts.
	spec2 := spec
	spec2.Key = "dev1b"
	if _, err := m.Attach(context.Background(), ownerA, spec2); err == nil {
		t.Fatal("double attach should fail")
	}
	if err := m.Release(context.Background(), h.EnvID); err != nil {
		t.Fatal(err)
	}
	e, _ := m.store.GetEnv(h.EnvID)
	if e.Phase != envspec.StateReleased || e.Presence != store.PresencePresent {
		t.Fatalf("after disconnect: phase=%s presence=%s", e.Phase, e.Presence)
	}
	if world.Count() != 1 {
		t.Fatal("attached device must survive release (disconnect only)")
	}
	dev.WriteFile("/x", []byte("y")) // still usable by its real owner
	// Unknown serial fails validation-side, no env left acquiring.
	if _, err := m.Attach(context.Background(), ownerA,
		envspec.Spec{Profile: "fake-android", Params: map[string]string{"serial": "nope"}}); err == nil {
		t.Fatal("attach of missing device should fail")
	}
}

// Required collector barrier: provision waits for ack; a dying
// required collector fails the provision and cleans up.
func TestRequiredCollectorBarrier(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	created.TickStream = "test.tick@1"
	cfg := testConfig(t.TempDir(), created)
	cfg.Signals = signalcore.NewHub()
	m := openMgr(t, cfg)
	spec := envspec.Spec{Profile: "fake", Signals: []envspec.SignalRequest{{Stream: "test.tick@1", Required: true}}}
	h, err := m.Provision(context.Background(), ownerA, spec)
	if err != nil {
		t.Fatal(err)
	}
	act, err := m.store.GetActivity(h.EnvID, h.Generation, "signal:test.tick@1")
	if err != nil || act.State != store.ActivityRunning {
		t.Fatalf("collector activity: %+v %v", act, err)
	}
	if err := m.Release(context.Background(), h.EnvID); err != nil {
		t.Fatal(err)
	}

	// Dying required collector: barrier never acks → fail + cleanup.
	created.TickFail = true
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	before := world.Count()
	if _, err := m.Provision(ctx, ownerA, spec); err == nil {
		t.Fatal("provision with dying required collector should fail")
	}
	eventually(t, "failed provision cleaned up", func() bool { return world.Count() == before })
}

// MutationPolicy gate: repair denied → activity held, no repair runs.
type holdPolicy struct{ allow atomic.Bool }

func (p *holdPolicy) AllowMutation(envID, kind string) (bool, string) {
	if p.allow.Load() {
		return true, ""
	}
	return false, "experiment_hold"
}

func TestMutationPolicyHoldsRepair(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	var repairs atomic.Int32
	created.Probe = &adapter.Probe{
		Name: "widget", Interval: 10 * time.Millisecond,
		Check: func(ctx context.Context) (envspec.Condition, error) {
			return envspec.Condition{Status: envspec.ConditionFalse, Reason: "probe_failed"}, nil
		},
		Repair: func(ctx context.Context) error { repairs.Add(1); return nil },
	}
	pol := &holdPolicy{}
	cfg := testConfig(t.TempDir(), created)
	cfg.MutationPolicy = pol
	m := openMgr(t, cfg)
	h, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "activity held", func() bool {
		act, err := m.store.GetActivity(h.EnvID, h.Generation, "probe:widget")
		return err == nil && act.State == store.ActivityHeld
	})
	if repairs.Load() != 0 {
		t.Fatalf("repairs ran under hold: %d", repairs.Load())
	}
	pol.allow.Store(true)
	eventually(t, "repair runs once allowed", func() bool { return repairs.Load() > 0 })
	_ = m.Release(context.Background(), h.EnvID)
}

// RenewLease is the only renewal path; use does not renew.
func TestNoRenewOnUseAndLeaseExpiry(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	clock := envtestfake.NewClock()
	cfg := testConfig(t.TempDir(), created)
	cfg.Clock = clock
	m := openMgr(t, cfg)
	h, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	lease1, _ := m.store.GetLease(h.EnvID)
	clock.Advance(30 * time.Second)
	if _, _, _, err := h.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	lease2, _ := m.store.GetLease(h.EnvID)
	if !lease2.Expires.Equal(lease1.Expires) {
		t.Fatal("Exec renewed the lease (renew-on-use must not exist)")
	}
	if err := h.RenewLease(); err != nil {
		t.Fatal(err)
	}
	lease3, _ := m.store.GetLease(h.EnvID)
	if !lease3.Expires.After(lease1.Expires) {
		t.Fatal("RenewLease did not extend expiry")
	}
	// Expiry without renewal: reaper releases.
	clock.Advance(2 * time.Minute)
	m.ReconcileOnce(context.Background())
	eventually(t, "expired env released", func() bool {
		e, err := m.store.GetEnv(h.EnvID)
		return err == nil && e.Phase == envspec.StateReleased
	})
}

// Quotas bound per-owner counts.
func TestQuotaEnforced(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	cfg := testConfig(t.TempDir(), created)
	cfg.Quotas = []Quota{{OwnerGlob: "agent:*", MaxContainers: 1}}
	m := openMgr(t, cfg)
	if _, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"}); !errors.Is(err, ErrQuota) {
		t.Fatalf("second provision: %v, want quota error", err)
	}
}

// Push/Pull through the archive fast path round-trips files.
func TestPushPullRoundTrip(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m := openMgr(t, testConfig(t.TempDir(), created))
	h, err := m.Provision(context.Background(), ownerA, envspec.Spec{Profile: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	src := envspec.MemSource(map[string][]byte{"a.txt": []byte("hello"), "sub/b.txt": []byte("world")})
	if err := h.Push(context.Background(), src, "/work"); err != nil {
		t.Fatal(err)
	}
	sink := envspec.NewMemSink()
	if err := h.Pull(context.Background(), "/work", sink); err != nil {
		t.Fatal(err)
	}
	if string(sink.Files["a.txt"]) != "hello" || string(sink.Files["sub/b.txt"]) != "world" {
		t.Fatalf("round trip: %v", sink.Files)
	}
}

func TestPublishedTCPPortRequiresContainer(t *testing.T) {
	p := envspec.Profile{Name: "host-test", Kind: envspec.Host, Network: envspec.NetworkOpen}
	if _, err := mergeSpec(p, envspec.Spec{Profile: p.Name, PublishedTCPPort: 8080}); !errors.Is(err, envspec.ErrInvalid) {
		t.Fatalf("host published port: %v, want ErrInvalid", err)
	}
}
