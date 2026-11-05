package envspec

import "time"

// State is an environment's lifecycle phase as reported in Info. The
// durable core keeps desired disposition and observed presence as separate
// dimensions; State is the summary phase.
type State string

const (
	StateAcquiring State = "acquiring"
	StatePresent   State = "present"
	StateReleasing State = "releasing"
	StateReleased  State = "released"
	StateFailed    State = "failed"
)

// Terminal reports whether an environment in this state is over: it holds
// no instance or lease and nothing moves it on. A failed environment with
// an open release operation is StateReleasing, not terminal, until cleanup
// is verified.
func (s State) Terminal() bool {
	return s == StateReleased || s == StateFailed
}

// ConditionStatus is a condition's truth value. Unknown blocks decisions
// that require certainty; it never proves absence or death.
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition is one observed fact about an environment (reachable, booted,
// credentials ready, instrumentation ready, …). Freshness is computed by
// readers against ObservedAt; it is not stored state.
type Condition struct {
	Name       string          `json:"name"`
	Status     ConditionStatus `json:"status"`
	Reason     string          `json:"reason,omitempty"` // stable lower_snake_case code
	ObservedAt time.Time       `json:"observed_at"`
	Detail     string          `json:"detail,omitempty"`
}

// ComponentState is one managed component's health.
type ComponentState struct {
	Name        string    `json:"name"`
	State       string    `json:"state"` // ok, repairing, degraded, suspect, disabled
	Since       time.Time `json:"since"`
	Repairs     int       `json:"repairs"`
	LastFailure string    `json:"last_failure,omitempty"`
}

// Info is an environment's agent-facing description. It never contains host
// paths; serials are replaced by named endpoints only where the design
// permits (Info is the library API; the capability surface trims further).
type Info struct {
	ID         string            `json:"id"`
	Kind       Kind              `json:"kind"`
	Profile    string            `json:"profile,omitempty"`
	State      State             `json:"state"`
	Owner      Owner             `json:"owner"`
	Spec       Spec              `json:"spec"`
	Created    time.Time         `json:"created"`
	Image      string            `json:"image,omitempty"` // immutable digest that ran
	Workspace  string            `json:"workspace,omitempty"`
	Endpoints  map[string]string `json:"endpoints,omitempty"`
	LeaseID    string            `json:"lease,omitempty"`
	TTLLeft    time.Duration     `json:"ttl_left,omitempty"`
	Borrows    []string          `json:"borrows,omitempty"` // job IDs
	Components []ComponentState  `json:"components,omitempty"`
	Conditions []Condition       `json:"conditions,omitempty"`
	Warm       bool              `json:"warm,omitempty"`
	Services   []ServiceInfo     `json:"services,omitempty"`
}

// LifecycleEvent is one persisted lifecycle event, written in the same
// transaction as the state change it describes. Seq is the resume cursor;
// owner/session/job attribution is part of the record.
type LifecycleEvent struct {
	Seq     uint64         `json:"seq"`
	Kind    string         `json:"kind"`
	Env     string         `json:"env,omitempty"`
	Job     string         `json:"job,omitempty"`
	Session string         `json:"session,omitempty"`
	Owner   string         `json:"owner,omitempty"`
	Time    time.Time      `json:"time"`
	Data    map[string]any `json:"data,omitempty"`
}

// ProcLoad is one emulator process's load.
type ProcLoad struct {
	PID        int32   `json:"pid"`
	Name       string  `json:"name"`
	CPUPercent float64 `json:"cpu_percent"`
	RSSMB      int64   `json:"rss_mb"`
}

// HostSnapshot is one host sample, also the host.metrics@1 payload.
type HostSnapshot struct {
	Time       time.Time `json:"time"`
	CPUs       int       `json:"cpus"`
	CPUPercent float64   `json:"cpu_percent"`
	MemTotalMB int64     `json:"mem_total_mb"`
	MemFreeMB  int64     `json:"mem_free_mb"` // available
	MemUsedPct float64   `json:"mem_used_pct"`
	// DiskFreeMB keyed by watched path (state dir, Docker data, host work).
	DiskFreeMB map[string]int64 `json:"disk_free_mb,omitempty"`
	// Emulator load: qemu-system process CPU%/RSS by PID.
	Emulators []ProcLoad `json:"emulators,omitempty"`
	// Docker VM capacity (zero until the docker adapter reports it).
	DockerMemTotalMB int64 `json:"docker_mem_total_mb,omitempty"`
	DockerCPUs       int   `json:"docker_cpus,omitempty"`
	Pressure         bool  `json:"pressure,omitempty"`
}

// HostCapacity is one pool's budget for admission: limits and measured-free,
// whichever is lower governs. Field names are the JSON contract (no tags,
// as in the record this replaces).
type HostCapacity struct {
	CPUs     float64
	MemoryMB int64 // total budget after reserve
	FreeMB   int64 // measured free right now
	FreeCPUs float64
}

// Pool names an admission capacity pool. Host and the Docker VM are
// separate pools because they run out separately.
type Pool string

const (
	PoolHost   Pool = "host"   // host RAM/cores: emulators, host envs
	PoolDocker Pool = "docker" // the Docker VM's RAM/cores: containers
)

// PoolCapacity is one pool's budget plus what live reservations hold.
type PoolCapacity struct {
	Pool             Pool
	CPUs             float64
	MemoryMB         int64
	ReservedCPUs     float64
	ReservedMemoryMB int64
	FreeMB           int64
	FreeCPUs         float64
}

// ProfileStatus is the manager's authoritative readiness answer for one
// profile. NotReady names why the profile cannot provision right now
// (missing driver, unprepared image, backend down); State is the profile's
// preparation state.
type ProfileStatus struct {
	Profile  Profile `json:"profile"`
	Ready    bool    `json:"ready"`
	NotReady string  `json:"not_ready,omitempty"`
	State    string  `json:"state,omitempty"`
}
