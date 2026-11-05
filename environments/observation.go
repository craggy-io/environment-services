package environments

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// Observation is the signal surface: keeping collectors running
// (pins), asking what ran where (state, coverage, running), reading
// what was recorded (query) and pinning ranges against retention
// (captures). Sources are environment IDs, or "host" for host
// streams.
type Observation interface {
	// Pin starts the collector for stream on source with exactly
	// params (nil = defaults) and returns an idempotent unpin. It
	// fails synchronously when no collector provides the stream,
	// the source is not running, or the collector cannot run there.
	Pin(stream, source string, params json.RawMessage) (unpin func(), err error)
	// PinFresh is Pin after stopping any current run of the same
	// stream+params and waiting for its cleanup. Use it after
	// resetting a process whose observer cannot survive the reset.
	PinFresh(ctx context.Context, stream, source string, params json.RawMessage) (unpin func(), err error)
	// ConfiguredState reports whether a collector instance with
	// exactly params runs stream on source now.
	ConfiguredState(stream, source string, params json.RawMessage) envspec.StreamState
	// Query reads recorded events matching sel in [from,to] with
	// their loss/quality accounting.
	Query(sel envspec.Selector, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error)
	// ConfiguredCoverage reports, per stream+source in sel, how
	// much of [from,to] a collector with sel's params actually ran.
	ConfiguredCoverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage
	// Running lists live collector instances.
	Running() []RunningCollector
	// Capture pins the recorded range [from,to] for sel against
	// retention until the capture is closed.
	Capture(sel envspec.Selector, from, to time.Time) (Capture, error)
	// Retention reports what the recorder currently holds per
	// (source, stream): ring contents, spilled segments, time span.
	Retention() []envspec.StreamStats
}

// RunningCollector is one live collector instance.
type RunningCollector struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Streams []string `json:"streams"`
}

// Capture is a retention-pinned recorded range. Source exports it as
// a file tree (range.json plus one segment file per stream); Close
// releases the pin.
type Capture interface {
	ID() string
	Source() envspec.Source
	Close()
}

type observation struct{ m *Manager }

var _ Observation = (*observation)(nil)

func (o *observation) Pin(stream, source string, params json.RawMessage) (func(), error) {
	return o.pin(context.Background(), stream, source, params, false)
}

func (o *observation) PinFresh(ctx context.Context, stream, source string, params json.RawMessage) (func(), error) {
	return o.pin(ctx, stream, source, params, true)
}

func (o *observation) pin(ctx context.Context, stream, source string, params json.RawMessage, fresh bool) (func(), error) {
	if source == "host" {
		// The host sampler publishes host.metrics continuously;
		// there is no collector to start.
		if stream == envspec.StreamHostMetrics {
			return func() {}, nil
		}
		return nil, fmt.Errorf("%w: %s on host", signalcore.ErrNoStream, stream)
	}
	return o.m.core.PinCollector(ctx, source, stream, params, fresh)
}

func (o *observation) ConfiguredState(stream, source string, params json.RawMessage) envspec.StreamState {
	return o.m.hub.ConfiguredState(stream, source, params)
}

func (o *observation) Query(sel envspec.Selector, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error) {
	return o.m.rec.Query(sel, from, to)
}

func (o *observation) ConfiguredCoverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage {
	return o.m.hub.ConfiguredCoverage(sel, from, to)
}

func (o *observation) Running() []RunningCollector {
	infos := o.m.hub.Running()
	out := make([]RunningCollector, 0, len(infos))
	for _, ci := range infos {
		out = append(out, RunningCollector{ID: ci.ID, Name: ci.Name, Source: ci.Source, Streams: ci.Streams})
	}
	return out
}

func (o *observation) Retention() []envspec.StreamStats { return o.m.rec.Stats() }

func (o *observation) Capture(sel envspec.Selector, from, to time.Time) (Capture, error) {
	return o.m.rec.Capture(sel, from, to)
}
