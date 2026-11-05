package core

import (
	"context"
	"errors"
	"fmt"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

// Sentinel errors. envspec sentinels are re-exported by wrapping at use
// sites; these are core's finer-grained causes.
var (
	ErrNoCapacity      = errors.New("no capacity")
	ErrQuota           = errors.New("quota exceeded")
	ErrClosed          = errors.New("closed")
	ErrScopeClosed     = errors.New("scope closed")
	ErrEnvironmentLost = errors.New("environment lost")
	ErrStoreUnhealthy  = errors.New("durable store unhealthy: refusing state-changing work")
	ErrStaleHandle     = fmt.Errorf("%w: stale handle", envspec.ErrConflict)

	// Failure causes marked onto errors so failureReason can name them
	// without reading message text.
	ErrBackendDown      = adapter.ErrBackendDown
	ErrImageUnavailable = errors.New("image unavailable")
	ErrPreparation      = errors.New("preparation failed")
	ErrCredentials      = errors.New("credential injection failed")
	ErrDriver           = errors.New("driver error")
)

// MarkAs returns err marked so errors.Is(err, kind) holds, keeping its
// message and chain. Already-marked and nil errors pass through.
func MarkAs(kind, err error) error {
	if err == nil || errors.Is(err, kind) {
		return err
	}
	return &markedError{kind: kind, err: err}
}

type markedError struct{ kind, err error }

func (e *markedError) Error() string        { return e.err.Error() }
func (e *markedError) Unwrap() error        { return e.err }
func (e *markedError) Is(target error) bool { return target == e.kind }

// HintError carries an agent-facing next step: a code, one sentence,
// and a hint naming the call that would work.
type HintError struct {
	Code string
	Msg  string
	Hint string
	Err  error
}

func (e *HintError) Error() string {
	s := e.Msg
	if e.Code != "" {
		s = e.Code + ": " + s
	}
	if e.Hint != "" {
		s += " hint: " + e.Hint
	}
	return s
}

func (e *HintError) Unwrap() error { return e.Err }

// WithHint wraps err with a code and an agent-facing hint.
func WithHint(code string, err error, hintFormat string, args ...any) error {
	if err == nil {
		return nil
	}
	return &HintError{Code: code, Msg: err.Error(), Hint: fmt.Sprintf(hintFormat, args...), Err: err}
}

// Invalidf builds an envspec.ErrInvalid error.
func Invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", envspec.ErrInvalid, fmt.Sprintf(format, args...))
}

// Reason codes: stable lower_snake_case causes in lifecycle events,
// classified from typed errors only, never from message text.
const (
	ReasonCancelled         = "cancelled"
	ReasonTimeout           = "timeout"
	ReasonScopeClosed       = "scope_closed"
	ReasonEnvironmentLost   = "environment_lost"
	ReasonNoCapacity        = "no_capacity"
	ReasonQuotaExceeded     = "quota_exceeded"
	ReasonBackendDown       = "backend_down"
	ReasonImageUnavailable  = "image_unavailable"
	ReasonPreparationFailed = "preparation_failed"
	ReasonCredentialsFailed = "credentials_failed"
	ReasonNotFound          = "not_found"
	ReasonInvalid           = "invalid"
	ReasonUnsupported       = "unsupported"
	ReasonNotReady          = "not_ready"
	ReasonConflict          = "conflict"
	ReasonClosed            = "closed"
	ReasonDriverError       = "driver_error"
	ReasonUnknown           = "unknown"
	ReasonExperimentHold    = "experiment_hold"
	ReasonStaleObservation  = "stale_observation"
)

var reasonOrder = []struct {
	err  error
	code string
}{
	{context.Canceled, ReasonCancelled},
	{context.DeadlineExceeded, ReasonTimeout},
	{ErrEnvironmentLost, ReasonEnvironmentLost},
	{ErrScopeClosed, ReasonScopeClosed},
	{ErrBackendDown, ReasonBackendDown},
	{ErrNoCapacity, ReasonNoCapacity},
	{ErrQuota, ReasonQuotaExceeded},
	{ErrImageUnavailable, ReasonImageUnavailable},
	{ErrPreparation, ReasonPreparationFailed},
	{ErrCredentials, ReasonCredentialsFailed},
	{envspec.ErrNotFound, ReasonNotFound},
	{envspec.ErrConflict, ReasonConflict},
	{envspec.ErrNotReady, ReasonNotReady},
	{envspec.ErrInvalid, ReasonInvalid},
	{adapter.ErrUnsupported, ReasonUnsupported},
	{ErrClosed, ReasonClosed},
	{ErrDriver, ReasonDriverError},
}

// FailureReason classifies err into a stable reason code ("" for nil).
func FailureReason(err error) string {
	if err == nil {
		return ""
	}
	for _, r := range reasonOrder {
		if errors.Is(err, r.err) {
			return r.code
		}
	}
	return ReasonUnknown
}
