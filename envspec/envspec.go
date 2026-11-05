// Package envspec holds the records and narrow interfaces consumers use to
// declare and run environments. It imports only the standard library, so
// importing it pulls in no Docker client, no SQLite and no drivers. That is
// load-bearing: consumer domain packages alias these record types and must
// stay dependency-light (enforced by arch_test.go at the module root).
//
// The JSON shapes in this package are the frozen wire contract for signal
// streams and receipts: fields are never renamed (golden tests pin them).
package envspec

import (
	"context"
	"io"
	"io/fs"
	"iter"
	"time"
)

// Kind identifies an environment runtime class.
type Kind string

const (
	Container Kind = "container"
	Android   Kind = "android"
	Host      Kind = "host"
)

// Network selects an environment's network posture.
type Network string

const (
	NetworkNone     Network = "none"     // no network at all
	NetworkIsolated Network = "isolated" // private network, no egress
	NetworkEgress   Network = "egress"   // egress through the capture proxy
	NetworkOpen     Network = "open"     // unrestricted (the only host option)
)

// Resources are an environment's resource request/limit.
type Resources struct {
	CPUs     float64 `json:"cpus,omitempty"`
	MemoryMB int64   `json:"memory_mb,omitempty"`
	DiskMB   int64   `json:"disk_mb,omitempty"`
}

// ImageKind selects an Android system-image flavour.
type ImageKind string

const (
	GoogleAPIs ImageKind = "google_apis"
	GooglePlay ImageKind = "google_apis_playstore"
	AOSP       ImageKind = "aosp"
)

// RootMode selects how an Android environment obtains root.
type RootMode string

const (
	RootNone   RootMode = "none"
	RootADB    RootMode = "adb"    // userdebug images: `adb root`
	RootMagisk RootMode = "magisk" // patched ramdisk; any image
)

// FridaOptions configures the managed frida-server component.
type FridaOptions struct {
	Version string `json:"version,omitempty"` // "" = match the host frida client
	Port    int    `json:"port,omitempty"`    // default 27042
	Name    string `json:"name,omitempty"`    // on-device binary name; default randomised
}

// RepairPolicy bounds automatic component repair.
type RepairPolicy struct {
	MayReboot   bool `json:"may_reboot,omitempty"`   // off by default
	MaxAttempts int  `json:"max_attempts,omitempty"` // default 3 per window
}

// AndroidOptions are the Kind == Android fields of a Spec.
type AndroidOptions struct {
	API     int           `json:"api,omitempty"`
	Image   ImageKind     `json:"image,omitempty"`
	Root    RootMode      `json:"root,omitempty"`
	Frida   *FridaOptions `json:"frida,omitempty"`
	ProxyCA bool          `json:"proxy_ca,omitempty"`
	Repair  RepairPolicy  `json:"repair,omitempty"`
	ABI     string        `json:"abi,omitempty"` // default "x86_64"
}

// HostOptions are the Kind == Host fields of a Spec.
type HostOptions struct {
	Path []string          `json:"path,omitempty"` // prepended to PATH; "${VAR}" expands
	Env  map[string]string `json:"env,omitempty"`  // added to every Exec; "${VAR}" expands
}

// CredRequest names a credential to lease and inject at provision.
type CredRequest struct {
	Cred          string            `json:"cred,omitempty"`   // credential ID, or empty to select
	Kind          string            `json:"kind,omitempty"`   // selector kind
	Labels        map[string]string `json:"labels,omitempty"` // selector labels
	Deliver       string            `json:"deliver"`          // "env:NAME", "file:/path", "account", "type"
	Field         string            `json:"field,omitempty"`  // secret field for env/file delivery
	RequireSticky bool              `json:"require_sticky,omitempty"`
}

// SignalRequest pins a collector from boot. Required pins block readiness
// until the collector acknowledges; the default is best-effort.
type SignalRequest struct {
	Stream   string            `json:"stream"`
	Params   map[string]string `json:"params,omitempty"`
	Required bool              `json:"required,omitempty"`
}

// Spec is an environment request. Fields left zero inherit from Profile.
type Spec struct {
	Kind      Kind              `json:"kind,omitempty"`
	Profile   string            `json:"profile,omitempty"`
	Image     string            `json:"image,omitempty"`
	Resources Resources         `json:"resources,omitempty"`
	Network   Network           `json:"network,omitempty"`
	TTL       time.Duration     `json:"ttl,omitempty"`
	Creds     []CredRequest     `json:"creds,omitempty"`
	Signals   []SignalRequest   `json:"signals,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Android   *AndroidOptions   `json:"android,omitempty"`
	Host      *HostOptions      `json:"host,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	// Key makes Provision idempotent: repeating a call with the same owner
	// and key returns the existing environment.
	Key string `json:"key,omitempty"`
}

// Build is a Dockerfile build context carried as an fs.FS.
type Build struct {
	Context    fs.FS             `json:"-"`
	Dockerfile string            `json:"dockerfile,omitempty"` // default "Dockerfile"
	Args       map[string]string `json:"args,omitempty"`
}

// Profile is a named, validated Spec template. Modules declare profiles as
// data beside their capabilities; core registers them with the manager.
type Profile struct {
	Name        string    `json:"name"`
	Kind        Kind      `json:"kind"`
	Description string    `json:"description,omitempty"` // says when to choose it
	Image       string    `json:"image,omitempty"`       // prebuilt ref; xor Build
	Build       *Build    `json:"build,omitempty"`
	Resources   Resources `json:"resources,omitempty"`

	// Ceilings bound what a caller may override.
	MaxResources Resources `json:"max_resources,omitempty"`
	// AllowImages globs constrain Image and Dockerfile FROM lines.
	AllowImages []string `json:"allow_images,omitempty"`
	Network     Network  `json:"network,omitempty"`
	// AllowNetworks, when non-empty, bounds overridable networks.
	AllowNetworks []Network         `json:"allow_networks,omitempty"`
	TTL           time.Duration     `json:"ttl,omitempty"`
	MaxTTL        time.Duration     `json:"max_ttl,omitempty"`
	Warm          int               `json:"warm,omitempty"`    // warm-pool size (emulators)
	Prepare       bool              `json:"prepare,omitempty"` // build/prepare at startup
	Reuse         bool              `json:"reuse,omitempty"`   // reset-and-return-to-pool on release (emulators)
	Android       *AndroidOptions   `json:"android,omitempty"`
	Host          *HostOptions      `json:"host,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Params        map[string]string `json:"params,omitempty"`
}

// Owner is the trusted identity an environment runs for. Core maps it from
// the registry caller and job ID; capability arguments never set it.
type Owner struct {
	ID      string `json:"id"`                // "agent:triage-7", "module:apk-decompile", "user:phil"
	Session string `json:"session,omitempty"` // quotas and session cleanup
	Parent  string `json:"parent,omitempty"`  // who started the owner
	Job     string `json:"job,omitempty"`     // execution ID; empty = session-held
}

// Entry is one file in a transferred tree. Streams of entries are sorted
// lexicographically by Path. Symlinks use Mode 0o120000 with the target as
// content (git convention).
type Entry struct {
	Path   string `json:"path"`
	Mode   int64  `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"` // "sha256:<hex>"
}

// IsDir reports whether the entry is a directory.
func (e Entry) IsDir() bool { return e.Mode&0o170000 == 0o040000 }

// Source is a readable tree of files. Bytes cross boundaries only as
// streams through Source/Sink.
type Source interface {
	Entries(ctx context.Context) iter.Seq2[Entry, error]
	Open(ctx context.Context, e Entry) (io.ReadCloser, error)
}

// Sink receives files, one Add per file.
type Sink interface {
	Add(path string, mode int64, r io.Reader) error
}

// Cmd is the one command request shape for Handle.Exec: argv (never a
// shell), extra environment, and a timeout.
type Cmd struct {
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// Handle is the holder's surface on a provisioned environment. It is
// lease-aware: every operation re-validates the holder's generation and
// lease epoch, so a handle from before reassignment is permanently stale.
type Handle interface {
	ID() string
	Info() Info
	Exec(ctx context.Context, cmd Cmd) (stdout, stderr []byte, exit int, err error)
	Push(ctx context.Context, src Source, dst string) error
	Pull(ctx context.Context, src string, dst Sink) error
	Release(ctx context.Context) error
}

// MutationPolicy is the experiment-hold port: autonomous mutation (repair,
// reap, reset) asks before acting on an environment. A denial carries the
// reason recorded on the held activity.
type MutationPolicy interface {
	AllowMutation(envID, kind string) (bool, string)
}
