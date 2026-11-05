package rules

import (
	"fmt"

	"go.starlark.net/starlark"

	"github.com/craggy-io/script-services/values"
)

// All Go<->Starlark conversion goes through script-services' values
// package; the wrappers below keep the engine's call sites terse.
// toGo is total: a value that cannot cross the JSON boundary (a
// function, a helper object, a non-string dict key) becomes its
// string form rather than faulting the callback — rules are
// observers, and a lossy emit beats a disabled instance. fromGo
// degrades the same way for event data that is not JSON-marshalable.

func toGo(v starlark.Value) any {
	g, err := values.ToGo(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return g
}

func fromGo(v any) starlark.Value {
	sv, err := values.FromGo(v)
	if err != nil {
		return starlark.String(fmt.Sprintf("%v", v))
	}
	return sv
}

// toGoMap converts v when it is a string-keyed dict (rule where/params,
// trigger data, capture specs).
func toGoMap(v starlark.Value) (map[string]any, bool) {
	m, ok := toGo(v).(map[string]any)
	return m, ok
}

func asFloat(v starlark.Value) (float64, error) {
	f, ok := starlark.AsFloat(v)
	if !ok {
		return 0, fmt.Errorf("expected number, got %s", v.Type())
	}
	return f, nil
}

func asInt64(v starlark.Value) (int64, error) {
	var i int64
	if err := starlark.AsInt(v, &i); err != nil {
		return 0, err
	}
	return i, nil
}
