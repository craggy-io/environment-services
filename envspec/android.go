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

// ChallengeHostAlias is the address the standard Android emulator routes
// to the host's loopback interface. Challenge servers advertise it by
// default; physical devices or other bridges need an explicit
// AdvertiseHost.
const ChallengeHostAlias = "10.0.2.2"

// TLSFailureUntrustedChain is the single validation failure a TLS
// challenge presents: the leaf name matches the expected host, but its
// issuing CA is ephemeral and outside every trust store.
const TLSFailureUntrustedChain = "untrusted-chain"

// TLSChallengeOptions configure a host-owned TLS peer for a device.
// ExpectedHost is the name (DNS or IP) the leaf certificate is issued for
// and defaults to the advertise host. AdvertiseHost is the host the
// device dials and defaults to ChallengeHostAlias. Response is the body
// returned to a client that completes the handshake.
type TLSChallengeOptions struct {
	ExpectedHost  string
	AdvertiseHost string
	Response      []byte
}

// TLSSession is one server-observed completed TLS connection.
type TLSSession struct {
	At          string `json:"at"`
	RemoteAddr  string `json:"remoteAddr"`
	Version     uint16 `json:"version"`
	CipherSuite uint16 `json:"cipherSuite"`
}

// TLSChallengeReceipt fixes the challenge's presented certificate and
// validation failure independently of the app result; Sessions are the
// connections that completed a handshake against it (a client that
// correctly rejects the chain completes none).
type TLSChallengeReceipt struct {
	URL               string       `json:"url"`
	ExpectedHost      string       `json:"expectedHost"`
	Failure           string       `json:"failure"`
	CertificateSHA256 string       `json:"certificateSha256"`
	CertificateDER    []byte       `json:"certificateDer"`
	StartedAt         string       `json:"startedAt"`
	Sessions          []TLSSession `json:"sessions"`
}

// CleartextChallengeOptions configure a host-owned HTTP peer for a
// device. AdvertiseHost defaults to ChallengeHostAlias.
type CleartextChallengeOptions struct {
	AdvertiseHost string
	Response      []byte
}

// CleartextRequest is one server-observed HTTP request. The raw challenge
// path is deliberately omitted: only its digest and whether it matched
// are retained.
type CleartextRequest struct {
	At         string `json:"at"`
	RemoteAddr string `json:"remoteAddr"`
	Method     string `json:"method"`
	PathSHA256 string `json:"pathSha256"`
	Matched    bool   `json:"matched"`
}

// CleartextChallengeReceipt records an HTTP-only endpoint carrying an
// unguessable path and every request it observed.
type CleartextChallengeReceipt struct {
	URL        string             `json:"url"`
	PathSHA256 string             `json:"pathSha256"`
	StartedAt  string             `json:"startedAt"`
	Requests   []CleartextRequest `json:"requests"`
}
