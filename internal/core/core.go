// Package core is the durable lifecycle manager: desired state,
// leases with epochs, durable operations, admission, conditions and
// supervised activities over runtime adapters. It is the only writer
// of the state store and the only place lifecycle decisions are made;
// adapters (internal/adapter) report facts and execute actions.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/hostmon"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/store"
	"github.com/craggy-io/environment-services/internal/vault"
	"github.com/craggy-io/foundation-kit/fsx"
)

// Clock abstracts time for tests.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// Quota bounds environments held per owner pattern and per session.
type Quota struct {
	OwnerGlob     string
	MaxContainers int
	MaxAndroid    int
	MaxHost       int
	MaxTotal      int
	MaxPerSession int
}

var defaultQuotas = []Quota{
	{OwnerGlob: "agent:*", MaxContainers: 6, MaxAndroid: 2, MaxHost: 4, MaxPerSession: 8},
}

// AccountInjector delivers an "account" credential. It receives a
// lease-checked handle and the vault lease; values stay inside Use.
type AccountInjector func(ctx context.Context, h Handle, lease *vault.Lease) error

// Config opens a Manager.
type Config struct {
	Dir       string
	ManagerID string // random when empty
	Adapters  []adapter.Adapter
	Host      *hostmon.Monitor  // optional; nil disables measured admission
	Vault     *vault.Vault      // optional; specs with Creds fail without it
	Redactor  *vault.Redactor   // optional; refreshed when cred leases change
	Signals   *signalcore.Hub   // optional; nil disables collector activities
	Profiles  []envspec.Profile // beside stock and <Dir>/profiles; a duplicate name is an error
	Quotas    []Quota
	Clock     Clock
	// MutationPolicy gates autonomous repair/reap; nil allows all.
	MutationPolicy envspec.MutationPolicy
	Injectors      map[string]AccountInjector

	// Tuning (zero = defaults).
	HealthInterval   time.Duration // presence-probe interval; 15s
	ReapInterval     time.Duration // op/lease reconciler tick; 2s
	EventRetention   time.Duration // 24h
	DefaultBorrowTTL time.Duration // 10m
	BackoffBase      time.Duration // 1s
	BackoffMax       time.Duration // 30s
	ActivityStopWait time.Duration // ack wait when stopping activities; 2s
}

// ShutdownMode selects what Close does with live environments.
type ShutdownMode int

const (
	ShutdownDisconnect ShutdownMode = iota // stop loops; resources keep running for the next Open
	ShutdownDrain                          // release everything first (durable ops retry next Open if cut short)
)

// Filter selects environments for List.
type Filter struct {
	Owner   string
	Session string
	Kind    envspec.Kind
	State   envspec.State
	Live    bool // only non-terminal
}

// runtimeEnv is the live, never-escaping runtime object for one env
// generation. A fresh one is built per generation (warm claim, pool
// return, recovery); it is never reused across holders.
type runtimeEnv struct {
	id         string
	generation int64
	inst       adapter.Instance
	ad         adapter.Adapter

	mu         sync.Mutex
	credEnv    map[string]string
	credLeases []string
	lastObs    adapter.Observation
	life       context.Context
	cancelLife context.CancelCauseFunc
}

func newRuntime(id string, gen int64, inst adapter.Instance, ad adapter.Adapter) *runtimeEnv {
	life, cancel := context.WithCancelCause(context.Background())
	return &runtimeEnv{id: id, generation: gen, inst: inst, ad: ad,
		credEnv: map[string]string{}, life: life, cancelLife: cancel}
}

func (r *runtimeEnv) operationContext(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(r.life, func() { cancel(context.Cause(r.life)) })
	return ctx, func() { stop(); cancel(nil) }
}

func (r *runtimeEnv) cancelOperations(cause error) { r.cancelLife(cause) }

type provisionCall struct {
	done chan struct{}
}

// Manager owns environments, leases, admission and lifecycle events.
// Safe for concurrent use. Exactly one controller per Dir (lock file).
type Manager struct {
	cfg   Config
	store *store.Store
	lock  *fsx.Lock
	clock Clock

	mu           sync.Mutex
	profiles     map[string]*profileState
	runtime      map[string]*runtimeEnv
	provisions   map[string]*provisionCall
	services     map[string]*managedService
	closed       bool
	driveLocks   map[string]*sync.Mutex
	backendStart chan struct{}
	unwatch      []func()
	provMu       sync.Mutex // serialises admission + provision insert
	warmMu       sync.Mutex
	warming      map[string]bool

	acts *activityEngine

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Open opens the manager: state database, profiles, crash recovery of
// pending operations, then background reconcilers.
func Open(ctx context.Context, c Config) (*Manager, error) {
	if c.Dir == "" {
		return nil, Invalidf("Config.Dir is required")
	}
	for _, d := range []string{"", "signals", "tmp", "host/instances", "android/instances"} {
		if err := os.MkdirAll(filepath.Join(c.Dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	if c.ManagerID == "" {
		c.ManagerID = envspec.NewID("mgr_")
	}
	if c.HealthInterval <= 0 {
		c.HealthInterval = 15 * time.Second
	}
	if c.ReapInterval <= 0 {
		c.ReapInterval = 2 * time.Second
	}
	if c.EventRetention <= 0 {
		c.EventRetention = 24 * time.Hour
	}
	if c.DefaultBorrowTTL <= 0 {
		c.DefaultBorrowTTL = 10 * time.Minute
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.ActivityStopWait <= 0 {
		c.ActivityStopWait = 2 * time.Second
	}
	if len(c.Quotas) == 0 {
		c.Quotas = defaultQuotas
	}

	// An OS file lock, so a crashed manager never leaves a stale lock.
	lockPath := filepath.Join(c.Dir, "manager.lock")
	lock, err := fsx.TryLock(lockPath)
	if err != nil {
		if errors.Is(err, fsx.ErrLocked) {
			return nil, fmt.Errorf("core: another manager owns %s (single controller per Dir): %w", lockPath, err)
		}
		return nil, err
	}
	st, err := store.Open(filepath.Join(c.Dir, "state.db"))
	if err != nil {
		_ = lock.Unlock()
		return nil, err
	}
	_ = os.RemoveAll(filepath.Join(c.Dir, "tmp"))
	_ = os.MkdirAll(filepath.Join(c.Dir, "tmp"), 0o755)

	m := &Manager{
		cfg: c, store: st, lock: lock,
		profiles:     map[string]*profileState{},
		runtime:      map[string]*runtimeEnv{},
		provisions:   map[string]*provisionCall{},
		services:     map[string]*managedService{},
		driveLocks:   map[string]*sync.Mutex{},
		warming:      map[string]bool{},
		backendStart: make(chan struct{}, 1),
	}
	m.acts = newActivityEngine(m)
	if c.Clock != nil {
		m.clock = c.Clock
	} else {
		m.clock = realClock{}
	}
	for _, ad := range c.Adapters {
		if w, ok := ad.(adapter.ComponentWatcher); ok {
			m.unwatch = append(m.unwatch, w.WatchComponents(m.recordComponentChange))
		}
	}
	if c.Signals != nil {
		m.unwatch = append(m.unwatch, c.Signals.WatchLifecycle(m.recordCollectorLifecycle))
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())

	all, err := loadProfiles(ctx, c.Dir, c.Profiles)
	if err != nil {
		m.shutdown()
		return nil, err
	}
	if err := m.RegisterProfiles(all...); err != nil {
		m.shutdown()
		return nil, err
	}

	if err := m.recover(ctx); err != nil {
		m.shutdown()
		return nil, err
	}
	m.reconcileServicesAtOpen(ctx)

	for _, ad := range c.Adapters {
		for _, kind := range ad.Describe().Kinds {
			if _, starts := ad.(adapter.BackendStarter); !starts {
				m.prepareProfiles(kind)
			} else if chk, ok := ad.(adapter.Checker); ok && chk.Check(ctx) == nil {
				m.prepareProfiles(kind)
			}
		}
	}
	if c.Host != nil {
		c.Host.Subscribe(func(s envspec.HostSnapshot) { m.publishHostMetrics(s) })
	}

	m.wg.Add(2)
	go m.opLoop()
	go m.actLoop()
	go m.topUpWarmPools()
	return m, nil
}

func (m *Manager) now() time.Time { return m.clock.Now() }

// lockDrive serialises lifecycle drives (acquire/release) per env: the
// op loop, a synchronous Provision/Release call and recovery must
// never drive the same env concurrently.
func (m *Manager) lockDrive(envID string) func() {
	m.mu.Lock()
	l, ok := m.driveLocks[envID]
	if !ok {
		l = &sync.Mutex{}
		m.driveLocks[envID] = l
	}
	m.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ManagerID returns this manager's identity.
func (m *Manager) ManagerID() string { return m.cfg.ManagerID }

// Dir returns the state directory.
func (m *Manager) Dir() string { return m.cfg.Dir }

// Store exposes the durable store (in-repo tests and assembly only).
func (m *Manager) Store() *store.Store { return m.store }

// Healthy reports whether durable writes are succeeding. While false,
// state-changing calls fail with ErrStoreUnhealthy.
func (m *Manager) Healthy() bool { return m.store.Healthy() }

func (m *Manager) requireHealthy() error {
	if !m.store.Healthy() {
		return ErrStoreUnhealthy
	}
	return nil
}

func (m *Manager) publishHostMetrics(s envspec.HostSnapshot) {
	if m.cfg.Signals == nil {
		return
	}
	m.cfg.Signals.Publish(envspec.SignalEvent{Stream: envspec.StreamHostMetrics, Source: "host", Time: s.Time, Data: s})
}

// adapterFor picks the adapter serving kind. attach=true requires an
// Attacher; otherwise a Creator is preferred (so an attach-only
// physical adapter never receives provisioning).
func (m *Manager) adapterFor(kind envspec.Kind, attach bool) (adapter.Adapter, bool) {
	var fallback adapter.Adapter
	for _, ad := range m.cfg.Adapters {
		serves := false
		for _, k := range ad.Describe().Kinds {
			if k == kind {
				serves = true
			}
		}
		if !serves {
			continue
		}
		if attach {
			if _, ok := ad.(adapter.Attacher); ok {
				return ad, true
			}
			continue
		}
		if _, ok := ad.(adapter.Creator); ok {
			return ad, true
		}
		if fallback == nil {
			fallback = ad
		}
	}
	return fallback, fallback != nil
}

// adapterForEnv finds the adapter that can re-open an env's instance:
// creators first, then attachers, matching relationship by op history.
func (m *Manager) adapterForEnv(e store.Env) (adapter.Adapter, bool) {
	if op, err := m.store.OpenOperation(e.ID, store.OpAttach); err == nil && op.ID != "" {
		return m.adapterFor(e.Kind, true)
	}
	ops, _ := m.store.ListOperations(e.ID)
	for _, op := range ops {
		if op.Kind == store.OpAttach {
			return m.adapterFor(e.Kind, true)
		}
	}
	return m.adapterFor(e.Kind, false)
}

func (m *Manager) relationshipOf(ad adapter.Adapter) adapter.Relationship {
	return ad.Describe().Relationship
}

func (m *Manager) driverKinds() string {
	seen := map[envspec.Kind]bool{}
	var kinds []string
	for _, ad := range m.cfg.Adapters {
		for _, k := range ad.Describe().Kinds {
			if !seen[k] {
				seen[k] = true
				kinds = append(kinds, string(k))
			}
		}
	}
	out := ""
	for i, k := range kinds {
		if i > 0 {
			out += ", "
		}
		out += k
	}
	return out
}

// Drivers lists registered adapter descriptions.
func (m *Manager) Drivers() []adapter.Description {
	out := make([]adapter.Description, 0, len(m.cfg.Adapters))
	for _, ad := range m.cfg.Adapters {
		out = append(out, ad.Describe())
	}
	return out
}

func refOf(e store.Env) adapter.Ref {
	var ref adapter.Ref
	if e.Instance != "" {
		_ = json.Unmarshal([]byte(e.Instance), &ref)
	}
	if ref.EnvID == "" {
		ref.EnvID = e.ID
	}
	if ref.Name == "" {
		ref.Name = adapter.NameFor(e.ID)
	}
	return ref
}

func refJSON(ref adapter.Ref) string {
	b, _ := json.Marshal(ref)
	return string(b)
}

func (m *Manager) freshEnv(id string) (store.Env, error) { return m.store.GetEnv(id) }

func (m *Manager) refreshRuntimeEnv(id string) *runtimeEnv {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.runtime[id]
}

func (m *Manager) putRuntime(r *runtimeEnv) {
	m.mu.Lock()
	m.runtime[r.id] = r
	m.mu.Unlock()
}

func (m *Manager) dropRuntime(id string, cause error) {
	m.mu.Lock()
	r := m.runtime[id]
	delete(m.runtime, id)
	m.mu.Unlock()
	if r != nil {
		r.cancelOperations(cause)
	}
	m.acts.dropEnv(id)
}

func (m *Manager) emit(kind, envID string, data map[string]any) {
	ev := envspec.LifecycleEvent{Kind: kind, Env: envID, Time: m.now(), Data: data}
	if envID != "" {
		if row, err := m.store.GetEnv(envID); err == nil {
			ev = store.EventFor(kind, row, m.now(), data)
		}
	}
	_ = m.store.Emit(ev)
}

func (m *Manager) redact(s string) string {
	if m.cfg.Redactor == nil {
		return s
	}
	return m.cfg.Redactor.RedactString(s)
}

func (m *Manager) failureData(reason string, err error, extra map[string]any) map[string]any {
	data := map[string]any{}
	for k, v := range extra {
		data[k] = v
	}
	if reason != "" {
		data["reason"] = reason
	}
	if err != nil {
		data["error"] = m.redact(err.Error())
	}
	return data
}

func (m *Manager) refreshRedactor() {
	if m.cfg.Redactor != nil && m.cfg.Vault != nil {
		_ = m.cfg.Redactor.Refresh(m.cfg.Vault)
	}
}

// recordComponentChange is the adapters' component sink.
func (m *Manager) recordComponentChange(envID string, c adapter.ComponentChange) {
	kind := ""
	if c.From != c.To {
		switch c.To {
		case adapter.ComponentRepairing:
			kind = "component.repairing"
		case adapter.ComponentDegraded:
			kind = "component.degraded"
		case adapter.ComponentSuspect:
			kind = "component.suspect"
		case adapter.ComponentDisabled:
			kind = "component.failed"
		case adapter.ComponentOK:
			if c.From != "" {
				kind = "component.repaired"
			}
		}
	}
	if kind == "" {
		return
	}
	data := map[string]any{"component": c.Component, "from": c.From, "to": c.To, "repairs": c.Repairs}
	if c.Reason != "" {
		data["reason"] = c.Reason
	}
	if c.Error != "" {
		data["error"] = m.redact(c.Error)
	}
	if c.Capture != "" {
		data["capture"] = c.Capture
	}
	if c.Package != "" {
		data["package"] = c.Package
	}
	m.emit(kind, envID, data)
}

func (m *Manager) recordCollectorLifecycle(l signalcore.Lifecycle) {
	if l.Kind != "collector.failed" {
		return
	}
	reason := FailureReason(l.Err)
	if reason == "" {
		reason = ReasonUnknown
	}
	m.emit("collector.failed", l.Source, m.failureData(reason, l.Err,
		map[string]any{"collector": l.Collector, "name": l.Name, "source": l.Source}))
}

// --- recovery ---

// recover re-observes every non-terminal env and resumes its persisted
// intent: pending provision/attach ops are re-driven, releases keep
// releasing, present envs get fresh runtime objects. Backend-down
// observations leave presence unknown and ops pending — absence is
// never invented.
func (m *Manager) recover(ctx context.Context) error {
	rows, err := m.store.ListEnvs()
	if err != nil {
		return err
	}
	for _, e := range rows {
		if e.Phase.Terminal() {
			continue
		}
		ad, ok := m.adapterForEnv(e)
		if !ok {
			continue // no adapter this Open; rows stay for a later Open that has one
		}
		m.recoverEnv(ctx, e, ad)
	}
	return nil
}

func (m *Manager) recoverEnv(ctx context.Context, e store.Env, ad adapter.Adapter) {
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	ref := refOf(e)
	obs, err := ad.Observe(ctx, oc, ref)
	if err != nil {
		obs = adapter.Observation{Ref: ref, Presence: adapter.Unknown, Detail: err.Error()}
	}
	if obs.Presence == adapter.Unknown && e.Instance == "" {
		// No recorded instance and the backend cannot answer:
		// rediscovery is impossible right now. Stay unknown.
		_, _ = m.store.SetPresence(e, store.PresenceUnknown, m.now())
		return
	}
	if obs.Presence == adapter.Absent && e.Instance == "" {
		// Maybe creation happened but was never recorded: rediscover by
		// deterministic name through Discover.
		if cand, found := m.discoverFor(ctx, ad, e); found {
			ref = cand.Ref
			if _, err := m.store.RecordInstance(e, refJSON(ref), e.Image, store.PresencePresent, m.now()); err == nil {
				if fresh, err := m.freshEnv(e.ID); err == nil {
					e = fresh
				}
			}
			obs, _ = ad.Observe(ctx, oc, ref)
		}
	}

	switch {
	case e.Desired == store.DesiredReleased:
		// Release intent survives: make sure an open release op exists;
		// the op loop drives it.
		if _, err := m.store.OpenOperation(e.ID, store.OpRelease); err != nil {
			_, _, _ = m.store.BeginRelease(e, m.newOp(e, store.OpRelease, "", ""), m.now())
		}
		if obs.Presence == adapter.Present {
			m.reopenRuntime(ctx, e, ad, ref, obs)
		}
	case e.Phase == envspec.StateAcquiring:
		switch obs.Presence {
		case adapter.Present:
			m.reopenRuntime(ctx, e, ad, ref, obs)
			// The op loop resumes the readiness barriers.
		case adapter.Absent:
			if e.Instance != "" {
				// It existed (recorded) and is definitively gone.
				op := m.openAcquireOp(e)
				_, _ = m.store.MarkFailed(e, opIDOf(op), "resource lost during provisioning", m.now())
			}
			// No instance recorded and nothing discovered: the op loop
			// re-dispatches Create/Attach (idempotent by name).
		case adapter.Unknown:
			_, _ = m.store.SetPresence(e, store.PresenceUnknown, m.now())
		}
	case e.Phase == envspec.StatePresent:
		switch obs.Presence {
		case adapter.Present:
			m.reopenRuntime(ctx, e, ad, ref, obs)
		case adapter.Absent:
			_, _ = m.store.MarkFailed(e, "", "environment lost: not found on reconcile", m.now())
			if m.cfg.Vault != nil {
				_ = m.cfg.Vault.ReleaseHolder(ctx, "env:"+e.ID)
			}
		case adapter.Unknown:
			_, _ = m.store.SetPresence(e, store.PresenceUnknown, m.now())
			m.reopenRuntime(ctx, e, ad, ref, obs) // probes keep trying; handles stay fenced by phase+lease
		}
	}
}

func (m *Manager) discoverFor(ctx context.Context, ad adapter.Adapter, e store.Env) (adapter.Candidate, bool) {
	cands, err := ad.Discover(ctx, adapter.Scope{ManagerID: m.cfg.ManagerID, EnvID: e.ID})
	if err != nil {
		return adapter.Candidate{}, false
	}
	want := adapter.NameFor(e.ID)
	for _, c := range cands {
		if c.Foreign {
			continue
		}
		if c.Ref.EnvID == e.ID || c.Ref.Name == want {
			if c.Ref.EnvID == "" {
				c.Ref.EnvID = e.ID
			}
			return c, true
		}
	}
	return adapter.Candidate{}, false
}

func (m *Manager) reopenRuntime(ctx context.Context, e store.Env, ad adapter.Adapter, ref adapter.Ref, obs adapter.Observation) {
	var inst adapter.Instance
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	switch a := ad.(type) {
	case adapter.Creator:
		inst, _ = a.Open(ctx, oc, ref)
	case adapter.Attacher:
		inst, _ = a.Open(ctx, oc, ref)
	}
	if inst == nil {
		return // op loop / provision resume retries
	}
	r := newRuntime(e.ID, e.Generation, inst, ad)
	r.lastObs = obs
	m.putRuntime(r)
	m.acts.declareForEnv(e, r)
	m.rehydrateCreds(ctx, e, r)
}

func (m *Manager) openAcquireOp(e store.Env) store.Operation {
	if op, err := m.store.OpenOperation(e.ID, store.OpProvision); err == nil {
		return op
	}
	op, _ := m.store.OpenOperation(e.ID, store.OpAttach)
	return op
}

func opIDOf(op store.Operation) string { return op.ID }

func (m *Manager) newOp(e store.Env, kind store.OpKind, ownerScope, key string) store.Operation {
	now := m.now()
	return store.Operation{
		ID: envspec.NewID("op_"), Env: e.ID, Generation: e.Generation, Kind: kind,
		OwnerScope: ownerScope, RequestKey: key, Phase: string(kind),
		Outcome: store.OutcomePending, Created: now, Updated: now,
	}
}

// --- read models ---

// conditionTTLs bounds how long an observation stays fresh per
// condition name; freshness is computed at read, never stored.
var conditionTTLs = map[string]time.Duration{
	"reachable":   45 * time.Second,
	"presence":    45 * time.Second,
	"creds_ready": 0, // set once at a barrier; never goes stale
}

const defaultConditionTTL = 30 * time.Second

func freshen(c envspec.Condition, now time.Time) envspec.Condition {
	ttl, ok := conditionTTLs[c.Name]
	if !ok {
		ttl = defaultConditionTTL
	}
	if ttl > 0 && c.Status != envspec.ConditionUnknown && now.Sub(c.ObservedAt) > ttl {
		c.Status = envspec.ConditionUnknown
		c.Reason = ReasonStaleObservation
	}
	return c
}

// InfoFor projects one env row into the public Info record.
func (m *Manager) InfoFor(e store.Env) envspec.Info {
	info := envspec.Info{ID: e.ID, Kind: e.Kind, Profile: e.Profile, State: e.Phase,
		Owner: e.Owner, Spec: e.Spec, Created: e.Created, Image: e.Image, Warm: e.Warm}
	if lease, err := m.store.GetLease(e.ID); err == nil && lease.State == store.LeaseHeld {
		info.LeaseID = lease.LeaseID
		if !lease.Expires.IsZero() && !e.Phase.Terminal() {
			if left := lease.Expires.Sub(m.now()); left > 0 {
				info.TTLLeft = left
			}
		}
	}
	if bs, err := m.store.Borrows(e.ID); err == nil {
		for _, b := range bs {
			info.Borrows = append(info.Borrows, b.Job)
		}
	}
	if conds, err := m.store.Conditions(e.ID, e.Generation); err == nil {
		now := m.now()
		for _, c := range conds {
			info.Conditions = append(info.Conditions, freshen(c, now))
		}
	}
	info.Services = m.serviceInfos(e.ID, e.Generation)
	if r := m.refreshRuntimeEnv(e.ID); r != nil && r.generation == e.Generation {
		r.mu.Lock()
		info.Workspace = r.lastObs.Workspace
		info.Endpoints = r.lastObs.Endpoints
		r.mu.Unlock()
		if cr, ok := r.inst.(adapter.ComponentReporter); ok {
			info.Components = cr.Components()
		}
	}
	return info
}

// Get returns a holder handle for an env, fenced by its current lease.
func (m *Manager) Get(id string) (Handle, error) {
	if !envspec.HasPrefix(id, "env_") {
		return Handle{}, WithHint("invalid", Invalidf("%q is not an environment ID (want env_…)", id),
			"pass an environment ID from provision or list")
	}
	e, err := m.freshEnv(id)
	if err != nil {
		return Handle{}, err
	}
	lease, err := m.store.GetLease(id)
	if err != nil || lease.State != store.LeaseHeld {
		return Handle{}, fmt.Errorf("environment %s is %s (not held)", id, e.Phase)
	}
	return Handle{m: m, EnvID: id, Generation: e.Generation, LeaseID: lease.LeaseID, Epoch: lease.Epoch}, nil
}

// Info reads one env's Info.
func (m *Manager) Info(id string) (envspec.Info, error) {
	e, err := m.freshEnv(id)
	if err != nil {
		return envspec.Info{}, err
	}
	return m.InfoFor(e), nil
}

// List returns env infos matching f.
func (m *Manager) List(f Filter) []envspec.Info {
	rows, err := m.store.ListEnvs()
	if err != nil {
		return nil
	}
	var out []envspec.Info
	for _, e := range rows {
		if f.Owner != "" && e.Owner.ID != f.Owner {
			continue
		}
		if f.Session != "" && e.Owner.Session != f.Session {
			continue
		}
		if f.Kind != "" && e.Kind != f.Kind {
			continue
		}
		if f.State != "" && e.Phase != f.State {
			continue
		}
		if f.Live && e.Phase.Terminal() {
			continue
		}
		out = append(out, m.InfoFor(e))
	}
	return out
}

// Events streams persisted lifecycle events after seq, following new
// ones until ctx ends (at-least-once; consumers keep their own cursor).
func (m *Manager) Events(ctx context.Context, since uint64) iter.Seq2[envspec.LifecycleEvent, error] {
	return func(yield func(envspec.LifecycleEvent, error) bool) {
		seq := since
		for {
			evs, err := m.store.EventsSince(ctx, seq, 512)
			if err != nil {
				if errors.Is(err, context.Canceled) || ctx.Err() != nil {
					return
				}
				yield(envspec.LifecycleEvent{}, err)
				return
			}
			for _, ev := range evs {
				seq = ev.Seq
				if !yield(ev, nil) {
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
}

// EventsSince reads one page without following.
func (m *Manager) EventsSince(ctx context.Context, since uint64, limit int) ([]envspec.LifecycleEvent, error) {
	if limit <= 0 {
		limit = 512
	}
	return m.store.EventsSince(ctx, since, limit)
}

// RecentEvents reads the newest limit events, newest first.
func (m *Manager) RecentEvents(ctx context.Context, limit int) ([]envspec.LifecycleEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	return m.store.RecentEvents(ctx, limit)
}

// --- borrows, scopes, quotas ---

func (m *Manager) requireOpenOwner(o envspec.Owner) error {
	for _, scope := range []struct{ kind, id string }{{"session", o.Session}, {"job", o.Job}} {
		if m.store.ScopeClosed(scope.kind, scope.id) {
			return fmt.Errorf("%w: %s %s is closed", ErrScopeClosed, scope.kind, scope.id)
		}
	}
	return nil
}

// Borrow attaches a job to an env so destruction defers until the job
// finishes. The returned func drops the borrow (idempotent); ctx end
// also drops it.
func (m *Manager) Borrow(ctx context.Context, id, job string) (func(), error) {
	if job == "" {
		return nil, Invalidf("borrow requires a job ID")
	}
	if m.store.ScopeClosed("job", job) {
		return nil, fmt.Errorf("%w: job %s", ErrScopeClosed, job)
	}
	if _, err := m.freshEnv(id); err != nil {
		return nil, err
	}
	if err := m.store.SetBorrow(id, job, m.now().Add(m.cfg.DefaultBorrowTTL)); err != nil {
		return nil, err
	}
	var once sync.Once
	released := make(chan struct{})
	release := func() { once.Do(func() { _ = m.store.DropBorrow(id, job); close(released) }) }
	go func() {
		select {
		case <-ctx.Done():
			release()
		case <-released:
		}
	}()
	return release, nil
}

// RenewBorrow extends a borrow.
func (m *Manager) RenewBorrow(id, job string) error {
	return m.store.SetBorrow(id, job, m.now().Add(m.cfg.DefaultBorrowTTL))
}

// CloseSession closes a session scope and releases its session-held
// envs (job-held work and borrowed envs defer to the reaper).
func (m *Manager) CloseSession(ctx context.Context, session string) error {
	return m.closeScope(ctx, "session", session)
}

// CloseJob closes a job scope, drops its borrows and releases its envs.
func (m *Manager) CloseJob(ctx context.Context, job string) error {
	return m.closeScope(ctx, "job", job)
}

func (m *Manager) closeScope(ctx context.Context, kind, id string) error {
	if id == "" {
		return Invalidf("%s is required", kind)
	}
	if err := m.store.CloseScope(kind, id); err != nil {
		return err
	}
	rows, err := m.store.ListEnvs()
	if err != nil {
		return err
	}
	for _, e := range rows {
		if kind == "job" {
			_ = m.store.DropBorrow(e.ID, id)
		}
		match := (kind == "session" && e.Owner.Session == id && e.Owner.Job == "") ||
			(kind == "job" && e.Owner.Job == id)
		if !match || e.Phase.Terminal() || e.Warm {
			continue
		}
		if bs, _ := m.store.Borrows(e.ID); len(bs) > 0 {
			continue // reaper finishes once borrows drain and the lease lapses
		}
		if err := m.Release(ctx, e.ID); err != nil {
			return err
		}
	}
	return nil
}

func checkQuotas(quotas []Quota, rows []store.Env, owner envspec.Owner, kind envspec.Kind) error {
	live := func(e store.Env) bool {
		return e.Phase == envspec.StatePresent || e.Phase == envspec.StateAcquiring
	}
	countOwner := map[envspec.Kind]int{}
	countTotal, countSession := 0, 0
	for _, e := range rows {
		if !live(e) || e.Warm {
			continue
		}
		if e.Owner.ID == owner.ID && owner.ID != "" {
			countOwner[e.Kind]++
			countTotal++
		}
		if owner.Session != "" && e.Owner.Session == owner.Session {
			countSession++
		}
	}
	for _, q := range quotas {
		if q.OwnerGlob != "" {
			if ok, _ := path.Match(q.OwnerGlob, owner.ID); !ok {
				continue
			}
		}
		byKind := map[envspec.Kind]int{envspec.Container: q.MaxContainers, envspec.Android: q.MaxAndroid, envspec.Host: q.MaxHost}
		if max := byKind[kind]; max > 0 && countOwner[kind] >= max {
			return WithHint("unavailable", fmt.Errorf("%w: owner %s holds %d %s environments (max %d)", ErrQuota, owner.ID, countOwner[kind], kind, max),
				"release one, then retry")
		}
		if q.MaxTotal > 0 && countTotal >= q.MaxTotal {
			return WithHint("unavailable", fmt.Errorf("%w: owner %s holds %d environments (max %d)", ErrQuota, owner.ID, countTotal, q.MaxTotal),
				"release one, then retry")
		}
		if q.MaxPerSession > 0 && owner.Session != "" && countSession >= q.MaxPerSession {
			return WithHint("unavailable", fmt.Errorf("%w: session %s holds %d environments (max %d)", ErrQuota, owner.Session, countSession, q.MaxPerSession),
				"release one, then retry")
		}
	}
	return nil
}

// PoolCapacity reports a pool's budget plus current SUM reservations.
func (m *Manager) PoolCapacity(pool envspec.Pool) envspec.PoolCapacity {
	var pc envspec.PoolCapacity
	if m.cfg.Host != nil {
		pc = m.cfg.Host.PoolCapacity(pool)
	} else {
		pc = envspec.PoolCapacity{Pool: pool, CPUs: 1 << 20, MemoryMB: 1 << 40, FreeCPUs: 1 << 20, FreeMB: 1 << 40}
	}
	if cpus, mem, err := m.store.ReservedCost(pool); err == nil {
		pc.ReservedCPUs, pc.ReservedMemoryMB = cpus, mem
	}
	return pc
}

// --- background loops ---

func (m *Manager) opLoop() {
	defer m.wg.Done()
	t := time.NewTicker(m.cfg.ReapInterval)
	defer t.Stop()
	var lastPrune time.Time
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.ReconcileOnce(m.ctx)
			if m.now().Sub(lastPrune) > time.Hour {
				lastPrune = m.now()
				_, _ = m.store.PruneEvents(m.cfg.EventRetention, m.now())
			}
		}
	}
}

func (m *Manager) actLoop() {
	defer m.wg.Done()
	iv := m.cfg.ReapInterval
	if iv > 250*time.Millisecond {
		iv = 250 * time.Millisecond
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			m.acts.reconcile()
		case <-m.acts.wake:
			m.acts.reconcile()
		}
	}
}

// rematerialiseMissing re-runs recovery for envs that have a recorded
// instance but no runtime — the backend was down when recovery tried
// to Open them at startup. Without this they would stay unusable
// ("not running under this manager") until a manager restart, even
// after the backend returns. Envs an in-flight Provision is driving
// already retry Open on their own path.
func (m *Manager) rematerialiseMissing(ctx context.Context) {
	rows, err := m.store.ListEnvs()
	if err != nil {
		return
	}
	for _, e := range rows {
		if e.Phase.Terminal() || e.Instance == "" || m.refreshRuntimeEnv(e.ID) != nil {
			continue
		}
		m.mu.Lock()
		_, inflight := m.provisions[e.ID]
		m.mu.Unlock()
		if inflight {
			continue
		}
		ad, ok := m.adapterForEnv(e)
		if !ok {
			continue
		}
		m.recoverEnv(ctx, e, ad)
	}
}

// ReconcileOnce drives one pass of durable work: pending operations
// whose backoff expired, lease/borrow expiry, vault lease expiry and
// warm-pool top-up. It is the op loop's body and is exported so tests
// can drive retries deterministically.
func (m *Manager) ReconcileOnce(ctx context.Context) {
	if m.cfg.Vault != nil {
		_, _ = m.cfg.Vault.ExpireLeases(ctx)
	}
	m.rematerialiseMissing(ctx)
	m.reapExpired(ctx)
	ops, err := m.store.PendingOperations()
	if err != nil {
		return
	}
	now := m.now()
	for _, op := range ops {
		if !op.NextAttempt.IsZero() && now.Before(op.NextAttempt) {
			continue
		}
		e, err := m.freshEnv(op.Env)
		if err != nil || e.Phase.Terminal() {
			continue
		}
		// Skip ops an in-flight Provision call is already driving.
		m.mu.Lock()
		_, inflight := m.provisions[op.Env]
		m.mu.Unlock()
		if inflight && op.Kind != store.OpRelease {
			continue
		}
		switch op.Kind {
		case store.OpProvision, store.OpAttach:
			_ = m.resumeAcquire(ctx, e, op)
		case store.OpRelease:
			_ = m.driveRelease(ctx, e.ID, op, false)
		}
	}
}

func (m *Manager) reapExpired(ctx context.Context) {
	now := m.now()
	if m.cfg.Host != nil && m.cfg.Host.Pressure() {
		m.reapWarmUnderPressure(ctx)
	}
	rows, err := m.store.ListEnvs()
	if err != nil {
		return
	}
	for _, e := range rows {
		if e.Phase != envspec.StatePresent || e.Warm {
			continue
		}
		bs, _ := m.store.Borrows(e.ID)
		live := 0
		for _, b := range bs {
			if !b.Expires.IsZero() && now.After(b.Expires) {
				_ = m.store.DropBorrow(e.ID, b.Job)
				m.emit("lease.expired", e.ID, map[string]any{"kind": "borrow", "job": b.Job})
				continue
			}
			live++
		}
		lease, err := m.store.GetLease(e.ID)
		if err != nil {
			continue
		}
		holderAlive := lease.State == store.LeaseHeld && (lease.Expires.IsZero() || now.Before(lease.Expires))
		if !holderAlive && lease.State == store.LeaseHeld && !lease.Expires.IsZero() && now.After(lease.Expires) {
			_ = m.store.ExpireLease(e.ID, e, now)
		}
		if !holderAlive && live == 0 {
			if !m.allowMutation(e.ID, "reap") {
				continue
			}
			_ = m.Release(ctx, e.ID)
		}
	}
}

func (m *Manager) reapWarmUnderPressure(ctx context.Context) {
	rows, err := m.store.ListEnvs()
	if err != nil {
		return
	}
	for _, e := range rows {
		if e.Warm && e.Phase == envspec.StatePresent && m.allowMutation(e.ID, "reap") {
			_ = m.releaseForce(ctx, e.ID)
		}
	}
}

// allowMutation applies the experiment-hold gate to autonomous
// mutation. A denial is recorded as an event and returns false.
func (m *Manager) allowMutation(envID, kind string) bool {
	if m.cfg.MutationPolicy == nil {
		return true
	}
	ok, reason := m.cfg.MutationPolicy.AllowMutation(envID, kind)
	if !ok {
		if reason == "" {
			reason = ReasonExperimentHold
		}
		m.emit("mutation.held", envID, map[string]any{"kind": kind, "reason": reason})
	}
	return ok
}

func (m *Manager) allowMutationQuiet(envID, kind string) (bool, string) {
	if m.cfg.MutationPolicy == nil {
		return true, ""
	}
	ok, reason := m.cfg.MutationPolicy.AllowMutation(envID, kind)
	if !ok && reason == "" {
		reason = ReasonExperimentHold
	}
	return ok, reason
}

// backoffFor returns the retry delay for an attempt count (1s→30s).
func (m *Manager) backoffFor(attempt int) time.Duration {
	d := m.cfg.BackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= m.cfg.BackoffMax {
			return m.cfg.BackoffMax
		}
	}
	return d
}

// Close stops loops and closes the store. Drain releases everything
// live first (bounded by ctx; unfinished releases stay durable ops
// for the next Open). Disconnect leaves resources running.
func (m *Manager) Close(ctx context.Context, mode ShutdownMode) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	var err error
	if mode == ShutdownDrain {
		err = m.drain(ctx)
	}
	m.shutdown()
	return err
}

func (m *Manager) drain(ctx context.Context) error {
	rows, err := m.store.ListEnvs()
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, e := range rows {
		if e.Phase.Terminal() {
			continue
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if err := m.releaseForce(ctx, id); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("release %s: %w", id, err))
				mu.Unlock()
			}
		}(e.ID)
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (m *Manager) shutdown() {
	if m.cancel != nil {
		m.cancel()
	}
	m.wg.Wait()
	if m.acts != nil {
		m.acts.stopAll()
	}
	for _, stop := range m.unwatch {
		stop()
	}
	if m.store != nil {
		_ = m.store.Close()
	}
	if m.lock != nil {
		_ = m.lock.Unlock()
	}
}

// simulateCrash stops the manager like a process kill: loops end and
// the store closes without any release work. Tests use it to prove
// crash recovery against the same Dir.
func (m *Manager) simulateCrash() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.shutdown()
}
