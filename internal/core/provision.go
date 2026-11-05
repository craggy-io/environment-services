package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/store"
)

// ProvisionOption tunes Provision/Attach.
type ProvisionOption func(*provisionOpts)

type provisionOpts struct {
	progress   func(envspec.Progress)
	noWait     bool
	requestKey string
}

// OnProgress receives throttled stage progress.
func OnProgress(fn func(envspec.Progress)) ProvisionOption {
	return func(o *provisionOpts) { o.progress = fn }
}

// NoWait fails with ErrNoCapacity instead of queueing.
func NoWait() ProvisionOption { return func(o *provisionOpts) { o.noWait = true } }

// WithRequestKey sets the idempotency key (overrides Spec.Key).
func WithRequestKey(key string) ProvisionOption {
	return func(o *provisionOpts) { o.requestKey = key }
}

func requestDigest(merged envspec.Spec, owner envspec.Owner, attach bool) string {
	merged.Key = ""
	b, _ := json.Marshal(struct {
		Spec   envspec.Spec  `json:"spec"`
		Owner  envspec.Owner `json:"owner"`
		Attach bool          `json:"attach"`
	}{merged, owner, attach})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func ownerScopeOf(o envspec.Owner) string { return o.ID }

func costOf(ad adapter.Adapter, merged envspec.Spec) adapter.Cost {
	if c, ok := ad.(adapter.Coster); ok {
		return c.Cost(merged)
	}
	pool := envspec.PoolDocker
	if merged.Kind != envspec.Container {
		pool = envspec.PoolHost
	}
	return adapter.Cost{Pool: pool, CPUs: merged.Resources.CPUs, MemoryMB: merged.Resources.MemoryMB, DiskMB: merged.Resources.DiskMB}
}

// fitsLocked reports whether cost fits the pool given SUM reservations.
// Callers hold provMu so check+insert are atomic against other provisions.
func (m *Manager) fits(cost adapter.Cost) (fits bool, impossible bool, err error) {
	pc := m.PoolCapacity(cost.Pool)
	if cost.MemoryMB > pc.MemoryMB || (cost.CPUs > 0 && cost.CPUs > pc.CPUs) {
		return false, true, nil
	}
	availMem := pc.MemoryMB - pc.ReservedMemoryMB
	if pc.FreeMB-pc.ReservedMemoryMB < availMem && pc.FreeMB > 0 {
		availMem = pc.FreeMB - pc.ReservedMemoryMB
	}
	availCPU := pc.CPUs - pc.ReservedCPUs
	return cost.MemoryMB <= availMem && (cost.CPUs == 0 || cost.CPUs <= availCPU), false, nil
}

// Provision creates an environment: tx-before-dispatch durable insert,
// admission by reservation SUM, dispatch, identity verify, credential
// barrier, required-activity barrier, then — last — a lease-fenced
// handle. Repeating with the same key+digest resolves to the same op.
func (m *Manager) Provision(ctx context.Context, o envspec.Owner, s envspec.Spec, opts ...ProvisionOption) (Handle, error) {
	return m.acquire(ctx, o, s, false, opts)
}

// Attach connects to an independently existing resource through an
// Attacher adapter, with the same durable operation discipline.
func (m *Manager) Attach(ctx context.Context, o envspec.Owner, s envspec.Spec, opts ...ProvisionOption) (Handle, error) {
	return m.acquire(ctx, o, s, true, opts)
}

func (m *Manager) acquire(ctx context.Context, o envspec.Owner, s envspec.Spec, attach bool, opts []ProvisionOption) (Handle, error) {
	if err := m.requireHealthy(); err != nil {
		return Handle{}, err
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return Handle{}, ErrClosed
	}
	if o.ID == "" {
		return Handle{}, Invalidf("owner ID is required (core maps it from the registry caller)")
	}
	var po provisionOpts
	for _, f := range opts {
		f(&po)
	}
	report, flush := envspec.Throttled(po.progress, 4)
	defer flush()
	if err := m.requireOpenOwner(o); err != nil {
		return Handle{}, err
	}

	profileName := s.Profile
	if profileName == "" {
		kind := s.Kind
		if kind == "" {
			kind = envspec.Container
		}
		profileName = DefaultProfileFor(kind)
	}
	m.mu.Lock()
	ps, ok := m.profiles[profileName]
	m.mu.Unlock()
	if !ok {
		return Handle{}, WithHint("not_found", fmt.Errorf("%w: profile %q", envspec.ErrNotFound, profileName),
			"list profiles first")
	}
	merged, err := mergeSpec(ps.p, s)
	if err != nil {
		return Handle{}, err
	}
	if s.Kind != "" && s.Kind != merged.Kind {
		return Handle{}, Invalidf("spec kind %q conflicts with profile %s kind %q", s.Kind, profileName, merged.Kind)
	}
	key := s.Key
	if po.requestKey != "" {
		key = po.requestKey
	}
	digest := requestDigest(merged, o, attach)
	scope := ownerScopeOf(o)

	ad, found := m.adapterFor(merged.Kind, attach)
	if !found {
		return Handle{}, WithHint("unavailable", fmt.Errorf("no adapter registered for kind %q", merged.Kind),
			"provision a kind with a registered adapter (%s)", m.driverKinds())
	}

	e, lease, err := m.insertAcquire(ctx, acquireInsert{
		owner: o, merged: merged, profile: profileName, profileDef: ps.p,
		key: key, digest: digest, scope: scope, attach: attach, ad: ad,
		noWait: po.noWait, report: report,
	})
	if err != nil {
		return Handle{}, err
	}
	if e.Phase == envspec.StatePresent {
		// Idempotent join or warm claim: already present.
		return Handle{m: m, EnvID: e.ID, Generation: e.Generation, LeaseID: lease.LeaseID, Epoch: lease.Epoch}, nil
	}
	op := m.openAcquireOp(e)
	if err := m.driveAcquire(ctx, e, op, ad, report); err != nil {
		return Handle{}, err
	}
	e, err = m.freshEnv(e.ID)
	if err != nil {
		return Handle{}, err
	}
	lease, err = m.store.GetLease(e.ID)
	if err != nil {
		return Handle{}, err
	}
	return Handle{m: m, EnvID: e.ID, Generation: e.Generation, LeaseID: lease.LeaseID, Epoch: lease.Epoch}, nil
}

type acquireInsert struct {
	owner      envspec.Owner
	merged     envspec.Spec
	profile    string
	profileDef envspec.Profile
	key        string
	digest     string
	scope      string
	attach     bool
	ad         adapter.Adapter
	noWait     bool
	report     func(envspec.Progress)
	warm       bool // system warm-pool provision: unowned, no join
}

// insertAcquire performs, serialised: idempotent join, warm claim,
// quotas, admission wait, and the durable tx1 insert.
func (m *Manager) insertAcquire(ctx context.Context, in acquireInsert) (store.Env, store.Lease, error) {
	cost := costOf(in.ad, in.merged)
	for {
		m.provMu.Lock()
		// Idempotent join.
		if in.key != "" && !in.warm {
			if existing, err := m.store.OperationByKey(in.scope, in.key); err == nil {
				if existing.RequestDigest != in.digest {
					m.provMu.Unlock()
					return store.Env{}, store.Lease{}, fmt.Errorf("%w: request key %q reused with a different request", envspec.ErrConflict, in.key)
				}
				m.provMu.Unlock()
				return m.joinAcquire(ctx, existing)
			}
		}
		// Warm claim.
		if !in.warm && !in.attach && warmEligible(in.merged, in.profileDef) {
			now := m.now()
			var exp time.Time
			if in.merged.TTL > 0 {
				exp = now.Add(in.merged.TTL)
			}
			claimOp := store.Operation{
				ID: envspec.NewID("op_"), Kind: store.OpProvision, OwnerScope: in.scope,
				RequestKey: in.key, RequestDigest: in.digest, Created: now, Updated: now,
			}
			e, lease, won, err := m.store.ClaimWarm(in.profile, in.owner, envspec.NewID("lease_"), exp, claimOp, now)
			if err != nil {
				m.provMu.Unlock()
				return store.Env{}, store.Lease{}, err
			}
			if won {
				m.provMu.Unlock()
				m.onWarmClaimed(e)
				if in.report != nil {
					in.report(envspec.Progress{Op: "provision", Stage: "warm-claim", EnvID: e.ID, Message: "claimed warm instance"})
				}
				return e, lease, nil
			}
		}
		// Quotas.
		rows, err := m.store.ListEnvs()
		if err != nil {
			m.provMu.Unlock()
			return store.Env{}, store.Lease{}, err
		}
		if !in.warm {
			if err := checkQuotas(m.cfg.Quotas, rows, in.owner, in.merged.Kind); err != nil {
				m.provMu.Unlock()
				return store.Env{}, store.Lease{}, err
			}
		}
		// Admission.
		fits, impossible, err := m.fits(cost)
		if err != nil {
			m.provMu.Unlock()
			return store.Env{}, store.Lease{}, err
		}
		if impossible {
			m.provMu.Unlock()
			pc := m.PoolCapacity(cost.Pool)
			return store.Env{}, store.Lease{}, WithHint("unavailable",
				fmt.Errorf("%w: %s request %.2f CPU/%d MiB exceeds pool capacity %.2f CPU/%d MiB", ErrNoCapacity, cost.Pool, cost.CPUs, cost.MemoryMB, pc.CPUs, pc.MemoryMB),
				"request a smaller profile or increase the %s pool capacity", cost.Pool)
		}
		if !fits {
			if in.noWait || in.warm {
				m.provMu.Unlock()
				return store.Env{}, store.Lease{}, fmt.Errorf("%w: %s pool is full", ErrNoCapacity, cost.Pool)
			}
			m.provMu.Unlock()
			if in.report != nil {
				pc := m.PoolCapacity(cost.Pool)
				in.report(envspec.Progress{Op: "provision", Stage: "queued", QueuePosition: 1,
					RequestedCPUs: cost.CPUs, RequestedMemoryMB: cost.MemoryMB,
					CapacityCPUs: pc.CPUs, CapacityMemoryMB: pc.MemoryMB,
					ReservedCPUs: pc.ReservedCPUs, ReservedMemoryMB: pc.ReservedMemoryMB,
					Message: fmt.Sprintf("queued for %s capacity", cost.Pool)})
			}
			select {
			case <-ctx.Done():
				return store.Env{}, store.Lease{}, ctx.Err()
			case <-time.After(25 * time.Millisecond):
			}
			continue
		}

		// tx1: env + op + lease before any dispatch.
		now := m.now()
		id := envspec.NewID("env_")
		kind := store.OpProvision
		if in.attach {
			kind = store.OpAttach
		}
		e := store.Env{
			ID: id, Kind: in.merged.Kind, Profile: in.profile, Spec: in.merged, Owner: in.owner,
			Generation: 1, Revision: 1, Desired: store.DesiredPresent,
			Phase: envspec.StateAcquiring, Presence: store.PresenceUnknown, Warm: in.warm,
			CostPool: cost.Pool, CostCPUs: cost.CPUs, CostMemMB: cost.MemoryMB, CostDiskMB: cost.DiskMB,
			Created: now,
		}
		op := store.Operation{
			ID: envspec.NewID("op_"), Env: id, Generation: 1, Kind: kind,
			OwnerScope: in.scope, RequestKey: in.key, RequestDigest: in.digest,
			Phase: string(kind), Outcome: store.OutcomePending, Created: now, Updated: now,
		}
		lease := store.Lease{Env: id, LeaseID: envspec.NewID("lease_"), Epoch: 1, Holder: in.owner.ID, State: store.LeaseHeld}
		if in.warm {
			lease.LeaseID, lease.State = "", store.LeaseReleased
		} else if in.merged.TTL > 0 {
			lease.Expires = now.Add(in.merged.TTL)
		}
		e2, op2, existed, err := m.store.InsertProvision(e, op, lease, now)
		m.provMu.Unlock()
		if err != nil {
			return store.Env{}, store.Lease{}, err
		}
		if existed {
			return m.joinAcquire(ctx, op2)
		}
		return e2, lease, nil
	}
}

// joinAcquire waits for an existing idempotent op to resolve, then
// returns its env + current lease.
func (m *Manager) joinAcquire(ctx context.Context, op store.Operation) (store.Env, store.Lease, error) {
	for {
		e, err := m.freshEnv(op.Env)
		if err != nil {
			return store.Env{}, store.Lease{}, err
		}
		cur, err := m.store.GetOperation(op.ID)
		if err != nil {
			return store.Env{}, store.Lease{}, err
		}
		switch {
		case e.Phase == envspec.StatePresent:
			lease, err := m.store.GetLease(e.ID)
			return e, lease, err
		case cur.Outcome == store.OutcomeFailed || e.Phase.Terminal():
			return store.Env{}, store.Lease{}, fmt.Errorf("provision %s failed: %s", op.ID, cur.Error)
		}
		select {
		case <-ctx.Done():
			return store.Env{}, store.Lease{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func warmEligible(merged envspec.Spec, p envspec.Profile) bool {
	if len(merged.Creds) > 0 || len(merged.Signals) > 0 {
		return false
	}
	if p.Build == nil && merged.Image != p.Image {
		return false
	}
	if merged.Resources != p.Resources && p.Resources != (envspec.Resources{}) {
		// merged carries profile defaults; a caller override differs.
		if merged.Resources.CPUs != p.Resources.CPUs || merged.Resources.MemoryMB != p.Resources.MemoryMB {
			return false
		}
	}
	if merged.Params["avd.writable"] != p.Params["avd.writable"] ||
		merged.Params["avd.foreground"] != p.Params["avd.foreground"] {
		return false
	}
	return true
}

// onWarmClaimed rebuilds runtime state for a claimed warm env under its
// new generation and tops the pool back up.
func (m *Manager) onWarmClaimed(e store.Env) {
	// The old runtime object belongs to the previous generation: drop
	// it and build a fresh one around the same instance.
	old := m.refreshRuntimeEnv(e.ID)
	m.dropRuntime(e.ID, ErrClosed)
	ad, ok := m.adapterForEnv(e)
	if !ok {
		return
	}
	var inst adapter.Instance
	if old != nil {
		inst = old.inst
	} else if c, isC := ad.(adapter.Creator); isC {
		inst, _ = c.Open(m.ctx, adapter.OpContext{EnvID: e.ID, Generation: e.Generation}, refOf(e))
	}
	if inst != nil {
		r := newRuntime(e.ID, e.Generation, inst, ad)
		oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
		if obs, err := ad.Observe(m.ctx, oc, refOf(e)); err == nil {
			r.lastObs = obs
		}
		m.putRuntime(r)
		m.acts.declareForEnv(e, r)
		m.acts.wakeUp()
	}
	go m.topUpWarmPools()
}

// driveAcquire dispatches Create/Attach and walks the readiness
// barriers. It is used by the synchronous Provision call and, through
// resumeAcquire, by recovery.
func (m *Manager) driveAcquire(ctx context.Context, e store.Env, op store.Operation, ad adapter.Adapter, report func(envspec.Progress)) error {
	unlock := m.lockDrive(e.ID)
	defer unlock()
	if report == nil {
		report = func(envspec.Progress) {}
	}
	// Never re-drive a completed op (op loop racing the synchronous call).
	if freshOp, ferr := m.store.GetOperation(op.ID); ferr == nil {
		switch freshOp.Outcome {
		case store.OutcomeOK:
			return nil
		case store.OutcomeFailed:
			return fmt.Errorf("provision %s failed: %s", op.ID, freshOp.Error)
		}
		op = freshOp
	}
	if fresh, ferr := m.freshEnv(e.ID); ferr == nil {
		e = fresh
	}
	// Register the in-flight call so release can join it.
	call := &provisionCall{done: make(chan struct{})}
	m.mu.Lock()
	m.provisions[e.ID] = call
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		if m.provisions[e.ID] == call {
			delete(m.provisions, e.ID)
		}
		m.mu.Unlock()
		close(call.done)
	}()

	merged := e.Spec
	attach := op.Kind == store.OpAttach
	oc := adapter.OpContext{OpID: op.ID, EnvID: e.ID, Generation: e.Generation, Attempt: op.Attempt + 1}

	op.Attempt++
	op.Phase = "dispatch"
	op.NextAttempt = time.Time{}
	_ = m.store.UpdateOperation(op, m.now())

	if err := m.ensureBackend(ctx, merged.Kind, ad, report); err != nil {
		return m.failAcquire(e, op, MarkAs(ErrBackendDown, err), false)
	}
	// Container profile images: build/pull before create.
	if !attach && merged.Kind == envspec.Container {
		m.mu.Lock()
		ps := m.profiles[e.Profile]
		m.mu.Unlock()
		if ps != nil && ps.p.Build != nil {
			if err := m.ensureProfileImage(ctx, e.Profile, report); err != nil {
				return m.failAcquire(e, op, fmt.Errorf("profile %s image: %w", e.Profile, err), false)
			}
			m.mu.Lock()
			if ps2 := m.profiles[e.Profile]; ps2 != nil && ps2.imageRef != "" {
				merged.Image = ps2.imageRef
			}
			m.mu.Unlock()
		}
	}
	if err := ad.Validate(ctx, oc, merged); err != nil {
		return m.failAcquire(e, op, err, false)
	}

	progressWithID := func(p envspec.Progress) {
		if p.EnvID == "" {
			p.EnvID = e.ID
		}
		if p.Op == "" {
			p.Op = "provision"
		}
		report(p)
	}

	// Reuse an already-recorded instance (resume after crash).
	r := m.refreshRuntimeEnv(e.ID)
	var inst adapter.Instance
	var dispatchErr error
	if r != nil && r.generation == e.Generation {
		inst = r.inst
	} else if attach {
		att, ok := ad.(adapter.Attacher)
		if !ok {
			return m.failAcquire(e, op, fmt.Errorf("%w: adapter cannot attach", adapter.ErrUnsupported), false)
		}
		inst, dispatchErr = att.Attach(ctx, oc, merged, progressWithID)
	} else {
		cr, ok := ad.(adapter.Creator)
		if !ok {
			return m.failAcquire(e, op, fmt.Errorf("%w: adapter cannot create", adapter.ErrUnsupported), false)
		}
		inst, dispatchErr = cr.Create(ctx, oc, merged, progressWithID)
	}

	if inst != nil {
		// tx2: record the external identity immediately.
		image := ""
		if im, ok := inst.(interface{ ImageDigest() string }); ok {
			image = im.ImageDigest()
		}
		if fresh, err := m.freshEnv(e.ID); err == nil {
			e = fresh
		}
		if e.Instance == "" {
			if _, err := m.store.RecordInstance(e, refJSON(inst.Ref()), image, store.PresencePresent, m.now()); err != nil {
				if errors.Is(err, store.ErrConflict) {
					if fresh, ferr := m.freshEnv(e.ID); ferr == nil {
						e = fresh
						_, _ = m.store.RecordInstance(e, refJSON(inst.Ref()), image, store.PresencePresent, m.now())
					}
				}
			}
			if fresh, err := m.freshEnv(e.ID); err == nil {
				e = fresh
			}
		}
		rt := newRuntime(e.ID, e.Generation, inst, ad)
		m.putRuntime(rt)
		r = rt
	}

	if dispatchErr != nil {
		return m.failAcquire(e, op, MarkAs(ErrDriver, dispatchErr), inst != nil)
	}
	if inst == nil {
		return m.failAcquire(e, op, fmt.Errorf("%w: adapter returned no instance", ErrDriver), false)
	}

	// Release may have landed during dispatch: desired wins.
	if fresh, err := m.freshEnv(e.ID); err == nil {
		e = fresh
	}
	if e.Desired == store.DesiredReleased {
		op.Outcome, op.Error = store.OutcomeFailed, "released during provisioning"
		_ = m.store.UpdateOperation(op, m.now())
		if rel, err := m.store.OpenOperation(e.ID, store.OpRelease); err == nil {
			_ = m.driveReleaseInner(ctx, e.ID, rel, false, false)
		}
		return fmt.Errorf("%w: environment %s released during provisioning", envspec.ErrConflict, e.ID)
	}

	// Barriers: identity verify → creds → required activities → present.
	return m.finishAcquire(ctx, e, op, ad, r, report)
}

// finishAcquire runs the post-dispatch readiness barriers.
func (m *Manager) finishAcquire(ctx context.Context, e store.Env, op store.Operation, ad adapter.Adapter, r *runtimeEnv, report func(envspec.Progress)) error {
	oc := adapter.OpContext{OpID: op.ID, EnvID: e.ID, Generation: e.Generation, Attempt: op.Attempt}
	report(envspec.Progress{Op: "provision", Stage: "verify", EnvID: e.ID})
	obs, err := ad.Observe(ctx, oc, refOf(e))
	if err != nil {
		obs = adapter.Observation{Presence: adapter.Unknown, Detail: err.Error()}
	}
	r.mu.Lock()
	r.lastObs = obs
	r.mu.Unlock()
	switch obs.Presence {
	case adapter.Absent:
		return m.failAcquire(e, op, fmt.Errorf("%w: created resource not observable", ErrEnvironmentLost), false)
	case adapter.Unknown:
		op.Outcome = store.OutcomeUnknown
		op.Phase = "verify"
		op.Error = obs.Detail
		op.NextAttempt = m.now().Add(m.backoffFor(op.Attempt))
		_ = m.store.UpdateOperation(op, m.now())
		if fresh, ferr := m.freshEnv(e.ID); ferr == nil {
			_, _ = m.store.SetPresence(fresh, store.PresenceUnknown, m.now())
		}
		return fmt.Errorf("%w: cannot verify %s yet: %s", ErrBackendDown, e.ID, obs.Detail)
	}
	_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
		Name: "reachable", Status: envspec.ConditionTrue, Reason: "observed", ObservedAt: m.now()})

	// Activities declared before the creds barrier so required
	// collectors start as early as the old boot pins did.
	m.acts.declareForEnv(e, r)
	m.acts.wakeUp()

	if len(e.Spec.Creds) > 0 {
		report(envspec.Progress{Op: "provision", Stage: "inject-creds", EnvID: e.ID})
		if err := m.injectCreds(ctx, e, r, e.Spec.Creds); err != nil {
			_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
				Name: "creds_ready", Status: envspec.ConditionFalse, Reason: ReasonCredentialsFailed,
				ObservedAt: m.now(), Detail: m.redact(err.Error())})
			return m.failAcquire(e, op, MarkAs(ErrCredentials, err), true)
		}
	}
	_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
		Name: "creds_ready", Status: envspec.ConditionTrue, Reason: "injected", ObservedAt: m.now()})

	if err := m.acts.waitRequired(ctx, e.ID, e.Generation); err != nil {
		return m.failAcquire(e, op, err, true)
	}

	if fresh, ferr := m.freshEnv(e.ID); ferr == nil {
		e = fresh
	}
	if _, err := m.store.MarkPresent(e, op.ID, m.now()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Desired flipped under us: feed the release op.
			if fresh, ferr := m.freshEnv(e.ID); ferr == nil && fresh.Desired == store.DesiredReleased {
				if rel, rerr := m.store.OpenOperation(e.ID, store.OpRelease); rerr == nil {
					_ = m.driveReleaseInner(ctx, e.ID, rel, false, false)
				}
			}
			return fmt.Errorf("%w: environment %s changed during provisioning", envspec.ErrConflict, e.ID)
		}
		return err
	}
	return nil
}

// failAcquire resolves a failed dispatch/barrier: confirm presence,
// then either fail terminally (nothing exists), schedule cleanup
// (something exists), or park the op as unknown (backend down).
func (m *Manager) failAcquire(e store.Env, op store.Operation, cause error, mayExist bool) error {
	if fresh, err := m.freshEnv(e.ID); err == nil {
		e = fresh
	}
	if e.Desired == store.DesiredReleased {
		op.Outcome, op.Error = store.OutcomeFailed, cause.Error()
		_ = m.store.UpdateOperation(op, m.now())
		if rel, err := m.store.OpenOperation(e.ID, store.OpRelease); err == nil {
			_ = m.driveReleaseInner(m.ctx, e.ID, rel, false, false)
		}
		return cause
	}
	ad, ok := m.adapterForEnv(e)
	presence := adapter.Absent
	if mayExist && ok {
		presence = adapter.Unknown
		oc := adapter.OpContext{OpID: op.ID, EnvID: e.ID, Generation: e.Generation}
		if obs, err := ad.Observe(m.ctx, oc, refOf(e)); err == nil {
			presence = obs.Presence
			if presence == adapter.Absent && e.Instance == "" {
				if _, found := m.discoverFor(m.ctx, ad, e); found {
					presence = adapter.Present
				}
			}
		}
	}
	switch presence {
	case adapter.Present:
		// Something exists: the cleanup obligation takes over and the
		// provision op records the failure.
		op.Outcome, op.Error = store.OutcomeFailed, cause.Error()
		_ = m.store.UpdateOperation(op, m.now())
		m.emit("env.failed", e.ID, m.failureData(FailureReason(cause), cause, map[string]any{"op": op.ID}))
		_, rel, err := m.store.BeginRelease(e, m.newOp(e, store.OpRelease, "", ""), m.now())
		if err == nil && rel.ID != "" {
			_ = m.driveReleaseInner(m.ctx, e.ID, rel, false, false)
		}
		return cause
	case adapter.Unknown:
		op.Outcome = store.OutcomeUnknown
		op.Error = cause.Error()
		op.NextAttempt = m.now().Add(m.backoffFor(op.Attempt))
		_ = m.store.UpdateOperation(op, m.now())
		return cause
	default: // Absent: nothing to clean up.
		_, err := m.store.MarkFailed(e, op.ID, cause.Error(), m.now())
		if err != nil && errors.Is(err, store.ErrConflict) {
			if fresh, ferr := m.freshEnv(e.ID); ferr == nil {
				_, _ = m.store.MarkFailed(fresh, op.ID, cause.Error(), m.now())
			}
		}
		m.dropRuntime(e.ID, cause)
		return cause
	}
}

// resumeAcquire re-drives a pending provision/attach op after restart
// or backoff: re-observe first, never blindly re-dispatch.
func (m *Manager) resumeAcquire(ctx context.Context, e store.Env, op store.Operation) error {
	ad, ok := m.adapterForEnv(e)
	if !ok {
		return nil
	}
	oc := adapter.OpContext{OpID: op.ID, EnvID: e.ID, Generation: e.Generation, Attempt: op.Attempt}
	ref := refOf(e)
	obs, err := ad.Observe(ctx, oc, ref)
	if err != nil {
		obs = adapter.Observation{Presence: adapter.Unknown, Detail: err.Error()}
	}
	if obs.Presence == adapter.Absent && e.Instance == "" {
		if cand, found := m.discoverFor(ctx, ad, e); found {
			if _, err := m.store.RecordInstance(e, refJSON(cand.Ref), e.Image, store.PresencePresent, m.now()); err == nil {
				if fresh, ferr := m.freshEnv(e.ID); ferr == nil {
					e = fresh
				}
			}
			obs, _ = ad.Observe(ctx, oc, cand.Ref)
		}
	}
	switch obs.Presence {
	case adapter.Unknown:
		op.Outcome = store.OutcomeUnknown
		op.Error = obs.Detail
		op.NextAttempt = m.now().Add(m.backoffFor(op.Attempt + 1))
		op.Attempt++
		_ = m.store.UpdateOperation(op, m.now())
		if fresh, ferr := m.freshEnv(e.ID); ferr == nil {
			_, _ = m.store.SetPresence(fresh, store.PresenceUnknown, m.now())
		}
		return nil
	case adapter.Absent:
		if e.Instance != "" {
			_, _ = m.store.MarkFailed(e, op.ID, "resource lost before provisioning completed", m.now())
			m.dropRuntime(e.ID, ErrEnvironmentLost)
			return nil
		}
		// Nothing was ever recorded or discovered: (re)dispatch.
		return m.driveAcquire(ctx, e, op, ad, func(envspec.Progress) {})
	case adapter.Present:
		if m.refreshRuntimeEnv(e.ID) == nil {
			m.reopenRuntime(ctx, e, ad, ref, obs)
		}
		if e.Phase == envspec.StatePresent {
			op.Outcome, op.Error = store.OutcomeOK, ""
			return m.store.UpdateOperation(op, m.now())
		}
		return m.driveAcquire(ctx, e, op, ad, func(envspec.Progress) {})
	}
	return nil
}

// topUpWarmPools provisions unowned ready instances for Warm profiles.
func (m *Manager) topUpWarmPools() {
	m.mu.Lock()
	var names []string
	for name, ps := range m.profiles {
		if ps.p.Warm > 0 {
			names = append(names, name)
		}
	}
	m.mu.Unlock()
	for _, name := range names {
		m.warmMu.Lock()
		if m.warming[name] {
			m.warmMu.Unlock()
			continue
		}
		m.warming[name] = true
		m.warmMu.Unlock()
		m.topUpOne(name)
		m.warmMu.Lock()
		m.warming[name] = false
		m.warmMu.Unlock()
	}
}

func (m *Manager) topUpOne(name string) {
	m.mu.Lock()
	ps := m.profiles[name]
	m.mu.Unlock()
	if ps == nil {
		return
	}
	want := ps.p.Warm
	rows, err := m.store.ListEnvs()
	if err != nil {
		return
	}
	have := 0
	for _, e := range rows {
		if e.Warm && e.Profile == name && (e.Phase == envspec.StatePresent || e.Phase == envspec.StateAcquiring) {
			have++
		}
	}
	for i := have; i < want; i++ {
		if err := m.provisionWarm(name); err != nil {
			return
		}
	}
}

func (m *Manager) provisionWarm(profileName string) error {
	m.mu.Lock()
	ps := m.profiles[profileName]
	m.mu.Unlock()
	if ps == nil {
		return fmt.Errorf("%w: profile %q", envspec.ErrNotFound, profileName)
	}
	merged, err := mergeSpec(ps.p, envspec.Spec{Profile: profileName})
	if err != nil {
		return err
	}
	ad, ok := m.adapterFor(merged.Kind, false)
	if !ok {
		return nil
	}
	if chk, isChk := ad.(adapter.Checker); isChk {
		if err := chk.Check(m.ctx); err != nil {
			return nil // runtime down; skip warm-up silently
		}
	}
	owner := envspec.Owner{ID: "system:warm-pool"}
	e, _, err := m.insertAcquire(m.ctx, acquireInsert{
		owner: owner, merged: merged, profile: profileName, profileDef: ps.p,
		digest: requestDigest(merged, owner, false), scope: owner.ID,
		ad: ad, noWait: true, warm: true, report: func(envspec.Progress) {},
	})
	if err != nil {
		return nil // no capacity etc.: warm-up is best-effort
	}
	op := m.openAcquireOp(e)
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
	defer cancel()
	return m.driveAcquire(ctx, e, op, ad, func(envspec.Progress) {})
}
