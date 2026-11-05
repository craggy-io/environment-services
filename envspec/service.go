package envspec

import (
	"context"
	"time"
)

// ServiceSpec declares a supervised application inside an ordinary environment.
// Commands use argv, never a shell. Ready must test application readiness, not
// merely process existence. No command is automatically replayed after failure.
type ServiceSpec struct {
	Name           string        `json:"name"`
	Version        string        `json:"version"`
	Start          []string      `json:"start"`
	Ready          []string      `json:"ready"`
	Stop           []string      `json:"stop,omitempty"`
	StartupTimeout time.Duration `json:"startup_timeout"`
	ProbeInterval  time.Duration `json:"probe_interval"`
	ProbeTimeout   time.Duration `json:"probe_timeout"`
	IdleTimeout    time.Duration `json:"idle_timeout"`
	StopTimeout    time.Duration `json:"stop_timeout"`
}

// ServiceRequest's Key is a caller-authorised sharing scope (for example a lab
// and project revision). Owner, complete definition and profile identity also
// participate in sharing. Environment.Kind still selects host/container/etc.
type ServiceRequest struct {
	Key         string
	Environment Spec
	Service     ServiceSpec
}

// ServiceInfo is the persisted lifecycle identity; it contains no host paths or
// credentials. A replacement always has a new ID and generation.
type ServiceInfo struct {
	ID          string `json:"id"`
	Generation  string `json:"generation"`
	Environment string `json:"environment"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	State       string `json:"state"`
	Borrowers   int    `json:"borrowers"`
	Image       string `json:"image,omitempty"`
	Failure     string `json:"failure,omitempty"`
}

// ServiceLease is a connection through the environment's exec/transfer
// transport. Release drops this borrow, not the shared environment. Methods
// reject released and failed generations (Pull permits failed generations
// for evidence retention). Existing borrowers may finish during draining; new
// acquisition cannot join that generation. Lock serialises a complete
// application transaction (including its transfers and checkpoint publication).
type ServiceLease interface {
	Handle
	ServiceInfo() ServiceInfo
	Lock(context.Context) (func(), error)
}

// ServiceProvider acquires and stops managed services. StopService is
// generation-fenced: it takes the service ID and the generation to stop.
type ServiceProvider interface {
	AcquireService(context.Context, Owner, ServiceRequest) (ServiceLease, error)
	StopService(context.Context, string, string) error
}
