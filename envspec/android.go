package envspec

import (
	"encoding/json"
	"time"
)

// PackageMutation is one host-controlled package-manager transition. APK is
// required for install and forbidden for uninstall. The package name is an
// expected identity, not a shell fragment.
type PackageMutation struct {
	Operation string `json:"operation"` // install | uninstall
	Package   string `json:"package"`
	APK       []byte `json:"-"`
	Replace   bool   `json:"replace,omitempty"`
}

// PackageSnapshot is the device identity observed around a mutation.
type PackageSnapshot struct {
	Package    string `json:"package"`
	Installed  bool   `json:"installed"`
	UID        int    `json:"uid,omitempty"`
	CodePath   string `json:"codePath,omitempty"`
	CodeSHA256 string `json:"codeSha256,omitempty"`
}

// PackageMutationReceipt is one completed transition in an ordered sequence.
type PackageMutationReceipt struct {
	Index       int             `json:"index"`
	Operation   string          `json:"operation"`
	Package     string          `json:"package"`
	StartedAt   string          `json:"startedAt"`
	CompletedAt string          `json:"completedAt"`
	Before      PackageSnapshot `json:"before"`
	After       PackageSnapshot `json:"after"`
}

// PackageSequenceReceipt proves the exact order and observed identity around
// every host-controlled install/uninstall step.
type PackageSequenceReceipt struct {
	StartedAt   string                   `json:"startedAt"`
	CompletedAt string                   `json:"completedAt"`
	Steps       []PackageMutationReceipt `json:"steps"`
}

// PackageAction asks an installed package to perform a command as its own
// UID. The activity receives a base64 JSON extra named "cmd" containing a
// nonce and the supplied command, then writes JSON to
// files/craggy-actions/<nonce>.json.
type PackageAction struct {
	Package  string          `json:"package"`
	Activity string          `json:"activity"` // package-relative, for example .ActionActivity
	Command  json.RawMessage `json:"command"`
	Timeout  time.Duration   `json:"-"`
}

// PackageActionReceipt proves the package identity around an app-UID action
// and retains the bounded result returned by that app.
type PackageActionReceipt struct {
	Package      string          `json:"package"`
	Activity     string          `json:"activity"`
	StartedAt    string          `json:"startedAt"`
	CompletedAt  string          `json:"completedAt"`
	Before       PackageSnapshot `json:"before"`
	After        PackageSnapshot `json:"after"`
	Result       json.RawMessage `json:"result"`
	ResultSHA256 string          `json:"resultSha256"`
}

// PackageSequenceActionReceipt couples the ordered package mutations and
// the immediately following holder action in one production protocol receipt.
type PackageSequenceActionReceipt struct {
	Sequence PackageSequenceReceipt `json:"sequence"`
	Action   PackageActionReceipt   `json:"action"`
}

// BackupObject is one exact file the selected Android backup transport
// retained for a package.
type BackupObject struct {
	Path    string        `json:"path"`
	SHA256  string        `json:"sha256"`
	Bytes   []byte        `json:"bytes"`
	Entries []BackupEntry `json:"entries,omitempty"`
}

// BackupEntry is one regular file decoded from Android's full-backup tar
// stream. Bytes are the exact recipient-owned payload for that archive entry.
type BackupEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Bytes  []byte `json:"bytes"`
}

// BackupReceipt records a host-triggered transfer to Android's debug-only
// local transport and the exact recipient-owned bytes read back as root.
type BackupReceipt struct {
	Package           string         `json:"package"`
	Transport         string         `json:"transport"`
	PreviousTransport string         `json:"previousTransport,omitempty"`
	BackupEnabled     bool           `json:"backupEnabled"`
	EnabledByHost     bool           `json:"enabledByHost,omitempty"`
	Recipient         string         `json:"recipient"`
	StartedAt         string         `json:"startedAt"`
	CompletedAt       string         `json:"completedAt"`
	CommandOutput     string         `json:"commandOutput"`
	Objects           []BackupObject `json:"objects"`
}

// PendingIntentProtocol is the checkpoint/readiness protocol spoken by the
// system-server PendingIntent lifecycle observer (the observer script
// itself is consumer-owned).
const PendingIntentProtocol = "android-pendingintent@1"
