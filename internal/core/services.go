package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/store"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Managed services are kind=service activities on a dedicated env.
// Three identities stay distinct (the Ghidra contract): the service
// generation, each borrower's borrow lease, and the transaction
// Lock. StopService is generation-fenced: a stale generation is
// rejected with ErrConflict and application work is never replayed.
// The env's holder ("system") lease is non-expiring while the
// service activity's desired state is running: the supervisor holds
// it by construction, not by an auto-Keepalive renewal tick. The
// supervisor provisions the env, starts the process, probes to
// ready, then probes on ProbeInterval; borrowers share the env via
// borrows whose release never stops the service. Idle expiry, drain
// and failure retention follow the old env/services.go semantics.

type serviceParams struct {
	Info envspec.ServiceInfo `json:"info"`
	Spec envspec.ServiceSpec `json:"spec"`
}

type managedService struct {
	m        *Manager
	mu       sync.Mutex
	info     envspec.ServiceInfo
	spec     envspec.ServiceSpec
	envID    string
	gate     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	ready    chan struct{}
	stopOnce sync.Once
	lastUse  time.Time
	leases   map[string]func()
}

func normalizeService(r envspec.ServiceRequest) (envspec.ServiceRequest, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	r = envspec.ServiceRequest{}
	if err = json.Unmarshal(b, &r); err != nil {
		return r, err
	}
	s := &r.Service
	if r.Key == "" || s.Name == "" || s.Version == "" || len(s.Start) == 0 || len(s.Ready) == 0 {
		return r, Invalidf("service requires key, name, version, start and readiness commands")
	}
	for _, d := range []*time.Duration{&s.StartupTimeout, &s.ProbeInterval, &s.ProbeTimeout, &s.IdleTimeout, &s.StopTimeout} {
		if *d < 0 {
			return r, Invalidf("negative service duration")
		}
	}
	if s.StartupTimeout == 0 {
		s.StartupTimeout = time.Minute
	}
	if s.ProbeInterval == 0 {
		s.ProbeInterval = 15 * time.Second
	}
	if s.ProbeTimeout == 0 {
		s.ProbeTimeout = 5 * time.Second
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = 5 * time.Minute
	}
	if s.StopTimeout == 0 {
		s.StopTimeout = 10 * time.Second
	}
	return r, nil
}

// AcquireService shares only within an exact owner/session and
// caller-selected key. Job identity belongs to the borrow.
func (m *Manager) AcquireService(ctx context.Context, owner envspec.Owner, request envspec.ServiceRequest) (envspec.ServiceLease, error) {
	if err := m.requireHealthy(); err != nil {
		return nil, err
	}
	if owner.ID == "" {
		return nil, Invalidf("service owner is required")
	}
	if err := m.requireOpenOwner(owner); err != nil {
		return nil, err
	}
	borrowOwner := owner
	r, err := normalizeService(request)
	if err != nil {
		return nil, err
	}
	owner.Job = ""
	profileDigest := m.profileDigest(r.Environment.Profile)
	b, _ := json.Marshal([]any{owner, r, profileDigest})
	h := sha256.Sum256(b)
	key := hex.EncodeToString(h[:])

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	s := m.services[key]
	if s != nil {
		s.mu.Lock()
		// Only a starting/ready generation can be joined; draining,
		// stopped, failed and lost generations are kept addressable
		// for their borrowers while a replacement starts.
		terminal := s.info.State != "starting" && s.info.State != "ready"
		s.mu.Unlock()
		if terminal {
			m.services[s.info.ID] = s // keep addressable for its borrowers
			s = nil
		}
	}
	if s == nil {
		s = &managedService{
			m: m, spec: r.Service, ready: make(chan struct{}), done: make(chan struct{}),
			stop: make(chan struct{}), gate: make(chan struct{}, 1),
			lastUse: m.now(), leases: map[string]func(){},
			info: envspec.ServiceInfo{ID: envspec.NewID("svc_"), Generation: envspec.NewID("gen_"),
				Name: r.Service.Name, Version: r.Service.Version, State: "starting"},
		}
		m.services[key] = s
		m.wg.Add(1)
		go s.run(owner, r.Environment)
	}
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.ready:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := m.requireOpenOwner(borrowOwner); err != nil {
		return nil, err
	}
	if s.info.State != "ready" {
		return nil, fmt.Errorf("%w: service %s: %s", envspec.ErrNotReady, s.info.ID, s.info.Failure)
	}
	id := envspec.NewID("service_borrow_")
	release, err := m.Borrow(m.ctx, s.envID, id)
	if err != nil {
		return nil, err
	}
	// A service borrow lasts until Release or drain, not the generic
	// borrow TTL: clear its expiry once. The borrower's identity (the
	// borrow row + Handle.borrowJob) stays distinct from the service
	// generation and from the transaction Lock.
	if err := m.store.SetBorrow(s.envID, id, time.Time{}); err != nil {
		release()
		return nil, err
	}
	s.leases[id] = release
	s.lastUse = m.now()
	s.info.Borrowers = len(s.leases)
	s.persistLocked("service.borrowed")
	e, err := m.freshEnv(s.envID)
	if err != nil {
		release()
		delete(s.leases, id)
		return nil, err
	}
	return &serviceLease{
		s: s, id: id,
		h: Handle{m: m, EnvID: s.envID, Generation: e.Generation, borrowJob: id},
	}, nil
}

func (s *managedService) activityName() string { return "service:" + s.info.ID }

// persistLocked writes the ServiceInfo into its activity row + event.
func (s *managedService) persistLocked(kind string) {
	m := s.m
	if s.envID == "" {
		return
	}
	e, err := m.freshEnv(s.envID)
	if err != nil {
		return
	}
	raw, _ := json.Marshal(serviceParams{Info: s.info, Spec: s.spec})
	act, err := m.store.GetActivity(s.envID, e.Generation, s.activityName())
	if err != nil {
		act = store.Activity{Env: s.envID, Generation: e.Generation, Name: s.activityName(),
			Kind: store.ActivityService, Desired: "running", State: store.ActivityRunning}
	}
	act.Params = string(raw)
	if s.info.State == "ready" || s.info.State == "draining" {
		act.State = store.ActivityRunning
	} else if s.info.State == "failed" || s.info.State == "lost" {
		act.State = store.ActivityFailed
		act.LastError = s.info.Failure
	} else if s.info.State == "stopped" {
		act.State = store.ActivityStopped
		act.Desired = "stopped"
	}
	if err := m.store.UpsertActivity(act); err == nil {
		_ = m.store.UpdateActivity(act)
	}
	m.emit(kind, s.envID, map[string]any{"service": s.info.ID, "generation": s.info.Generation, "state": s.info.State})
}

func (s *managedService) setState(state string, cause error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info.State = state
	if cause != nil {
		s.info.Failure = s.m.redact(cause.Error())
	}
	s.persistLocked("service." + state)
}

func (s *managedService) variables(input map[string]string) map[string]string {
	out := make(map[string]string, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	out["CRAGGY_SERVICE_GENERATION"] = s.info.Generation
	return out
}

func (s *managedService) run(owner envspec.Owner, spec envspec.Spec) {
	m := s.m
	defer m.wg.Done()
	defer close(s.done)
	readyClosed := false
	defer func() {
		if !readyClosed {
			close(s.ready)
		}
	}()

	spec.Key = s.info.ID // dedicated env; never collides with user keys
	spec.TTL = 0         // system lease: non-expiring while desired=running
	startup, cancel := context.WithTimeout(m.ctx, s.spec.StartupTimeout)
	defer cancel()
	h, err := m.Provision(startup, owner, spec)
	if err != nil {
		s.setState("failed", err)
		return
	}
	// A profile default may still have armed a lease expiry. Clear it
	// once, at acquisition: the supervisor holds this lease for the
	// service's life and no tick renews it.
	if _, err := m.store.RenewLease(h.EnvID, h.LeaseID, h.Epoch, time.Time{}, m.now()); err != nil {
		s.setState("failed", err)
		return
	}
	s.mu.Lock()
	s.envID = h.EnvID
	s.info.Environment = h.EnvID
	s.info.Image = h.Info().Image
	s.mu.Unlock()
	s.setState("starting", nil)
	defer func() {
		c, stop := context.WithTimeout(context.Background(), s.spec.StopTimeout)
		defer stop()
		_ = m.Release(c, h.EnvID)
	}()

	lifetime, end := context.WithCancel(m.ctx)
	defer end()
	e, r, err := m.checkHandle(h)
	if err != nil {
		s.setState("failed", err)
		return
	}
	p, _, err := m.execProcess(lifetime, e, r, adapter.ExecCmd{
		Argv: s.spec.Start, Env: s.variables(nil)})
	if err != nil {
		s.setState("failed", err)
		return
	}
	var output sync.WaitGroup
	for _, rd := range []io.Reader{p.Stdout, p.Stderr} {
		if rd != nil {
			output.Add(1)
			go func(rd io.Reader) { defer output.Done(); _, _ = io.Copy(io.Discard, rd) }(rd)
		}
	}
	exited := make(chan error, 1)
	go func() {
		code, err := p.Wait()
		output.Wait()
		if err == nil {
			err = fmt.Errorf("service process exited %d", code)
		}
		exited <- err
	}()
	defer func() {
		end()
		_ = p.Signal("KILL")
		select {
		case <-exited:
		case <-time.After(s.spec.StopTimeout):
		}
	}()

	probe := func(ctx context.Context) error {
		c, ccancel := context.WithTimeout(ctx, s.spec.ProbeTimeout)
		defer ccancel()
		e2, r2, err := m.checkHandle(h)
		if err != nil {
			return err
		}
		_, stderr, code, err := m.execCapture(c, e2, r2, envspec.Cmd{Argv: s.spec.Ready, Env: s.variables(nil)})
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("readiness exited %d: %s", code, stderr)
		}
		return nil
	}
	for {
		if err = probe(startup); err == nil {
			break
		}
		select {
		case <-startup.Done():
			s.setState("failed", fmt.Errorf("startup: %w (%v)", startup.Err(), err))
			return
		case err = <-exited:
			exited <- err
			s.setState("failed", err)
			return
		case <-s.stop:
			s.setState("stopped", nil)
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.setState("ready", nil)
	close(s.ready)
	readyClosed = true

	tick := time.NewTicker(min(s.spec.ProbeInterval, time.Second))
	defer tick.Stop()
	lastProbe := m.now()
	for {
		select {
		case <-m.ctx.Done():
			s.setState("lost", errors.New("manager closed; restore from committed application state"))
			return
		case err = <-exited:
			exited <- err
			s.setState("failed", err)
			s.retainFailed()
			return
		case <-s.stop:
			s.setState("draining", nil)
			c, cstop := context.WithTimeout(m.ctx, s.spec.StopTimeout)
			drained := s.waitBorrowers(c)
			if drained && len(s.spec.Stop) > 0 {
				_, _, _, _ = m.ExecCapture(c, h, envspec.Cmd{Argv: s.spec.Stop, Env: s.variables(nil)})
			}
			cstop()
			if drained {
				s.setState("stopped", nil)
			} else {
				s.setState("lost", errors.New("drain deadline exceeded"))
			}
			return
		case <-tick.C:
			// No lease renewal here: the system lease is non-expiring
			// and borrower borrows are non-expiring (AcquireService).
			s.mu.Lock()
			idle := len(s.leases) == 0 && m.now().Sub(s.lastUse) >= s.spec.IdleTimeout
			s.mu.Unlock()
			if idle {
				s.stopOnce.Do(func() { close(s.stop) })
				continue
			}
			if m.now().Sub(lastProbe) >= s.spec.ProbeInterval {
				lastProbe = m.now()
				if err = probe(m.ctx); err != nil {
					s.setState("failed", err)
					s.retainFailed()
					return
				}
			}
		}
	}
}

func (s *managedService) retainFailed() {
	ctx, cancel := context.WithCancel(s.m.ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
		}
		timer := time.NewTimer(s.spec.StopTimeout)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
			cancel()
		}
	}()
	if !s.waitBorrowers(ctx) {
		s.setState("lost", errors.New("failed service drain interrupted or deadline exceeded"))
	}
}

func (s *managedService) waitBorrowers(ctx context.Context) bool {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		s.mu.Lock()
		n := len(s.leases)
		s.mu.Unlock()
		if n == 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
	}
}

// runServiceActivity is the reconciler's view of a service activity:
// in this manager's life the supervisor goroutine drives it, so the
// runner just reflects the persisted info until stopped. Rows left by
// a previous manager were marked lost at Open.
func (m *Manager) runServiceActivity(ctx context.Context, e store.Env, act store.Activity) {
	m.mu.Lock()
	var found *managedService
	for _, s := range m.services {
		if s.activityName() == act.Name {
			found = s
		}
	}
	m.mu.Unlock()
	if found == nil {
		m.acts.finish(act, fmt.Errorf("service generation not supervised by this manager"), true)
		return
	}
	m.acts.markRunning(act)
	select {
	case <-ctx.Done():
	case <-found.done:
	}
	m.acts.finish(act, nil, false)
}

// StopService drains this exact generation. Stale generations are
// rejected with ErrConflict; application work is never replayed.
func (m *Manager) StopService(ctx context.Context, id, generation string) error {
	m.mu.Lock()
	var found *managedService
	for _, s := range m.services {
		if s.info.ID == id {
			found = s
			break
		}
	}
	m.mu.Unlock()
	if found == nil {
		return fmt.Errorf("%w: service %s", envspec.ErrNotFound, id)
	}
	found.mu.Lock()
	gen := found.info.Generation
	if gen != generation {
		found.mu.Unlock()
		return fmt.Errorf("%w: stale service generation", envspec.ErrConflict)
	}
	// Mark draining before signalling stop: existing borrowers may
	// finish, but AcquireService only joins a ready generation.
	if found.info.State == "ready" {
		found.info.State = "draining"
		found.persistLocked("service.draining")
	}
	found.mu.Unlock()
	found.stopOnce.Do(func() { close(found.stop) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-found.done:
		return nil
	}
}

// serviceInfos projects persisted service activities for Info.
func (m *Manager) serviceInfos(envID string, gen int64) []envspec.ServiceInfo {
	acts, err := m.store.Activities(envID, gen)
	if err != nil {
		return nil
	}
	var out []envspec.ServiceInfo
	for _, act := range acts {
		if act.Kind != store.ActivityService {
			continue
		}
		var sp serviceParams
		if json.Unmarshal([]byte(act.Params), &sp) == nil && sp.Info.ID != "" {
			out = append(out, sp.Info)
		}
	}
	return out
}

// reconcileServicesAtOpen marks previous managers' services lost:
// application connections are never reattached unverified. Their envs
// are released through the ordinary durable path.
func (m *Manager) reconcileServicesAtOpen(ctx context.Context) {
	rows, err := m.store.ListEnvs()
	if err != nil {
		return
	}
	for _, e := range rows {
		acts, err := m.store.Activities(e.ID, e.Generation)
		if err != nil {
			continue
		}
		for _, act := range acts {
			if act.Kind != store.ActivityService {
				continue
			}
			var sp serviceParams
			if json.Unmarshal([]byte(act.Params), &sp) != nil || sp.Info.ID == "" {
				continue
			}
			if sp.Info.State == "stopped" || sp.Info.State == "lost" {
				continue
			}
			sp.Info.State = "lost"
			sp.Info.Borrowers = 0
			sp.Info.Failure = "manager restarted; application connection not reattached"
			raw, _ := json.Marshal(sp)
			act.Params = string(raw)
			act.State = store.ActivityFailed
			act.Desired = "stopped"
			_ = m.store.UpdateActivity(act)
			m.emit("service.lost", e.ID, map[string]any{"service": sp.Info.ID, "generation": sp.Info.Generation})
			if !e.Phase.Terminal() {
				_ = m.Release(ctx, e.ID)
			}
		}
	}
}

// serviceLease is one borrower's connection. Release drops the borrow,
// not the shared environment.
type serviceLease struct {
	s  *managedService
	id string
	h  Handle

	mu       sync.Mutex
	released bool
}

var _ envspec.ServiceLease = (*serviceLease)(nil)

func (l *serviceLease) ServiceInfo() envspec.ServiceInfo {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	return l.s.info
}

func (l *serviceLease) ID() string { return l.h.EnvID }

func (l *serviceLease) Info() envspec.Info { return l.h.Info() }

func (l *serviceLease) valid() error {
	l.mu.Lock()
	released := l.released
	l.mu.Unlock()
	if released {
		return ErrClosed
	}
	i := l.ServiceInfo()
	if i.State != "ready" && i.State != "draining" {
		return fmt.Errorf("%w: service %s is %s: %s", envspec.ErrNotReady, i.ID, i.State, i.Failure)
	}
	return nil
}

// Lock serialises a complete application transaction.
func (l *serviceLease) Lock(ctx context.Context) (func(), error) {
	if err := l.valid(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case l.s.gate <- struct{}{}:
	}
	if err := l.valid(); err != nil {
		<-l.s.gate
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { <-l.s.gate }) }, nil
}

func (l *serviceLease) Exec(ctx context.Context, cmd envspec.Cmd) ([]byte, []byte, int, error) {
	if err := l.valid(); err != nil {
		return nil, nil, 0, err
	}
	cmd.Env = l.s.variables(cmd.Env)
	return l.h.Exec(ctx, cmd)
}

func (l *serviceLease) Push(ctx context.Context, src envspec.Source, dst string) error {
	if err := l.valid(); err != nil {
		return err
	}
	_, err := l.s.m.Push(ctx, l.h, src, dst, xfer.Options{})
	return err
}

func (l *serviceLease) Pull(ctx context.Context, src string, dst envspec.Sink) error {
	// Pull remains available after process failure for evidence retention.
	l.mu.Lock()
	released := l.released
	l.mu.Unlock()
	if released {
		return ErrClosed
	}
	_, err := l.s.m.Pull(ctx, l.h, src, dst, xfer.Options{})
	return err
}

func (l *serviceLease) Release(context.Context) error {
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	l.mu.Unlock()
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if release := l.s.leases[l.id]; release != nil {
		release()
		delete(l.s.leases, l.id)
	}
	l.s.info.Borrowers = len(l.s.leases)
	l.s.lastUse = l.s.m.now()
	l.s.persistLocked("service.returned")
	return nil
}
