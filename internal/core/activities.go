package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/store"
)

// One activity reconciler drives every desired activity row — presence
// probes, adapter probes/components, collectors and services — with
// shared backoff (1s→30s), cancellation and status. Collectors run
// once and return their error; restarting is the reconciler's job.
// The signal hub owns no lifecycle (no refcount, no linger, no pins):
// these rows are the lifecycle.

type actKey struct {
	env  string
	gen  int64
	name string
}

type actRunner struct {
	cancel context.CancelFunc
	done   chan struct{}
	held   bool // last mutation-gate state, to emit held once
}

type activityEngine struct {
	m       *Manager
	mu      sync.Mutex
	runners map[actKey]*actRunner
	wake    chan struct{}
}

func newActivityEngine(m *Manager) *activityEngine {
	return &activityEngine{m: m, runners: map[actKey]*actRunner{}, wake: make(chan struct{}, 1)}
}

func (a *activityEngine) wakeUp() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

type collectorParams struct {
	Stream string            `json:"stream"`
	Params map[string]string `json:"params,omitempty"`
	// Raw carries runtime-pin params verbatim (PinCollector); when set
	// it is passed to the collector instead of the marshalled Params.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// declareForEnv (re)declares the desired activity rows for an env
// generation: the built-in presence probe, adapter probes/components
// and the spec's signal requests.
func (a *activityEngine) declareForEnv(e store.Env, r *runtimeEnv) {
	st := a.m.store
	up := func(name string, kind store.ActivityKind, required bool, params string) {
		_ = st.UpsertActivity(store.Activity{
			Env: e.ID, Generation: e.Generation, Name: name, Kind: kind,
			Desired: "running", Required: required, State: store.ActivityPending, Params: params,
		})
	}
	up("presence", store.ActivityProbe, false, "")
	if pp, ok := r.inst.(adapter.ProbeProvider); ok {
		for _, p := range pp.Probes() {
			kind := store.ActivityProbe
			if p.Repair != nil {
				kind = store.ActivityComponent
			}
			up("probe:"+p.Name, kind, p.Required, "")
		}
	}
	for _, sig := range e.Spec.Signals {
		raw, _ := json.Marshal(collectorParams{Stream: sig.Stream, Params: sig.Params})
		up("signal:"+sig.Stream, store.ActivityCollector, sig.Required, string(raw))
	}
}

// reconcile starts due runners and stops undesired ones.
func (a *activityEngine) reconcile() {
	m := a.m
	now := m.now()
	m.mu.Lock()
	runtimes := make([]*runtimeEnv, 0, len(m.runtime))
	for _, r := range m.runtime {
		runtimes = append(runtimes, r)
	}
	m.mu.Unlock()
	live := map[actKey]bool{}
	for _, r := range runtimes {
		e, err := m.store.GetEnv(r.id)
		if err != nil || e.Phase.Terminal() || e.Generation != r.generation {
			continue
		}
		acts, err := m.store.Activities(e.ID, e.Generation)
		if err != nil {
			continue
		}
		for _, act := range acts {
			key := actKey{e.ID, e.Generation, act.Name}
			live[key] = true
			a.mu.Lock()
			_, running := a.runners[key]
			a.mu.Unlock()
			switch {
			case act.Desired == "running" && !running && !now.Before(act.NextAttempt) && e.Desired == store.DesiredPresent:
				a.start(e, r, act)
			case act.Desired != "running" && running:
				a.stop(key)
			}
		}
	}
	// Runners for envs that vanished from runtime (released, replaced).
	a.mu.Lock()
	var stale []actKey
	for key := range a.runners {
		if !live[key] {
			stale = append(stale, key)
		}
	}
	a.mu.Unlock()
	for _, key := range stale {
		a.stop(key)
	}
}

func (a *activityEngine) start(e store.Env, r *runtimeEnv, act store.Activity) {
	m := a.m
	key := actKey{e.ID, e.Generation, act.Name}
	ctx, cancel := context.WithCancel(m.ctx)
	runner := &actRunner{cancel: cancel, done: make(chan struct{})}
	a.mu.Lock()
	if _, dup := a.runners[key]; dup {
		a.mu.Unlock()
		cancel()
		return
	}
	a.runners[key] = runner
	a.mu.Unlock()
	act.State = store.ActivityStarting
	_ = m.store.UpdateActivity(act)
	go func() {
		defer close(runner.done)
		defer func() {
			a.mu.Lock()
			if a.runners[key] == runner {
				delete(a.runners, key)
			}
			a.mu.Unlock()
		}()
		a.run(ctx, e, r, act, runner)
	}()
}

func (a *activityEngine) run(ctx context.Context, e store.Env, r *runtimeEnv, act store.Activity, runner *actRunner) {
	switch act.Kind {
	case store.ActivityCollector:
		a.runCollector(ctx, e, r, act)
	case store.ActivityProbe, store.ActivityComponent:
		if act.Name == "presence" {
			a.runPresence(ctx, e, r, act)
		} else {
			a.runProbe(ctx, e, r, act, runner)
		}
	case store.ActivityService:
		a.m.runServiceActivity(ctx, e, act)
	}
}

// finish records a runner's outcome: clean stop, failure with backoff,
// or terminal failure for unresolvable activities.
func (a *activityEngine) finish(act store.Activity, err error, terminal bool) {
	m := a.m
	fresh, ferr := m.store.GetActivity(act.Env, act.Generation, act.Name)
	if ferr == nil {
		act = fresh
	}
	now := m.now()
	if err == nil {
		act.State = store.ActivityStopped
		act.LastError = ""
		_ = m.store.UpdateActivity(act)
		return
	}
	act.Attempts++
	act.LastError = m.redact(err.Error())
	if terminal {
		act.State = store.ActivityFailed
	} else {
		act.State = store.ActivityBackoff
		act.NextAttempt = now.Add(m.backoffFor(act.Attempts))
	}
	_ = m.store.UpdateActivity(act)
	a.wakeUp()
}

func (a *activityEngine) markRunning(act store.Activity) {
	if fresh, err := a.m.store.GetActivity(act.Env, act.Generation, act.Name); err == nil {
		act = fresh
	}
	act.State = store.ActivityRunning
	act.LastOK = a.m.now()
	act.LastError = ""
	_ = a.m.store.UpdateActivity(act)
}

func (a *activityEngine) runCollector(ctx context.Context, e store.Env, r *runtimeEnv, act store.Activity) {
	m := a.m
	var cp collectorParams
	_ = json.Unmarshal([]byte(act.Params), &cp)
	condName := "signal:" + cp.Stream
	failCond := func(err error) {
		_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
			Name: condName, Status: envspec.ConditionFalse, Reason: "collector_failed",
			ObservedAt: m.now(), Detail: m.redact(err.Error())})
	}
	if m.cfg.Signals == nil {
		err := fmt.Errorf("no signal hub configured for %s", cp.Stream)
		failCond(err)
		a.finish(act, err, true)
		return
	}
	col, err := resolveCollector(ctx, r, cp.Stream)
	if err != nil {
		failCond(err)
		a.finish(act, err, true)
		return
	}
	// Register with the hub (idempotent) so Coverage/stream registries
	// know it, then run once. Running is the readiness ack.
	m.cfg.Signals.Register(col)
	params := cp.Raw
	if len(params) == 0 && len(cp.Params) > 0 {
		params, _ = json.Marshal(cp.Params)
	}
	a.markRunning(act)
	_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
		Name: condName, Status: envspec.ConditionTrue, Reason: "running", ObservedAt: m.now()})
	_, err = m.cfg.Signals.Run(ctx, col, e.ID, params)
	if err != nil && ctx.Err() == nil {
		failCond(err)
		m.emit("collector.failed", e.ID, m.failureData(FailureReason(err), err,
			map[string]any{"collector": col.Name(), "stream": cp.Stream}))
		a.finish(act, err, false)
		return
	}
	a.finish(act, nil, false)
}

func (a *activityEngine) runPresence(ctx context.Context, e store.Env, r *runtimeEnv, act store.Activity) {
	m := a.m
	iv := m.cfg.HealthInterval
	a.markRunning(act)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.finish(act, nil, false)
			return
		case <-t.C:
		}
		fresh, err := m.freshEnv(e.ID)
		if err != nil || fresh.Phase.Terminal() || fresh.Generation != e.Generation || fresh.Desired != store.DesiredPresent {
			a.finish(act, nil, false)
			return
		}
		octx, cancel := context.WithTimeout(ctx, 10*time.Second)
		obs, oerr := r.ad.Observe(octx, adapter.OpContext{EnvID: e.ID, Generation: e.Generation}, refOf(fresh))
		cancel()
		now := m.now()
		if oerr != nil || obs.Presence == adapter.Unknown {
			detail := obs.Detail
			if oerr != nil {
				detail = oerr.Error()
			}
			_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
				Name: "reachable", Status: envspec.ConditionUnknown, Reason: ReasonBackendDown,
				ObservedAt: now, Detail: m.redact(detail)})
			if fresh.Presence != store.PresenceUnknown {
				_, _ = m.store.SetPresence(fresh, store.PresenceUnknown, now)
			}
			continue
		}
		r.mu.Lock()
		r.lastObs = obs
		r.mu.Unlock()
		if obs.Presence == adapter.Absent {
			// Definitive absence of a present env: it is lost.
			_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
				Name: "reachable", Status: envspec.ConditionFalse, Reason: ReasonNotFound, ObservedAt: now})
			if fresh.Phase == envspec.StatePresent {
				_, _ = m.store.MarkFailed(fresh, "", "environment lost: resource absent", now)
				if m.cfg.Vault != nil {
					_ = m.cfg.Vault.ReleaseHolder(m.ctx, "env:"+e.ID)
					m.refreshRedactor()
				}
				m.dropRuntime(e.ID, ErrEnvironmentLost)
			}
			a.finish(act, nil, false)
			return
		}
		_ = m.store.SetCondition(e.ID, e.Generation, envspec.Condition{
			Name: "reachable", Status: envspec.ConditionTrue, Reason: "observed", ObservedAt: now})
		if fresh.Presence != store.PresencePresent && fresh.Phase == envspec.StatePresent {
			_, _ = m.store.SetPresence(fresh, store.PresencePresent, now)
		}
		for _, c := range obs.Conditions {
			if c.ObservedAt.IsZero() {
				c.ObservedAt = now
			}
			_ = m.store.SetCondition(e.ID, e.Generation, c)
		}
	}
}

func (a *activityEngine) runProbe(ctx context.Context, e store.Env, r *runtimeEnv, act store.Activity, runner *actRunner) {
	m := a.m
	name := act.Name[len("probe:"):]
	var probe *adapter.Probe
	if pp, ok := r.inst.(adapter.ProbeProvider); ok {
		for _, p := range pp.Probes() {
			if p.Name == name {
				probe = &p
				break
			}
		}
	}
	if probe == nil || probe.Check == nil {
		a.finish(act, fmt.Errorf("probe %s no longer provided", name), true)
		return
	}
	iv := probe.Interval
	if iv <= 0 {
		iv = m.cfg.HealthInterval
	}
	a.markRunning(act)
	check := func() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cond, err := probe.Check(cctx)
		now := m.now()
		if cond.Name == "" {
			cond.Name = name
		}
		if cond.ObservedAt.IsZero() {
			cond.ObservedAt = now
		}
		if err != nil && cond.Status == "" {
			cond.Status = envspec.ConditionUnknown
			cond.Reason = FailureReason(err)
			cond.Detail = m.redact(err.Error())
		}
		_ = m.store.SetCondition(e.ID, e.Generation, cond)
		if cond.Status != envspec.ConditionFalse || probe.Repair == nil {
			if runner.held {
				runner.held = false
			}
			return
		}
		// Autonomous repair passes the experiment-hold gate.
		allowed, reason := m.allowMutationQuiet(e.ID, "repair")
		if !allowed {
			if !runner.held {
				runner.held = true
				if fresh, ferr := m.store.GetActivity(act.Env, act.Generation, act.Name); ferr == nil {
					fresh.State = store.ActivityHeld
					fresh.LastError = reason
					_ = m.store.UpdateActivity(fresh)
				}
				m.emit("activity.held", e.ID, map[string]any{"activity": act.Name, "reason": reason, "kind": "repair"})
			}
			return
		}
		if runner.held {
			runner.held = false
			a.markRunning(act)
		}
		if probe.BeforeRepair != nil {
			_ = probe.BeforeRepair(cctx) // evidence first; failure does not block repair
		}
		if rerr := probe.Repair(cctx); rerr != nil {
			m.emit("component.repair_failed", e.ID, m.failureData(FailureReason(rerr), rerr,
				map[string]any{"component": name}))
		}
	}
	check()
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.finish(act, nil, false)
			return
		case <-t.C:
			check()
		}
	}
}

func (a *activityEngine) stop(key actKey) {
	a.mu.Lock()
	r := a.runners[key]
	a.mu.Unlock()
	if r == nil {
		return
	}
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(a.m.cfg.ActivityStopWait):
	}
}

// stopForRelease flips every activity of a generation to stopped and
// waits for runners to acknowledge (bounded).
func (a *activityEngine) stopForRelease(envID string, gen int64) {
	m := a.m
	acts, err := m.store.Activities(envID, gen)
	if err == nil {
		for _, act := range acts {
			if act.Desired != "stopped" {
				act.Desired = "stopped"
				act.State = store.ActivityStopping
				_ = m.store.UpdateActivity(act)
			}
		}
	}
	a.mu.Lock()
	var dones []chan struct{}
	for key, r := range a.runners {
		if key.env == envID && key.gen == gen {
			r.cancel()
			dones = append(dones, r.done)
		}
	}
	a.mu.Unlock()
	deadline := time.After(m.cfg.ActivityStopWait)
	for _, d := range dones {
		select {
		case <-d:
		case <-deadline:
			return
		}
	}
}

func (a *activityEngine) dropEnv(envID string) {
	a.mu.Lock()
	var stops []*actRunner
	for key, r := range a.runners {
		if key.env == envID {
			stops = append(stops, r)
		}
	}
	a.mu.Unlock()
	for _, r := range stops {
		r.cancel()
	}
}

func (a *activityEngine) stopAll() {
	a.mu.Lock()
	runners := make([]*actRunner, 0, len(a.runners))
	for _, r := range a.runners {
		runners = append(runners, r)
	}
	a.mu.Unlock()
	for _, r := range runners {
		r.cancel()
	}
	for _, r := range runners {
		select {
		case <-r.done:
		case <-time.After(a.m.cfg.ActivityStopWait):
		}
	}
}

// ackStability is how long a collector must stay continuously Running
// (same attempt) before it counts as acknowledged: a collector that
// dies at startup flaps Starting→Running→Backoff, and a momentary
// Running sighting is not an ack.
const ackStability = 100 * time.Millisecond

// waitRequired blocks until every required activity of the generation
// is acked: collectors stably running, required probes True.
func (a *activityEngine) waitRequired(ctx context.Context, envID string, gen int64) error {
	m := a.m
	type candidate struct {
		attempts int
		since    time.Time
	}
	seen := map[string]candidate{}
	for {
		acts, err := m.store.Activities(envID, gen)
		if err != nil {
			return err
		}
		pending := 0
		for _, act := range acts {
			if !act.Required || act.Desired != "running" {
				continue
			}
			switch act.Kind {
			case store.ActivityCollector:
				switch act.State {
				case store.ActivityRunning:
					c, ok := seen[act.Name]
					if !ok || c.attempts != act.Attempts {
						seen[act.Name] = candidate{attempts: act.Attempts, since: m.now()}
						pending++
					} else if m.now().Sub(c.since) < ackStability {
						pending++
					}
				case store.ActivityFailed:
					return fmt.Errorf("%w: required collector %s: %s", envspec.ErrNotReady, act.Name, act.LastError)
				default:
					delete(seen, act.Name)
					pending++
				}
			case store.ActivityProbe, store.ActivityComponent:
				name := act.Name
				if len(name) > len("probe:") {
					name = name[len("probe:"):]
				}
				okTrue := false
				if conds, cerr := m.store.Conditions(envID, gen); cerr == nil {
					for _, c := range conds {
						if c.Name == name && c.Status == envspec.ConditionTrue {
							okTrue = true
						}
					}
				}
				if !okTrue {
					if act.State == store.ActivityFailed {
						return fmt.Errorf("%w: required probe %s: %s", envspec.ErrNotReady, act.Name, act.LastError)
					}
					pending++
				}
			default:
				if act.State != store.ActivityRunning {
					pending++
				}
			}
		}
		if pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: required activities for %s not acked: %v", envspec.ErrNotReady, envID, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
