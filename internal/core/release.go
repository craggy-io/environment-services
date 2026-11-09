package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/store"
)

// Release records release intent and drives it: stop activities,
// release credential leases, dispose (destroy/reset/disconnect),
// verify, and only then mark released (returning the reservation by
// leaving the admission SUM). Destroy failures stay pending and retry
// across restarts. Idempotent.
func (m *Manager) Release(ctx context.Context, id string) error {
	if err := m.requireHealthy(); err != nil {
		return err
	}
	e, err := m.freshEnv(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if e.Phase.Terminal() {
		return nil
	}
	e, op, err := m.store.BeginRelease(e, m.newOp(e, store.OpRelease, "", ""), m.now())
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			// Lost a race with another transition: re-read and retry once.
			if fresh, ferr := m.freshEnv(id); ferr == nil {
				e, op, err = m.store.BeginRelease(fresh, m.newOp(fresh, store.OpRelease, "", ""), m.now())
			}
		}
		if err != nil {
			return err
		}
	}
	if op.ID == "" {
		return nil
	}
	return m.driveRelease(ctx, id, op, false)
}

// releaseForce is Release without warm-reset (drain, pressure reap):
// the resource is destroyed/disconnected, never pooled.
func (m *Manager) releaseForce(ctx context.Context, id string) error {
	e, err := m.freshEnv(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if e.Phase.Terminal() {
		return nil
	}
	_, op, err := m.store.BeginRelease(e, m.newOp(e, store.OpRelease, "", ""), m.now())
	if err != nil {
		return err
	}
	if op.ID == "" {
		return nil
	}
	return m.driveRelease(ctx, id, op, true)
}

// driveRelease performs one release attempt for an open release op.
// Failure leaves the op pending with backoff — the obligation is the
// op row plus desired=released, retried by the op loop. joinProvision
// waits for an in-flight provision first; callers that ARE that
// provision (its release handoff) must pass false or they self-join.
func (m *Manager) driveRelease(ctx context.Context, envID string, op store.Operation, force bool, joinProvision ...bool) error {
	unlock := m.lockDrive(envID)
	defer unlock()
	return m.driveReleaseInner(ctx, envID, op, force, joinProvision...)
}

// driveReleaseInner is driveRelease for callers already holding the
// env's drive lock (the provision handoff inside driveAcquire).
func (m *Manager) driveReleaseInner(ctx context.Context, envID string, op store.Operation, force bool, joinProvision ...bool) error {
	e, err := m.freshEnv(envID)
	if err != nil {
		return err
	}
	if e.Phase.Terminal() {
		return nil
	}
	// A stale op snapshot (op loop racing a synchronous Release) must
	// never re-drive completed work.
	if freshOp, ferr := m.store.GetOperation(op.ID); ferr == nil {
		op = freshOp
	}
	if op.Outcome == store.OutcomeOK || op.Outcome == store.OutcomeFailed {
		return nil
	}
	if e.Desired != store.DesiredReleased {
		return nil // already returned to pool or otherwise resolved
	}
	// A provisioning call may still hold the only instance reference:
	// join it so its late result feeds this release.
	join := len(joinProvision) == 0 || joinProvision[0]
	m.mu.Lock()
	call := m.provisions[envID]
	m.mu.Unlock()
	if call != nil && join {
		select {
		case <-call.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if fresh, ferr := m.freshEnv(envID); ferr == nil {
			e = fresh
		}
		if e.Phase.Terminal() {
			return nil
		}
	}

	op.Attempt++
	op.Phase = "stop-activities"
	op.NextAttempt = time.Time{}
	_ = m.store.UpdateOperation(op, m.now())

	// 1. Stop activities (ack or timeout).
	m.acts.stopForRelease(e.ID, e.Generation)

	// 2. Credential leases end with the env; an error is recorded,
	// not fatal to physical cleanup.
	if m.cfg.Vault != nil {
		if verr := m.cfg.Vault.ReleaseHolder(ctx, "env:"+e.ID); verr != nil {
			op.Error = "vault release: " + verr.Error()
			_ = m.store.UpdateOperation(op, m.now())
			m.emit("lease.released", e.ID, map[string]any{"kind": "credential", "error": m.redact(verr.Error())})
		} else {
			m.emit("lease.released", e.ID, map[string]any{"kind": "credential"})
		}
		m.refreshRedactor()
	}

	ad, hasAd := m.adapterForEnv(e)
	oc := adapter.OpContext{OpID: op.ID, EnvID: e.ID, Generation: e.Generation, Attempt: op.Attempt}
	fail := func(cause error) error {
		op.Outcome = store.OutcomePending
		op.Error = cause.Error()
		op.NextAttempt = m.now().Add(m.backoffFor(op.Attempt))
		_ = m.store.UpdateOperation(op, m.now())
		return cause
	}

	// Resolve an instance: runtime, re-open, or rediscover. No instance
	// anywhere means cleanup is already verified absent.
	inst := m.instanceFor(ctx, e, ad, hasAd)
	if inst == nil {
		if hasAd && e.Instance != "" {
			obs, oerr := ad.Observe(ctx, oc, refOf(e))
			if oerr != nil || obs.Presence == adapter.Unknown {
				return fail(fmt.Errorf("%w: cannot verify cleanup of %s", ErrBackendDown, e.ID))
			}
			if obs.Presence == adapter.Present {
				return fail(fmt.Errorf("cleanup of %s unverified: still present, no handle", e.ID))
			}
		}
		return m.completeRelease(e, op, adapter.Destroy, store.PresenceAbsent)
	}

	// 3. Disposition: warm reset when the profile reuses and the
	// instance can reset; disconnect for attached; destroy otherwise.
	disp := adapter.Destroy
	if hasAd && m.relationshipOf(ad) == adapter.Attached {
		disp = adapter.Disconnect
	}
	if !force && disp == adapter.Destroy && e.Phase != envspec.StateAcquiring && m.profileReuses(e.Profile) {
		if rs, ok := inst.(adapter.Resetter); ok {
			op.Phase = "reset"
			_ = m.store.UpdateOperation(op, m.now())
			if rerr := rs.Reset(ctx, oc); rerr == nil {
				// Verify it survived reset, then pool it.
				obs, oerr := ad.Observe(ctx, oc, refOf(e))
				if oerr == nil && obs.Presence == adapter.Present {
					return m.returnToPool(e, op, ad, inst, obs)
				}
			} else {
				m.emit("env.reset_failed", e.ID, m.failureData(FailureReason(rerr), rerr, nil))
			}
			// Reset failed: fall through to destroy.
		}
	}

	op.Phase = "dispose"
	op.Disposition = string(disp)
	_ = m.store.UpdateOperation(op, m.now())
	if err := inst.Dispose(ctx, oc, disp); err != nil {
		return fail(fmt.Errorf("dispose %s: %w", e.ID, err))
	}

	// 4. Verify before declaring completion.
	op.Phase = "verify"
	_ = m.store.UpdateOperation(op, m.now())
	obs, oerr := ad.Observe(ctx, oc, refOf(e))
	if oerr != nil {
		return fail(fmt.Errorf("%w: verify cleanup of %s: %v", ErrBackendDown, e.ID, oerr))
	}
	switch disp {
	case adapter.Disconnect:
		if obs.Presence == adapter.Present && obs.Attached {
			return fail(fmt.Errorf("disconnect %s unverified: still attached", e.ID))
		}
		if obs.Presence == adapter.Unknown {
			return fail(fmt.Errorf("%w: cannot verify disconnect of %s", ErrBackendDown, e.ID))
		}
		pres := store.PresencePresent
		if obs.Presence == adapter.Absent {
			pres = store.PresenceAbsent
		}
		return m.completeRelease(e, op, disp, pres)
	default: // Destroy / RetainWorkspace
		switch obs.Presence {
		case adapter.Absent:
			return m.completeRelease(e, op, disp, store.PresenceAbsent)
		case adapter.Present:
			return fail(fmt.Errorf("destroy %s unverified: still present", e.ID))
		default:
			return fail(fmt.Errorf("%w: cannot verify destroy of %s: %s", ErrBackendDown, e.ID, obs.Detail))
		}
	}
}

// instanceFor resolves an instance for cleanup: live runtime, adapter
// re-open from the recorded ref, or rediscovery by name.
func (m *Manager) instanceFor(ctx context.Context, e store.Env, ad adapter.Adapter, hasAd bool) adapter.Instance {
	if r := m.refreshRuntimeEnv(e.ID); r != nil {
		return r.inst
	}
	if !hasAd {
		return nil
	}
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	open := func(ref adapter.Ref) adapter.Instance {
		switch a := ad.(type) {
		case adapter.Creator:
			inst, _ := a.Open(ctx, oc, ref)
			return inst
		case adapter.Attacher:
			inst, _ := a.Open(ctx, oc, ref, e.Spec)
			return inst
		}
		return nil
	}
	if e.Instance != "" {
		if inst := open(refOf(e)); inst != nil {
			return inst
		}
	}
	if cand, found := m.discoverFor(ctx, ad, e); found {
		if e.Instance == "" {
			if fresh, err := m.freshEnv(e.ID); err == nil {
				_, _ = m.store.RecordInstance(fresh, refJSON(cand.Ref), fresh.Image, store.PresencePresent, m.now())
			}
		}
		return open(cand.Ref)
	}
	return nil
}

func (m *Manager) completeRelease(e store.Env, op store.Operation, disp adapter.Disposition, presence store.Presence) error {
	if fresh, err := m.freshEnv(e.ID); err == nil {
		e = fresh
	}
	if _, err := m.store.CompleteRelease(e, op.ID, string(disp), presence, m.now()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			if fresh, ferr := m.freshEnv(e.ID); ferr == nil && !fresh.Phase.Terminal() {
				_, err = m.store.CompleteRelease(fresh, op.ID, string(disp), presence, m.now())
			}
		}
		if err != nil {
			return err
		}
	}
	m.dropRuntime(e.ID, ErrClosed)
	return nil
}

func (m *Manager) returnToPool(e store.Env, op store.Operation, ad adapter.Adapter, inst adapter.Instance, obs adapter.Observation) error {
	if fresh, err := m.freshEnv(e.ID); err == nil {
		e = fresh
	}
	next, err := m.store.ReturnToPool(e, op.ID, envspec.Owner{ID: "system:warm-pool"}, m.now())
	if err != nil {
		return err
	}
	// Fresh runtime object under the new generation; the old holder's
	// operations are cancelled and its cred env is gone with it.
	m.dropRuntime(e.ID, ErrClosed)
	r := newRuntime(next.ID, next.Generation, inst, ad)
	r.lastObs = obs
	m.putRuntime(r)
	m.acts.declareForEnv(next, r)
	m.acts.wakeUp()
	m.emit("env.released", e.ID, map[string]any{"returned_to_warm_pool": true})
	return nil
}

// RenewLease is the sole holder-renewal path: it extends the lease of
// the handle's exact lease ID + epoch. Nothing else renews implicitly.
func (m *Manager) RenewLease(h Handle) error {
	if err := m.requireHealthy(); err != nil {
		return err
	}
	e, err := m.freshEnv(h.EnvID)
	if err != nil {
		return err
	}
	if e.Generation != h.Generation {
		return fmt.Errorf("%w: generation %d, env is %d", ErrStaleHandle, h.Generation, e.Generation)
	}
	ttl := e.Spec.TTL
	if ttl <= 0 {
		return nil // no expiry armed
	}
	_, err = m.store.RenewLease(h.EnvID, h.LeaseID, h.Epoch, m.now().Add(ttl), m.now())
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: lease %s epoch %d no longer held", ErrStaleHandle, h.LeaseID, h.Epoch)
	}
	return err
}

// Transfer hands an env to a new owner under a fresh lease (epoch+1).
// Active borrows block it until they drain.
func (m *Manager) Transfer(id string, to envspec.Owner) (Handle, error) {
	if err := m.requireHealthy(); err != nil {
		return Handle{}, err
	}
	e, err := m.freshEnv(id)
	if err != nil {
		return Handle{}, err
	}
	if bs, _ := m.store.Borrows(id); len(bs) > 0 {
		return Handle{}, fmt.Errorf("%w: environment %s has active borrows; transfer waits for them to drain", envspec.ErrConflict, id)
	}
	if e.Warm {
		return Handle{}, Invalidf("environment %s is in the warm pool", id)
	}
	var exp time.Time
	if e.Spec.TTL > 0 {
		exp = m.now().Add(e.Spec.TTL)
	}
	_, lease, err := m.store.TransferLease(e, to, envspec.NewID("lease_"), exp, m.now())
	if err != nil {
		return Handle{}, err
	}
	return Handle{m: m, EnvID: id, Generation: e.Generation, LeaseID: lease.LeaseID, Epoch: lease.Epoch}, nil
}

// Reset is the durable reset op: reset in place under the same holder
// is not a reassignment, so the lease survives but activities restart.
func (m *Manager) Reset(ctx context.Context, h Handle) error {
	if err := m.requireHealthy(); err != nil {
		return err
	}
	e, r, err := m.checkHandle(h)
	if err != nil {
		return err
	}
	rs, ok := r.inst.(adapter.Resetter)
	if !ok {
		return fmt.Errorf("%w: reset on %s", adapter.ErrUnsupported, e.Kind)
	}
	m.acts.stopForRelease(e.ID, e.Generation)
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	if err := rs.Reset(ctx, oc); err != nil {
		return err
	}
	if obs, oerr := r.ad.Observe(ctx, oc, refOf(e)); oerr == nil {
		r.mu.Lock()
		r.lastObs = obs
		r.mu.Unlock()
		if obs.Presence != adapter.Present {
			return fmt.Errorf("%w: env not present after reset", ErrEnvironmentLost)
		}
	}
	m.acts.declareForEnv(e, r)
	m.acts.wakeUp()
	m.emit("env.reset", e.ID, map[string]any{"generation": e.Generation})
	return nil
}
