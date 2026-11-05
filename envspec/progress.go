package envspec

import (
	"sync"
	"time"
)

// Progress is one lossy progress update from a slow operation. Counters are
// local to the current stage. Total == 0 means indeterminate.
type Progress struct {
	Op                string  `json:"op"`
	Stage             string  `json:"stage"`
	Done              int64   `json:"done,omitempty"`
	Total             int64   `json:"total,omitempty"`
	Unit              string  `json:"unit,omitempty"`
	Message           string  `json:"message,omitempty"`
	EnvID             string  `json:"env_id,omitempty"`
	ProcID            string  `json:"proc_id,omitempty"`
	StdoutBytes       int64   `json:"stdout_bytes,omitempty"`
	StderrBytes       int64   `json:"stderr_bytes,omitempty"`
	QueuePosition     int     `json:"queue_position,omitempty"`
	RequestedCPUs     float64 `json:"requested_cpus,omitempty"`
	RequestedMemoryMB int64   `json:"requested_memory_mb,omitempty"`
	AvailableCPUs     float64 `json:"available_cpus,omitempty"`
	AvailableMemoryMB int64   `json:"available_memory_mb,omitempty"`
	CapacityCPUs      float64 `json:"capacity_cpus,omitempty"`
	CapacityMemoryMB  int64   `json:"capacity_memory_mb,omitempty"`
	ReservedCPUs      float64 `json:"reserved_cpus,omitempty"`
	ReservedMemoryMB  int64   `json:"reserved_memory_mb,omitempty"`
}

// Throttled wraps a progress callback: at most maxPerSec updates per second,
// but always the first update of every stage and the final update delivered
// through Flush. It is safe for concurrent use; a nil callback yields a
// no-op reporter.
func Throttled(fn func(Progress), maxPerSec int) (report func(Progress), flush func()) {
	if fn == nil {
		return func(Progress) {}, func() {}
	}
	if maxPerSec <= 0 {
		maxPerSec = 4
	}
	minGap := time.Second / time.Duration(maxPerSec)
	var mu sync.Mutex
	var last time.Time
	var lastStage string
	var pending *Progress
	report = func(p Progress) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		firstOfStage := p.Stage != lastStage
		if firstOfStage || now.Sub(last) >= minGap {
			fn(p)
			last, lastStage = now, p.Stage
			pending = nil
			return
		}
		cp := p
		pending = &cp
	}
	flush = func() {
		mu.Lock()
		defer mu.Unlock()
		if pending != nil {
			fn(*pending)
			lastStage = pending.Stage
			pending = nil
		}
	}
	return report, flush
}
