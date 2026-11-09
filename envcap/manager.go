package envcap

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

// Manager is everything the capability handlers call, and nothing
// else. It is owned by this package so envcap never imports the
// assembly package; environments.NewEnvCapDeps satisfies it (the
// compile-time assertion lives in environments).
//
// Error contract: Manager errors wrap envspec sentinels
// (ErrNotFound, ErrInvalid, ErrNotReady, ErrConflict) or this
// package's sentinels and carriers below (ErrNoCapacity,
// ErrBackendDown, ErrUnsupported, the stream errors, *HintError,
// *PolicyError). Handlers translate only those into agent-facing
// codes and hints; anything else surfaces as an internal error.
type Manager interface {
	// Environments.
	ProvisionEnv(ctx context.Context, req ProvisionRequest) (EnvResult, error)
	AttachEnv(ctx context.Context, req ProvisionRequest) (EnvResult, error)
	ReleaseEnv(ctx context.Context, id string) (EnvResult, error)
	EnvInfo(id string) (envspec.Info, error)
	EnvIdentity(id string) (EnvIdentity, error)
	ListEnvs(f ListFilter) []envspec.Info
	RenewLease(id string) error
	Borrow(ctx context.Context, id, job string) (func(), error)
	Profiles() []envspec.Profile
	Profile(name string) (envspec.Profile, bool)
	ProfileStatus(name string) envspec.ProfileStatus
	ExecEnv(ctx context.Context, id string, cmd envspec.Cmd) (ExecResult, error)
	Push(ctx context.Context, id string, src envspec.Source, dst string) (envspec.TransferStats, error)
	Pull(ctx context.Context, id, src string, dst envspec.Sink) (envspec.TransferStats, error)
	Stat(ctx context.Context, id, path string) (envspec.Entry, error)
	ListFS(ctx context.Context, id, path string, recursive bool, limit int) ([]envspec.Entry, error)
	Snapshot(ctx context.Context, id, name string) error
	Restore(ctx context.Context, id, name string) error
	Snapshots(ctx context.Context, id string) ([]string, error)
	HostSnapshot() envspec.HostSnapshot
	PoolCapacities() []envspec.PoolCapacity

	// Android devices & secret typing. The secret value behind
	// TypeSecret never crosses this interface.
	Android(id string) (AndroidDevice, error)
	TypeSecret(ctx context.Context, envID, leaseID, field string) error

	// Signals.
	Streams(envID string) []StreamInfo
	PinStream(ctx context.Context, ownerID, envID, stream string, params json.RawMessage) (PinRecord, error)
	UnpinStream(pinID string) (PinRecord, error)
	Pins(envID string) []PinRecord
	StreamState(stream, source string, params json.RawMessage) envspec.StreamState
	RunningCollectors() []RunningCollector
	QuerySignals(sel envspec.Selector, where map[string]any, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error)
	TailSignals(ctx context.Context, sel envspec.Selector, where map[string]any, wait time.Duration, limit int) ([]envspec.SignalEvent, envspec.Quality, error)
	SignalCoverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage
	CaptureSignals(sel envspec.Selector, where map[string]any, from, to time.Time) (Capture, error)

	// Credentials. No secret value ever crosses this interface
	// except into PutCred, the one way secrets enter.
	ListCreds(ctx context.Context) ([]CredView, error)
	LeaseCred(ctx context.Context, req LeaseCredRequest) (CredLease, error)
	ReleaseCredLease(ctx context.Context, leaseID string) error
	ListCredLeases(ctx context.Context) ([]CredLease, error)
	PutCred(ctx context.Context, in CredInput) (string, error)
	DeleteCred(ctx context.Context, cred string) error
	SetCredPolicy(ctx context.Context, cred string, p CredPolicy) error

	// Rules.
	LoadRule(ctx context.Context, source []byte, codeRef string, params map[string]any) (string, error)
	UnloadRule(id string) error
	ListRules() []RuleInfo
	TestRule(ctx context.Context, source []byte, params map[string]any, events []envspec.SignalEvent) (RuleTest, error)
	WaitRuleTriggers(ctx context.Context, rule string, wait time.Duration) ([]RuleTrigger, error)
	RecentRuleTriggers(since time.Time, rule string) []RuleTrigger
}

// ProvisionRequest is one provision or attach call. Progress may be nil.
type ProvisionRequest struct {
	Owner    envspec.Owner
	Spec     envspec.Spec
	Progress func(envspec.Progress)
	NoWait   bool // fail at once instead of queueing at capacity
}

// EnvResult is the outcome of a lifecycle-changing call: the durable
// operation that performed it, distinctly from the registry job, the
// environment and its lease. OperationID is always set on success —
// lifecycle work without a durable operation does not happen.
type EnvResult struct {
	Env         string
	Info        envspec.Info // provision or attach; zero after release
	OperationID string
	Generation  int64
	LeaseEpoch  int64
}

// EnvIdentity is an environment's fencing identity right now.
type EnvIdentity struct {
	Generation int64
	LeaseID    string
	LeaseEpoch int64
}

// ListFilter selects environments for ListEnvs; zero fields match any.
type ListFilter struct {
	Owner   string
	Session string
	Live    bool
}

// ExecResult is one completed command: full (uncapped) streams, the
// process identity its output was teed under, and its exit code.
// Handlers cap what they inline.
type ExecResult struct {
	ProcID string
	Stdout []byte
	Stderr []byte
	Exit   int
}

// AndroidDevice is the device surface the android capabilities use.
type AndroidDevice interface {
	Install(ctx context.Context, apk io.Reader, replace bool) error
	Screenshot(ctx context.Context) ([]byte, error)
	Tap(ctx context.Context, x, y int) error
	Swipe(ctx context.Context, x1, y1, x2, y2, durationMS int) error
	KeyEvent(ctx context.Context, code int) error
	Text(ctx context.Context, text string) error
}

// StreamInfo is one stream's discoverability view. Available/WhyNot
// and the running fields are set only when an environment was named.
type StreamInfo struct {
	Stream      string
	Description string
	Cost        string
	// Bytes marks a byte stream (event Data carries an opaque blob
	// reference, not structured data).
	Bytes       bool
	Available   *bool
	WhyNot      string
	Running     bool
	LastFailure string
	FailedAt    time.Time
	Pins        []string
}

// PinRecord is one pinned stream on one environment. Pin ids are
// the capability layer's handle on the manager's collector
// activities; unpinning stops the activity.
type PinRecord struct {
	ID     string
	Env    string
	Stream string
	Params json.RawMessage
	Owner  string
	Since  time.Time
}

// RunningCollector is one live collector instance.
type RunningCollector struct {
	ID      string
	Name    string
	Source  string
	Streams []string
}

// Capture is a retention-pinned recorded range.
type Capture interface {
	ID() string
	Source() envspec.Source
	Events() ([]envspec.SignalEvent, envspec.Quality, error)
	Close()
}

// CredView is the agent-visible credential summary: public fields,
// labels and availability — never secrets.
type CredView struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Public    map[string]string `json:"public,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"`
}

// CredPolicy is a credential's lease policy.
type CredPolicy struct {
	MaxConcurrent int           `json:"max_concurrent,omitempty"`
	Cooldown      time.Duration `json:"cooldown,omitempty"`
	MaxPerDay     int           `json:"max_per_day,omitempty"`
	HolderKinds   []string      `json:"holder_kinds,omitempty"`
	Owners        []string      `json:"owners,omitempty"`
	Sticky        bool          `json:"sticky,omitempty"`
	OnePerHolder  string        `json:"one_per_holder,omitempty"`
}

// LeaseCredRequest leases one credential by id or by selector.
type LeaseCredRequest struct {
	Cred       string
	Kind       string
	Labels     map[string]string
	Holder     string
	HolderKind string
	Owner      string
	Purpose    string
	TTL        time.Duration
}

// CredLease is one live credential lease (public view only).
type CredLease struct {
	Lease  string
	Cred   string
	Kind   string
	Holder string
	Owner  string
	Public map[string]string
}

// CredInput creates or replaces a credential. Secret is write-only:
// it is stored, never returned.
type CredInput struct {
	ID     string
	Kind   string
	Name   string
	Public map[string]string
	Secret map[string]string
	Labels map[string]string
	Policy CredPolicy
}

// RuleInfo is one loaded rule instance.
type RuleInfo struct {
	Instance    string
	Rule        string
	Version     int
	Description string
	CodeRef     string
	State       string
	Events      uint64
	Emits       uint64
	Triggers    uint64
	Faults      uint64
	LastTrigger time.Time
	LoadedAt    time.Time
}

// RuleTrigger is one rule firing.
type RuleTrigger struct {
	Rule      string
	Kind      string
	Data      map[string]any
	CaptureID string
	Time      time.Time
}

// RuleTest is a backtest outcome; Emits counts derived events.
type RuleTest struct {
	Rule     string
	Emits    int
	Triggers []RuleTrigger
	Faults   uint64
}
