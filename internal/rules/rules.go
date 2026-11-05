// Package rules is the Starlark signal-rule engine. Rules observe and
// announce — they emit derived signals, raise triggers and pin
// captures; they never provision, exec or call capabilities. One
// goroutine and one mailbox (4096) per instance, step+wall budgets, a
// depth-4 loop guard, hot reload, and a virtual-clock backtest
// (Engine.Test).
//
// Execution runs on script-services, never on raw Starlark plumbing:
// sources are compiled with dialect.Compile and initialised with
// prog.Init on a script.NewThread, and globals are NEVER frozen, so
// the `state` dict and top-level containers (lists/dicts/helpers)
// stay mutable across events for the instance's whole life. Callbacks run via script.Call under
// script.Limits{MaxSteps: 1_000_000, Timeout: 250ms} per event/timer
// invocation. script.Exec and script.DefaultLimits are deliberately
// not used: Exec freezes globals and is one-shot program-shaped,
// while rules are long-lived and stateful. All Go<->Starlark
// conversion goes through values.FromGo/ToGo; there is no init() and
// no mutation of global Starlark resolver flags (the dialect owns
// syntax options).
//
// Accepted language deltas from the move (documented, tested):
// while loops and top-level control flow are allowed; integral
// float64 values become Starlark ints (and Starlark ints become Go
// int64, not float64 — see the Where-matching regression test);
// []byte event data reaches rules as a base64 string via the JSON
// round-trip; the print hook carries no position. load() is rejected
// at Load/Test time: check.Analyze runs as an advisory pass (it
// always predeclares `input`, which rules do not provide, so
// dialect.Compile stays authoritative) and any entry in
// Analysis.Loads fails the load; the thread's Load hook also refuses.
//
// Known gap carried knowingly: the `state` dict size cap is not
// enforced — a raw *starlark.Dict cannot enforce one.
package rules

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/script-services/check"
	"github.com/craggy-io/script-services/dialect"
	"github.com/craggy-io/script-services/modules"
	"github.com/craggy-io/script-services/script"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// Default callback budgets (the old engine's, not script-services'
// one-shot defaults).
const (
	DefaultStepBudget = 1_000_000
	DefaultWallBudget = 250 * time.Millisecond
)

// ruleBuiltins are the predeclared names every rule sees. They are the
// check.Analyze predeclared set and the dialect.Compile predicate, so
// a rule that checks clean also compiles.
var ruleBuiltins = []string{
	"rule", "on", "every", "emit", "trigger", "log",
	"window", "ewma", "counter", "quantiles", "distinct", "dedupe",
	"state", "params", "env", "host",
}

// Trigger is one rule.triggered fact. The engine forwards it to the
// TriggerSink (manager lifecycle + captures) and keeps it in List.
type Trigger struct {
	Rule      string         `json:"rule"`
	Version   int            `json:"version"`
	Instance  string         `json:"instance"`
	Kind      string         `json:"kind"`
	Data      map[string]any `json:"data,omitempty"`
	Capture   *CaptureSpec   `json:"capture,omitempty"`
	CaptureID string         `json:"capture_id,omitempty"`
	CodeRef   string         `json:"code_ref,omitempty"`
	Time      time.Time      `json:"time"`
}

// CaptureSpec is a trigger's capture window.
type CaptureSpec struct {
	Streams []string      `json:"streams,omitempty"`
	Sources []string      `json:"sources,omitempty"`
	Before  time.Duration `json:"before,omitempty"`
	After   time.Duration `json:"after,omitempty"`
}

// TriggerSink receives triggers (the manager persists rule.triggered and
// creates captures, returning the capture ID).
type TriggerSink func(t Trigger) (captureID string, err error)

// Lookups back env.info / host.snapshot read-only built-ins.
type Lookups struct {
	EnvInfo      func(id string) (map[string]any, bool)
	HostSnapshot func() map[string]any
}

// EngineOption tunes the engine.
type EngineOption func(*Engine)

func WithTriggerSink(fn TriggerSink) EngineOption { return func(e *Engine) { e.sink = fn } }
func WithLookups(l Lookups) EngineOption          { return func(e *Engine) { e.lookups = l } }
func WithBudgets(steps uint64, wall time.Duration) EngineOption {
	return func(e *Engine) { e.stepBudget, e.wallBudget = steps, wall }
}

// Engine owns rule instances against a Hub (+ optional Recorder for Test).
type Engine struct {
	hub *signalcore.Hub
	rec *signalcore.Recorder

	sink       TriggerSink
	lookups    Lookups
	stepBudget uint64
	wallBudget time.Duration

	mu        sync.Mutex
	instances map[string]*instance
	triggers  []Trigger // recent, for rules.triggers.wait/status
}

// InstanceID identifies one (rule@version, params) instance.
type InstanceID string

// NewEngine builds an engine.
func NewEngine(h *signalcore.Hub, r *signalcore.Recorder, opts ...EngineOption) *Engine {
	e := &Engine{hub: h, rec: r, instances: map[string]*instance{},
		stepBudget: DefaultStepBudget, wallBudget: DefaultWallBudget}
	for _, o := range opts {
		o(e)
	}
	return e
}

// InstanceInfo is the List view of one rule instance.
type InstanceInfo struct {
	ID          string    `json:"id"`
	Rule        string    `json:"rule"`
	Version     int       `json:"version"`
	Description string    `json:"description,omitempty"`
	CodeRef     string    `json:"code_ref,omitempty"`
	State       string    `json:"state"` // active, faulted, unloaded
	Events      uint64    `json:"events"`
	Emits       uint64    `json:"emits"`
	Triggers    uint64    `json:"triggers"`
	Faults      uint64    `json:"faults"`
	LastTrigger time.Time `json:"last_trigger,omitempty"`
	LoadedAt    time.Time `json:"loaded_at"`
}

// Load parses, checks and activates a rule. A failed load leaves any
// running instance of the same rule untouched (hot reload, §8.3): the swap
// happens only after subscriptions resolve. A new version starts fresh.
func (e *Engine) Load(ctx context.Context, src []byte, codeRef string, params map[string]any) (InstanceID, error) {
	decl, globals, err := e.compile(src, params, time.Now)
	if err != nil {
		return "", err
	}
	inst := newInstance(e, decl, codeRef, params, time.Now)
	inst.state = globals.stateDict
	if err := inst.bind(globals); err != nil {
		return "", err
	}
	if err := inst.start(ctx); err != nil {
		inst.stop()
		return "", err
	}
	e.mu.Lock()
	old, hasOld := e.instances[inst.key()]
	e.instances[inst.key()] = inst
	e.mu.Unlock()
	if hasOld && old != inst {
		old.stop()
	}
	e.event("rule.loaded", map[string]any{"rule": decl.Name, "version": decl.Version,
		"instance": string(inst.id), "code_ref": codeRef})
	return inst.id, nil
}

// Unload stops an instance. Idempotent.
func (e *Engine) Unload(id InstanceID) error {
	e.mu.Lock()
	var found *instance
	for k, in := range e.instances {
		if in.id == id {
			found = in
			delete(e.instances, k)
		}
	}
	e.mu.Unlock()
	if found == nil {
		return nil
	}
	found.stop()
	e.event("rule.unloaded", map[string]any{"rule": found.decl.Name, "instance": string(id)})
	return nil
}

// List returns instance summaries.
func (e *Engine) List() []InstanceInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]InstanceInfo, 0, len(e.instances))
	for _, in := range e.instances {
		out = append(out, in.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}

// RecentTriggers returns triggers after a time (rules.triggers.wait base).
func (e *Engine) RecentTriggers(since time.Time, rule string) []Trigger {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Trigger
	for _, t := range e.triggers {
		if t.Time.After(since) && (rule == "" || t.Rule == rule) {
			out = append(out, t)
		}
	}
	return out
}

// WaitTriggers blocks until a trigger for rule arrives or ctx/timeout ends.
func (e *Engine) WaitTriggers(ctx context.Context, rule string, timeout time.Duration) ([]Trigger, error) {
	deadline := time.Now().Add(timeout)
	since := time.Now().Add(-time.Millisecond)
	for {
		if ts := e.RecentTriggers(since, rule); len(ts) > 0 {
			return ts, nil
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (e *Engine) recordTrigger(t Trigger) Trigger {
	if e.sink != nil {
		if cid, err := e.sink(t); err == nil {
			t.CaptureID = cid
		}
	}
	e.mu.Lock()
	e.triggers = append(e.triggers, t)
	if len(e.triggers) > 1024 {
		e.triggers = e.triggers[len(e.triggers)-1024:]
	}
	e.mu.Unlock()
	return t
}

func (e *Engine) event(kind string, data map[string]any) {
	// rule.loaded/faulted reach lifecycle through the trigger sink's
	// engine-level callback if provided as a TriggerSink wrapper; the
	// plain path records them as synthetic triggers-free events via hub.
	if e.hub != nil {
		e.hub.Publish(envspec.SignalEvent{Stream: "rule.events@1", Source: "host",
			Data: mergeMap(data, map[string]any{"kind": kind})})
	}
	if e.sink != nil && kind == "rule.faulted" {
		_, _ = e.sink(Trigger{Kind: kind, Data: data, Time: time.Now()})
	}
}

func mergeMap(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// --- Compilation ---

type ruleDecl struct {
	Name        string
	Version     int
	Description string
	Params      map[string]string // name → type name
}

type compileState struct {
	decl      *ruleDecl
	err       error
	params    map[string]any
	subs      []subSpec
	timers    []timerSpec
	nowFn     func() time.Time
	inst      *instance // nil during first compile pass
	recorder  *testRun  // non-nil in Test
	stateDict *starlark.Dict
}

type subSpec struct {
	stream string
	source string
	where  map[string]any
	params json.RawMessage
	fn     starlark.Value
}

type timerSpec struct {
	every time.Duration
	fn    starlark.Value
}

// compile runs the source once to collect the declaration, subscriptions
// and timers. Callbacks are not invoked during compilation. The module
// globals are left unfrozen: callbacks close over them, so top-level
// bindings and the `state` dict stay mutable across events.
func (e *Engine) compile(src []byte, params map[string]any, nowFn func() time.Time) (*ruleDecl, *compileState, error) {
	// Advisory static pass. check.Analyze always predeclares `input`,
	// which rules do not provide, so its diagnostics do not gate the
	// load — dialect.Compile below is authoritative. The one hard
	// rule taken from it: rules may not load() anything.
	analysis := check.Analyze(check.Env{Predeclared: ruleBuiltins}, "rule.star", src)
	if len(analysis.Loads) > 0 {
		return nil, nil, fmt.Errorf("rules: load() is not permitted in rules (found load of %q)", analysis.Loads[0])
	}

	cs := &compileState{params: params, nowFn: nowFn, stateDict: starlark.NewDict(64)}
	predeclared := e.makeGlobals(cs, nil)
	prog, err := dialect.Compile("rule.star", src, predeclared.Has)
	if err != nil {
		return nil, nil, fmt.Errorf("rules: load: %w", err)
	}
	thread := script.NewThread("rule-compile", script.Limits{MaxSteps: 1 << 22}, nil)
	thread.Load = func(*starlark.Thread, string) (starlark.StringDict, error) {
		return nil, modules.ErrNoSource
	}
	_, err = script.Run(context.Background(), script.Limits{Timeout: 5 * time.Second}, thread,
		func() (starlark.Value, error) {
			// Init only — never Freeze the globals (see package doc).
			globals, err := prog.Init(thread, predeclared)
			_ = globals
			return nil, err
		})
	if err != nil {
		return nil, nil, fmt.Errorf("rules: load: %w", script.Failure(context.Background(), "rule.star", err, false, thread.ExecutionSteps()))
	}
	if cs.err != nil {
		return nil, nil, cs.err
	}
	if cs.decl == nil {
		return nil, nil, fmt.Errorf("rules: source must call rule(name=..., version=..., params={...}) exactly once")
	}
	return cs.decl, cs, nil
}
