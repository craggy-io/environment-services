package rules

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/script-services/script"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

const mailboxCap = 4096

type instance struct {
	engine  *Engine
	id      InstanceID
	decl    *ruleDecl
	codeRef string
	params  map[string]any
	cs      *compileState

	state    *starlark.Dict // `state` built-in, capped
	nowFn    func() time.Time
	subs     []*signalcore.Subscription
	cancel   context.CancelFunc
	mail     chan func()
	done     chan struct{}
	doneOnce sync.Once

	events   atomic.Uint64
	emits    atomic.Uint64
	triggers atomic.Uint64
	faults   atomic.Uint64

	mu          sync.Mutex
	status      string
	loadedAt    time.Time
	lastTrigger time.Time
	faultTimes  []time.Time
	trigDedupe  map[string]time.Time
}

func newInstance(e *Engine, decl *ruleDecl, codeRef string, params map[string]any, nowFn func() time.Time) *instance {
	return &instance{
		engine: e, id: InstanceID(envspec.NewID("rinst_")), decl: decl, codeRef: codeRef,
		params: params, state: starlark.NewDict(64), nowFn: nowFn,
		mail: make(chan func(), mailboxCap), done: make(chan struct{}),
		status: "active", loadedAt: nowFn(), trigDedupe: map[string]time.Time{},
	}
}

func (in *instance) key() string {
	return fmt.Sprintf("%s@%d", in.decl.Name, in.decl.Version)
}

func (in *instance) info() InstanceInfo {
	in.mu.Lock()
	defer in.mu.Unlock()
	return InstanceInfo{ID: string(in.id), Rule: in.decl.Name, Version: in.decl.Version,
		Description: in.decl.Description, CodeRef: in.codeRef, State: in.status,
		Events: in.events.Load(), Emits: in.emits.Load(), Triggers: in.triggers.Load(),
		Faults: in.faults.Load(), LastTrigger: in.lastTrigger, LoadedAt: in.loadedAt}
}

func (in *instance) bind(cs *compileState) error {
	cs.inst = in
	in.cs = cs
	// A rule can't subscribe to its own outputs (§8.3).
	own := "rule." + in.decl.Name + "."
	for _, s := range cs.subs {
		if len(s.stream) > len(own) && s.stream[:len(own)] == own {
			return fmt.Errorf("rules: %s cannot subscribe to its own output %s", in.decl.Name, s.stream)
		}
		for _, t := range cs.timers {
			if t.every < 250*time.Millisecond {
				return fmt.Errorf("rules: every() minimum interval is 250ms, got %s", t.every)
			}
		}
	}
	return nil
}

func (in *instance) start(parent context.Context) error {
	if in.engine.hub == nil {
		return fmt.Errorf("rules: engine has no hub")
	}
	ctx, cancel := context.WithCancel(parent)
	in.cancel = cancel
	// Resolve subscriptions first; Applies errors fail Load (§8.2).
	for _, s := range in.cs.subs {
		sel, err := selectorFor(s)
		if err != nil {
			cancel()
			return err
		}
		sub, err := in.engine.hub.Subscribe(ctx, sel, signalcore.WithBuffer(1024))
		if err != nil {
			cancel()
			return fmt.Errorf("rules: on(%s): %w", s.stream, err)
		}
		in.subs = append(in.subs, sub)
		s := s
		go func() {
			for ev := range sub.Events() {
				in.enqueueEvent(s, ev)
			}
		}()
	}
	go in.run(ctx)
	for _, t := range in.cs.timers {
		t := t
		go func() {
			tick := time.NewTicker(t.every)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					in.enqueue(func() { in.callTimer(t) })
				}
			}
		}()
	}
	return nil
}

func selectorFor(s subSpec) (envspec.Selector, error) {
	sel := envspec.Selector{Streams: []string{s.stream}, Params: s.params}
	if s.source != "" {
		sel.Sources = []string{s.source}
	}
	if len(s.where) > 0 {
		f, err := signalcore.Where(s.where)
		if err != nil {
			return sel, err
		}
		sel.Where = f
	}
	return sel, nil
}

func (in *instance) enqueueEvent(s subSpec, ev envspec.SignalEvent) {
	if ev.Depth >= 4 {
		return // loop guard, counted at the hub too
	}
	in.events.Add(1)
	in.enqueue(func() { in.callEvent(s, ev) })
}

func (in *instance) enqueue(fn func()) {
	select {
	case in.mail <- fn:
	default:
		// Mailbox full: signals are lossy; the hub counts its own drops.
		in.faults.Add(0)
		select {
		case <-in.mail:
		default:
		}
		select {
		case in.mail <- fn:
		default:
		}
	}
}

func (in *instance) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-in.done:
			return
		case fn := <-in.mail:
			fn()
		}
	}
}

func (in *instance) stop() {
	in.doneOnce.Do(func() {
		close(in.done)
		if in.cancel != nil {
			in.cancel()
		}
		for _, s := range in.subs {
			s.Close()
		}
		in.mu.Lock()
		if in.status == "active" {
			in.status = "unloaded"
		}
		in.mu.Unlock()
	})
}

// call runs one Starlark callback under the per-invocation budgets
// (script.Limits{1_000_000 steps, 250ms} by default) via script.Call;
// faults are counted and 5 in a minute disable the instance with
// rule.faulted. The mailbox goroutine is the only caller, so
// callbacks for one instance never run concurrently.
func (in *instance) call(fnName string, fn starlark.Value, args starlark.Tuple) {
	if fn == nil {
		return
	}
	callable, ok := fn.(starlark.Callable)
	if !ok {
		in.fault(fnName, fmt.Errorf("%s is not callable", fn.Type()))
		return
	}
	lim := script.Limits{MaxSteps: in.engine.stepBudget, Timeout: in.engine.wallBudget}
	_, _, err := script.Call(context.Background(), lim, in.decl.Name, callable, args, nil)
	if err != nil {
		in.fault(fnName, err)
	}
}

func (in *instance) fault(where string, err error) {
	in.faults.Add(1)
	now := in.nowFn()
	in.mu.Lock()
	cutoff := now.Add(-time.Minute)
	kept := in.faultTimes[:0]
	for _, t := range in.faultTimes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	in.faultTimes = append(kept, now)
	disable := len(in.faultTimes) >= 5
	if disable {
		in.status = "faulted"
	}
	in.mu.Unlock()
	if disable {
		in.engine.event("rule.faulted", map[string]any{
			"rule": in.decl.Name, "version": in.decl.Version, "instance": string(in.id),
			"code_ref": in.codeRef, "where": where, "error": err.Error(),
		})
		go in.stop()
	}
}

func (in *instance) callEvent(s subSpec, ev envspec.SignalEvent) {
	in.call("on:"+s.stream, s.fn, starlark.Tuple{eventToStarlark(ev)})
}

func (in *instance) callTimer(t timerSpec) {
	in.call("every", t.fn, starlark.Tuple{})
}

// eventToStarlark builds the `ev` value: ev.stream, ev.source, ev.time
// (epoch seconds float), ev.data (dict of fields + attribute access).
func eventToStarlark(ev envspec.SignalEvent) starlark.Value {
	d := starlark.NewDict(6)
	_ = d.SetKey(starlark.String("stream"), starlark.String(ev.Stream))
	_ = d.SetKey(starlark.String("source"), starlark.String(ev.Source))
	_ = d.SetKey(starlark.String("time"), starlark.Float(float64(ev.Time.UnixNano())/1e9))
	_ = d.SetKey(starlark.String("seq"), starlark.MakeInt64(int64(ev.Seq)))
	_ = d.SetKey(starlark.String("data"), &dataValue{ev: ev})
	return &eventValue{dict: d, ev: ev}
}

type eventValue struct {
	dict *starlark.Dict
	ev   envspec.SignalEvent
}

func (e *eventValue) String() string        { return "event(" + e.ev.Stream + ")" }
func (e *eventValue) Type() string          { return "event" }
func (e *eventValue) Freeze()               {}
func (e *eventValue) Truth() starlark.Bool  { return starlark.True }
func (e *eventValue) Hash() (uint32, error) { return 0, fmt.Errorf("event is not hashable") }
func (e *eventValue) Attr(name string) (starlark.Value, error) {
	v, found, err := e.dict.Get(starlark.String(name))
	if err != nil || !found {
		return nil, starlark.NoSuchAttrError(name)
	}
	return v, nil
}
func (e *eventValue) AttrNames() []string { return []string{"stream", "source", "time", "seq", "data"} }

// dataValue exposes event fields as attributes (ev.data.bytes) and items.
type dataValue struct{ ev envspec.SignalEvent }

func (d *dataValue) String() string        { return "data" }
func (d *dataValue) Type() string          { return "data" }
func (d *dataValue) Freeze()               {}
func (d *dataValue) Truth() starlark.Bool  { return starlark.True }
func (d *dataValue) Hash() (uint32, error) { return 0, fmt.Errorf("data is not hashable") }
func (d *dataValue) Attr(name string) (starlark.Value, error) {
	v, ok := signalcore.DataField(d.ev, name)
	if !ok {
		return starlark.None, nil
	}
	return fromGo(v), nil
}
func (d *dataValue) AttrNames() []string { return nil }
func (d *dataValue) Get(k starlark.Value) (starlark.Value, bool, error) {
	ks, ok := k.(starlark.String)
	if !ok {
		return nil, false, nil
	}
	v, ok := signalcore.DataField(d.ev, string(ks))
	if !ok {
		return starlark.None, false, nil
	}
	return fromGo(v), true, nil
}

func unquote(v starlark.Value) string {
	if s, ok := v.(starlark.String); ok {
		return string(s)
	}
	return v.String()
}
