package rules

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.starlark.net/starlark"

	"github.com/craggy-io/environment-services/envspec"
)

func parseDuration(v starlark.Value) (time.Duration, error) {
	switch t := v.(type) {
	case starlark.String:
		return time.ParseDuration(string(t))
	case starlark.Int:
		s, _ := t.Int64()
		return time.Duration(s) * time.Second, nil
	case starlark.Float:
		return time.Duration(float64(t) * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("duration must be a string like \"10s\" or seconds")
	}
}

func kwMap(kwargs []starlark.Tuple) map[string]starlark.Value {
	out := map[string]starlark.Value{}
	for _, kv := range kwargs {
		if len(kv) == 2 {
			out[unquote(kv[0])] = kv[1]
		}
	}
	return out
}

// makeGlobals builds the rule predeclared environment. No built-in imports
// files, the network or capabilities (rule 8).
func (e *Engine) makeGlobals(cs *compileState, inst *instance) starlark.StringDict {
	if inst != nil {
		cs.inst = inst
	}
	dictOf := func() *starlark.Dict {
		if cs.stateDict != nil {
			return cs.stateDict
		}
		if cs.inst != nil {
			return cs.inst.state
		}
		return starlark.NewDict(8)
	}
	g := starlark.StringDict{
		"rule": starlark.NewBuiltin("rule", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if cs.decl != nil {
				cs.err = fmt.Errorf("rule(): called more than once")
				return nil, cs.err
			}
			kw := kwMap(kwargs)
			d := &ruleDecl{Params: map[string]string{}}
			if v, ok := kw["name"]; ok {
				d.Name = unquote(v)
			}
			if v, ok := kw["version"]; ok {
				if i, err := asInt64(v); err == nil {
					d.Version = int(i)
				}
			}
			if v, ok := kw["description"]; ok {
				d.Description = unquote(v)
			}
			if v, ok := kw["params"]; ok {
				if pd, ok := v.(*starlark.Dict); ok {
					for _, k := range pd.Keys() {
						val, _, _ := pd.Get(k)
						d.Params[unquote(k)] = val.Type()
					}
				}
			}
			if d.Name == "" {
				cs.err = fmt.Errorf("rule(): name is required")
				return nil, cs.err
			}
			cs.decl = d
			return starlark.None, nil
		}),
		"on": starlark.NewBuiltin("on", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) < 1 {
				return nil, fmt.Errorf("on(stream, source=, where=, params=, fn=)")
			}
			kw := kwMap(kwargs)
			spec := subSpec{stream: unquote(args[0])}
			if v, ok := kw["source"]; ok {
				spec.source = unquote(v)
			}
			if v, ok := kw["where"]; ok {
				if m, ok := toGoMap(v); ok {
					spec.where = m
				}
			}
			if v, ok := kw["params"]; ok {
				if m, ok := toGoMap(v); ok {
					b, _ := json.Marshal(m)
					spec.params = b
				}
			}
			if v, ok := kw["fn"]; ok {
				spec.fn = v
			}
			if spec.fn == nil && len(args) > 1 {
				spec.fn = args[len(args)-1]
			}
			if spec.fn == nil {
				return nil, fmt.Errorf("on(): fn= callback is required")
			}
			cs.subs = append(cs.subs, spec)
			return starlark.None, nil
		}),
		"every": starlark.NewBuiltin("every", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 2 {
				return nil, fmt.Errorf("every(duration, fn)")
			}
			d, err := parseDuration(args[0])
			if err != nil {
				return nil, err
			}
			if d < 250*time.Millisecond {
				return nil, fmt.Errorf("every(): minimum interval is 250ms")
			}
			cs.timers = append(cs.timers, timerSpec{every: d, fn: args[1]})
			return starlark.None, nil
		}),
		"window": starlark.NewBuiltin("window", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("window(duration)")
			}
			d, err := parseDuration(args[0])
			if err != nil {
				return nil, err
			}
			return &slWindow{w: &Window{dur: d, nowFn: cs.nowFn}}, nil
		}),
		"ewma": starlark.NewBuiltin("ewma", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			kw := kwMap(kwargs)
			hl := 5 * time.Minute
			if v, ok := kw["half_life"]; ok {
				d, err := parseDuration(v)
				if err != nil {
					return nil, err
				}
				hl = d
			} else if len(args) == 1 {
				d, err := parseDuration(args[0])
				if err != nil {
					return nil, err
				}
				hl = d
			}
			return &slEWMA{e: &EWMA{halfLife: hl, nowFn: cs.nowFn}}, nil
		}),
		"counter": starlark.NewBuiltin("counter", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			return &slCounter{c: &Counter{nowFn: cs.nowFn}}, nil
		}),
		"quantiles": starlark.NewBuiltin("quantiles", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("quantiles(duration)")
			}
			d, err := parseDuration(args[0])
			if err != nil {
				return nil, err
			}
			return &slQuantiles{q: &Quantiles{w: &Window{dur: d, nowFn: cs.nowFn}}}, nil
		}),
		"distinct": starlark.NewBuiltin("distinct", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("distinct(duration)")
			}
			d, err := parseDuration(args[0])
			if err != nil {
				return nil, err
			}
			return &slDistinct{d: &Distinct{dur: d, seen: map[string]time.Time{}, nowFn: cs.nowFn}}, nil
		}),
		"dedupe": starlark.NewBuiltin("dedupe", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) != 1 {
				return nil, fmt.Errorf("dedupe(duration)")
			}
			d, err := parseDuration(args[0])
			if err != nil {
				return nil, err
			}
			return &slDedupe{d: &Dedupe{dur: d, last: map[string]time.Time{}, nowFn: cs.nowFn}}, nil
		}),
		"state": dictOf(),
		"emit": starlark.NewBuiltin("emit", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) < 1 || len(args) > 3 {
				return nil, fmt.Errorf("emit(stream, data[, depth_base_event_time])")
			}
			stream := unquote(args[0])
			var data any
			if len(args) >= 2 {
				data = toGo(args[1])
			}
			if cs.inst == nil {
				return starlark.None, nil // compile pass
			}
			cs.inst.doEmit(stream, data, 1)
			return starlark.None, nil
		}),
		"trigger": starlark.NewBuiltin("trigger", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			if len(args) < 1 || len(args) > 2 {
				return nil, fmt.Errorf("trigger(kind, data, capture=, dedupe=)")
			}
			kw := kwMap(kwargs)
			kind := unquote(args[0])
			var data map[string]any
			if len(args) == 2 {
				if m, ok := toGoMap(args[1]); ok {
					data = m
				} else {
					data = map[string]any{"value": toGo(args[1])}
				}
			}
			var capSpec *CaptureSpec
			if v, ok := kw["capture"]; ok {
				capSpec = parseCapture(v)
			}
			var dedupeDur time.Duration
			if v, ok := kw["dedupe"]; ok {
				d, err := parseDuration(v)
				if err != nil {
					return nil, err
				}
				dedupeDur = d
			}
			if cs.inst == nil {
				return starlark.None, nil
			}
			cs.inst.doTrigger(kind, data, capSpec, dedupeDur)
			return starlark.None, nil
		}),
		"log": starlark.NewBuiltin("log", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			parts := make([]string, len(args))
			for i, a := range args {
				parts[i] = fmt.Sprintf("%v", toGo(a))
			}
			if cs.inst != nil && e.hub != nil {
				stream := "rule." + cs.inst.decl.Name + fmt.Sprintf(".log@%d", cs.inst.decl.Version)
				e.hub.Publish(envspec.SignalEvent{Stream: stream, Source: "host",
					CodeRef: cs.inst.codeRef, Data: map[string]any{"text": joinStr(parts, " ")}})
			}
			return starlark.None, nil
		}),
		"env":  envLookup{lookups: e.lookups},
		"host": hostLookup{lookups: e.lookups},
	}
	// params: declared params supplied at Load, as attribute access.
	g["params"] = &paramsValue{params: cs.params}
	return g
}

func joinStr(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

func parseCapture(v starlark.Value) *CaptureSpec {
	m, ok := toGoMap(v)
	if !ok {
		return nil
	}
	c := &CaptureSpec{}
	if ss, ok := m["streams"].([]any); ok {
		for _, s := range ss {
			c.Streams = append(c.Streams, fmt.Sprintf("%v", s))
		}
	}
	if ss, ok := m["sources"].([]any); ok {
		for _, s := range ss {
			c.Sources = append(c.Sources, fmt.Sprintf("%v", s))
		}
	}
	if s, ok := m["before"].(string); ok {
		c.Before, _ = time.ParseDuration(s)
	}
	if s, ok := m["after"].(string); ok {
		c.After, _ = time.ParseDuration(s)
	}
	return c
}

// doEmit publishes a derived event (loop depth enforced).
func (in *instance) doEmit(stream string, data any, depth int) {
	if depth >= 4 {
		return
	}
	full := in.derivedStream(stream)
	in.emits.Add(1)
	ev := envspec.SignalEvent{Stream: full, Source: "host", Data: data, Depth: depth, CodeRef: in.codeRef}
	if rec := in.cs.recorder; rec != nil {
		rec.emits = append(rec.emits, ev)
		return
	}
	if in.engine.hub != nil {
		in.engine.hub.Publish(ev)
	}
}

// derivedStream names the stream a rule emit lands on. Bare and
// unversioned names, and every stream a registered collector defines,
// go under rule.<name>.<stream>@<version>: a rule may never publish on
// a collector-owned stream, whose events consumers treat as acquisitions.
func (in *instance) derivedStream(stream string) string {
	if strings.Contains(stream, ".") && strings.Contains(stream, "@") && !in.collectorOwned(stream) {
		return stream
	}
	base, _, _ := strings.Cut(stream, "@")
	return fmt.Sprintf("rule.%s.%s@%d", in.decl.Name, base, in.decl.Version)
}

func (in *instance) collectorOwned(stream string) bool {
	if in.engine.hub == nil {
		return false
	}
	_, ok := in.engine.hub.CollectorFor(stream)
	return ok
}

// doTrigger raises a lifecycle trigger via the engine sink (with dedupe).
func (in *instance) doTrigger(kind string, data map[string]any, capSpec *CaptureSpec, dedupeDur time.Duration) {
	now := in.nowFn()
	if dedupeDur > 0 {
		in.mu.Lock()
		if last, ok := in.trigDedupe[kind]; ok && now.Sub(last) < dedupeDur {
			in.mu.Unlock()
			return
		}
		in.trigDedupe[kind] = now
		in.mu.Unlock()
	}
	in.triggers.Add(1)
	in.mu.Lock()
	in.lastTrigger = now
	in.mu.Unlock()
	t := Trigger{Rule: in.decl.Name, Version: in.decl.Version, Instance: string(in.id),
		Kind: kind, Data: data, Capture: capSpec, CodeRef: in.codeRef, Time: now}
	if rec := in.cs.recorder; rec != nil {
		rec.triggers = append(rec.triggers, t)
		return
	}
	in.engine.recordTrigger(t)
}

// paramsValue exposes Load params by attribute (params.env).
type paramsValue struct{ params map[string]any }

func (p *paramsValue) String() string        { return "params" }
func (p *paramsValue) Type() string          { return "params" }
func (p *paramsValue) Freeze()               {}
func (p *paramsValue) Truth() starlark.Bool  { return starlark.True }
func (p *paramsValue) Hash() (uint32, error) { return 0, fmt.Errorf("params is not hashable") }
func (p *paramsValue) Attr(name string) (starlark.Value, error) {
	v, ok := p.params[name]
	if !ok {
		return nil, starlark.NoSuchAttrError("params has no " + name + " (supply it at load)")
	}
	return fromGo(v), nil
}
func (p *paramsValue) AttrNames() []string {
	names := make([]string, 0, len(p.params))
	for k := range p.params {
		names = append(names, k)
	}
	return names
}

// envLookup / hostLookup are read-only lookup singletons.
type envLookup struct{ lookups Lookups }

func (e envLookup) String() string        { return "env" }
func (e envLookup) Type() string          { return "env" }
func (e envLookup) Freeze()               {}
func (e envLookup) Truth() starlark.Bool  { return starlark.True }
func (e envLookup) Hash() (uint32, error) { return 0, fmt.Errorf("env is not hashable") }
func (e envLookup) Attr(name string) (starlark.Value, error) {
	if name != "info" {
		return nil, starlark.NoSuchAttrError(name)
	}
	return starlark.NewBuiltin("info", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if len(args) != 1 || e.lookups.EnvInfo == nil {
			return starlark.None, nil
		}
		info, ok := e.lookups.EnvInfo(unquote(args[0]))
		if !ok {
			return starlark.None, nil
		}
		return fromGo(info), nil
	}), nil
}
func (e envLookup) AttrNames() []string { return []string{"info"} }

type hostLookup struct{ lookups Lookups }

func (h hostLookup) String() string        { return "host" }
func (h hostLookup) Type() string          { return "host" }
func (h hostLookup) Freeze()               {}
func (h hostLookup) Truth() starlark.Bool  { return starlark.True }
func (h hostLookup) Hash() (uint32, error) { return 0, fmt.Errorf("host is not hashable") }
func (h hostLookup) Attr(name string) (starlark.Value, error) {
	if name != "snapshot" {
		return nil, starlark.NoSuchAttrError(name)
	}
	return starlark.NewBuiltin("snapshot", func(_ *starlark.Thread, b *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
		if h.lookups.HostSnapshot == nil {
			return starlark.None, nil
		}
		return fromGo(h.lookups.HostSnapshot()), nil
	}), nil
}
func (h hostLookup) AttrNames() []string { return []string{"snapshot"} }
