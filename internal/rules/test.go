package rules

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// TestResult is a backtest outcome: emits and triggers on a virtual clock,
// nothing published (§8.3).
type TestResult struct {
	Rule     string                `json:"rule"`
	Version  int                   `json:"version"`
	Emits    []envspec.SignalEvent `json:"emits"`
	Triggers []Trigger             `json:"triggers"`
	Faults   uint64                `json:"faults"`
}

type testRun struct {
	emits    []envspec.SignalEvent
	triggers []Trigger
}

// Test replays recorded events through a rule on a virtual clock. Timer
// callbacks fire at their intervals between event timestamps, so backtest
// output equals the live run on the same recording (phase 4 acceptance).
func (e *Engine) Test(ctx context.Context, src []byte, params map[string]any, events []envspec.SignalEvent) (TestResult, error) {
	vc := &virtualClock{}
	if len(events) > 0 {
		vc.set(events[0].Time)
	} else {
		vc.set(time.Unix(0, 0))
	}
	decl, cs, err := e.compile(src, params, vc.now)
	if err != nil {
		return TestResult{}, err
	}
	inst := newInstance(e, decl, "test", params, vc.now)
	inst.state = cs.stateDict
	if err := inst.bind(cs); err != nil {
		return TestResult{}, err
	}
	run := &testRun{}
	cs.recorder = run

	ordered := append([]envspec.SignalEvent(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Time.Before(ordered[j].Time) })

	// Timer schedule seeded from the first event (or epoch).
	start := vc.now()
	type sched struct {
		spec timerSpec
		next time.Time
	}
	var timers []*sched
	for _, t := range cs.timers {
		timers = append(timers, &sched{spec: t, next: start.Add(t.every)})
	}
	fireTimersUntil := func(until time.Time) {
		for {
			var due *sched
			for _, s := range timers {
				if !s.next.After(until) && (due == nil || s.next.Before(due.next)) {
					due = s
				}
			}
			if due == nil {
				return
			}
			vc.set(due.next)
			inst.callTimer(due.spec)
			due.next = due.next.Add(due.spec.every)
			if ctx.Err() != nil {
				return
			}
		}
	}
	for _, ev := range ordered {
		fireTimersUntil(ev.Time)
		vc.set(ev.Time)
		for _, s := range cs.subs {
			sel, err := selectorFor(s)
			if err != nil {
				continue
			}
			if signalcore.SelectorMatch(sel, ev) {
				inst.callEvent(s, ev)
			}
		}
		if ctx.Err() != nil {
			return TestResult{}, ctx.Err()
		}
	}
	// Let trailing timers fire up to one interval past the last event so
	// window checks at range end run (matches querying right after).
	if len(ordered) > 0 && len(timers) > 0 {
		end := ordered[len(ordered)-1].Time
		fireTimersUntil(end.Add(timers[0].spec.every))
	}
	return TestResult{Rule: decl.Name, Version: decl.Version,
		Emits: run.emits, Triggers: run.triggers, Faults: inst.faults.Load()}, nil
}

type virtualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *virtualClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *virtualClock) set(t time.Time) {
	c.mu.Lock()
	if t.After(c.t) {
		c.t = t
	}
	c.mu.Unlock()
}
