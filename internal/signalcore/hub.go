package signalcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

// HubOption configures a Hub.
type HubOption func(*Hub)

// WithRedactor installs a secret redactor.
func WithRedactor(r Redactor) HubOption { return func(h *Hub) { h.redactor = r } }

// WithRecorder installs the recorder (receives every published event).
func WithRecorder(r *Recorder) HubOption { return func(h *Hub) { h.recorder = r } }

// WithCollectors registers collectors in the stream catalogue at
// construction. Registration never starts anything.
func WithCollectors(cs ...Collector) HubOption {
	return func(h *Hub) {
		for _, c := range cs {
			h.Register(c)
		}
	}
}

// WithClock injects a clock (tests).
func WithClock(now func() time.Time) HubOption { return func(h *Hub) { h.now = now } }

// Lifecycle is one collector lifecycle fact.
type Lifecycle struct {
	Kind      string         // collector.started, collector.stopped or collector.failed
	Collector string         // running instance ID
	Name      string         // collector name
	Source    string         // "host" or an environment ID
	Data      map[string]any // extra facts; always carries the collector's "name"
	Err       error          // the collector's error (collector.failed)
}

// WatchLifecycle adds a listener for collector lifecycle facts.
// Listeners run with the hub's lock held: a listener that reaches back
// into the hub or the env manager must hand off to another goroutine.
// The returned func removes the listener; once it returns, the
// listener is not running and will not run again.
func (h *Hub) WatchLifecycle(fn func(Lifecycle)) (remove func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.watchSeq++
	id := h.watchSeq
	h.watchers[id] = fn
	return func() {
		h.mu.Lock()
		delete(h.watchers, id)
		h.mu.Unlock()
	}
}

type runKey struct {
	name   string
	source string
	digest string
}

// activeRun is one in-flight Hub.Run call. The hub tracks it for state
// and coverage answers; it does not own it — the caller's ctx does.
type activeRun struct {
	key  runKey
	id   string
	c    Collector
	span *RunSpan
}

// Hub fans published events out to filtered subscriptions and the
// recorder, and executes single collector runs on behalf of the
// lifecycle core. It starts nothing on its own: no reference counting,
// no linger, no pins — core's activities own those decisions.
type Hub struct {
	mu         sync.Mutex
	collectors map[string]Collector // by collector name (catalogue)
	byStream   map[string]string    // stream → collector name
	active     map[string]*activeRun
	subs       map[*Subscription]struct{}
	redactor   Redactor
	recorder   *Recorder
	watchers   map[uint64]func(Lifecycle)
	watchSeq   uint64
	now        func() time.Time
	closed     bool
	spans      map[spanKey][]*RunSpan // run history for coverage answers
}

// NewHub builds a Hub.
func NewHub(opts ...HubOption) *Hub {
	h := &Hub{
		collectors: map[string]Collector{},
		byStream:   map[string]string{},
		active:     map[string]*activeRun{},
		subs:       map[*Subscription]struct{}{},
		spans:      map[spanKey][]*RunSpan{},
		watchers:   map[uint64]func(Lifecycle){},
		now:        time.Now,
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Register adds a collector to the stream catalogue. Later registrations
// replace by name.
func (h *Hub) Register(c Collector) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.registerLocked(c)
}

func (h *Hub) registerLocked(c Collector) {
	h.collectors[c.Name()] = c
	for _, s := range c.Streams() {
		h.byStream[s.Name] = c.Name()
	}
}

// Collectors lists registered collectors.
func (h *Hub) Collectors() []Collector {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]Collector, 0, len(h.collectors))
	for _, c := range h.collectors {
		out = append(out, c)
	}
	return out
}

// CollectorFor resolves a stream to its catalogued collector.
func (h *Hub) CollectorFor(stream string) (Collector, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name, ok := h.byStream[stream]
	if !ok {
		return nil, false
	}
	c, ok := h.collectors[name]
	return c, ok
}

// Streams lists all stream definitions.
func (h *Hub) Streams() []StreamDef {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []StreamDef
	for _, c := range h.collectors {
		out = append(out, c.Streams()...)
	}
	return out
}

func paramsDigest(p json.RawMessage) string {
	if len(p) == 0 {
		return ""
	}
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

// Run executes one collector run for (collector, source, params) and
// blocks until it returns. It stamps the emitter's sequence, opens and
// closes the run span used by State/Coverage, notes a recorder restart on
// failure, and reports lifecycle facts. Overlapping runs of the same
// collector for the same source and params are possible if the caller
// asks twice; preventing that is the caller's (activity) concern. The
// returned instance ID names this run in events, spans and State.
func (h *Hub) Run(ctx context.Context, c Collector, source string, params json.RawMessage) (string, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return "", fmt.Errorf("signalcore: hub closed")
	}
	h.registerLocked(c)
	rc := &activeRun{
		key: runKey{name: c.Name(), source: source, digest: paramsDigest(params)},
		id:  envspec.NewID("col_"),
		c:   c,
	}
	h.active[rc.id] = rc
	h.openSpanLocked(rc)
	h.notifyLocked("collector.started", rc, nil, nil)
	h.mu.Unlock()

	err := c.Run(ctx, params, &collectorEmitter{hub: h, rc: rc})

	h.mu.Lock()
	delete(h.active, rc.id)
	if err != nil && ctx.Err() == nil {
		h.closeSpanLocked(rc, err.Error())
		h.notifyLocked("collector.failed", rc, map[string]any{"error": err.Error()}, err)
		if h.recorder != nil {
			h.recorder.noteRestart(source, rc.id, c.Streams(), h.now())
		}
	} else {
		h.closeSpanLocked(rc, "")
		h.notifyLocked("collector.stopped", rc, nil, nil)
	}
	h.mu.Unlock()
	return rc.id, err
}

// Running lists live collector instances (for env.status).
func (h *Hub) Running() []CollectorInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]CollectorInfo, 0, len(h.active))
	for _, rc := range h.active {
		streams := make([]string, 0)
		for _, s := range rc.c.Streams() {
			streams = append(streams, s.Name)
		}
		out = append(out, CollectorInfo{ID: rc.id, Name: rc.c.Name(), Source: rc.key.source, Streams: streams})
	}
	return out
}

// CollectorInfo describes one running collector instance.
type CollectorInfo struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Source  string   `json:"source"`
	Streams []string `json:"streams"`
}

func (h *Hub) notifyLocked(kind string, rc *activeRun, data map[string]any, err error) {
	if len(h.watchers) == 0 {
		return
	}
	facts := map[string]any{"name": rc.c.Name()}
	for k, v := range data {
		facts[k] = v
	}
	for _, fn := range h.watchers {
		fn(Lifecycle{Kind: kind, Collector: rc.id, Name: rc.c.Name(), Source: rc.key.source, Data: facts, Err: err})
	}
}

type collectorEmitter struct {
	hub *Hub
	rc  *activeRun
	seq uint64
	mu  sync.Mutex
}

func (e *collectorEmitter) Emit(ev envspec.SignalEvent) {
	e.mu.Lock()
	e.seq++
	seq := e.seq
	e.mu.Unlock()
	if ev.Seq == 0 {
		ev.Seq = seq
	}
	if ev.Collector == "" {
		ev.Collector = e.rc.id
	}
	if ev.Source == "" {
		ev.Source = e.rc.key.source
	}
	e.hub.Publish(ev)
}

// Publish injects an event from outside a collector run (the exec tee,
// rule emits, host metrics). Redaction runs here, before fan-out and
// recording.
func (h *Hub) Publish(ev envspec.SignalEvent) {
	if ev.Depth >= 4 {
		return // rule loop guard
	}
	if ev.Time.IsZero() {
		ev.Time = h.now()
	}
	containsSecret := false
	if h.redactor != nil {
		if b, ok := ev.Data.([]byte); ok {
			containsSecret = h.redactor.ContainsSecret(b)
		} else {
			ev.Data = h.redactor.Redact(ev.Stream, ev.Data)
		}
	}
	h.mu.Lock()
	subs := make([]*Subscription, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	rec := h.recorder
	h.mu.Unlock()
	if rec != nil {
		rec.record(ev, containsSecret)
	}
	for _, s := range subs {
		s.deliver(ev)
	}
}

// SubOption configures a subscription.
type SubOption func(*Subscription)

// WithBuffer sets the subscription buffer size (default 256). Overflow
// drops the oldest event and counts into Quality.
func WithBuffer(n int) SubOption { return func(s *Subscription) { s.bufSize = n } }

// Subscribe opens a filtered subscription. It starts no collectors:
// whether anything publishes into it is the lifecycle core's business.
// Cancelling ctx (or Close) ends it.
func (h *Hub) Subscribe(ctx context.Context, sel envspec.Selector, opts ...SubOption) (*Subscription, error) {
	cs, err := compileSelector(sel)
	if err != nil {
		return nil, err
	}
	s := &Subscription{hub: h, sel: sel, cs: cs, bufSize: 256, lastSeq: map[string]uint64{}}
	for _, o := range opts {
		o(s)
	}
	if s.bufSize <= 0 {
		s.bufSize = 256
	}
	s.ch = make(chan envspec.SignalEvent, s.bufSize)

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, fmt.Errorf("signalcore: hub closed")
	}
	h.subs[s] = struct{}{}
	h.mu.Unlock()

	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s, nil
}

// Close ends all subscriptions and refuses new runs and subscriptions.
// Collector runs in flight belong to their callers' contexts; Close does
// not cancel them.
func (h *Hub) Close() {
	h.mu.Lock()
	h.closed = true
	subs := make([]*Subscription, 0, len(h.subs))
	for s := range h.subs {
		subs = append(subs, s)
	}
	h.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}

// Subscription is one filtered event stream.
type Subscription struct {
	hub     *Hub
	sel     envspec.Selector
	cs      compiledSelector
	ch      chan envspec.SignalEvent
	bufSize int
	once    sync.Once

	mu      sync.Mutex
	quality envspec.Quality
	lastSeq map[string]uint64 // collector → last seq seen
	closed  bool
}

// Events returns the delivery channel. It closes on Close.
func (s *Subscription) Events() <-chan envspec.SignalEvent { return s.ch }

// Quality returns loss counters for this subscription.
func (s *Subscription) Quality() envspec.Quality {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quality
}

// Selector returns the subscription's selector.
func (s *Subscription) Selector() envspec.Selector { return s.sel }

func (s *Subscription) deliver(ev envspec.SignalEvent) {
	if !s.cs.match(ev) {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if last, ok := s.lastSeq[ev.Collector]; ok && ev.Seq > last+1 {
		s.quality.SeqGaps += ev.Seq - last - 1
		s.quality.Gaps = append(s.quality.Gaps, envspec.Gap{Collector: ev.Collector, FromSeq: last + 1, ToSeq: ev.Seq - 1, To: ev.Time, Reason: "seq-gap"})
	}
	if ev.Seq > s.lastSeq[ev.Collector] {
		s.lastSeq[ev.Collector] = ev.Seq
	}
	s.mu.Unlock()

	select {
	case s.ch <- ev:
	default: // drop oldest
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- ev:
		default:
		}
		s.mu.Lock()
		s.quality.Dropped++
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.quality.Received++
	s.mu.Unlock()
}

// Close closes the channel. Idempotent.
func (s *Subscription) Close() {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.hub.mu.Lock()
		delete(s.hub.subs, s)
		s.hub.mu.Unlock()
		close(s.ch)
	})
}

// --- Run spans, state and coverage ---

// spanKey groups one mechanism on one source; each span retains its configuration.
type spanKey struct{ collector, source string }

// RunSpan is one period a collector instance ran on a source. To is zero
// while it still runs; Failure is the error it stopped with, if any.
type RunSpan struct {
	Instance     string    `json:"instance"`
	Collector    string    `json:"collector"`
	Source       string    `json:"source"`
	From         time.Time `json:"from"`
	To           time.Time `json:"to,omitempty"`
	Failure      string    `json:"failure,omitempty"`
	ParamsDigest string    `json:"paramsDigest,omitempty"`
}

// maxSpans bounds the run history kept per collector and source.
const maxSpans = 64

func (h *Hub) openSpanLocked(rc *activeRun) {
	sp := &RunSpan{Instance: rc.id, Collector: rc.c.Name(), Source: rc.key.source, From: h.now(), ParamsDigest: rc.key.digest}
	rc.span = sp
	k := spanKey{rc.c.Name(), rc.key.source}
	list := append(h.spans[k], sp)
	if len(list) > maxSpans {
		list = list[len(list)-maxSpans:]
	}
	h.spans[k] = list
}

func (h *Hub) closeSpanLocked(rc *activeRun, failure string) {
	if rc.span == nil || !rc.span.To.IsZero() {
		return
	}
	rc.span.To = h.now()
	rc.span.Failure = failure
}

// State reports a stream's collector state on one source.
func (h *Hub) State(stream, source string) envspec.StreamState {
	return h.state(stream, source, nil)
}

// ConfiguredState observes only instances with exactly these parameter
// bytes. A different script or package selection cannot make this
// observer ready.
func (h *Hub) ConfiguredState(stream, source string, params json.RawMessage) envspec.StreamState {
	digest := paramsDigest(params)
	return h.state(stream, source, &digest)
}

func (h *Hub) state(stream, source string, digest *string) envspec.StreamState {
	h.mu.Lock()
	defer h.mu.Unlock()
	var st envspec.StreamState
	name, ok := h.byStream[stream]
	if !ok {
		return st
	}
	for _, rc := range h.active {
		if rc.key.name == name && rc.key.source == source && (digest == nil || rc.key.digest == *digest) {
			st.Running = true
			st.Collectors = append(st.Collectors, rc.id)
			if rc.span != nil && (st.Since.IsZero() || rc.span.From.Before(st.Since)) {
				st.Since = rc.span.From
			}
		}
	}
	for _, sp := range h.spans[spanKey{name, source}] {
		if digest != nil && sp.ParamsDigest != *digest {
			continue
		}
		if sp.Failure != "" && sp.To.After(st.FailedAt) {
			st.LastFailure, st.FailedAt = sp.Failure, sp.To
		}
	}
	sort.Strings(st.Collectors)
	return st
}

// Coverage answers, for every catalogued stream sel names (globs
// expanded) on every source it names (or every source a collector has
// ever run on), whether a collector ran during [from, to]. A selector
// without streams yields nothing: "everything" has no useful coverage
// answer. NotRunnable is left empty: whether a stream could run on a
// source now is the adapter/core's answer, not the hub's run history.
func (h *Hub) Coverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage {
	return h.coverage(sel, from, to, nil)
}

// ConfiguredCoverage restricts coverage to sel.Params, including nil for
// the default instance. Coverage remains the aggregate inventory view.
func (h *Hub) ConfiguredCoverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage {
	digest := paramsDigest(sel.Params)
	return h.coverage(sel, from, to, &digest)
}

func (h *Hub) coverage(sel envspec.Selector, from, to time.Time, digest *string) []envspec.Coverage {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(sel.Streams) == 0 {
		return nil
	}
	now := h.now()
	if to.IsZero() || to.After(now) {
		to = now
	}
	if !from.Before(to) {
		return nil
	}
	sources := map[string]bool{}
	for k := range h.spans {
		sources[k.source] = true
	}
	for _, src := range sel.Sources {
		// Literally named sources are answered for even if nothing ever
		// ran there; glob sources only match sources with history.
		if src == "" || strings.ContainsAny(src, "*?[\\") {
			continue
		}
		sources[src] = true
	}
	var out []envspec.Coverage
	seen := map[[2]string]bool{}
	for _, pattern := range sel.Streams {
		for name, collector := range h.byStream {
			if !MatchGlob(pattern, name) {
				continue
			}
			for source := range sources {
				if len(sel.Sources) > 0 && !globAny(sel.Sources, source) {
					continue
				}
				if seen[[2]string{name, source}] {
					continue
				}
				cov, ok := h.coverageLocked(collector, name, source, from, to, len(sel.Sources) > 0, now, digest, sel.Collectors)
				if !ok {
					continue
				}
				seen[[2]string{name, source}] = true
				out = append(out, cov)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Stream < out[j].Stream
	})
	return out
}

// coverageLocked computes one stream/source answer. Sources the caller
// did not name are skipped when the stream never ran there, so a wide
// query does not list every unrelated environment.
func (h *Hub) coverageLocked(collector, stream, source string, from, to time.Time, named bool, now time.Time, digest *string, collectors []string) (envspec.Coverage, bool) {
	cov := envspec.Coverage{Stream: stream, Source: source, Window: to.Sub(from)}
	if digest != nil {
		cov.ParamsDigest = *digest
	}
	spans := h.spans[spanKey{collector, source}]
	if !named && len(spans) == 0 {
		return cov, false
	}
	// Spans of parallel instances (different params) may overlap; merge
	// before summing so coverage never exceeds the window.
	type iv struct{ a, b time.Time }
	var ivs []iv
	for _, sp := range spans {
		if len(collectors) > 0 && !slices.Contains(collectors, sp.Instance) {
			continue
		}
		if digest != nil && sp.ParamsDigest != *digest {
			continue
		}
		end := sp.To
		if end.IsZero() {
			end = now
		}
		a, b := sp.From, end
		if a.Before(from) {
			a = from
		}
		if b.After(to) {
			b = to
		}
		if b.After(a) {
			ivs = append(ivs, iv{a, b})
		}
		if sp.Failure != "" && !sp.To.Before(from) && !sp.To.After(to) {
			cov.Failure = sp.Failure
		}
	}
	sort.Slice(ivs, func(i, j int) bool { return ivs[i].a.Before(ivs[j].a) })
	var cur iv
	for i, v := range ivs {
		if i == 0 {
			cur = v
			continue
		}
		if !v.a.After(cur.b) {
			if v.b.After(cur.b) {
				cur.b = v.b
			}
			continue
		}
		cov.Ran += cur.b.Sub(cur.a)
		cur = v
	}
	if len(ivs) > 0 {
		cov.Ran += cur.b.Sub(cur.a)
	}
	switch {
	case cov.Ran <= 0:
		cov.Status = "not_running"
	case cov.Ran >= cov.Window:
		cov.Status = "covered"
	default:
		cov.Status = "partial"
	}
	return cov, true
}
