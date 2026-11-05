package environments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/craggy-io/environment-services/envcap"
	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/core"
	"github.com/craggy-io/environment-services/internal/rules"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/store"
	"github.com/craggy-io/environment-services/internal/vault"
)

// markAs returns err marked so errors.Is(err, kind) holds across the
// envcap boundary, keeping its message and chain.
func markAs(kind, err error) error {
	if err == nil || errors.Is(err, kind) {
		return err
	}
	return &markedErr{kind: kind, err: err}
}

type markedErr struct{ kind, err error }

func (e *markedErr) Error() string        { return e.err.Error() }
func (e *markedErr) Unwrap() error        { return e.err }
func (e *markedErr) Is(target error) bool { return target == e.kind }

// xlate translates internal errors into the envcap error contract:
// everything the capability layer translates (envspec + envcap
// sentinels, *envcap.HintError, *envcap.PolicyError) is marked or
// converted here, in exactly one place. Unknown errors pass through
// and surface as internal errors, as before.
func xlate(err error) error {
	if err == nil {
		return nil
	}
	var he *core.HintError
	if errors.As(err, &he) {
		return &envcap.HintError{Code: he.Code, Msg: he.Msg, Hint: he.Hint, Err: err}
	}
	var pe *vault.PolicyError
	if errors.As(err, &pe) {
		return &envcap.PolicyError{Rule: pe.Rule, Detail: pe.Detail, Holder: pe.Holder}
	}
	switch {
	case errors.Is(err, core.ErrNoCapacity), errors.Is(err, core.ErrQuota):
		return markAs(envcap.ErrNoCapacity, err)
	case errors.Is(err, adapter.ErrBackendDown):
		return markAs(envcap.ErrBackendDown, err)
	case errors.Is(err, adapter.ErrUnsupported):
		return markAs(envcap.ErrUnsupported, err)
	case errors.Is(err, core.ErrScopeClosed):
		return markAs(envspec.ErrInvalid, err)
	case errors.Is(err, signalcore.ErrNoStream):
		return markAs(envcap.ErrNoStream, err)
	case errors.Is(err, signalcore.ErrNoTarget):
		return markAs(envcap.ErrNoTarget, err)
	case errors.Is(err, signalcore.ErrNotApplicable):
		return markAs(envcap.ErrNotApplicable, err)
	case errors.Is(err, signalcore.ErrPreflight):
		return markAs(envcap.ErrPreflight, err)
	case errors.Is(err, vault.ErrNotFound):
		return markAs(envspec.ErrNotFound, err)
	case errors.Is(err, vault.ErrPolicy):
		return markAs(envcap.ErrPolicy, err)
	}
	return err
}

// latestOp returns the newest durable operation of kind for envID.
// Lifecycle results must always name their operation, so a missing
// row after a successful lifecycle call is an internal error, never
// a silently empty id.
func (d *EnvCapDeps) latestOp(envID string, kind store.OpKind) (string, error) {
	ops, err := d.m.core.Store().ListOperations(envID)
	if err != nil {
		return "", xlate(err)
	}
	for i := len(ops) - 1; i >= 0; i-- {
		if ops[i].Kind == kind {
			return ops[i].ID, nil
		}
	}
	return "", fmt.Errorf("environments: no %s operation recorded for %s", kind, envID)
}

// ProvisionEnv provisions via the durable operation machinery and
// reports its operation id plus the holder fencing identity.
func (d *EnvCapDeps) ProvisionEnv(ctx context.Context, req envcap.ProvisionRequest) (envcap.EnvResult, error) {
	opts := []core.ProvisionOption{}
	if req.Progress != nil {
		opts = append(opts, core.OnProgress(req.Progress))
	}
	if req.NoWait {
		opts = append(opts, core.NoWait())
	}
	h, err := d.m.core.Provision(ctx, req.Owner, req.Spec, opts...)
	if err != nil {
		return envcap.EnvResult{}, xlate(err)
	}
	opID, err := d.latestOp(h.EnvID, store.OpProvision)
	if err != nil {
		// Warm claims record under the original provision; attach
		// under attach. Try both before giving up.
		if opID, err = d.latestOp(h.EnvID, store.OpAttach); err != nil {
			return envcap.EnvResult{}, err
		}
	}
	return envcap.EnvResult{
		Env: h.EnvID, Info: h.Info(), OperationID: opID,
		Generation: h.Generation, LeaseEpoch: h.Epoch,
	}, nil
}

// ReleaseEnv releases and reports the release operation.
func (d *EnvCapDeps) ReleaseEnv(ctx context.Context, id string) (envcap.EnvResult, error) {
	var gen, epoch int64
	if h, err := d.m.core.Get(id); err == nil {
		gen, epoch = h.Generation, h.Epoch
	}
	if err := d.m.core.Release(ctx, id); err != nil {
		return envcap.EnvResult{}, xlate(err)
	}
	opID, err := d.latestOp(id, store.OpRelease)
	if err != nil {
		return envcap.EnvResult{}, err
	}
	return envcap.EnvResult{Env: id, OperationID: opID, Generation: gen, LeaseEpoch: epoch}, nil
}

// EnvInfo is the envcap spelling of Info.
func (d *EnvCapDeps) EnvInfo(id string) (envspec.Info, error) { return d.Info(id) }

// EnvIdentity reports the current holder fencing identity.
func (d *EnvCapDeps) EnvIdentity(id string) (envcap.EnvIdentity, error) {
	h, err := d.m.core.Get(id)
	if err != nil {
		return envcap.EnvIdentity{}, xlate(err)
	}
	return envcap.EnvIdentity{Generation: h.Generation, LeaseID: h.LeaseID, LeaseEpoch: h.Epoch}, nil
}

// ListEnvs adapts List to the envcap filter shape.
func (d *EnvCapDeps) ListEnvs(f envcap.ListFilter) []envspec.Info {
	return d.m.core.List(core.Filter{Owner: f.Owner, Session: f.Session, Live: f.Live})
}

// RenewLease is the sole holder-renewal path, by environment id.
func (d *EnvCapDeps) RenewLease(id string) error {
	h, err := d.m.core.Get(id)
	if err != nil {
		return xlate(err)
	}
	return xlate(d.m.core.RenewLease(h))
}

// ExecEnv runs one command to completion with full (uncapped)
// streams and the process id its output is teed under.
func (d *EnvCapDeps) ExecEnv(ctx context.Context, id string, cmd envspec.Cmd) (envcap.ExecResult, error) {
	h, err := d.handle(id)
	if err != nil {
		return envcap.ExecResult{}, xlate(err)
	}
	p, procID, err := d.m.core.ExecStream(ctx, h,
		adapter.ExecCmd{Argv: cmd.Argv, Env: cmd.Env, Timeout: cmd.Timeout})
	if err != nil {
		return envcap.ExecResult{}, xlate(err)
	}
	var outBuf, errBuf bytes.Buffer
	done := make(chan struct{}, 2)
	for _, st := range []struct {
		r io.Reader
		w *bytes.Buffer
	}{{p.Stdout, &outBuf}, {p.Stderr, &errBuf}} {
		go func(rd io.Reader, w *bytes.Buffer) {
			if rd != nil {
				_, _ = io.Copy(w, rd)
			}
			done <- struct{}{}
		}(st.r, st.w)
	}
	exit, werr := p.Wait()
	<-done
	<-done
	return envcap.ExecResult{ProcID: procID, Stdout: outBuf.Bytes(), Stderr: errBuf.Bytes(), Exit: exit}, xlate(werr)
}

// PoolCapacities reports both admission pools.
func (d *EnvCapDeps) PoolCapacities() []envspec.PoolCapacity {
	return []envspec.PoolCapacity{
		d.m.core.PoolCapacity(envspec.PoolHost),
		d.m.core.PoolCapacity(envspec.PoolDocker),
	}
}

// --- signals ---

type pinEntry struct {
	rec   envcap.PinRecord
	unpin func()
}

func pinKey(envID, stream string, params json.RawMessage) string {
	sum := sha256.Sum256(params)
	return envID + "\x00" + stream + "\x00" + hex.EncodeToString(sum[:8])
}

// PinStream starts (or joins) the durable collector activity for
// stream on envID and mints the capability layer's pin id for it.
// Repeated calls with the same env/stream/params return the same
// pin. The core resolves, preflights and waits for the running ack,
// so start failures fail here with their reason.
func (d *EnvCapDeps) PinStream(ctx context.Context, ownerID, envID, stream string, params json.RawMessage) (envcap.PinRecord, error) {
	key := pinKey(envID, stream, params)
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, e := range d.pins {
		if pinKey(e.rec.Env, e.rec.Stream, e.rec.Params) == key {
			return e.rec, nil
		}
	}
	unpin, err := d.m.core.PinCollector(ctx, envID, stream, params, false)
	if err != nil {
		return envcap.PinRecord{}, xlate(err)
	}
	rec := envcap.PinRecord{
		ID: envspec.NewID("pin_"), Env: envID, Stream: stream,
		Params: append(json.RawMessage(nil), params...), Owner: ownerID, Since: time.Now(),
	}
	d.pins[rec.ID] = &pinEntry{rec: rec, unpin: unpin}
	return rec, nil
}

// UnpinStream stops one pin's collector activity.
func (d *EnvCapDeps) UnpinStream(pinID string) (envcap.PinRecord, error) {
	d.mu.Lock()
	e, ok := d.pins[pinID]
	if ok {
		delete(d.pins, pinID)
	}
	d.mu.Unlock()
	if !ok {
		return envcap.PinRecord{}, xlate(fmt.Errorf("%w: pin %s", envspec.ErrNotFound, pinID))
	}
	e.unpin()
	return e.rec, nil
}

// Pins lists a env's pins; Pins("") lists every pin (unpin lookup).
func (d *EnvCapDeps) Pins(envID string) []envcap.PinRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []envcap.PinRecord{}
	for _, e := range d.pins {
		if envID == "" || e.rec.Env == envID {
			out = append(out, e.rec)
		}
	}
	return out
}

// StreamState reports a configured collector's state on a source.
func (d *EnvCapDeps) StreamState(stream, source string, params json.RawMessage) envspec.StreamState {
	return d.m.hub.ConfiguredState(stream, source, params)
}

// RunningCollectors lists live collector instances.
func (d *EnvCapDeps) RunningCollectors() []envcap.RunningCollector {
	out := []envcap.RunningCollector{}
	for _, ci := range d.m.hub.Running() {
		out = append(out, envcap.RunningCollector{ID: ci.ID, Name: ci.Name, Source: ci.Source, Streams: ci.Streams})
	}
	return out
}

// Streams enumerates stream definitions. Collectors are owned by
// adapter instances, so with an env the list comes from that env's
// instance (with applicability and live state); without one it is
// the union over the hub and every live instance, without
// per-environment answers.
func (d *EnvCapDeps) Streams(envID string) []envcap.StreamInfo {
	byName := map[string]*envcap.StreamInfo{}
	order := []string{}
	put := func(def signalcore.StreamDef) *envcap.StreamInfo {
		if si, ok := byName[def.Name]; ok {
			return si
		}
		si := &envcap.StreamInfo{Stream: def.Name, Description: def.Description, Cost: def.Cost, Bytes: def.Bytes}
		byName[def.Name] = si
		order = append(order, def.Name)
		return si
	}
	for _, def := range d.m.hub.Streams() {
		put(def)
	}
	collect := func(id string, detailed bool) {
		h, err := d.m.core.Get(id)
		if err != nil {
			return
		}
		inst, err := d.m.core.Instance(h)
		if err != nil {
			return
		}
		cp, ok := inst.(adapter.CollectorProvider)
		if !ok {
			return
		}
		for _, c := range cp.Collectors() {
			for _, def := range c.Streams() {
				si := put(def)
				if !detailed {
					continue
				}
				avail := true
				if ap, ok := c.(signalcore.Applicability); ok {
					if aerr := ap.Applies(cp.CollectorEnvInfo()); aerr != nil {
						avail = false
						si.WhyNot = aerr.Error()
					}
				}
				si.Available = &avail
				st := d.m.hub.State(def.Name, id)
				si.Running = st.Running
				si.LastFailure = st.LastFailure
				si.FailedAt = st.FailedAt
			}
		}
	}
	if envID != "" {
		collect(envID, true)
		for _, p := range d.Pins(envID) {
			if si, ok := byName[p.Stream]; ok {
				si.Pins = append(si.Pins, p.ID)
			}
		}
	} else {
		for _, info := range d.m.core.List(core.Filter{Live: true}) {
			collect(info.ID, false)
		}
	}
	out := make([]envcap.StreamInfo, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out
}

// withWhere folds an agent equality map into the selector predicates.
func withWhere(sel envspec.Selector, where map[string]any) (envspec.Selector, error) {
	if len(where) == 0 {
		return sel, nil
	}
	f, err := signalcore.Where(where)
	if err != nil {
		return sel, xlate(err)
	}
	sel.Where = f
	return sel, nil
}

// QuerySignals reads recorded events.
func (d *EnvCapDeps) QuerySignals(sel envspec.Selector, where map[string]any, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error) {
	sel, err := withWhere(sel, where)
	if err != nil {
		return nil, envspec.Quality{}, err
	}
	evs, q, err := d.m.rec.Query(sel, from, to)
	return evs, q, xlate(err)
}

// TailSignals waits for new matching events on the live hub. It
// starts no collectors: hub subscriptions only see what running
// collectors publish.
func (d *EnvCapDeps) TailSignals(ctx context.Context, sel envspec.Selector, where map[string]any, wait time.Duration, limit int) ([]envspec.SignalEvent, envspec.Quality, error) {
	sel, err := withWhere(sel, where)
	if err != nil {
		return nil, envspec.Quality{}, err
	}
	sub, err := d.m.hub.Subscribe(ctx, sel)
	if err != nil {
		return nil, envspec.Quality{}, xlate(err)
	}
	defer sub.Close()
	var got []envspec.SignalEvent
	timer := time.NewTimer(wait)
	defer timer.Stop()
loop:
	for len(got) < limit {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				break loop
			}
			got = append(got, ev)
		case <-timer.C:
			break loop
		case <-ctx.Done():
			break loop
		}
	}
	return got, sub.Quality(), nil
}

// SignalCoverage reports per stream+source collection coverage.
func (d *EnvCapDeps) SignalCoverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage {
	return d.m.hub.ConfiguredCoverage(sel, from, to)
}

// CaptureSignals pins a recorded range against retention.
func (d *EnvCapDeps) CaptureSignals(sel envspec.Selector, where map[string]any, from, to time.Time) (envcap.Capture, error) {
	sel, err := withWhere(sel, where)
	if err != nil {
		return nil, err
	}
	capObj, err := d.m.rec.Capture(sel, from, to)
	if err != nil {
		return nil, xlate(err)
	}
	return capObj, nil
}

// --- credentials ---

func credPolicyOf(p envcap.CredPolicy) vault.Policy {
	return vault.Policy{MaxConcurrent: p.MaxConcurrent, Cooldown: p.Cooldown, MaxPerDay: p.MaxPerDay,
		HolderKinds: p.HolderKinds, Owners: p.Owners, Sticky: p.Sticky, OnePerHolder: p.OnePerHolder}
}

func credLeaseOf(l *vault.Lease) envcap.CredLease {
	return envcap.CredLease{Lease: l.ID, Cred: l.Cred, Kind: l.Kind(),
		Holder: l.Holder, Owner: l.Owner, Public: l.Public()}
}

// ListCreds lists credentials, public view only.
func (d *EnvCapDeps) ListCreds(ctx context.Context) ([]envcap.CredView, error) {
	views, err := d.m.vault.List(ctx)
	if err != nil {
		return nil, xlate(err)
	}
	out := make([]envcap.CredView, 0, len(views))
	for _, v := range views {
		out = append(out, envcap.CredView{ID: v.ID, Kind: v.Kind, Name: v.Name,
			Public: v.Public, Labels: v.Labels, Available: v.Available, Reason: v.Reason})
	}
	return out, nil
}

// LeaseCred leases one credential; only public fields come back.
func (d *EnvCapDeps) LeaseCred(ctx context.Context, req envcap.LeaseCredRequest) (envcap.CredLease, error) {
	lease, err := d.m.vault.Lease(ctx, vault.LeaseRequest{
		Cred: req.Cred, Holder: req.Holder, HolderKind: req.HolderKind,
		Owner: req.Owner, Purpose: req.Purpose, TTL: req.TTL,
		Select: vault.Selector{Kind: req.Kind, Labels: req.Labels},
	})
	if err != nil {
		return envcap.CredLease{}, xlate(err)
	}
	return credLeaseOf(lease), nil
}

// ReleaseCredLease releases one credential lease.
func (d *EnvCapDeps) ReleaseCredLease(ctx context.Context, leaseID string) error {
	return xlate(d.m.vault.ReleaseByID(ctx, leaseID))
}

// ListCredLeases lists live leases (env.status).
func (d *EnvCapDeps) ListCredLeases(ctx context.Context) ([]envcap.CredLease, error) {
	leases, err := d.m.vault.Leases(ctx, "")
	if err != nil {
		return nil, xlate(err)
	}
	out := make([]envcap.CredLease, 0, len(leases))
	for _, l := range leases {
		out = append(out, credLeaseOf(l))
	}
	return out, nil
}

// PutCred creates or replaces a credential. Secrets enter the vault
// and never come back out this surface.
func (d *EnvCapDeps) PutCred(ctx context.Context, in envcap.CredInput) (string, error) {
	sec := make(map[string]vault.Secret, len(in.Secret))
	for k, v := range in.Secret {
		sec[k] = vault.NewSecret(v)
	}
	c, err := d.m.vault.Put(vault.Credential{ID: in.ID, Kind: in.Kind, Name: in.Name,
		Public: in.Public, Secret: sec, Labels: in.Labels, Policy: credPolicyOf(in.Policy)})
	if err != nil {
		return "", xlate(err)
	}
	return c.ID, nil
}

// DeleteCred really deletes a credential.
func (d *EnvCapDeps) DeleteCred(ctx context.Context, cred string) error {
	return xlate(d.m.vault.Delete(cred))
}

// SetCredPolicy replaces a credential's lease policy.
func (d *EnvCapDeps) SetCredPolicy(ctx context.Context, cred string, p envcap.CredPolicy) error {
	return xlate(d.m.vault.SetPolicy(cred, credPolicyOf(p)))
}

// --- rules ---

func ruleTriggerOf(t rules.Trigger) envcap.RuleTrigger {
	return envcap.RuleTrigger{Rule: t.Rule, Kind: t.Kind, Data: t.Data,
		CaptureID: t.CaptureID, Time: t.Time}
}

// LoadRule loads (or hot-reloads) one rule instance.
func (d *EnvCapDeps) LoadRule(ctx context.Context, source []byte, codeRef string, params map[string]any) (string, error) {
	id, err := d.m.rules.Load(ctx, source, codeRef, params)
	if err != nil {
		return "", xlate(err)
	}
	return string(id), nil
}

// UnloadRule stops one rule instance.
func (d *EnvCapDeps) UnloadRule(id string) error {
	return xlate(d.m.rules.Unload(rules.InstanceID(id)))
}

// ListRules lists rule instances.
func (d *EnvCapDeps) ListRules() []envcap.RuleInfo {
	out := []envcap.RuleInfo{}
	for _, i := range d.m.rules.List() {
		out = append(out, envcap.RuleInfo{Instance: i.ID, Rule: i.Rule, Version: i.Version,
			Description: i.Description, CodeRef: i.CodeRef, State: i.State,
			Events: i.Events, Emits: i.Emits, Triggers: i.Triggers, Faults: i.Faults,
			LastTrigger: i.LastTrigger, LoadedAt: i.LoadedAt})
	}
	return out
}

// TestRule backtests a rule over recorded events.
func (d *EnvCapDeps) TestRule(ctx context.Context, source []byte, params map[string]any, events []envspec.SignalEvent) (envcap.RuleTest, error) {
	res, err := d.m.rules.Test(ctx, source, params, events)
	if err != nil {
		return envcap.RuleTest{}, xlate(err)
	}
	out := envcap.RuleTest{Rule: res.Rule, Emits: len(res.Emits), Faults: res.Faults}
	for _, t := range res.Triggers {
		out.Triggers = append(out.Triggers, ruleTriggerOf(t))
	}
	return out, nil
}

// WaitRuleTriggers waits for rule firings.
func (d *EnvCapDeps) WaitRuleTriggers(ctx context.Context, rule string, wait time.Duration) ([]envcap.RuleTrigger, error) {
	ts, err := d.m.rules.WaitTriggers(ctx, rule, wait)
	if err != nil {
		return nil, xlate(err)
	}
	out := make([]envcap.RuleTrigger, 0, len(ts))
	for _, t := range ts {
		out = append(out, ruleTriggerOf(t))
	}
	return out, nil
}

// RecentRuleTriggers lists recent firings (env.status).
func (d *EnvCapDeps) RecentRuleTriggers(since time.Time, rule string) []envcap.RuleTrigger {
	out := []envcap.RuleTrigger{}
	for _, t := range d.m.rules.RecentTriggers(since, rule) {
		out = append(out, ruleTriggerOf(t))
	}
	return out
}
