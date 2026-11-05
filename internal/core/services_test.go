package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/envtestfake"
	"github.com/craggy-io/environment-services/internal/store"
)

// Services as activities: acquire provisions a dedicated env, the
// supervisor probes to ready, borrowers share, stop is
// generation-fenced, and borrower release keeps the service alive.
func TestManagedServiceLifecycle(t *testing.T) {
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	m := openMgr(t, testConfig(t.TempDir(), created))

	req := envspec.ServiceRequest{
		Key:         "lab:proj",
		Environment: envspec.Spec{Profile: "fake"},
		Service: envspec.ServiceSpec{
			Name: "thing", Version: "1",
			Start: []string{"sleep-forever"}, Ready: []string{"true"},
			StartupTimeout: 5 * time.Second, ProbeInterval: 100 * time.Millisecond,
			ProbeTimeout: time.Second, IdleTimeout: time.Hour, StopTimeout: 5 * time.Second,
		},
	}
	sl, err := m.AcquireService(context.Background(), ownerA, req)
	if err != nil {
		t.Fatal(err)
	}
	info := sl.ServiceInfo()
	if info.State != "ready" || info.Environment == "" || info.Generation == "" {
		t.Fatalf("service info: %+v", info)
	}
	// A second borrower shares the same service env/generation.
	sl2, err := m.AcquireService(context.Background(), ownerA, req)
	if err != nil {
		t.Fatal(err)
	}
	if sl2.ServiceInfo().ID != info.ID {
		t.Fatal("second acquire made a new service")
	}
	if _, _, _, err := sl.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatalf("service exec: %v", err)
	}
	unlock, err := sl.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	// Stale generation stop is rejected.
	if err := m.StopService(context.Background(), info.ID, "gen_stale"); !errors.Is(err, envspec.ErrConflict) {
		t.Fatalf("stale stop: %v", err)
	}
	// Borrower release keeps the service running.
	if err := sl.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := sl2.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatalf("surviving borrower exec: %v", err)
	}
	if err := sl2.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.StopService(context.Background(), info.ID, info.Generation); err != nil {
		t.Fatal(err)
	}
	e, _ := m.store.GetEnv(info.Environment)
	if e.Phase != envspec.StateReleased {
		t.Fatalf("service env phase = %s, want released", e.Phase)
	}
}

// --- Stage S6 parity tests (ported from old env/services_test.go) ---

// serviceFixture scripts a daemon process plus a probe that can be
// made to fail, mirroring the old envtest fixture.
type serviceFixture struct {
	driver   *envtestfake.Created
	starts   atomic.Int32
	badProbe atomic.Bool
	mu       sync.Mutex
	exits    map[string]func()
}

func newServiceFixture(world *envtestfake.World) *serviceFixture {
	f := &serviceFixture{driver: envtestfake.NewCreated(world, envspec.Container), exits: map[string]func(){}}
	f.driver.OnExec = func(r *envtestfake.Resource, c adapter.ExecCmd) (*adapter.Process, error) {
		if len(c.Argv) == 0 {
			return nil, nil
		}
		if c.Argv[0] == "daemon" {
			f.starts.Add(1)
			done := make(chan struct{})
			var once sync.Once
			stop := func() { once.Do(func() { close(done) }) }
			f.mu.Lock()
			f.exits[r.EnvID] = stop
			f.mu.Unlock()
			return adapter.NewProcess(strings.NewReader("started"), nil, nil,
				func() (int, error) { <-done; return 17, nil },
				func(string) error { stop(); return nil }), nil
		}
		code := 0
		if c.Argv[0] == "probe" && f.badProbe.Load() {
			code = 1
		}
		return adapter.NewProcess(strings.NewReader("ok"), nil, nil,
			func() (int, error) { return code, nil }, nil), nil
	}
	return f
}

// exitFor returns the kill func for the daemon running in envID.
func (f *serviceFixture) exitFor(t *testing.T, envID string) func() {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	stop, ok := f.exits[envID]
	if !ok {
		t.Fatalf("no daemon exit for env %s (have %v)", envID, f.exits)
	}
	return stop
}

func svcRequest() envspec.ServiceRequest {
	return envspec.ServiceRequest{Key: "lab-a/project-1", Environment: envspec.Spec{Profile: "fake"}, Service: envspec.ServiceSpec{
		Name: "analysis", Version: "v1", Start: []string{"daemon"}, Ready: []string{"probe"},
		StartupTimeout: time.Second, ProbeInterval: 50 * time.Millisecond, ProbeTimeout: time.Second,
		IdleTimeout: time.Hour, StopTimeout: time.Second,
	}}
}

func acquireSvc(t *testing.T, m *Manager, o envspec.Owner, r envspec.ServiceRequest) envspec.ServiceLease {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := m.AcquireService(ctx, o, r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Release(context.Background()) })
	return l
}

// S6 gate: stale-generation StopService is rejected; sharing, Lock
// serialisation and the system lease's non-expiry are asserted on DB
// rows.
func TestServiceStaleGenerationAndSystemLease(t *testing.T) {
	world := envtestfake.NewWorld()
	f := newServiceFixture(world)
	m := openMgr(t, testConfig(t.TempDir(), f.driver))
	a := acquireSvc(t, m, ownerA, svcRequest())
	b := acquireSvc(t, m, ownerA, svcRequest())
	if a.ServiceInfo().ID != b.ServiceInfo().ID || f.starts.Load() != 1 || a.ServiceInfo().Borrowers != 2 {
		t.Fatalf("not shared: %+v / %+v starts=%d", a.ServiceInfo(), b.ServiceInfo(), f.starts.Load())
	}
	info := a.ServiceInfo()

	// The system (holder) lease is non-expiring while desired=running,
	// and the service activity row exists with kind=service.
	lease, err := m.store.GetLease(info.Environment)
	if err != nil {
		t.Fatal(err)
	}
	if lease.State != store.LeaseHeld || !lease.Expires.IsZero() {
		t.Fatalf("system lease = %+v, want held with zero expiry", lease)
	}
	e, _ := m.store.GetEnv(info.Environment)
	acts, err := m.store.Activities(e.ID, e.Generation)
	if err != nil {
		t.Fatal(err)
	}
	foundSvc := false
	for _, act := range acts {
		if act.Kind == store.ActivityService {
			foundSvc = true
			if act.Desired != "running" {
				t.Fatalf("service activity desired = %q, want running", act.Desired)
			}
		}
	}
	if !foundSvc {
		t.Fatalf("no kind=service activity row: %+v", acts)
	}
	// Each borrower has its own non-expiring borrow row, distinct from
	// the generation and from each other.
	bs, _ := m.store.Borrows(info.Environment)
	if len(bs) != 2 {
		t.Fatalf("borrows = %+v, want 2", bs)
	}
	for _, br := range bs {
		if !br.Expires.IsZero() {
			t.Fatalf("borrow %s expires %v, want non-expiring", br.Job, br.Expires)
		}
		if br.Job == info.Generation || br.Job == info.ID {
			t.Fatalf("borrow identity %q conflated with service identity", br.Job)
		}
	}
	if bs[0].Job == bs[1].Job {
		t.Fatal("borrowers share one borrow identity")
	}

	// Stale generation rejected; the live generation is untouched.
	if err := m.StopService(context.Background(), info.ID, "gen_stale"); !errors.Is(err, envspec.ErrConflict) {
		t.Fatalf("stale stop: %v", err)
	}
	if a.ServiceInfo().State != "ready" {
		t.Fatalf("stale stop disturbed the service: %+v", a.ServiceInfo())
	}
	// Unknown service ID is ErrNotFound, not a generation conflict.
	if err := m.StopService(context.Background(), "svc_nope", info.Generation); !errors.Is(err, envspec.ErrNotFound) {
		t.Fatalf("unknown stop: %v", err)
	}

	// Lock serialises transactions across borrowers.
	unlock, err := a.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err = b.Lock(c); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	unlock() // idempotent

	// Released borrower handle is closed; the other borrower lives.
	if err = a.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = a.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); !errors.Is(err, ErrClosed) {
		t.Fatalf("released handle exec: %v", err)
	}
	if _, _, _, err = b.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatalf("surviving borrower exec: %v", err)
	}
	if got := b.ServiceInfo().Borrowers; got != 1 {
		t.Fatalf("borrowers after release = %d, want 1", got)
	}
	// Version change mints a new generation.
	r := svcRequest()
	r.Service.Version = "v2"
	other := acquireSvc(t, m, ownerA, r)
	if other.ServiceInfo().Generation == info.Generation {
		t.Fatal("version change reused generation")
	}
	// A different session never shares.
	o := ownerA
	o.Session = "another"
	separate := acquireSvc(t, m, o, svcRequest())
	if separate.ID() == b.ID() {
		t.Fatal("shared across sessions")
	}
}

func TestServiceDrainWaitsForBorrowers(t *testing.T) {
	world := envtestfake.NewWorld()
	f := newServiceFixture(world)
	m := openMgr(t, testConfig(t.TempDir(), f.driver))
	a := acquireSvc(t, m, ownerA, svcRequest())
	info := a.ServiceInfo()
	done := make(chan error, 1)
	go func() { done <- m.StopService(context.Background(), info.ID, info.Generation) }()
	eventually(t, "draining", func() bool { return a.ServiceInfo().State == "draining" })
	select {
	case <-done:
		t.Fatal("stopped while borrowed")
	default:
	}
	// Existing borrower may finish work while draining.
	if _, _, _, err := a.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); err != nil {
		t.Fatalf("draining borrower exec: %v", err)
	}
	_ = a.Release(context.Background())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a.ServiceInfo().State != "stopped" {
		t.Fatalf("state after drain = %s, want stopped", a.ServiceInfo().State)
	}
	// A fresh acquire starts a new generation.
	b := acquireSvc(t, m, ownerA, svcRequest())
	if b.ServiceInfo().Generation == info.Generation {
		t.Fatal("reused stopped generation")
	}
}

func TestServiceFailureRetainsUntilBorrowReturned(t *testing.T) {
	world := envtestfake.NewWorld()
	f := newServiceFixture(world)
	m := openMgr(t, testConfig(t.TempDir(), f.driver))
	a := acquireSvc(t, m, ownerA, svcRequest())
	f.exitFor(t, a.ID())()
	eventually(t, "failed", func() bool { return a.ServiceInfo().State == "failed" })
	if _, _, _, err := a.Exec(context.Background(), envspec.Cmd{Argv: []string{"true"}}); !errors.Is(err, envspec.ErrNotReady) {
		t.Fatalf("failed generation accepted exec: %v", err)
	}
	// The env is retained while the borrower holds it.
	e, err := m.store.GetEnv(a.ID())
	if err != nil || e.Phase.Terminal() {
		t.Fatalf("partial workspace removed early: %+v err=%v", e, err)
	}
	_ = a.Release(context.Background())
	eventually(t, "released after borrow returned", func() bool {
		e, err := m.store.GetEnv(a.ID())
		return err != nil || e.Phase == envspec.StateReleased
	})
}

func TestServiceStartupFailureAndIdleExpiry(t *testing.T) {
	world := envtestfake.NewWorld()
	f := newServiceFixture(world)
	f.badProbe.Store(true)
	m := openMgr(t, testConfig(t.TempDir(), f.driver))
	r := svcRequest()
	r.Service.StartupTimeout = 200 * time.Millisecond
	if _, err := m.AcquireService(context.Background(), ownerA, r); err == nil {
		t.Fatal("unready service admitted")
	}
	f.badProbe.Store(false)
	r.Service.IdleTimeout = 50 * time.Millisecond
	a := acquireSvc(t, m, ownerA, r)
	_ = a.Release(context.Background())
	eventually(t, "idle stopped", func() bool { return a.ServiceInfo().State == "stopped" })
}

func TestServiceRestartInvalidatesGeneration(t *testing.T) {
	world := envtestfake.NewWorld()
	f := newServiceFixture(world)
	dir := t.TempDir()
	m := openMgr(t, testConfig(dir, f.driver))
	a, err := m.AcquireService(context.Background(), ownerA, svcRequest())
	if err != nil {
		t.Fatal(err)
	}
	gen := a.ServiceInfo().Generation
	oldEnv := a.ID()
	_ = a.Release(context.Background())
	if err := m.Close(context.Background(), ShutdownDisconnect); err != nil {
		t.Fatal(err)
	}
	m2 := openMgr(t, testConfig(dir, f.driver))
	b := acquireSvc(t, m2, ownerA, svcRequest())
	if b.ServiceInfo().Generation == gen {
		t.Fatal("reattached unverified generation after restart")
	}
	// The old generation was marked lost at Open, never reattached.
	infos := m2.serviceInfos(oldEnv, 1)
	for _, in := range infos {
		if in.Generation == gen && in.State != "lost" && in.State != "stopped" {
			t.Fatalf("old generation state = %s, want lost/stopped", in.State)
		}
	}
}
