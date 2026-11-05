package envcap

import (
	"context"
	"errors"
	"fmt"
	"strings"

	registry "github.com/craggy-io/capability-services/registry"

	"github.com/craggy-io/environment-services/envspec"
)

// Sentinels a Manager implementation wraps so handlers can translate
// failures into agent-facing codes without importing the manager's
// internals.
var (
	ErrNoCapacity    = errors.New("no capacity")
	ErrBackendDown   = errors.New("backend down")
	ErrUnsupported   = errors.New("unsupported")
	ErrNoStream      = errors.New("no collector provides this stream")
	ErrNoTarget      = errors.New("environment not running")
	ErrNotApplicable = errors.New("stream not available on this environment")
	ErrPreflight     = errors.New("collector preflight failed")
	ErrPolicy        = errors.New("credential policy denied")
)

// HintError carries an agent-facing next step minted below the
// capability layer: a code, one sentence, and the call that works.
type HintError struct {
	Code string
	Msg  string
	Hint string
	Err  error
}

func (e *HintError) Error() string {
	s := e.Msg
	if e.Hint != "" {
		s += " hint: " + e.Hint
	}
	return s
}

func (e *HintError) Unwrap() error { return e.Err }

// PolicyError is a credential lease denial with its rule and holder.
type PolicyError struct {
	Rule   string
	Detail string
	Holder string
}

func (e *PolicyError) Error() string {
	return fmt.Sprintf("credential policy %s: %s", e.Rule, e.Detail)
}

func (e *PolicyError) Is(target error) bool { return target == ErrPolicy }

// agentError is the agent-facing error shape: a code, one sentence,
// and a hint naming the call that would work. It never leaves the
// package; the registry wrapper converts it to a registry.Error.
type agentError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

func (e *agentError) Error() string {
	s := e.Code + ": " + e.Message
	if e.Hint != "" {
		s += " hint: " + e.Hint
	}
	return s
}

// agentErr converts any error into the agent-facing shape, preserving
// hints minted deeper in the stack. An error already in the shape
// passes through untouched.
func agentErr(err error) error {
	if err == nil {
		return nil
	}
	var ae *agentError
	if errors.As(err, &ae) {
		return ae
	}
	var he *HintError
	if errors.As(err, &he) {
		code := he.Code
		if code == "" {
			code = codeFor(err)
		}
		return &agentError{Code: code, Message: trimStack(he.Msg), Hint: he.Hint}
	}
	var pe *PolicyError
	if errors.As(err, &pe) {
		hint := "list alternatives with vault.creds.list"
		if pe.Holder != "" {
			hint = "held by " + pe.Holder + "; wait for it or choose another credential"
		}
		return &agentError{Code: "unavailable", Message: pe.Detail, Hint: hint}
	}
	return &agentError{Code: codeFor(err), Message: trimStack(err.Error())}
}

func codeFor(err error) string {
	switch {
	case errors.Is(err, envspec.ErrNotFound):
		return "not_found"
	case errors.Is(err, envspec.ErrInvalid):
		return "invalid"
	case errors.Is(err, envspec.ErrConflict):
		return "conflict"
	case errors.Is(err, envspec.ErrNotReady),
		errors.Is(err, ErrNoCapacity),
		errors.Is(err, ErrBackendDown),
		errors.Is(err, ErrUnsupported),
		errors.Is(err, ErrPolicy),
		errors.Is(err, ErrNoTarget),
		errors.Is(err, ErrNotApplicable),
		errors.Is(err, ErrPreflight):
		return "unavailable"
	default:
		return "error"
	}
}

func trimStack(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	// Sentinel prefixes ("invalid: …") duplicate the code.
	for _, p := range []string{"invalid: ", "not found: ", "conflict: ", "not ready: "} {
		s = strings.TrimPrefix(s, p)
	}
	return s
}

// registryErr converts a handler error to the coded registry form.
// Cancellations pass through untouched (the registry maps them to
// Cancelled itself); everything else goes through agentErr.
func registryErr(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	ae, ok := agentErr(err).(*agentError)
	if !ok {
		return registry.WrapCode(registry.Internal, err)
	}
	code := registry.Internal
	switch ae.Code {
	case "invalid":
		code = registry.Invalid
	case "not_found":
		code = registry.NotFound
	case "conflict":
		code = registry.Conflict
	case "unavailable":
		code = registry.Unavailable
	case "denied":
		code = registry.Denied
	}
	msg := ae.Message
	if ae.Hint != "" {
		msg += " Hint: " + ae.Hint
	}
	re := &registry.Error{Code: code, Message: msg, Err: err}
	if code == registry.Unavailable {
		re.Reason = unavailableReason(err)
	}
	return re
}

// unavailableReason splits Unavailable by cause: a full pool (wait
// or free capacity) versus a dead backend (fix the runtime).
func unavailableReason(err error) string {
	switch {
	case errors.Is(err, ErrNoCapacity):
		return registry.ReasonNoCapacity
	case errors.Is(err, ErrBackendDown):
		return registry.ReasonBackendDown
	}
	return ""
}
