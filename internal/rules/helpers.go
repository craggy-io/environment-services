package rules

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"go.starlark.net/starlark"
)

// Go-backed stateful helpers (§8.2): hot-path math never runs in the
// interpreter. All take times from the instance clock so backtests on a
// virtual clock match live runs.

// Window is a rolling time window over (time, value) samples.
type Window struct {
	mu     sync.Mutex
	dur    time.Duration
	events []winEv
	nowFn  func() time.Time
}

type winEv struct {
	t time.Time
	v float64
}

func (w *Window) pruneLocked(now time.Time) {
	cutoff := now.Add(-w.dur)
	i := 0
	for i < len(w.events) && w.events[i].t.Before(cutoff) {
		i++
	}
	w.events = w.events[i:]
}

// Add records a sample.
func (w *Window) Add(t time.Time, v float64) {
	w.mu.Lock()
	w.events = append(w.events, winEv{t, v})
	w.pruneLocked(w.nowFn())
	w.mu.Unlock()
}

func (w *Window) values() []float64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(w.nowFn())
	out := make([]float64, len(w.events))
	for i, e := range w.events {
		out[i] = e.v
	}
	return out
}

// Sum, Count, Mean, Max, Rate expose the window aggregates.
func (w *Window) Sum() float64 {
	s := 0.0
	for _, v := range w.values() {
		s += v
	}
	return s
}
func (w *Window) Count() int { return len(w.values()) }
func (w *Window) Mean() float64 {
	vs := w.values()
	if len(vs) == 0 {
		return 0
	}
	return w.Sum() / float64(len(vs))
}
func (w *Window) Max() float64 {
	m := math.Inf(-1)
	for _, v := range w.values() {
		if v > m {
			m = v
		}
	}
	if math.IsInf(m, -1) {
		return 0
	}
	return m
}
func (w *Window) Rate() float64 { return w.Sum() / w.dur.Seconds() }

// EWMA is an exponentially weighted moving average by half-life.
type EWMA struct {
	mu       sync.Mutex
	halfLife time.Duration
	value    float64
	last     time.Time
	seen     bool
	nowFn    func() time.Time
}

// Observe folds a value in.
func (e *EWMA) Observe(v float64) { e.ObserveAt(e.nowFn(), v) }

// ObserveAt folds a value at a timestamp.
func (e *EWMA) ObserveAt(t time.Time, v float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.seen {
		e.value, e.seen, e.last = v, true, t
		return
	}
	dt := t.Sub(e.last).Seconds()
	if dt < 0 {
		dt = 0
	}
	alpha := 1 - math.Pow(0.5, dt/e.halfLife.Seconds())
	if e.halfLife <= 0 {
		alpha = 1
	}
	e.value = alpha*v + (1-alpha)*e.value
	e.last = t
}

// Value returns the current average.
func (e *EWMA) Value() float64 { e.mu.Lock(); defer e.mu.Unlock(); return e.value }

// Counter is a monotonic counter with a windowed rate.
type Counter struct {
	mu    sync.Mutex
	total float64
	hits  []time.Time
	nowFn func() time.Time
}

// Inc bumps the counter.
func (c *Counter) Inc(v float64) {
	c.mu.Lock()
	c.total += v
	c.hits = append(c.hits, c.nowFn())
	c.mu.Unlock()
}

// Total since creation.
func (c *Counter) Total() float64 { c.mu.Lock(); defer c.mu.Unlock(); return c.total }

// Quantiles computes windowed quantiles on demand.
type Quantiles struct{ w *Window }

// Quantile returns the q-quantile (0..1) of window samples.
func (qt *Quantiles) Quantile(q float64) float64 {
	vs := qt.w.values()
	if len(vs) == 0 {
		return 0
	}
	sort.Float64s(vs)
	if q <= 0 {
		return vs[0]
	}
	if q >= 1 {
		return vs[len(vs)-1]
	}
	rank := q * float64(len(vs)-1)
	lo := int(math.Floor(rank))
	frac := rank - float64(lo)
	if lo+1 >= len(vs) {
		return vs[lo]
	}
	return vs[lo]*(1-frac) + vs[lo+1]*frac
}

// Distinct counts distinct keys seen in a window (bounded).
type Distinct struct {
	mu    sync.Mutex
	dur   time.Duration
	seen  map[string]time.Time
	nowFn func() time.Time
	Cap   int // default 10000
}

// Add records a key and returns whether it was new in the window.
func (d *Distinct) Add(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.nowFn()
	for k, t := range d.seen {
		if now.Sub(t) > d.dur {
			delete(d.seen, k)
		}
	}
	_, existed := d.seen[key]
	cap := d.Cap
	if cap <= 0 {
		cap = 10000
	}
	if !existed && len(d.seen) < cap {
		d.seen[key] = now
	}
	return !existed
}

// Count of distinct keys in the window.
func (d *Distinct) Count() int { d.mu.Lock(); defer d.mu.Unlock(); return len(d.seen) }

// Dedupe suppresses repeats within a duration.
type Dedupe struct {
	mu    sync.Mutex
	dur   time.Duration
	last  map[string]time.Time
	nowFn func() time.Time
}

// Allow reports whether key passes (and records it when it does).
func (d *Dedupe) Allow(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.nowFn()
	if t, ok := d.last[key]; ok && now.Sub(t) < d.dur {
		return false
	}
	d.last[key] = now
	return true
}

// --- Starlark wrappers ---

type slWindow struct{ w *Window }

func (s *slWindow) String() string       { return "window()" }
func (s *slWindow) Type() string         { return "window" }
func (s *slWindow) Freeze()              {}
func (s *slWindow) Truth() starlark.Bool { return starlark.True }
func (s *slWindow) Hash() (uint32, error) {
	return 0, fmt.Errorf("window is not hashable")
}
func (s *slWindow) Attr(name string) (starlark.Value, error) {
	switch name {
	case "add":
		return starlark.NewBuiltin("add", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) < 1 || len(args) > 2 {
				return nil, fmt.Errorf("window.add(time, value) or window.add(value)")
			}
			t := s.w.nowFn()
			val := args[0]
			if len(args) == 2 {
				if tt, err := asFloat(args[0]); err == nil {
					t = time.Unix(int64(tt), 0)
				}
				val = args[1]
			}
			f, err := asFloat(val)
			if err != nil {
				return nil, err
			}
			s.w.Add(t, f)
			return starlark.None, nil
		}), nil
	case "sum":
		return builtinFloat("sum", s.w.Sum), nil
	case "count":
		return builtinFloat("count", func() float64 { return float64(s.w.Count()) }), nil
	case "mean":
		return builtinFloat("mean", s.w.Mean), nil
	case "max":
		return builtinFloat("max", s.w.Max), nil
	case "rate":
		return builtinFloat("rate", s.w.Rate), nil
	}
	return nil, starlark.NoSuchAttrError(name)
}
func (s *slWindow) AttrNames() []string {
	return []string{"add", "sum", "count", "mean", "max", "rate"}
}

func builtinFloat(name string, fn func() float64) starlark.Value {
	return starlark.NewBuiltin(name, func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		return starlark.Float(fn()), nil
	})
}

type slEWMA struct{ e *EWMA }

func (s *slEWMA) String() string        { return "ewma()" }
func (s *slEWMA) Type() string          { return "ewma" }
func (s *slEWMA) Freeze()               {}
func (s *slEWMA) Truth() starlark.Bool  { return starlark.True }
func (s *slEWMA) Hash() (uint32, error) { return 0, fmt.Errorf("ewma is not hashable") }
func (s *slEWMA) Attr(name string) (starlark.Value, error) {
	switch name {
	case "observe":
		return starlark.NewBuiltin("observe", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("ewma.observe(value)")
			}
			f, err := asFloat(args[0])
			if err != nil {
				return nil, err
			}
			s.e.Observe(f)
			return starlark.None, nil
		}), nil
	case "value":
		return builtinFloat("value", s.e.Value), nil
	}
	return nil, starlark.NoSuchAttrError(name)
}
func (s *slEWMA) AttrNames() []string { return []string{"observe", "value"} }

type slCounter struct{ c *Counter }

func (s *slCounter) String() string        { return "counter()" }
func (s *slCounter) Type() string          { return "counter" }
func (s *slCounter) Freeze()               {}
func (s *slCounter) Truth() starlark.Bool  { return starlark.True }
func (s *slCounter) Hash() (uint32, error) { return 0, fmt.Errorf("counter is not hashable") }
func (s *slCounter) Attr(name string) (starlark.Value, error) {
	switch name {
	case "inc":
		return starlark.NewBuiltin("inc", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			v := 1.0
			if len(args) == 1 {
				f, err := asFloat(args[0])
				if err != nil {
					return nil, err
				}
				v = f
			} else if len(args) > 1 {
				return nil, fmt.Errorf("counter.inc([value])")
			}
			s.c.Inc(v)
			return starlark.None, nil
		}), nil
	case "total":
		return builtinFloat("total", s.c.Total), nil
	}
	return nil, starlark.NoSuchAttrError(name)
}
func (s *slCounter) AttrNames() []string { return []string{"inc", "total"} }

type slQuantiles struct{ q *Quantiles }

func (s *slQuantiles) String() string        { return "quantiles()" }
func (s *slQuantiles) Type() string          { return "quantiles" }
func (s *slQuantiles) Freeze()               {}
func (s *slQuantiles) Truth() starlark.Bool  { return starlark.True }
func (s *slQuantiles) Hash() (uint32, error) { return 0, fmt.Errorf("quantiles is not hashable") }
func (s *slQuantiles) Attr(name string) (starlark.Value, error) {
	switch name {
	case "add":
		return starlark.NewBuiltin("add", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("quantiles.add(value)")
			}
			f, err := asFloat(args[0])
			if err != nil {
				return nil, err
			}
			s.q.w.Add(s.q.w.nowFn(), f)
			return starlark.None, nil
		}), nil
	case "quantile":
		return starlark.NewBuiltin("quantile", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("quantiles.quantile(q)")
			}
			f, err := asFloat(args[0])
			if err != nil {
				return nil, err
			}
			return starlark.Float(s.q.Quantile(f)), nil
		}), nil
	}
	return nil, starlark.NoSuchAttrError(name)
}
func (s *slQuantiles) AttrNames() []string { return []string{"add", "quantile"} }

type slDistinct struct{ d *Distinct }

func (s *slDistinct) String() string        { return "distinct()" }
func (s *slDistinct) Type() string          { return "distinct" }
func (s *slDistinct) Freeze()               {}
func (s *slDistinct) Truth() starlark.Bool  { return starlark.True }
func (s *slDistinct) Hash() (uint32, error) { return 0, fmt.Errorf("distinct is not hashable") }
func (s *slDistinct) Attr(name string) (starlark.Value, error) {
	switch name {
	case "add":
		return starlark.NewBuiltin("add", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("distinct.add(key)")
			}
			return starlark.Bool(s.d.Add(unquote(args[0]))), nil
		}), nil
	case "count":
		return builtinFloat("count", func() float64 { return float64(s.d.Count()) }), nil
	}
	return nil, starlark.NoSuchAttrError(name)
}
func (s *slDistinct) AttrNames() []string { return []string{"add", "count"} }

type slDedupe struct{ d *Dedupe }

func (s *slDedupe) String() string        { return "dedupe()" }
func (s *slDedupe) Type() string          { return "dedupe" }
func (s *slDedupe) Freeze()               {}
func (s *slDedupe) Truth() starlark.Bool  { return starlark.True }
func (s *slDedupe) Hash() (uint32, error) { return 0, fmt.Errorf("dedupe is not hashable") }
func (s *slDedupe) Attr(name string) (starlark.Value, error) {
	switch name {
	case "allow":
		return starlark.NewBuiltin("allow", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("dedupe.allow(key)")
			}
			return starlark.Bool(s.d.Allow(unquote(args[0]))), nil
		}), nil
	}
	return nil, starlark.NoSuchAttrError(name)
}
func (s *slDedupe) AttrNames() []string { return []string{"allow"} }
