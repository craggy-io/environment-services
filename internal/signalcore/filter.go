package signalcore

import (
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/craggy-io/environment-services/envspec"
)

// Matching lives here, over the envspec records: envspec.Predicate and
// envspec.Selector are pure data (JSON-frozen), so the compiled regexes
// and glob evaluation belong to the hub side.

// compiledPredicate is one envspec.Predicate with its regex ready.
type compiledPredicate struct {
	field string
	op    envspec.Op
	value any
	re    *regexp.Regexp
}

type compiledFilter []compiledPredicate

func compilePredicate(p envspec.Predicate) (compiledPredicate, error) {
	cp := compiledPredicate{field: p.Field, op: p.Op, value: p.Value}
	if cp.op == "" {
		cp.op = envspec.OpEq
	}
	if cp.op == envspec.OpRegex {
		s, ok := p.Value.(string)
		if !ok {
			return cp, fmt.Errorf("signalcore: regex predicate on %q needs a string value", p.Field)
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return cp, fmt.Errorf("signalcore: predicate %q: %w", p.Field, err)
		}
		cp.re = re
	}
	return cp, nil
}

func compileFilter(f envspec.Filter) (compiledFilter, error) {
	out := make(compiledFilter, len(f))
	for i, p := range f {
		cp, err := compilePredicate(p)
		if err != nil {
			return nil, err
		}
		out[i] = cp
	}
	return out, nil
}

// compiledSelector is an envspec.Selector ready for repeated matching.
type compiledSelector struct {
	sel   envspec.Selector
	where compiledFilter
}

func compileSelector(sel envspec.Selector) (compiledSelector, error) {
	where, err := compileFilter(sel.Where)
	if err != nil {
		return compiledSelector{}, err
	}
	return compiledSelector{sel: sel, where: where}, nil
}

func (c compiledSelector) match(ev envspec.SignalEvent) bool {
	if len(c.sel.Collectors) > 0 {
		matched := false
		for _, id := range c.sel.Collectors {
			if id == ev.Collector {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	if len(c.sel.Streams) > 0 && !globAny(c.sel.Streams, ev.Stream) {
		return false
	}
	if len(c.sel.Sources) > 0 && !globAny(c.sel.Sources, ev.Source) {
		return false
	}
	return c.where.match(ev)
}

func (c compiledSelector) streamMatches(stream string) bool {
	return len(c.sel.Streams) == 0 || globAny(c.sel.Streams, stream)
}

// SelectorMatch reports whether ev passes sel. It compiles on every call;
// hot paths inside the hub and recorder compile once.
func SelectorMatch(sel envspec.Selector, ev envspec.SignalEvent) bool {
	c, err := compileSelector(sel)
	if err != nil {
		return false
	}
	return c.match(ev)
}

// FilterMatch reports whether ev passes f (compiling on every call).
func FilterMatch(f envspec.Filter, ev envspec.SignalEvent) bool {
	c, err := compileFilter(f)
	if err != nil {
		return false
	}
	return c.match(ev)
}

// Where builds an envspec.Filter from a simple map of field → expected
// value(s) (rules builtins). A string starting "re:" is a regex, a list
// means "in".
func Where(m map[string]any) (envspec.Filter, error) {
	var f envspec.Filter
	for k, v := range m {
		switch t := v.(type) {
		case string:
			if strings.HasPrefix(t, "re:") {
				f = append(f, envspec.Predicate{Field: k, Op: envspec.OpRegex, Value: strings.TrimPrefix(t, "re:")})
			} else {
				f = append(f, envspec.Predicate{Field: k, Op: envspec.OpEq, Value: t})
			}
		case []any:
			f = append(f, envspec.Predicate{Field: k, Op: envspec.OpIn, Value: t})
		default:
			f = append(f, envspec.Predicate{Field: k, Op: envspec.OpEq, Value: v})
		}
	}
	if _, err := compileFilter(f); err != nil {
		return nil, err
	}
	return f, nil
}

func (f compiledFilter) match(ev envspec.SignalEvent) bool {
	if len(f) == 0 {
		return true
	}
	m := dataMap(ev)
	for _, p := range f {
		v, ok := lookup(m, p.field)
		if !p.matchValue(v, ok) {
			return false
		}
	}
	return true
}

func (p compiledPredicate) matchValue(v any, ok bool) bool {
	switch p.op {
	case envspec.OpExists:
		return ok
	case envspec.OpEq, "":
		return ok && valuesEqual(v, p.value)
	case envspec.OpNe:
		return !ok || !valuesEqual(v, p.value)
	case envspec.OpIn:
		if !ok {
			return false
		}
		list, isList := p.value.([]any)
		if !isList {
			return valuesEqual(v, p.value)
		}
		for _, item := range list {
			if valuesEqual(v, item) {
				return true
			}
		}
		return false
	case envspec.OpPrefix:
		s, sok := v.(string)
		prefix, pok := p.value.(string)
		return ok && sok && pok && strings.HasPrefix(s, prefix)
	case envspec.OpRegex:
		s, sok := v.(string)
		if !ok || !sok {
			return false
		}
		re := p.re
		if re == nil {
			pat, pok := p.value.(string)
			if !pok {
				return false
			}
			var err error
			if re, err = regexp.Compile(pat); err != nil {
				return false
			}
		}
		return re.MatchString(s)
	case envspec.OpGT, envspec.OpGE, envspec.OpLT, envspec.OpLE:
		a, aok := toFloat(v)
		b, bok := toFloat(p.value)
		if !ok || !aok || !bok {
			return false
		}
		switch p.op {
		case envspec.OpGT:
			return a > b
		case envspec.OpGE:
			return a >= b
		case envspec.OpLT:
			return a < b
		case envspec.OpLE:
			return a <= b
		}
	}
	return false
}

func globAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
}

// MatchGlob reports a single glob match.
func MatchGlob(pattern, name string) bool {
	ok, _ := path.Match(pattern, name)
	return ok
}

func dataMap(ev envspec.SignalEvent) map[string]any {
	m := map[string]any{
		"stream": ev.Stream, "source": ev.Source, "collector": ev.Collector,
	}
	switch d := ev.Data.(type) {
	case map[string]any:
		for k, v := range d {
			m[k] = v
		}
	case nil, []byte, string:
		// envelope only
	default:
		if b, err := json.Marshal(d); err == nil {
			var mm map[string]any
			if json.Unmarshal(b, &mm) == nil {
				for k, v := range mm {
					m[k] = v
				}
			}
		}
	}
	return m
}

// DataField reads one dotted field from an event (rules use it too).
func DataField(ev envspec.SignalEvent, field string) (any, bool) {
	return lookup(dataMap(ev), field)
}

func lookup(m map[string]any, field string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(field, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = mm[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func valuesEqual(a, b any) bool {
	if af, ok1 := toFloat(a); ok1 {
		if bf, ok2 := toFloat(b); ok2 {
			return af == bf
		}
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}
