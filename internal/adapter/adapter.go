// Package adapter is the runtime-adapter contract: the only seam between
// the durable lifecycle core and code that knows a runtime (Docker, the
// Android SDK, host processes, physical devices). Adapters report facts
// and action results; the core decides the next lifecycle action and is
// the only writer of durable state.
//
// Every call takes an OpContext carrying the durable operation, env and
// generation it serves, so an adapter can fence stale work and make
// retries discoverable. External resources are named deterministically
// from the env ID (NameFor): that name is the rediscovery key after a
// crash between an external action and its durable record.
package adapter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// OpContext identifies the durable operation an adapter call serves.
type OpContext struct {
	OpID       string // durable operation ID ("op_…"); "" for ephemeral calls
	EnvID      string
	Generation int64
	Attempt    int
}

// NameFor returns the deterministic external name for an env's primary
// resource. Adapters create resources under this name (plus their own
// suffixes for sub-resources) so recovery can rediscover them.
func NameFor(envID string) string { return "craggy-" + envID }

// Presence is what an adapter can confirm about a resource. Absent is
// definitive; Unknown means the backend could not answer and must never
// be read as absence.
type Presence string

const (
	Present Presence = "present"
	Absent  Presence = "absent"
	Unknown Presence = "unknown"
)

// Ref identifies one external resource durably: the env it belongs to,
// the adapter's instance identity, and the deterministic external name.
type Ref struct {
	EnvID    string `json:"env"`
	External string `json:"external,omitempty"` // container ID, adb serial, pid-ish identity
	Name     string `json:"name,omitempty"`     // deterministic external name (NameFor)
}

// Observation is one Observe result: confirmed presence plus the facts
// the core projects into conditions and Info.
type Observation struct {
	Ref        Ref
	Presence   Presence
	Attached   bool // for attached resources: our attachment is live
	Image      string
	Workspace  string
	Endpoints  map[string]string
	Conditions []envspec.Condition
	Detail     string // why Unknown, when it is
}

// Candidate is one Discover result within an ownership scope.
type Candidate struct {
	Ref     Ref
	Foreign bool // visible but not ours to adopt (other tooling's resource)
	Detail  string
}

// Scope bounds a Discover call.
type Scope struct {
	ManagerID string
	EnvID     string // discover for one env when set
}

// Relationship distinguishes resources the adapter creates and destroys
// from ones that exist independently and are only attached to.
type Relationship string

const (
	Created  Relationship = "created"
	Attached Relationship = "attached"
)

// Description declares what an adapter serves.
type Description struct {
	Name         string
	Kinds        []envspec.Kind
	Relationship Relationship
	Version      string
}

// Disposition is how a release ends a resource's involvement.
type Disposition string

const (
	Destroy         Disposition = "destroy"
	ResetToPool     Disposition = "reset_to_pool"
	Disconnect      Disposition = "disconnect"
	RetainWorkspace Disposition = "retain_workspace"
)

// Adapter is the mandatory contract. Optional capabilities are separate
// interfaces below; an adapter implements only what its runtime means.
type Adapter interface {
	Describe() Description
	// Validate checks a merged spec without provisioning.
	Validate(ctx context.Context, op OpContext, spec envspec.Spec) error
	// Observe reports confirmed facts about ref. A backend failure is
	// Presence Unknown with Detail, not an error and never Absent.
	Observe(ctx context.Context, op OpContext, ref Ref) (Observation, error)
	// Discover finds resources in scope for recovery. Candidates
	// marked Foreign are visible but never adopted.
	Discover(ctx context.Context, scope Scope) ([]Candidate, error)
}

// Instance is one live resource handle inside an adapter. It never
// escapes the core's runtime map; holders get lease-checked handles.
type Instance interface {
	Ref() Ref
	// Dispose ends the resource per disp: Destroy removes owned
	// resources, Disconnect removes only our attachments.
	Dispose(ctx context.Context, op OpContext, disp Disposition) error
}

// Creator provisions new resources. Create must be rediscoverable: a
// retry with the same OpContext.EnvID finds the earlier result (by
// deterministic name) instead of creating a second resource. Create may
// return a non-nil Instance with a non-nil error when creation
// partially succeeded; the core records the instance and cleans up.
type Creator interface {
	Create(ctx context.Context, op OpContext, spec envspec.Spec, progress func(envspec.Progress)) (Instance, error)
	// Open re-materialises an Instance from a Ref after restart.
	Open(ctx context.Context, op OpContext, ref Ref) (Instance, error)
}

// Attacher connects to resources that exist independently (physical
// devices). Attach verifies identity; release disconnects only.
type Attacher interface {
	Attach(ctx context.Context, op OpContext, spec envspec.Spec, progress func(envspec.Progress)) (Instance, error)
	// Open re-materialises the attachment using its persisted spec.
	Open(ctx context.Context, op OpContext, ref Ref, spec envspec.Spec) (Instance, error)
}

// Coster reports an env's admission cost. Without it the core derives
// cost from spec resources and kind.
type Coster interface {
	Cost(spec envspec.Spec) Cost
}

// Cost is what an environment takes from a pool.
type Cost struct {
	Pool     envspec.Pool
	CPUs     float64
	MemoryMB int64
	DiskMB   int64
}

// Process is a running command in an environment.
type Process struct {
	Stdout io.Reader
	Stderr io.Reader
	Stdin  io.WriteCloser

	wait   func() (int, error)
	signal func(string) error
	pid    int
}

// NewProcess builds a Process for adapter implementations.
func NewProcess(stdout, stderr io.Reader, stdin io.WriteCloser, wait func() (int, error), signal func(string) error) *Process {
	return &Process{Stdout: stdout, Stderr: stderr, Stdin: stdin, wait: wait, signal: signal}
}

func (p *Process) SetPID(pid int) { p.pid = pid }
func (p *Process) PID() int       { return p.pid }

// Wait blocks until the process exits and returns its exit code.
func (p *Process) Wait() (int, error) {
	if p.wait == nil {
		return 0, nil
	}
	return p.wait()
}

// Signal sends a named signal ("KILL", "TERM", …) where supported.
func (p *Process) Signal(sig string) error {
	if p.signal == nil {
		return ErrUnsupported
	}
	return p.signal(sig)
}

// ErrUnsupported marks an operation an adapter does not implement.
var ErrUnsupported = errors.New("unsupported")

// ErrBackendDown marks failures where the runtime could not be reached;
// the core maps them to Presence Unknown, never to absence.
var ErrBackendDown = errors.New("backend down")

// BackendDown wraps err as a backend-unavailable failure.
func BackendDown(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrBackendDown, err)
}

// Executor runs commands. Cmd is envspec.Cmd plus streaming concerns
// adapters need (stdin, workdir, user).
type Executor interface {
	Exec(ctx context.Context, op OpContext, cmd ExecCmd) (*Process, error)
}

// ExecCmd is one command execution request inside an adapter.
type ExecCmd struct {
	Argv    []string
	Env     map[string]string
	Dir     string // environment-scoped; relative resolves in the workspace
	User    string // container user / "shell"|"root" on Android; ignored on host
	Stdin   io.Reader
	TTY     bool
	Timeout time.Duration
}

// Transferrer moves file trees as tar (docker archive API, host local
// tar, device tar over adb). GetArchive entries are relative to src; its
// Close reports a source that failed after streaming began.
type Transferrer interface {
	PutArchive(ctx context.Context, op OpContext, dst string, tar io.Reader) error
	GetArchive(ctx context.Context, op OpContext, src string) (io.ReadCloser, error)
}

// Snapshotter saves and restores named snapshots.
type Snapshotter interface {
	Save(ctx context.Context, op OpContext, name string) error
	Load(ctx context.Context, op OpContext, name string) error
	Snapshots(ctx context.Context, op OpContext) ([]string, error)
}

// Resetter returns a reusable instance to a clean state for its pool.
type Resetter interface {
	Reset(ctx context.Context, op OpContext) error
}

// Preparer runs slow per-profile preparation (base AVDs, SDK installs).
type Preparer interface {
	PrepareProfile(ctx context.Context, p envspec.Profile, progress func(envspec.Progress)) error
	ProfilePrepared(p envspec.Profile) error // nil once prepared
}

// ImageBuilder builds or pulls profile images.
type ImageBuilder interface {
	EnsureImage(ctx context.Context, p envspec.Profile, digest string, progress func(envspec.Progress)) (ref, imageDigest string, err error)
	ImageExists(ctx context.Context, ref string) (string, bool, error)
}

// BackendStarter starts a stopped runtime on first need. Readiness
// probes stay passive and never call it.
type BackendStarter interface {
	Check(ctx context.Context) error
	StartBackend(ctx context.Context, progress func(envspec.Progress)) error
}

// Checker reports whether the runtime answers, without starting it.
type Checker interface {
	Check(ctx context.Context) error
}

// CapacityReporter knows its pool's size once the runtime runs.
type CapacityReporter interface {
	Capacity(ctx context.Context) (memMB int64, cpus int, err error)
}

// CredInjector delivers a leased credential into an environment. The
// value is handed over inside the call only; adapters must not retain
// or log it beyond delivery.
type CredInjector interface {
	InjectEnv(name, value string)
	InjectFile(ctx context.Context, path string, value []byte) error
}

// FSLister stats and lists without a full manifest walk.
type FSLister interface {
	StatFS(ctx context.Context, op OpContext, path string) (envspec.Entry, error)
	ListFS(ctx context.Context, op OpContext, path string) ([]envspec.Entry, error)
}

// Manifestor produces a hashed manifest of a tree (delta transfer).
type Manifestor interface {
	Manifest(ctx context.Context, op OpContext, root string) ([]envspec.Entry, error)
}

// CollectorProvider exposes the collectors this instance's adapter owns,
// already bound to typed ports. Core's activity reconciler runs them.
type CollectorProvider interface {
	Collectors() []signalcore.Collector
	CollectorEnvInfo() signalcore.EnvInfo
}

// Probe is one supervised health/component check. Check reports a
// condition; Repair, when set, is the autonomous fix the core runs
// (through the MutationPolicy gate) after a False condition.
type Probe struct {
	Name     string
	Interval time.Duration // zero = core default
	Required bool          // blocks the readiness barrier until True
	Check    func(ctx context.Context) (envspec.Condition, error)
	Repair   func(ctx context.Context) error // optional
	// BeforeRepair, when set, captures evidence before a repair runs
	// (record-before-repair); its error does not block repair.
	BeforeRepair func(ctx context.Context) error
}

// ProbeProvider exposes an instance's probes/components.
type ProbeProvider interface {
	Probes() []Probe
}

// ComponentChange is one component transition an adapter reports
// out-of-band (supervisors); the core records it as a lifecycle event
// and a condition.
type ComponentChange struct {
	Component string
	From, To  string
	Repairs   int
	Reason    string
	Error     string
	Capture   string
	Package   string
}

// Component states.
const (
	ComponentOK        = "ok"
	ComponentRepairing = "repairing"
	ComponentDegraded  = "degraded"
	ComponentSuspect   = "suspect"
	ComponentDisabled  = "disabled"
)

// ComponentWatcher installs a sink for out-of-band component changes.
type ComponentWatcher interface {
	WatchComponents(fn func(envID string, c ComponentChange)) (stop func())
}

// ComponentReporter surfaces managed-component health for Info.
type ComponentReporter interface {
	Components() []envspec.ComponentState
	EnsureComponent(ctx context.Context, name string) error
}
