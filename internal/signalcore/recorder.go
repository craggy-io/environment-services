package signalcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/klauspost/compress/zstd"
)

// RecorderConfig tunes the recorder ring and spill.
type RecorderConfig struct {
	Dir        string        // segment root; empty = memory only
	RingWindow time.Duration // default 15m
	RingBytes  int64         // per (source,stream); default 64 MiB
	Now        func() time.Time
}

type ringKey struct{ source, stream string }

type collectorRestart struct {
	source, collector string
	streams           []StreamDef
	at                time.Time
}

type storedEvent struct {
	ev             envspec.SignalEvent
	raw            json.RawMessage // marshalled envelope (Data as JSON)
	containsSecret bool
	size           int64
}

type segment struct {
	path           string
	from, to       time.Time
	fromSeq, toSeq uint64 // per collector is mixed; seq kept for index display
	count          int
}

// Recorder is a subscriber to every running collector (fed by the hub). It
// keeps a per-stream ring in memory, spills older events to zstd segments,
// and answers Range queries and Captures over both.
type Recorder struct {
	cfg   RecorderConfig
	mu    sync.Mutex
	rings map[ringKey][]storedEvent
	bytes map[ringKey]int64
	segs  map[ringKey][]segment
	// quality tracking per source
	gaps     map[ringKey][]envspec.Gap
	lastSeq  map[string]uint64 // collector → seq
	restarts []collectorRestart
	skew     map[string]time.Duration
	pinned   map[string]*Capture // capture ID → capture (retention pins)
}

// NewRecorder builds a recorder. Attach it with WithRecorder.
func NewRecorder(cfg RecorderConfig) *Recorder {
	if cfg.RingWindow <= 0 {
		cfg.RingWindow = 15 * time.Minute
	}
	if cfg.RingBytes <= 0 {
		cfg.RingBytes = 64 << 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	r := &Recorder{
		cfg: cfg, rings: map[ringKey][]storedEvent{}, bytes: map[ringKey]int64{},
		segs: map[ringKey][]segment{}, gaps: map[ringKey][]envspec.Gap{},
		lastSeq: map[string]uint64{},
		skew:    map[string]time.Duration{}, pinned: map[string]*Capture{},
	}
	if cfg.Dir != "" {
		r.loadIndex()
	}
	return r
}

func (r *Recorder) loadIndex() {
	entries, _ := filepath.Glob(filepath.Join(r.cfg.Dir, "*", "*", "*.meta.json"))
	for _, meta := range entries {
		b, err := os.ReadFile(meta)
		if err != nil {
			continue
		}
		var m struct {
			Source, Stream string
			From, To       time.Time
			FromSeq, ToSeq uint64
			Count          int
		}
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		key := ringKey{m.Source, m.Stream}
		r.segs[key] = append(r.segs[key], segment{
			path: strings.TrimSuffix(meta, ".meta.json") + ".zst",
			from: m.From, to: m.To, fromSeq: m.FromSeq, toSeq: m.ToSeq, count: m.Count,
		})
	}
	for k := range r.segs {
		sort.Slice(r.segs[k], func(i, j int) bool { return r.segs[k][i].from.Before(r.segs[k][j].from) })
	}
}

type wireEvent struct {
	Stream         string          `json:"stream"`
	Source         string          `json:"source"`
	Collector      string          `json:"collector"`
	Seq            uint64          `json:"seq"`
	Time           time.Time       `json:"time"`
	Data           json.RawMessage `json:"data,omitempty"`
	Depth          int             `json:"depth,omitempty"`
	CodeRef        string          `json:"code_ref,omitempty"`
	ContainsSecret bool            `json:"contains_secret,omitempty"`
}

func marshalEvent(ev envspec.SignalEvent, containsSecret bool) (storedEvent, error) {
	var data json.RawMessage
	if ev.Data != nil {
		var err error
		data, err = json.Marshal(ev.Data)
		if err != nil {
			return storedEvent{}, fmt.Errorf("signalcore: encode event data: %w", err)
		}
	}
	w := wireEvent{Stream: ev.Stream, Source: ev.Source, Collector: ev.Collector, Seq: ev.Seq,
		Time: ev.Time, Data: data, Depth: ev.Depth, CodeRef: ev.CodeRef, ContainsSecret: containsSecret}
	raw, err := json.Marshal(w)
	if err != nil {
		return storedEvent{}, fmt.Errorf("signalcore: encode event envelope: %w", err)
	}
	return storedEvent{ev: ev, raw: raw, containsSecret: containsSecret, size: int64(len(raw)) + 64}, nil
}

func unmarshalWire(raw json.RawMessage) (envspec.SignalEvent, bool, error) {
	var w wireEvent
	if err := json.Unmarshal(raw, &w); err != nil {
		return envspec.SignalEvent{}, false, err
	}
	ev := envspec.SignalEvent{Stream: w.Stream, Source: w.Source, Collector: w.Collector, Seq: w.Seq,
		Time: w.Time, Depth: w.Depth, CodeRef: w.CodeRef}
	if len(w.Data) > 0 {
		// Byte streams were marshalled from []byte → JSON string (base64).
		var b []byte
		if json.Unmarshal(w.Data, &b) == nil && len(w.Data) > 0 && w.Data[0] == '"' {
			ev.Data = b
		} else {
			var m any
			if json.Unmarshal(w.Data, &m) == nil {
				ev.Data = m
			}
		}
	}
	return ev, w.ContainsSecret, nil
}

func (r *Recorder) record(ev envspec.SignalEvent, containsSecret bool) {
	se, encodeErr := marshalEvent(ev, containsSecret)
	key := ringKey{ev.Source, ev.Stream}
	r.mu.Lock()
	defer r.mu.Unlock()
	if last, ok := r.lastSeq[ev.Collector]; ok && ev.Seq > last+1 && ev.Collector != "" {
		r.gaps[key] = append(r.gaps[key], envspec.Gap{Collector: ev.Collector, FromSeq: last + 1, ToSeq: ev.Seq - 1, To: ev.Time, Reason: "seq-gap"})
	}
	if ev.Collector != "" && ev.Seq > r.lastSeq[ev.Collector] {
		r.lastSeq[ev.Collector] = ev.Seq
	}
	if encodeErr != nil {
		r.noteLossLocked(key, ev, "encoding-failed")
		return
	}
	r.rings[key] = append(r.rings[key], se)
	r.bytes[key] += se.size
	r.evictLocked(key, ev.Time)
}

func (r *Recorder) evictLocked(key ringKey, now time.Time) {
	ring := r.rings[key]
	cutoff := now.Add(-r.cfg.RingWindow)
	n := 0
	for n < len(ring) && (ring[n].ev.Time.Before(cutoff) || r.bytes[key]-ring[n].size > r.cfg.RingBytes) {
		r.bytes[key] -= ring[n].size
		n++
	}
	// Evict strictly while over budget too.
	for r.bytes[key] > r.cfg.RingBytes && n < len(ring) {
		r.bytes[key] -= ring[n].size
		n++
	}
	if n == 0 {
		return
	}
	evicted := ring[:n]
	r.rings[key] = append([]storedEvent(nil), ring[n:]...)
	reason := "memory-evicted"
	if r.cfg.Dir != "" {
		if err := r.spillLocked(key, evicted); err == nil {
			return
		}
		reason = "spill-failed"
	}
	for _, event := range evicted {
		r.noteLossLocked(key, event.ev, reason)
	}
}

// Losses carry their envelope collector identity. Adjacent losses are merged
// to bound bookkeeping; a query overlapping any part remains incomplete.
func (r *Recorder) noteLossLocked(key ringKey, ev envspec.SignalEvent, reason string) {
	gap := envspec.Gap{Collector: ev.Collector, FromSeq: ev.Seq, ToSeq: ev.Seq, From: ev.Time, To: ev.Time, Dropped: 1, Reason: reason}
	list := r.gaps[key]
	if len(list) > 0 {
		last := &list[len(list)-1]
		if last.Reason == reason && last.Collector == ev.Collector && last.ToSeq+1 == ev.Seq && !ev.Time.Before(last.To) {
			last.ToSeq, last.To = ev.Seq, ev.Time
			last.Dropped++
			return
		}
	}
	r.gaps[key] = append(list, gap)
}

func segDir(base, source, stream string) string {
	clean := func(s string) string {
		return strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(s)
	}
	return filepath.Join(base, clean(source), clean(stream))
}

func (r *Recorder) spillLocked(key ringKey, evs []storedEvent) error {
	if len(evs) == 0 {
		return nil
	}
	dir := segDir(r.cfg.Dir, key.source, key.stream)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return err
	}
	defer zw.Close()
	for _, e := range evs {
		buf2 := e.raw
		if _, err := zw.Write(append(append([]byte(nil), buf2...), '\n')); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	// Time and sequence are not unique across collector instances. Allocate
	// the file exclusively so equal-timestamp events cannot overwrite a peer.
	file, err := os.CreateTemp(dir, "segment-*.zst")
	if err != nil {
		return err
	}
	zpath := file.Name()
	metaPath := strings.TrimSuffix(zpath, ".zst") + ".meta.json"
	complete := false
	defer func() {
		if !complete {
			_ = os.Remove(zpath)
			_ = os.Remove(metaPath)
		}
	}()
	_, writeErr := file.Write(buf.Bytes())
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	from, to := evs[0].ev.Time, evs[0].ev.Time
	for _, e := range evs[1:] {
		if e.ev.Time.Before(from) {
			from = e.ev.Time
		}
		if e.ev.Time.After(to) {
			to = e.ev.Time
		}
	}
	meta := map[string]any{
		"Source": key.source, "Stream": key.stream,
		"From": from, "To": to,
		"FromSeq": evs[0].ev.Seq, "ToSeq": evs[len(evs)-1].ev.Seq, "Count": len(evs),
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := os.WriteFile(metaPath, mb, 0600); err != nil {
		return err
	}
	r.segs[key] = append(r.segs[key], segment{path: zpath, from: from,
		to: to, fromSeq: evs[0].ev.Seq, toSeq: evs[len(evs)-1].ev.Seq, count: len(evs)})
	complete = true
	return nil
}

// Flush spills every ring to disk (shutdown, tests).
func (r *Recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, ring := range r.rings {
		if len(ring) == 0 || r.cfg.Dir == "" {
			continue
		}
		if err := r.spillLocked(key, ring); err != nil {
			return err
		}
		// Keep the ring (queries may still need it); mark spilled by
		// remembering segments — Range dedupes by (collector,seq,time).
	}
	return nil
}

func (r *Recorder) noteRestart(source, collector string, streams []StreamDef, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restarts = append(r.restarts, collectorRestart{source: source, collector: collector,
		streams: append([]StreamDef(nil), streams...), at: at})
}

// NoteSkew records a source's device→host clock skew for envspec.Quality.
func (r *Recorder) NoteSkew(source string, skew time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.skew[source] = skew
}

// Range returns recorded events matching sel in [from,to] in time order,
// merging spill segments and the in-memory ring.
func (r *Recorder) Range(sel envspec.Selector, from, to time.Time) iter.Seq2[envspec.SignalEvent, error] {
	return func(yield func(envspec.SignalEvent, error) bool) {
		evs, _, err := r.collect(sel, from, to)
		if err != nil {
			yield(envspec.SignalEvent{}, err)
			return
		}
		for _, ev := range evs {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// Query is Range collected into a slice with its envspec.Quality.
func (r *Recorder) Query(sel envspec.Selector, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error) {
	return r.collect(sel, from, to)
}

func (r *Recorder) collect(sel envspec.Selector, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error) {
	cs, err := compileSelector(sel)
	if err != nil {
		return nil, envspec.Quality{}, err
	}
	r.mu.Lock()
	var keys []ringKey
	seen := map[ringKey]bool{}
	for k := range r.rings {
		seen[k] = true
	}
	for k := range r.segs {
		seen[k] = true
	}
	for k := range r.gaps {
		seen[k] = true
	}
	for k := range seen {
		if cs.streamMatches(k.stream) && (len(sel.Sources) == 0 || globAny(sel.Sources, k.source)) {
			keys = append(keys, k)
		}
	}
	rings := make(map[ringKey][]storedEvent, len(keys))
	segs := make(map[ringKey][]segment, len(keys))
	var q envspec.Quality
	for _, k := range keys {
		rings[k] = append([]storedEvent(nil), r.rings[k]...)
		segs[k] = append([]segment(nil), r.segs[k]...)
		for _, g := range r.gaps[k] {
			if g.Collector != "" && len(sel.Collectors) > 0 && !globAny(sel.Collectors, g.Collector) {
				continue
			}
			if (from.IsZero() || g.To.IsZero() || !g.To.Before(from)) && (to.IsZero() || g.From.IsZero() || !g.From.After(to)) {
				q.Gaps = append(q.Gaps, g)
				q.Dropped += g.Dropped
				if g.Reason == "seq-gap" && g.ToSeq >= g.FromSeq {
					q.SeqGaps += g.ToSeq - g.FromSeq + 1
				}
			}
		}
		if s := r.skew[k.source]; s != 0 {
			q.ClockSkew = s
		}
	}
	for _, restart := range r.restarts {
		if (!from.IsZero() && restart.at.Before(from)) || (!to.IsZero() && restart.at.After(to)) ||
			(len(sel.Sources) > 0 && !globAny(sel.Sources, restart.source)) ||
			(len(sel.Collectors) > 0 && !globAny(sel.Collectors, restart.collector)) {
			continue
		}
		for _, stream := range restart.streams {
			if cs.streamMatches(stream.Name) {
				q.Restarts++
				break
			}
		}
	}
	r.mu.Unlock()

	dedupe := map[string]bool{}
	var out []envspec.SignalEvent
	add := func(ev envspec.SignalEvent, secret bool) {
		if !from.IsZero() && ev.Time.Before(from) {
			return
		}
		if !to.IsZero() && ev.Time.After(to) {
			return
		}
		id := fmt.Sprintf("%s/%d/%d", ev.Collector, ev.Seq, ev.Time.UnixNano())
		if ev.Collector != "" && dedupe[id] {
			return
		}
		dedupe[id] = true
		if !cs.match(ev) {
			return
		}
		if secret {
			q.ContainsSecret = true
		}
		out = append(out, ev)
	}
	for _, k := range keys {
		for _, seg := range segs[k] {
			if !to.IsZero() && seg.from.After(to) || !from.IsZero() && seg.to.Before(from) {
				continue
			}
			evs, err := readSegment(seg.path)
			if err != nil {
				return nil, q, err
			}
			for _, item := range evs {
				add(item.ev, item.containsSecret)
			}
		}
		for _, se := range rings[k] {
			ev, secret, err := unmarshalWire(se.raw)
			if err != nil {
				continue
			}
			// Ring entries retain their typed Data; prefer it.
			ev.Data = se.ev.Data
			add(ev, secret)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	q.Received = uint64(len(out))
	return out, q, nil
}

func readSegment(path string) ([]storedEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var out []storedEvent
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		ev, secret, err := unmarshalWire(line)
		if err != nil {
			continue
		}
		out = append(out, storedEvent{ev: ev, raw: append(json.RawMessage(nil), line...), containsSecret: secret})
	}
	return out, nil
}

// Latest returns the newest recorded time for a source (zero if none).
func (r *Recorder) Latest(source string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	var latest time.Time
	for k, ring := range r.rings {
		if k.source == source && len(ring) > 0 && ring[len(ring)-1].ev.Time.After(latest) {
			latest = ring[len(ring)-1].ev.Time
		}
	}
	return latest
}

// Stats lists retention per (source, stream), sorted by source then
// stream. It reads only the in-memory index: no segment is opened.
func (r *Recorder) Stats() []envspec.StreamStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	seen := map[ringKey]bool{}
	for k := range r.rings {
		seen[k] = true
	}
	for k := range r.segs {
		seen[k] = true
	}
	out := make([]envspec.StreamStats, 0, len(seen))
	for k := range seen {
		st := envspec.StreamStats{Source: k.source, Stream: k.stream, RingBytes: r.bytes[k], Gaps: len(r.gaps[k])}
		widen := func(from, to time.Time) {
			if !from.IsZero() && (st.Oldest.IsZero() || from.Before(st.Oldest)) {
				st.Oldest = from
			}
			if to.After(st.Newest) {
				st.Newest = to
			}
		}
		for _, seg := range r.segs[k] {
			st.Segments++
			st.SpilledEvents += seg.count
			widen(seg.from, seg.to)
		}
		if ring := r.rings[k]; len(ring) > 0 {
			st.RingEvents = len(ring)
			widen(ring[0].ev.Time, ring[len(ring)-1].ev.Time)
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].Stream < out[j].Stream
	})
	return out
}

// Tail returns up to n of the newest events matching sel, newest
// first. It reads only the in-memory rings (the last RingWindow), so
// it is cheap enough for status views; Query covers spilled history.
func (r *Recorder) Tail(sel envspec.Selector, n int) []envspec.SignalEvent {
	if n <= 0 {
		return nil
	}
	cs, err := compileSelector(sel)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	var cand []storedEvent
	for k, ring := range r.rings {
		if !cs.streamMatches(k.stream) || len(sel.Sources) > 0 && !globAny(sel.Sources, k.source) {
			continue
		}
		// Each ring is time-ordered, so only its last n can make the cut.
		kept := 0
		for i := len(ring) - 1; i >= 0 && kept < n; i-- {
			if cs.match(ring[i].ev) {
				cand = append(cand, ring[i])
				kept++
			}
		}
	}
	r.mu.Unlock()
	sort.SliceStable(cand, func(i, j int) bool { return cand[i].ev.Time.After(cand[j].ev.Time) })
	if len(cand) > n {
		cand = cand[:n]
	}
	out := make([]envspec.SignalEvent, 0, len(cand))
	for _, se := range cand {
		out = append(out, se.ev)
	}
	return out
}
