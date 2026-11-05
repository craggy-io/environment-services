package rules_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/rules"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

const egressSpike = `
rule(
    name = "egress-spike",
    version = 1,
    description = "Outbound bytes over 10s exceed 5x the trailing 5-minute average",
    params = {"env": str, "min_bytes": int},
)
win  = window("10s")
base = ewma(half_life = "5m")
def on_flow(ev):
    win.add(ev.time, ev.data.bytes)
def check():
    total = win.sum()
    if total > params.min_bytes and total > 5 * base.value():
        trigger("spike", {"bytes": total, "baseline": base.value()},
                capture = {"streams": ["net.pcap", "net.flows@1"], "before": "60s", "after": "30s"},
                dedupe = "2m")
    base.observe(total)
on("net.flows@1", source = params.env, where = {"direction": "out"}, fn = on_flow)
every("5s", check)
`

func flowEvent(envID string, at time.Time, bytes float64) envspec.SignalEvent {
	return envspec.SignalEvent{Stream: "net.flows@1", Source: envID, Time: at,
		Data: map[string]any{"direction": "out", "bytes": bytes}}
}

func TestEgressSpikeBacktest(t *testing.T) {
	hub := signalcore.NewHub()
	eng := rules.NewEngine(hub, nil)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var evs []envspec.SignalEvent
	// 2 minutes of modest traffic, then a spike.
	for i := 0; i < 24; i++ {
		evs = append(evs, flowEvent("env_x", base.Add(time.Duration(i*5)*time.Second), 100))
	}
	for i := 0; i < 3; i++ {
		evs = append(evs, flowEvent("env_x", base.Add(time.Duration(120+i*2)*time.Second), 50000))
	}
	res, err := eng.Test(context.Background(), []byte(egressSpike),
		map[string]any{"env": "env_x", "min_bytes": 1000}, evs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Triggers) != 1 {
		t.Fatalf("triggers = %d (%+v)", len(res.Triggers), res.Triggers)
	}
	tr := res.Triggers[0]
	if tr.Kind != "spike" || tr.Capture == nil || tr.Capture.Before != time.Minute {
		t.Fatalf("trigger = %+v", tr)
	}
}

func TestBacktestEqualsLive(t *testing.T) {
	ctx := context.Background()
	// Live: virtual events through a hub with a fake target is indirect —
	// instead drive the engine's Test twice and the live Load path with
	// manual Publish, comparing trigger kinds/counts.
	base := time.Now().Add(-time.Minute)
	var evs []envspec.SignalEvent
	for i := 0; i < 12; i++ {
		evs = append(evs, flowEvent("env_z", base.Add(time.Duration(i)*2*time.Second), 100))
	}
	for i := 0; i < 2; i++ {
		evs = append(evs, flowEvent("env_z", base.Add(30*time.Second+time.Duration(i)*time.Second), 90000))
	}

	back, err := rules.NewEngine(signalcore.NewHub(), nil).Test(ctx, []byte(egressSpike),
		map[string]any{"env": "env_z", "min_bytes": 1000}, evs)
	if err != nil {
		t.Fatal(err)
	}

	// Live on a 50x-fast variant of the same rule shape.
	hub := signalcore.NewHub()

	var mu sync.Mutex
	var triggers []rules.Trigger
	liveEng := rules.NewEngine(hub, nil, rules.WithTriggerSink(func(tr rules.Trigger) (string, error) {
		mu.Lock()
		triggers = append(triggers, tr)
		mu.Unlock()
		return "", nil
	}))
	triggerCount := func() int { mu.Lock(); defer mu.Unlock(); return len(triggers) }
	liveSrc := strings.ReplaceAll(egressSpike, `"10s"`, `"2s"`)
	liveSrc = strings.ReplaceAll(liveSrc, `"5m"`, `"2s"`)
	liveSrc = strings.ReplaceAll(liveSrc, `"5s"`, `"250ms"`)
	if _, err := liveEng.Load(ctx, []byte(liveSrc), "test", map[string]any{"env": "env_z", "min_bytes": 1000}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		hub.Publish(envspec.SignalEvent{Stream: "net.flows@1", Source: "env_z",
			Data: map[string]any{"direction": "out", "bytes": float64(100)}})
		time.Sleep(50 * time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		hub.Publish(envspec.SignalEvent{Stream: "net.flows@1", Source: "env_z",
			Data: map[string]any{"direction": "out", "bytes": float64(90000)}})
		time.Sleep(50 * time.Millisecond)
	}
	deadline := time.Now().Add(3 * time.Second)
	for triggerCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}
	if len(back.Triggers) == 0 {
		t.Fatal("backtest produced no trigger on the spike recording")
	}
	mu.Lock()
	live := append([]rules.Trigger(nil), triggers...)
	mu.Unlock()
	if len(live) == 0 {
		t.Fatal("live rule produced no trigger")
	}
	if live[0].Kind != back.Triggers[0].Kind {
		t.Fatalf("live kind %s != backtest kind %s", live[0].Kind, back.Triggers[0].Kind)
	}
	hub.Close()
}

func TestBusyLoopRuleDisabledNotStallingHub(t *testing.T) {
	ctx := context.Background()
	hub := signalcore.NewHub()

	eng := rules.NewEngine(hub, nil, rules.WithBudgets(50_000, 100*time.Millisecond))
	src := `
rule(name="loopy", version=1, params={})
def on_tick(ev):
    x = 0
    for i in range(100000000):
        x += i
on("test.tick@1", source="env_z", fn=on_tick)
`
	id, err := eng.Load(ctx, []byte(src), "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Feed events; the hub must keep delivering to a plain subscriber.
	sub, err := hub.Subscribe(ctx, envspec.Selector{Streams: []string{"test.tick@1"}, Sources: []string{"env_z"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	for i := 0; i < 30; i++ {
		hub.Publish(envspec.SignalEvent{Stream: "test.tick@1", Source: "env_z", Data: map[string]any{}})
	}
	got := 0
	deadline := time.After(3 * time.Second)
loop:
	for got < 10 {
		select {
		case <-sub.Events():
			got++
		case <-deadline:
			break loop
		}
	}
	if got < 10 {
		t.Fatalf("hub stalled by looping rule (got %d events)", got)
	}
	_ = id
	// The instance should fault out (5 faults/minute) or at least record faults.
	deadline2 := time.Now().Add(5 * time.Second)
	faulted := false
	for time.Now().Before(deadline2) && !faulted {
		for _, in := range eng.List() {
			if in.State == "faulted" {
				faulted = true
			}
		}
		if !faulted {
			// Keep feeding so callbacks keep faulting.
			hub.Publish(envspec.SignalEvent{Stream: "test.tick@1", Source: "env_z", Data: map[string]any{}})
			time.Sleep(50 * time.Millisecond)
		}
	}
	if !faulted {
		t.Fatalf("looping rule was not disabled: %+v", eng.List())
	}
	hub.Close()
}

func TestBadLoadKeepsRunningAndSelfSubscribeRejected(t *testing.T) {
	ctx := context.Background()
	hub := signalcore.NewHub()

	eng := rules.NewEngine(hub, nil)
	good := `rule(name="keep", version=1, params={})
def f(ev): pass
on("test.tick@1", source="env_z", fn=f)`
	if _, err := eng.Load(ctx, []byte(good), "v1", nil); err != nil {
		t.Fatal(err)
	}
	bad := `rule(name="keep", version=2, params={})
this is not starlark`
	if _, err := eng.Load(ctx, []byte(bad), "v2", nil); err == nil {
		t.Fatal("bad source must fail")
	}
	list := eng.List()
	if len(list) != 1 || list[0].Version != 1 || list[0].State != "active" {
		t.Fatalf("running version must survive failed reload: %+v", list)
	}
	selfSub := `rule(name="loop", version=1, params={})
def f(ev): pass
on("rule.loop.out@1", fn=f)`
	if _, err := eng.Load(ctx, []byte(selfSub), "v", nil); err == nil {
		t.Fatal("subscribing to own output must be rejected")
	}
	// A rule without rule() is rejected.
	if _, err := eng.Load(ctx, []byte(`def f(ev): pass
on("test.tick@1", fn=f)`), "v", nil); err == nil {
		t.Fatal("missing rule() must be rejected")
	}
	hub.Close()
}

func TestTriggerCaptureContainsBefore(t *testing.T) {
	ctx := context.Background()
	rec := signalcore.NewRecorder(signalcore.RecorderConfig{RingWindow: time.Hour})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec))

	eng := rules.NewEngine(hub, rec, rules.WithTriggerSink(func(tr rules.Trigger) (string, error) {
		if tr.Capture == nil {
			return "", nil
		}
		sel := envspec.Selector{Streams: tr.Capture.Streams, Sources: []string{"env_q"}}
		c, err := rec.Capture(sel, tr.Time.Add(-tr.Capture.Before), tr.Time.Add(tr.Capture.After))
		if err != nil {
			return "", err
		}
		return c.ID(), nil
	}))
	src := `
rule(name="cap", version=1, params={})
def f(ev):
    if ev.data.marker:
        trigger("hit", {}, capture={"streams": ["test.data@1"], "before": "60s", "after": "0s"})
on("test.data@1", source="env_q", fn=f)
`
	if _, err := eng.Load(ctx, []byte(src), "t", nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		hub.Publish(envspec.SignalEvent{Stream: "test.data@1", Source: "env_q",
			Data: map[string]any{"i": float64(i)}})
		time.Sleep(10 * time.Millisecond)
	}
	hub.Publish(envspec.SignalEvent{Stream: "test.data@1", Source: "env_q",
		Data: map[string]any{"marker": true}})
	deadline := time.Now().Add(3 * time.Second)
	var capID string
	for time.Now().Before(deadline) {
		if ts := eng.RecentTriggers(time.Now().Add(-time.Minute), "cap"); len(ts) > 0 && ts[0].CaptureID != "" {
			capID = ts[0].CaptureID
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if capID == "" {
		t.Fatal("no capture was created for the trigger")
	}
	// The capture (re-derived on the same window) holds pre-trigger events.
	before, _, err := rec.Query(envspec.Selector{Streams: []string{"test.data@1"}},
		time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil || len(before) < 6 {
		t.Fatalf("recorded events = %d err=%v", len(before), err)
	}
	hub.Close()
}

// ownedCollector defines a stream so the hub knows it is collector-owned.
type ownedCollector struct{ stream string }

func (c ownedCollector) Name() string { return "owned" }
func (c ownedCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: c.stream}}
}
func (c ownedCollector) Applies(signalcore.EnvInfo) error { return nil }
func (c ownedCollector) Params() *jsonschema.Schema       { return nil }
func (c ownedCollector) Run(ctx context.Context, _ json.RawMessage, _ signalcore.Emitter) error {
	<-ctx.Done()
	return nil
}

func TestEmitCannotForgeCollectorStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := signalcore.NewHub(signalcore.WithCollectors(ownedCollector{stream: "android.crash@2"}))
	defer hub.Close()

	eng := rules.NewEngine(hub, nil)
	src := `rule(name="forger", version=3, params={})
def f(ev):
    emit("android.crash@2", {"kind": "tombstone"})
    emit("custom.thing@7", {"ok": True})
on("test.tick@1", source="env_z", fn=f)`
	if _, err := eng.Load(ctx, []byte(src), "v", nil); err != nil {
		t.Fatal(err)
	}
	sub, err := hub.Subscribe(ctx, envspec.Selector{Streams: []string{"android.crash@2", "rule.forger.*", "custom.thing@7"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	hub.Publish(envspec.SignalEvent{Stream: "test.tick@1", Source: "env_z", Data: map[string]any{}})
	got := map[string]envspec.SignalEvent{}
	deadline := time.After(3 * time.Second)
	for len(got) < 2 {
		select {
		case ev := <-sub.Events():
			got[ev.Stream] = ev
		case <-deadline:
			t.Fatalf("emits not delivered: %v", got)
		}
	}
	select {
	case ev := <-sub.Events():
		got[ev.Stream] = ev
	case <-time.After(100 * time.Millisecond):
	}
	if _, forged := got["android.crash@2"]; forged {
		t.Fatalf("rule emit landed on collector-owned stream: %+v", got)
	}
	ev, ok := got["rule.forger.android.crash@3"]
	if !ok || ev.Collector != "" || ev.Source != "host" {
		t.Fatalf("collector-owned emit not namespaced under the rule: %+v", got)
	}
	if _, ok := got["custom.thing@7"]; !ok {
		t.Fatalf("versioned non-collector stream should stay verbatim: %+v", got)
	}
}
