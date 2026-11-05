package envtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/environments"
	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// FakeObservation is an in-memory environments.Observation. Publish
// feeds it events; Query/Coverage/Capture answer from what was
// published and pinned, with the same selector matching as the real
// hub (signalcore.SelectorMatch). Pins are bookkeeping only: Pin and
// PinFresh record the (stream, source, params) as running until
// unpinned, and fail for streams no published event or registered
// stream names — register stream names up front with AddStream, or
// publish an event on the stream first.
type FakeObservation struct {
	mu      sync.Mutex
	events  []envspec.SignalEvent
	streams map[string]bool
	pins    map[pinKey]*fakePin
	seq     uint64
	pinSeq  uint64
}

type pinKey struct {
	stream, source, digest string
}

type fakePin struct {
	key     pinKey
	id      string
	since   time.Time
	stopped time.Time // zero while running
}

var _ environments.Observation = (*FakeObservation)(nil)

// NewFakeObservation returns an empty FakeObservation.
func NewFakeObservation() *FakeObservation {
	return &FakeObservation{streams: map[string]bool{}, pins: map[pinKey]*fakePin{}}
}

// AddStream registers a stream name as pinnable without publishing
// an event on it first.
func (f *FakeObservation) AddStream(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streams[name] = true
}

// Publish records one event. Zero Seq/Time are filled in (per
// FakeObservation monotonic seq, time.Now), mirroring the real hub.
func (f *FakeObservation) Publish(ev envspec.SignalEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev.Seq == 0 {
		f.seq++
		ev.Seq = f.seq
	}
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	f.streams[ev.Stream] = true
	f.events = append(f.events, ev)
}

func paramsDigest(params json.RawMessage) string {
	sum := sha256.Sum256(params)
	return hex.EncodeToString(sum[:])
}

func (f *FakeObservation) pin(stream, source string, params json.RawMessage, fresh bool) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.streams[stream] {
		return nil, fmt.Errorf("%w: %s", errNoStream, stream)
	}
	key := pinKey{stream: stream, source: source, digest: paramsDigest(params)}
	if fresh {
		delete(f.pins, key)
	}
	f.pinSeq++
	digest := key.digest
	if len(digest) > 8 {
		digest = digest[:8]
	}
	p := &fakePin{key: key, since: time.Now(),
		id: fmt.Sprintf("fake:%s@%s#%s-%d", stream, source, digest, f.pinSeq)}
	f.pins[key] = p
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			if cur, ok := f.pins[key]; ok && cur == p {
				p.stopped = time.Now()
				delete(f.pins, key)
			}
		})
	}, nil
}

// errNoStream mirrors the real pin failure for unknown streams.
var errNoStream = fmt.Errorf("no collector provides this stream")

// Pin implements environments.Observation.
func (f *FakeObservation) Pin(stream, source string, params json.RawMessage) (func(), error) {
	return f.pin(stream, source, params, false)
}

// PinFresh implements environments.Observation.
func (f *FakeObservation) PinFresh(ctx context.Context, stream, source string, params json.RawMessage) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return f.pin(stream, source, params, true)
}

// ConfiguredState implements environments.Observation.
func (f *FakeObservation) ConfiguredState(stream, source string, params json.RawMessage) envspec.StreamState {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := pinKey{stream: stream, source: source, digest: paramsDigest(params)}
	if p, ok := f.pins[key]; ok {
		return envspec.StreamState{Running: true, Since: p.since,
			Collectors: []string{p.id}}
	}
	return envspec.StreamState{}
}

// Query implements environments.Observation. Quality counts the
// matched events as received and reports no loss.
func (f *FakeObservation) Query(sel envspec.Selector, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []envspec.SignalEvent
	for _, ev := range f.events {
		if !from.IsZero() && ev.Time.Before(from) {
			continue
		}
		if !to.IsZero() && ev.Time.After(to) {
			continue
		}
		if signalcore.SelectorMatch(sel, ev) {
			out = append(out, ev)
		}
	}
	return out, envspec.Quality{Received: uint64(len(out))}, nil
}

// ConfiguredCoverage implements environments.Observation: a pinned
// stream+source is "covered" for the whole window, anything else
// "not_running".
func (f *FakeObservation) ConfiguredCoverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage {
	f.mu.Lock()
	defer f.mu.Unlock()
	streams := sel.Streams
	if len(streams) == 0 {
		for s := range f.streams {
			streams = append(streams, s)
		}
		sort.Strings(streams)
	}
	sources := sel.Sources
	if len(sources) == 0 {
		sources = []string{"host"}
		for _, ev := range f.events {
			sources = appendSource(sources, ev.Source)
		}
		for k := range f.pins {
			sources = appendSource(sources, k.source)
		}
	}
	window := to.Sub(from)
	var out []envspec.Coverage
	for _, stream := range streams {
		for _, source := range sources {
			cov := envspec.Coverage{Stream: stream, Source: source, Status: "not_running", Window: window}
			for k := range f.pins {
				if signalcore.MatchGlob(stream, k.stream) && signalcore.MatchGlob(source, k.source) {
					cov.Status = "covered"
					cov.Ran = window
				}
			}
			out = append(out, cov)
		}
	}
	return out
}

func appendSource(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

// Running implements environments.Observation.
func (f *FakeObservation) Running() []environments.RunningCollector {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]environments.RunningCollector, 0, len(f.pins))
	for k, p := range f.pins {
		out = append(out, environments.RunningCollector{
			ID: p.id, Name: k.stream,
			Source: k.source, Streams: []string{k.stream}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Retention implements environments.Observation: one row per
// (source, stream) with published events, all in the "ring".
func (f *FakeObservation) Retention() []envspec.StreamStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	byKey := map[string]*envspec.StreamStats{}
	for _, ev := range f.events {
		k := ev.Source + "\x00" + ev.Stream
		st, ok := byKey[k]
		if !ok {
			st = &envspec.StreamStats{Source: ev.Source, Stream: ev.Stream, Oldest: ev.Time}
			byKey[k] = st
		}
		st.RingEvents++
		if ev.Time.Before(st.Oldest) {
			st.Oldest = ev.Time
		}
		if ev.Time.After(st.Newest) {
			st.Newest = ev.Time
		}
	}
	out := make([]envspec.StreamStats, 0, len(byKey))
	for _, st := range byKey {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Stream < out[j].Stream
	})
	return out
}

// Capture implements environments.Observation.
func (f *FakeObservation) Capture(sel envspec.Selector, from, to time.Time) (environments.Capture, error) {
	return &fakeCapture{id: envspec.NewID("cap_"), f: f, sel: sel, from: from, to: to}, nil
}

type fakeCapture struct {
	id       string
	f        *FakeObservation
	sel      envspec.Selector
	from, to time.Time
	closed   bool
}

var _ environments.Capture = (*fakeCapture)(nil)

func (c *fakeCapture) ID() string { return c.id }

func (c *fakeCapture) Close() { c.closed = true }

// Closed reports whether Close was called (test introspection).
func (c *fakeCapture) Closed() bool { return c.closed }

// Source exports the capture like the real one: range.json plus one
// JSONL segment per (source, stream), .pcap for byte streams.
func (c *fakeCapture) Source() envspec.Source {
	evs, q, err := c.f.Query(c.sel, c.from, c.to)
	if err != nil {
		return envspec.MemSource(map[string][]byte{})
	}
	files := map[string][]byte{}
	meta := envspec.RangeMeta{ID: c.id, Streams: c.sel.Streams, Sources: c.sel.Sources,
		Collectors: c.sel.Collectors, From: c.from, To: c.to, Quality: q}
	mb, _ := json.MarshalIndent(meta, "", "  ")
	files["range.json"] = mb
	byKey := map[string][]envspec.SignalEvent{}
	for _, ev := range evs {
		byKey[ev.Source+"/"+ev.Stream] = append(byKey[ev.Source+"/"+ev.Stream], ev)
	}
	for key, list := range byKey {
		isBytes := false
		for _, ev := range list {
			if _, ok := ev.Data.([]byte); ok {
				isBytes = true
			}
		}
		var buf bytes.Buffer
		if isBytes {
			for _, ev := range list {
				if b, ok := ev.Data.([]byte); ok {
					buf.Write(b)
				}
			}
			files[key+".pcap"] = buf.Bytes()
			continue
		}
		for _, ev := range list {
			b, _ := json.Marshal(ev)
			buf.Write(b)
			buf.WriteByte('\n')
		}
		files[key+".jsonl"] = buf.Bytes()
	}
	return envspec.MemSource(files)
}

// --- FakeServiceProvider ---

// FakeServiceProvider is an in-memory envspec.ServiceProvider. Each
// AcquireService mints a fresh service (no sharing) whose lease is
// an inert handle: Exec fails, Push/Pull copy nothing, Release drops
// the borrow. StopService is generation-fenced exactly like the real
// provider. It is for wiring tests, not behaviour tests.
type FakeServiceProvider struct {
	mu       sync.Mutex
	services map[string]*fakeService // service ID → service
	gen      int
}

type fakeService struct {
	info    envspec.ServiceInfo
	lockCh  chan struct{} // capacity-1 transaction lock
	stopped bool
}

var _ envspec.ServiceProvider = (*FakeServiceProvider)(nil)

// NewFakeServiceProvider returns an empty FakeServiceProvider.
func NewFakeServiceProvider() *FakeServiceProvider {
	return &FakeServiceProvider{services: map[string]*fakeService{}}
}

// AcquireService implements envspec.ServiceProvider.
func (p *FakeServiceProvider) AcquireService(ctx context.Context, owner envspec.Owner, request envspec.ServiceRequest) (envspec.ServiceLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gen++
	svc := &fakeService{
		info: envspec.ServiceInfo{
			ID: envspec.NewID("svc_"), Generation: fmt.Sprintf("gen-%d", p.gen),
			Environment: envspec.NewID("env_"), Name: request.Service.Name,
			Version: request.Service.Version, State: "running", Borrowers: 1,
		},
		lockCh: make(chan struct{}, 1),
	}
	svc.lockCh <- struct{}{}
	p.services[svc.info.ID] = svc
	return &fakeServiceLease{p: p, svc: svc}, nil
}

// StopService implements envspec.ServiceProvider.
func (p *FakeServiceProvider) StopService(ctx context.Context, id, generation string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	svc, ok := p.services[id]
	if !ok {
		return fmt.Errorf("%w: service %s", envspec.ErrNotFound, id)
	}
	if svc.info.Generation != generation {
		return fmt.Errorf("%w: service %s generation %s is not current", envspec.ErrConflict, id, generation)
	}
	svc.stopped = true
	svc.info.State = "stopped"
	return nil
}

type fakeServiceLease struct {
	p        *FakeServiceProvider
	svc      *fakeService
	released bool
}

var _ envspec.ServiceLease = (*fakeServiceLease)(nil)

func (l *fakeServiceLease) ID() string { return l.svc.info.Environment }

func (l *fakeServiceLease) Info() envspec.Info {
	return envspec.Info{ID: l.svc.info.Environment, State: envspec.StatePresent,
		Services: []envspec.ServiceInfo{l.ServiceInfo()}}
}

func (l *fakeServiceLease) ServiceInfo() envspec.ServiceInfo {
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	return l.svc.info
}

func (l *fakeServiceLease) live() error {
	if l.released || l.svc.stopped {
		return fmt.Errorf("%w: service lease released", envspec.ErrConflict)
	}
	return nil
}

func (l *fakeServiceLease) Exec(ctx context.Context, cmd envspec.Cmd) ([]byte, []byte, int, error) {
	if err := l.live(); err != nil {
		return nil, nil, 0, err
	}
	return nil, nil, 0, fmt.Errorf("envtest: FakeServiceProvider does not exec")
}

func (l *fakeServiceLease) Push(ctx context.Context, src envspec.Source, dst string) error {
	if err := l.live(); err != nil {
		return err
	}
	// Drain the source so copy bugs in callers still surface.
	for e, err := range src.Entries(ctx) {
		if err != nil {
			return err
		}
		rc, err := src.Open(ctx, e)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	return nil
}

func (l *fakeServiceLease) Pull(ctx context.Context, src string, dst envspec.Sink) error {
	return l.live()
}

func (l *fakeServiceLease) Release(ctx context.Context) error {
	l.p.mu.Lock()
	defer l.p.mu.Unlock()
	if !l.released {
		l.released = true
		if l.svc.info.Borrowers > 0 {
			l.svc.info.Borrowers--
		}
	}
	return nil
}

// Lock implements envspec.ServiceLease: one transaction at a time.
func (l *fakeServiceLease) Lock(ctx context.Context) (func(), error) {
	if err := l.live(); err != nil {
		return nil, err
	}
	select {
	case <-l.svc.lockCh:
		var once sync.Once
		return func() { once.Do(func() { l.svc.lockCh <- struct{}{} }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
