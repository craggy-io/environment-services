// Package signalcore is the signal hub, recorder, range query and
// capture over envspec records (envspec.SignalEvent and friends). It
// owns no collector lifecycle: core's activity reconciler decides when
// a collector runs and calls Hub.Run once per run; collectors are
// constructed by their own adapter already bound to that adapter's
// typed ports, so there is no Target, no exec shape and no
// stringly-typed handle lookup here. Signals are lossy and carry
// sequence numbers; lifecycle events are not signals.
package signalcore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
)

// StreamDef defines one stream once, by its collector.
type StreamDef struct {
	Name        string // "android.logcat@1", "net.pcap"
	Description string
	Schema      *jsonschema.Schema
	Bytes       bool
	Format      string // byte-stream format, e.g. "pcap"
	Cost        string // "low", "med", "high"
}

// EnvInfo is what a collector sees when deciding applicability. Adapters
// build it for their own environments; signalcore never fabricates one.
type EnvInfo struct {
	ID      string
	Kind    string // "container", "android", "host"
	Profile string
	Root    bool
	Labels  map[string]string
}

// Emitter is how collectors publish. Seq/Time/Collector are filled by the
// hub when zero.
type Emitter interface{ Emit(ev envspec.SignalEvent) }

// EmitterFunc adapts a function to Emitter.
type EmitterFunc func(envspec.SignalEvent)

func (f EmitterFunc) Emit(ev envspec.SignalEvent) { f(ev) }

// Collector produces one or more streams for one source. The instance is
// constructed by its adapter already bound to the typed ports it needs
// (an adb client, a container handle, a workspace directory). Run blocks
// until ctx ends or the mechanism fails, runs once, and returns the
// failure; supervising and restarting it is the caller's job.
type Collector interface {
	Name() string
	Streams() []StreamDef
	Params() *jsonschema.Schema
	Run(ctx context.Context, params json.RawMessage, out Emitter) error
}

// Applicability may be implemented by collectors that cannot run on every
// environment of their adapter's kind. Whoever resolves a stream to a
// collector for a source (core, on a pin) checks it and wraps a failure
// in ErrNotApplicable.
type Applicability interface {
	Applies(e EnvInfo) error
}

// Preflight may be implemented by collectors whose mechanism can be
// missing even when Applies passes (a binary, tracefs, a sidecar image).
// It is checked before Run so an explicit request fails with the reason
// instead of recording nothing; failures wrap ErrPreflight.
type Preflight interface {
	Preflight(ctx context.Context) error
}

// HostOnly may be implemented by host collectors (source "host", no env).
type HostOnly interface{ HostOnly() bool }

// Redactor scrubs secret values from text payloads before publishing.
// The vault's redactor implements it.
type Redactor interface {
	Redact(stream string, data any) any
	ContainsSecret(b []byte) bool
}

// Request errors for resolving a stream to a collector on a source, so
// callers can tell a typo from a missing environment from a mechanism
// the target lacks.
var (
	ErrNoStream      = errors.New("no collector provides this stream")
	ErrNoTarget      = errors.New("environment not running")
	ErrNotApplicable = errors.New("stream not available on this environment")
	ErrPreflight     = errors.New("collector preflight failed")
)

// SchemaFor derives a JSON Schema for a Go sample value or reflect.Type.
// Collectors use it to fill StreamDef.Schema.
func SchemaFor(sample any) *jsonschema.Schema {
	s, err := jsonschema.ForType(typeOf(sample), &jsonschema.ForOptions{})
	if err != nil {
		return nil
	}
	return s
}

func typeOf(v any) reflect.Type {
	if t, ok := v.(reflect.Type); ok {
		return t
	}
	return reflect.TypeOf(v)
}
