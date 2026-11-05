package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/store"
)

// resolveCollector finds the collector providing stream on r's
// instance and runs its applicability and preflight checks, so an
// explicit request fails with the reason instead of recording
// nothing. Errors wrap signalcore.ErrNoStream / ErrNotApplicable /
// ErrPreflight.
func resolveCollector(ctx context.Context, r *runtimeEnv, stream string) (signalcore.Collector, error) {
	cpProvider, ok := r.inst.(adapter.CollectorProvider)
	if !ok {
		return nil, fmt.Errorf("%w: adapter provides no collectors", signalcore.ErrNoStream)
	}
	var col signalcore.Collector
	for _, c := range cpProvider.Collectors() {
		for _, sd := range c.Streams() {
			if sd.Name == stream {
				col = c
			}
		}
	}
	if col == nil {
		return nil, fmt.Errorf("%w: %s", signalcore.ErrNoStream, stream)
	}
	if ap, ok := col.(signalcore.Applicability); ok {
		if err := ap.Applies(cpProvider.CollectorEnvInfo()); err != nil {
			return nil, fmt.Errorf("%w: %w", signalcore.ErrNotApplicable, err)
		}
	}
	if pf, ok := col.(signalcore.Preflight); ok {
		if err := pf.Preflight(ctx); err != nil {
			return nil, fmt.Errorf("%w: %w", signalcore.ErrPreflight, err)
		}
	}
	return col, nil
}

// pinName is the activity name of a runtime pin: the spec-declared
// name plus a short params digest, so distinct parameterisations of
// one stream are distinct activities (and distinct hub runs).
func pinName(stream string, params json.RawMessage) string {
	sum := sha256.Sum256(params)
	return "signal:" + stream + "#" + hex.EncodeToString(sum[:6])
}

// PinCollector starts (or keeps) a collector for stream on envID as a
// durable activity and returns an idempotent unpin. It resolves and
// preflights the collector synchronously, then waits (bounded by ctx)
// for the activity to report running or failed. With fresh set, any
// current runner for the same stream+params is stopped and its
// cleanup awaited before the replacement starts — the activity model
// has no reference counts, so fresh never refuses; it restarts.
//
// The host.metrics stream is not a collector activity (the host
// sampler publishes it continuously); pinning it on source "host" is
// the caller's concern, not core's.
func (m *Manager) PinCollector(ctx context.Context, envID, stream string, params json.RawMessage, fresh bool) (func(), error) {
	if err := m.requireHealthy(); err != nil {
		return nil, err
	}
	e, err := m.store.GetEnv(envID)
	if err != nil {
		return nil, fmt.Errorf("%w: environment %s", envspec.ErrNotFound, envID)
	}
	if e.Phase.Terminal() {
		return nil, fmt.Errorf("%w: environment %s is %s", signalcore.ErrNoTarget, envID, e.Phase)
	}
	r := m.refreshRuntimeEnv(envID)
	if r == nil {
		return nil, fmt.Errorf("%w: environment %s is not running", signalcore.ErrNoTarget, envID)
	}
	if _, err := resolveCollector(ctx, r, stream); err != nil {
		return nil, err
	}
	name := pinName(stream, params)
	key := actKey{env: e.ID, gen: e.Generation, name: name}
	if fresh {
		m.acts.stop(key)
	}
	raw, err := json.Marshal(collectorParams{Stream: stream, Raw: params})
	if err != nil {
		return nil, err
	}
	if err := m.store.UpsertActivity(store.Activity{
		Env: e.ID, Generation: e.Generation, Name: name, Kind: store.ActivityCollector,
		Desired: "running", State: store.ActivityPending, Params: string(raw),
	}); err != nil {
		return nil, err
	}
	m.acts.wakeUp()
	if err := m.waitPin(ctx, e.ID, e.Generation, name); err != nil {
		return nil, err
	}
	var once sync.Once
	unpin := func() {
		once.Do(func() {
			act, err := m.store.GetActivity(e.ID, e.Generation, name)
			if err != nil {
				return
			}
			act.Desired = "stopped"
			act.State = store.ActivityStopping
			_ = m.store.UpdateActivity(act)
			m.acts.wakeUp()
		})
	}
	return unpin, nil
}

// waitPin polls until the activity runs or terminally fails. A ctx
// with no deadline is capped so a wedged collector cannot hang a pin
// forever; the activity keeps reconciling either way.
func (m *Manager) waitPin(ctx context.Context, envID string, gen int64, name string) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		act, err := m.store.GetActivity(envID, gen, name)
		if err == nil {
			switch act.State {
			case store.ActivityRunning:
				return nil
			case store.ActivityFailed:
				return fmt.Errorf("collector %s failed to start: %s", name, act.LastError)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
