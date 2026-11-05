package envspec

import (
	"encoding/json"
	"time"
)

// SignalEvent is one signal envelope. Data is the stream's payload type
// ([]byte for byte streams such as pcap); it is JSON at boundaries. Signals
// are lossy and carry sequence numbers; lifecycle events are not signals.
type SignalEvent struct {
	Stream    string    `json:"stream"`
	Source    string    `json:"source"` // "host" or an env ID
	Collector string    `json:"collector"`
	Seq       uint64    `json:"seq"` // per collector instance; a gap means loss
	Time      time.Time `json:"time"`
	Data      any       `json:"data,omitempty"`
	Depth     int       `json:"depth,omitempty"` // rule derivations; >= 4 dropped
	CodeRef   string    `json:"code_ref,omitempty"`
}

// Op is a selector predicate operator.
type Op string

const (
	OpEq     Op = "eq"
	OpNe     Op = "ne"
	OpIn     Op = "in"
	OpPrefix Op = "prefix"
	OpRegex  Op = "regex"
	OpGT     Op = "gt"
	OpGE     Op = "ge"
	OpLT     Op = "lt"
	OpLE     Op = "le"
	OpExists Op = "exists"
)

// Predicate is one field test against an event's Data. Field is a dotted
// path into the JSON form of Data ("tag", "proc.name"). The envelope fields
// "stream", "source" and "collector" are also addressable. Matching is
// evaluated by the hub, not by this record.
type Predicate struct {
	Field string `json:"field"`
	Op    Op     `json:"op"`
	Value any    `json:"value,omitempty"`
}

// Filter is a conjunction of predicates.
type Filter []Predicate

// Selector chooses streams and sources for a subscription or query. Streams
// and Sources are globs ("net.*@1", "env_*"); empty means any.
type Selector struct {
	Collectors []string        `json:"collectors,omitempty"` // exact collector instance IDs, never payload fields
	Streams    []string        `json:"streams,omitempty"`
	Sources    []string        `json:"sources,omitempty"`
	Where      Filter          `json:"where,omitempty"`
	Params     json.RawMessage `json:"params,omitempty"` // collector params; part of collector identity
}

// Gap is one detected loss span in a stream range.
type Gap struct {
	Collector string    `json:"collector,omitempty"`
	Dropped   uint64    `json:"dropped,omitempty"`
	FromSeq   uint64    `json:"from_seq,omitempty"`
	ToSeq     uint64    `json:"to_seq,omitempty"`
	From      time.Time `json:"from,omitempty"`
	To        time.Time `json:"to,omitempty"`
	Reason    string    `json:"reason"` // seq-gap, drop, restart
}

// Quality reports loss and fidelity for a subscription or range.
type Quality struct {
	Received       uint64        `json:"received"`
	Dropped        uint64        `json:"dropped"`
	SeqGaps        uint64        `json:"seq_gaps"`
	Restarts       int           `json:"restarts"`
	ClockSkew      time.Duration `json:"clock_skew,omitempty"`
	ContainsSecret bool          `json:"contains_secret,omitempty"`
	Gaps           []Gap         `json:"gaps,omitempty"`
}

// LossFree reports whether the range has no detected loss.
func (q Quality) LossFree() bool {
	return q.Dropped == 0 && q.SeqGaps == 0 && len(q.Gaps) == 0
}

// RangeMeta describes an exported capture range (range.json).
type RangeMeta struct {
	Collectors []string  `json:"collectors,omitempty"`
	ID         string    `json:"id"`
	Streams    []string  `json:"streams"`
	Sources    []string  `json:"sources"`
	From       time.Time `json:"from"`
	To         time.Time `json:"to"`
	Quality    Quality   `json:"quality"`
}

// Coverage is how much of a time window a stream's collector ran for on one
// source. A stream with no events in a window it was not collected for is
// unknown, not quiet. Ran and Window are computed by the hub per query and
// are not part of the JSON contract.
type Coverage struct {
	ParamsDigest string        `json:"paramsDigest,omitempty"`
	Stream       string        `json:"stream"`
	Source       string        `json:"source"`
	Status       string        `json:"status"` // covered, partial, not_running
	Ran          time.Duration `json:"-"`
	Window       time.Duration `json:"-"`
	Failure      string        `json:"failure,omitempty"`      // a failure inside the window
	NotRunnable  string        `json:"not_runnable,omitempty"` // why it cannot run there now
}

// StreamStats is the recorder's retention for one (source, stream):
// what the in-memory ring holds now, what has spilled to segments,
// and how many loss spans were detected.
type StreamStats struct {
	Source        string    `json:"source"`
	Stream        string    `json:"stream"`
	RingEvents    int       `json:"ring_events"`
	RingBytes     int64     `json:"ring_bytes"`
	Segments      int       `json:"segments"`
	SpilledEvents int       `json:"spilled_events"`
	Oldest        time.Time `json:"oldest,omitempty"`
	Newest        time.Time `json:"newest,omitempty"`
	Gaps          int       `json:"gaps"`
}

// StreamState is whether a stream's collector runs on a source now, and the
// last failure it stopped with.
type StreamState struct {
	Collectors  []string  `json:"collectors,omitempty"`
	Running     bool      `json:"running"`
	Since       time.Time `json:"since,omitempty"`
	LastFailure string    `json:"last_failure,omitempty"`
	FailedAt    time.Time `json:"failed_at,omitempty"`
}
