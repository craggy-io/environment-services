package rules_test

// Stage S7 regressions for the script-services move: state mutation
// across events, the 1M/250ms budget fault, load() rejection, int
// preservation through values.FromGo/ToGo, and values.ToGo int64 in
// signal Where matching.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/rules"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

func waitTrigger(t *testing.T, eng *rules.Engine, rule string) rules.Trigger {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ts := eng.RecentTriggers(time.Now().Add(-time.Minute), rule); len(ts) > 0 {
			return ts[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no trigger from %s (instances: %+v)", rule, eng.List())
	return rules.Trigger{}
}

// Globals are never frozen: `state` and top-level bindings mutated by
// one event are visible to the next. This is the property script.Exec
// (which freezes) could never provide.
func TestStateAndTopLevelMutationAcrossEvents(t *testing.T) {
	ctx := context.Background()
	hub := signalcore.NewHub()
	defer hub.Close()
	eng := rules.NewEngine(hub, nil)
	src := `rule(name="counter-rule", version=1, params={})
seen = []
box = {"total": 0}
def f(ev):
    state["n"] = state.get("n", 0) + 1
    seen.append(ev.data.v)
    box["total"] = box["total"] + ev.data.v
    if state["n"] == 3:
        trigger("third", {"count": state["n"], "total": box["total"], "seen": seen})
def g(ev):
    trigger("later", {"count": state.get("n", 0)})
on("test.n@1", fn=f)
on("test.ask@1", fn=g)`
	if _, err := eng.Load(ctx, []byte(src), "t", nil); err != nil {
		t.Fatal(err)
	}
	for _, v := range []int{10, 20, 12} {
		hub.Publish(envspec.SignalEvent{Stream: "test.n@1", Source: "host",
			Data: map[string]any{"v": v}})
	}
	tr := waitTrigger(t, eng, "counter-rule")
	if tr.Kind != "third" {
		t.Fatalf("trigger kind = %q, want third", tr.Kind)
	}
	if got := tr.Data["count"]; got != int64(3) {
		t.Fatalf("count = %#v (%T), want int64(3)", got, got)
	}
	if got := tr.Data["total"]; got != int64(42) {
		t.Fatalf("total = %#v (%T), want int64(42) — top-level binding lost across events", got, got)
	}
	seen, ok := tr.Data["seen"].([]any)
	if !ok || len(seen) != 3 || seen[0] != int64(10) || seen[2] != int64(12) {
		t.Fatalf("seen = %#v, want [10 20 12] as int64 — top-level list mutation lost", tr.Data["seen"])
	}
	// A later, different callback sees the same state dict.
	hub.Publish(envspec.SignalEvent{Stream: "test.ask@1", Source: "host", Data: map[string]any{}})
	deadline := time.Now().Add(3 * time.Second)
	for {
		found := false
		for _, ts := range eng.RecentTriggers(time.Now().Add(-time.Minute), "counter-rule") {
			if ts.Kind == "later" {
				found = true
				if ts.Data["count"] != int64(3) {
					t.Fatalf("later count = %#v, want int64(3)", ts.Data["count"])
				}
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second callback never saw shared state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The default budgets are the old ones — 1M steps / 250ms per
// invocation via script.Call, not script-services' 5M/10min
// DefaultLimits. A runaway callback faults, and 5 faults in a minute
// disable the instance.
func TestDefaultBudgetFaultDisables(t *testing.T) {
	if rules.DefaultStepBudget != 1_000_000 || rules.DefaultWallBudget != 250*time.Millisecond {
		t.Fatalf("defaults = %d/%s, want 1000000/250ms", rules.DefaultStepBudget, rules.DefaultWallBudget)
	}
	ctx := context.Background()
	hub := signalcore.NewHub()
	defer hub.Close()
	eng := rules.NewEngine(hub, nil) // default budgets
	src := `rule(name="runaway", version=1, params={})
def f(ev):
    x = 0
    for i in range(1000000000):
        x += i
on("test.tick@1", fn=f)`
	if _, err := eng.Load(ctx, []byte(src), "t", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(6 * time.Second)
	faulted := false
	for time.Now().Before(deadline) && !faulted {
		hub.Publish(envspec.SignalEvent{Stream: "test.tick@1", Source: "host", Data: map[string]any{}})
		for _, in := range eng.List() {
			if in.State == "faulted" && in.Faults >= 5 {
				faulted = true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !faulted {
		t.Fatalf("runaway rule not fault-disabled under default budgets: %+v", eng.List())
	}
}

// load() is rejected at load time (check.Analyze advisory + hard
// Loads rule); dialect.Compile remains authoritative for everything
// else.
func TestLoadRejected(t *testing.T) {
	ctx := context.Background()
	hub := signalcore.NewHub()
	defer hub.Close()
	eng := rules.NewEngine(hub, nil)
	src := `load("helpers.star", "helper")
rule(name="loader", version=1, params={})
def f(ev): pass
on("test.tick@1", fn=f)`
	if _, err := eng.Load(ctx, []byte(src), "t", nil); err == nil || !strings.Contains(err.Error(), "load()") {
		t.Fatalf("Load with load(): err = %v, want a load() rejection", err)
	}
	if _, err := eng.Test(ctx, []byte(src), nil, nil); err == nil || !strings.Contains(err.Error(), "load()") {
		t.Fatalf("Test with load(): err = %v, want a load() rejection", err)
	}
	if len(eng.List()) != 0 {
		t.Fatalf("rejected rule left an instance: %+v", eng.List())
	}
	// No global resolver mutation survives in this package: loading a
	// normal rule right after must still work in the same process.
	good := `rule(name="plain", version=1, params={})
def f(ev): pass
on("test.tick@1", fn=f)`
	if _, err := eng.Load(ctx, []byte(good), "t", nil); err != nil {
		t.Fatalf("plain rule after rejected load: %v", err)
	}
}

// Ints survive the values round-trip: Starlark ints become Go int64
// (the old local converter made float64), and integral event values
// reach callbacks as Starlark ints.
func TestIntPreservation(t *testing.T) {
	ctx := context.Background()
	eng := rules.NewEngine(signalcore.NewHub(), nil)
	src := `rule(name="ints", version=1, params={})
def f(ev):
    emit("out", {"sum": ev.data.a + ev.data.b, "a": ev.data.a})
    trigger("got", {"a": ev.data.a, "ratio": ev.data.r})
on("test.ints@1", fn=f)`
	evs := []envspec.SignalEvent{
		{Stream: "test.ints@1", Source: "host", Time: time.Unix(1700000000, 0),
			Data: map[string]any{"a": 40, "b": int64(2), "r": 1.5}},
	}
	res, err := eng.Test(ctx, []byte(src), nil, evs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Triggers) != 1 || len(res.Emits) != 1 {
		t.Fatalf("triggers=%d emits=%d, want 1/1 (%+v)", len(res.Triggers), len(res.Emits), res)
	}
	if got := res.Triggers[0].Data["a"]; got != int64(40) {
		t.Fatalf("a = %#v (%T), want int64(40)", got, got)
	}
	if got := res.Triggers[0].Data["ratio"]; got != 1.5 {
		t.Fatalf("ratio = %#v (%T), want float64(1.5)", got, got)
	}
	data, ok := res.Emits[0].Data.(map[string]any)
	if !ok {
		t.Fatalf("emit data = %#v", res.Emits[0].Data)
	}
	if got := data["sum"]; got != int64(42) {
		t.Fatalf("sum = %#v (%T), want int64(42): int + int must stay int", got, got)
	}
}

// Regression: a where={"code": 7} predicate is built from values.ToGo output
// (int64 now, float64 under the old converter). It must still match
// events whose data carries float64(7) (JSON-shaped payloads) and
// int64(7) alike, through both the live subscription and backtest.
func TestWhereInt64MatchesNumericData(t *testing.T) {
	// Direct: an int64 predicate value against both data shapes.
	f, err := signalcore.Where(map[string]any{"code": int64(7)})
	if err != nil {
		t.Fatal(err)
	}
	sel := envspec.Selector{Streams: []string{"test.codes@1"}, Where: f}
	for name, data := range map[string]any{
		"float64": float64(7), "int64": int64(7), "int": 7,
	} {
		ev := envspec.SignalEvent{Stream: "test.codes@1", Data: map[string]any{"code": data}}
		if !signalcore.SelectorMatch(sel, ev) {
			t.Errorf("int64(7) predicate did not match %s data", name)
		}
	}
	ev8 := envspec.SignalEvent{Stream: "test.codes@1", Data: map[string]any{"code": float64(8)}}
	if signalcore.SelectorMatch(sel, ev8) {
		t.Error("int64(7) predicate matched code 8")
	}

	// Through a rule: where comes from a Starlark int literal.
	ctx := context.Background()
	eng := rules.NewEngine(signalcore.NewHub(), nil)
	src := `rule(name="codes", version=1, params={})
def f(ev):
    trigger("seven", {"code": ev.data.code})
on("test.codes@1", where={"code": 7}, fn=f)`
	base := time.Unix(1700000000, 0)
	evs := []envspec.SignalEvent{
		{Stream: "test.codes@1", Source: "host", Time: base, Data: map[string]any{"code": float64(7)}},
		{Stream: "test.codes@1", Source: "host", Time: base.Add(time.Second), Data: map[string]any{"code": int64(7)}},
		{Stream: "test.codes@1", Source: "host", Time: base.Add(2 * time.Second), Data: map[string]any{"code": float64(8)}},
	}
	res, err := eng.Test(ctx, []byte(src), nil, evs)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Triggers) != 2 {
		t.Fatalf("triggers = %d (%+v), want 2 (float64(7) and int64(7))", len(res.Triggers), res.Triggers)
	}
}

// Hot reload: loading the same name@version replaces the instance;
// Unload is idempotent.
func TestHotReloadReplaceAndUnload(t *testing.T) {
	ctx := context.Background()
	hub := signalcore.NewHub()
	defer hub.Close()
	eng := rules.NewEngine(hub, nil)
	mk := func(kind string) string {
		return `rule(name="hot", version=1, params={})
def f(ev):
    trigger("` + kind + `", {})
on("test.hot@1", fn=f)`
	}
	id1, err := eng.Load(ctx, []byte(mk("one")), "v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := eng.Load(ctx, []byte(mk("two")), "v2", nil)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("reload kept the same instance ID")
	}
	if list := eng.List(); len(list) != 1 || list[0].ID != string(id2) {
		t.Fatalf("after reload: %+v, want only the new instance", list)
	}
	hub.Publish(envspec.SignalEvent{Stream: "test.hot@1", Source: "host", Data: map[string]any{}})
	if tr := waitTrigger(t, eng, "hot"); tr.Kind != "two" {
		t.Fatalf("trigger kind = %q, want two (new instance)", tr.Kind)
	}
	if err := eng.Unload(id2); err != nil {
		t.Fatal(err)
	}
	if err := eng.Unload(id2); err != nil {
		t.Fatalf("second Unload not idempotent: %v", err)
	}
	if len(eng.List()) != 0 {
		t.Fatalf("instances after unload: %+v", eng.List())
	}
}

// Depth-4 loop guard: an event already at derivation depth 4 is
// dropped before it reaches a callback.
func TestDepthGuardDropsDerivedEvents(t *testing.T) {
	ctx := context.Background()
	hub := signalcore.NewHub()
	defer hub.Close()
	eng := rules.NewEngine(hub, nil)
	src := `rule(name="depthy", version=1, params={})
def f(ev):
    trigger("saw", {})
on("test.deep@1", fn=f)`
	if _, err := eng.Load(ctx, []byte(src), "t", nil); err != nil {
		t.Fatal(err)
	}
	hub.Publish(envspec.SignalEvent{Stream: "test.deep@1", Source: "host", Depth: 4, Data: map[string]any{}})
	time.Sleep(150 * time.Millisecond)
	if ts := eng.RecentTriggers(time.Now().Add(-time.Minute), "depthy"); len(ts) != 0 {
		t.Fatalf("depth-4 event reached the callback: %+v", ts)
	}
	hub.Publish(envspec.SignalEvent{Stream: "test.deep@1", Source: "host", Depth: 3, Data: map[string]any{}})
	waitTrigger(t, eng, "depthy")
}

// Accepted dialect deltas: while loops and top-level control flow,
// rejected by the old ExecFileOptions dialect, are allowed now.
func TestDialectAllowsWhileAndTopLevelFlow(t *testing.T) {
	ctx := context.Background()
	eng := rules.NewEngine(signalcore.NewHub(), nil)
	src := `rule(name="dialecty", version=1, params={})
acc = []
for i in range(3):
    acc.append(i)
if len(acc) == 3:
    acc.append(99)
def f(ev):
    n = 0
    total = 0
    for v in acc:
        total += v
    while n < 4:
        n += 1
    trigger("done", {"n": n, "total": total})
on("test.d@1", fn=f)`
	res, err := eng.Test(ctx, []byte(src), nil, []envspec.SignalEvent{
		{Stream: "test.d@1", Source: "host", Time: time.Unix(1700000000, 0), Data: map[string]any{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Triggers) != 1 {
		t.Fatalf("triggers = %+v", res.Triggers)
	}
	if res.Triggers[0].Data["n"] != int64(4) || res.Triggers[0].Data["total"] != int64(102) {
		t.Fatalf("trigger data = %+v, want n=4 total=102", res.Triggers[0].Data)
	}
}
