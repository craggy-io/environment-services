package signalcore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// tickCollector is already "bound" the way an adapter would bind it: no
// target or handle lookup at Run time.
type tickCollector struct {
	starts atomic.Int64
	seq    atomic.Uint64
}

func (c *tickCollector) Name() string { return "test.tick" }
func (c *tickCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: "test.tick@1", Cost: "low"}}
}
func (c *tickCollector) Applies(e signalcore.EnvInfo) error {
	if e.ID == "env_bad" {
		return fmt.Errorf("not for this env")
	}
	return nil
}
func (c *tickCollector) Params() *jsonschema.Schema { return nil }
func (c *tickCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	c.starts.Add(1)
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			out.Emit(envspec.SignalEvent{Stream: "test.tick@1", Data: map[string]any{"n": float64(c.seq.Add(1)), "kind": "tick"}})
		}
	}
}

func drainN(t *testing.T, sub *signalcore.Subscription, n int, timeout time.Duration) []envspec.SignalEvent {
	t.Helper()
	var got []envspec.SignalEvent
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case ev := <-sub.Events():
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("got %d of %d events", len(got), n)
		}
	}
	return got
}

func TestHubRunFilterQuality(t *testing.T) {
	ctx := context.Background()
	col := &tickCollector{}
	hub := signalcore.NewHub(signalcore.WithCollectors(col))
	defer hub.Close()
	s1, err := hub.Subscribe(ctx, envspec.Selector{Streams: []string{"test.tick@1"}, Sources: []string{"env_a"}})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := hub.Subscribe(ctx, envspec.Selector{Streams: []string{"test.*@1"}, Sources: []string{"env_a"},
		Where: envspec.Filter{{Field: "n", Op: envspec.OpGT, Value: float64(3)}}})
	if err != nil {
		t.Fatal(err)
	}
	// Subscribing starts nothing; the core-side Run call does.
	if col.starts.Load() != 0 || len(hub.Running()) != 0 {
		t.Fatalf("subscribe must not start collectors: starts=%d running=%v", col.starts.Load(), hub.Running())
	}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	var instance string
	go func() {
		var err error
		instance, err = hub.Run(runCtx, col, "env_a", nil)
		done <- err
	}()
	got := drainN(t, s1, 5, 3*time.Second)
	if got[0].Seq == 0 || got[0].Source != "env_a" || got[0].Collector == "" {
		t.Fatalf("envelope = %+v", got[0])
	}
	got2 := drainN(t, s2, 2, 3*time.Second)
	for _, ev := range got2 {
		if v, _ := signalcore.DataField(ev, "n"); v == float64(1) || v == float64(2) || v == float64(3) {
			t.Fatalf("filter let through n=%v", v)
		}
	}
	if col.starts.Load() != 1 {
		t.Fatalf("collector started %d times, want 1 (one Run)", col.starts.Load())
	}
	if st := hub.State("test.tick@1", "env_a"); !st.Running || len(st.Collectors) != 1 || st.Collectors[0] != got[0].Collector {
		t.Fatalf("state while running = %+v (events stamped %s)", st, got[0].Collector)
	}
	s1.Close()
	// Closing subscriptions does not stop the run; its context does.
	_ = drainN(t, s2, 1, 2*time.Second)
	s2.Close()
	if len(hub.Running()) != 1 {
		t.Fatalf("run must outlive its subscribers: %+v", hub.Running())
	}
	stop()
	if err := <-done; err != nil {
		t.Fatalf("run err = %v", err)
	}
	if instance == "" || instance != got[0].Collector {
		t.Fatalf("instance = %q, events stamped %q", instance, got[0].Collector)
	}
	if len(hub.Running()) != 0 {
		t.Fatalf("collector should stop when its ctx ends: %+v", hub.Running())
	}
}

func TestTickCollectorApplies(t *testing.T) {
	// Applicability is a plain collector-side check the resolver wraps;
	// the hub never consults it.
	var _ signalcore.Applicability = &tickCollector{}
	col := &tickCollector{}
	if err := col.Applies(signalcore.EnvInfo{ID: "env_bad"}); err == nil || !strings.Contains(err.Error(), "not for this env") {
		t.Fatalf("Applies(env_bad) = %v", err)
	}
	if err := col.Applies(signalcore.EnvInfo{ID: "env_a"}); err != nil {
		t.Fatalf("Applies(env_a) = %v", err)
	}
}

func TestRecorderRangeCaptureSpillQuality(t *testing.T) {
	dir := t.TempDir()
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: dir, RingWindow: time.Hour, RingBytes: 2048 /* tiny: forces spill */})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Publish 200 sizeable events with a seq gap at 100.
	for i := 1; i <= 200; i++ {
		seq := uint64(i)
		if i > 100 {
			seq = uint64(i + 5) // gap of 5
		}
		hub.Publish(envspec.SignalEvent{Stream: "test.data@1", Source: "env_a", Collector: "col_x", Seq: seq,
			Time: base.Add(time.Duration(i) * time.Second),
			Data: map[string]any{"i": float64(i), "pad": strings.Repeat("x", 64)}})
	}
	hub.Publish(envspec.SignalEvent{Stream: "net.pcap", Source: "env_a", Collector: "col_x", Seq: 999,
		Time: base.Add(300 * time.Second), Data: []byte{1, 2, 3, 4}})
	if err := rec.Flush(); err != nil {
		t.Fatal(err)
	}
	evs, q, err := rec.Query(envspec.Selector{Streams: []string{"test.data@1"}}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 200 {
		t.Fatalf("range returned %d events (spill+ring merge broken)", len(evs))
	}
	if q.SeqGaps == 0 || q.LossFree() {
		t.Fatalf("quality must report the seq gap: %+v", q)
	}
	// Where filter.
	evs2, _, _ := rec.Query(envspec.Selector{Streams: []string{"test.data@1"},
		Where: envspec.Filter{{Field: "i", Op: envspec.OpGE, Value: float64(195)}}}, base, base.Add(time.Hour))
	if len(evs2) != 6 {
		t.Fatalf("filtered range = %d", len(evs2))
	}
	// Capture exports a Source tree with range.json + pcap.
	capObj, err := rec.Capture(envspec.Selector{Streams: []string{"test.data@1", "net.pcap"}}, base, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	src := capObj.Source()
	var names []string
	for en, err := range src.Entries(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, en.Path)
		if en.Path == "range.json" {
			rc, _ := src.Open(context.Background(), en)
			b, _ := io.ReadAll(rc)
			rc.Close()
			if !strings.Contains(string(b), capObj.ID()) {
				t.Fatalf("range.json missing capture id: %s", b)
			}
		}
	}
	hasPCAP, hasRange := false, false
	for _, n := range names {
		hasPCAP = hasPCAP || strings.HasSuffix(n, ".pcap")
		hasRange = hasRange || n == "range.json"
	}
	if !hasPCAP || !hasRange {
		t.Fatalf("capture tree = %v", names)
	}
	hub.Close()
}

func TestFilterOps(t *testing.T) {
	ev := envspec.SignalEvent{Stream: "android.logcat@1", Source: "env_1",
		Data: map[string]any{"tag": "ActivityManager", "pid": float64(42), "text": "hello world"}}
	cases := []struct {
		f    map[string]any
		want bool
	}{
		{map[string]any{"tag": "ActivityManager"}, true},
		{map[string]any{"tag": "Other"}, false},
		{map[string]any{"tag": []any{"X", "ActivityManager"}}, true},
		{map[string]any{"text": "re:hell.*"}, true},
		{map[string]any{"text": "re:nope"}, false},
	}
	for _, c := range cases {
		f, err := signalcore.Where(c.f)
		if err != nil {
			t.Fatal(err)
		}
		if got := signalcore.FilterMatch(f, ev); got != c.want {
			t.Fatalf("Where(%v) match = %v want %v", c.f, got, c.want)
		}
	}
	sel := envspec.Selector{Streams: []string{"android.*@1"}, Sources: []string{"env_*"}}
	if !signalcore.SelectorMatch(sel, ev) {
		t.Fatal("glob selector should match")
	}
}

func TestRecorderStatsAndTail(t *testing.T) {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{Now: func() time.Time { return base }})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))
	for i := 0; i < 5; i++ {
		hub.Publish(envspec.SignalEvent{Stream: "a@1", Source: "host", Collector: "c1", Seq: uint64(i + 1),
			Time: base.Add(time.Duration(i) * time.Second), Data: map[string]any{"i": i}})
	}
	// Seq 3 → 5 on c2 leaves a gap of one.
	hub.Publish(envspec.SignalEvent{Stream: "b@1", Source: "env_x", Collector: "c2", Seq: 3, Time: base.Add(10 * time.Second)})
	hub.Publish(envspec.SignalEvent{Stream: "b@1", Source: "env_x", Collector: "c2", Seq: 5, Time: base.Add(2500 * time.Millisecond)})

	stats := rec.Stats()
	if len(stats) != 2 || stats[0].Source != "env_x" || stats[1].Source != "host" {
		t.Fatalf("stats = %+v", stats)
	}
	if a := stats[1]; a.RingEvents != 5 || a.RingBytes <= 0 || !a.Oldest.Equal(base) || !a.Newest.Equal(base.Add(4*time.Second)) || a.Gaps != 0 {
		t.Fatalf("host stats = %+v", a)
	}
	if b := stats[0]; b.RingEvents != 2 || b.Gaps != 1 {
		t.Fatalf("env_x stats = %+v", b)
	}

	// Newest first across rings: b's seq 3 (+10s), then a's +4s and +3s.
	tail := rec.Tail(envspec.Selector{}, 3)
	if len(tail) != 3 || tail[0].Stream != "b@1" || tail[0].Seq != 3 || tail[1].Seq != 5 || tail[2].Seq != 4 || tail[2].Stream != "a@1" {
		t.Fatalf("tail = %+v", tail)
	}
	if got := rec.Tail(envspec.Selector{Streams: []string{"a@1"}}, 10); len(got) != 5 || got[0].Seq != 5 {
		t.Fatalf("stream tail = %+v", got)
	}
	if got := rec.Tail(envspec.Selector{Sources: []string{"nobody"}}, 10); len(got) != 0 {
		t.Fatalf("source tail = %+v", got)
	}
}

func TestCaptureSelectsEnvelopeCollector(t *testing.T) {
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: t.TempDir(), RingWindow: time.Hour})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))
	defer hub.Close()
	now := time.Now()
	for _, id := range []string{"own", "unrelated"} {
		hub.Publish(envspec.SignalEvent{Stream: "test.data@1", Source: "env_a", Collector: id, Seq: 1,
			Time: now, Data: map[string]any{"collector": "own"}})
	}
	cap, err := rec.Capture(envspec.Selector{Collectors: []string{"own"}}, now.Add(-time.Second), now)
	if err != nil {
		t.Fatal(err)
	}
	defer cap.Close()
	evs, _, err := cap.Events()
	if err != nil || len(evs) != 1 || evs[0].Collector != "own" {
		t.Fatalf("events=%+v err=%v", evs, err)
	}
	meta, err := cap.Meta()
	if err != nil || len(meta.Collectors) != 1 || meta.Collectors[0] != "own" {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	for entry, err := range cap.Source().Entries(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		if entry.Path != "range.json" {
			continue
		}
		reader, err := cap.Source().Open(context.Background(), entry)
		if err != nil {
			t.Fatal(err)
		}
		var exported envspec.RangeMeta
		err = json.NewDecoder(reader).Decode(&exported)
		reader.Close()
		if err != nil || len(exported.Collectors) != 1 || exported.Collectors[0] != "own" {
			t.Fatalf("export=%+v err=%v", exported, err)
		}
	}
}
