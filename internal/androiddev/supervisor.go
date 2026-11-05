package androiddev

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

// Component state reasons (stable lower_snake_case, carried on
// conditions and component transitions).
const (
	ReasonProbeFailed           = "probe_failed"
	ReasonRepaired              = "repaired"
	ReasonRepairFailed          = "repair_failed"
	ReasonRepairIneffective     = "repair_ineffective"
	ReasonRepairBudgetExhausted = "repair_budget_exhausted"
	ReasonDiesOnStart           = "dies_on_start"
	ReasonRecovered             = "recovered"
	ReasonSuspectReset          = "suspect_reset"
	ReasonExperimentHold        = "experiment_hold"
)

// ErrHeld marks a repair denied by the mutation policy (experiment
// hold): the component stays as observed, nothing was mutated.
var ErrHeld = errors.New("mutation held by policy")

// Component is one managed instrument: a desired state, a probe and
// a repair. Repairs never touch the app under test.
type Component struct {
	Name  string
	Probe func(ctx context.Context) error
	// Repair fixes the instrument only. It must never restart, clear
	// or force-stop the app under test, reboot, or restore a snapshot.
	Repair func(ctx context.Context) error
}

// FailureInfo is captured before any repair runs (record before repair).
type FailureInfo struct {
	Component string
	Error     string
	Package   string // foreground package at failure time
}

// SupervisorHooks let the owner record evidence and lifecycle around
// repairs. Capture should pin logcat/stderr/tombstones and return a
// capture ID (may be empty). Transition receives every component
// state change, in order, with the supervisor's lock held: it must
// not call back into the supervisor.
type SupervisorHooks struct {
	Capture    func(f FailureInfo) string
	Transition func(c adapter.ComponentChange)
}

type compState struct {
	status      envspec.ComponentState
	attempts    []time.Time // repair attempt times within the window
	consecFails int         // consecutive probe failures (debounce)
	lastErr     string
	// suspect correlation: package → recent failure-after-start times
	suspectPkg  string
	failNearPkg map[string][]time.Time
}

// Supervisor keeps the managed components' health: probe/repair with
// evidence captured before repair, a repair budget, and suspect
// correlation that stops repairing components a target keeps killing
// (possible anti-tamper) so the evidence survives. It owns no loop:
// core's activity reconciler drives Check/Repair through
// Device.Probes() (with the core MutationPolicy gate in front of
// Repair), and Ensure gives dependent operations the same path
// synchronously. State lives here in memory; core mirrors it into
// activities/conditions from the probe results and transitions.
type Supervisor struct {
	hooks    SupervisorHooks
	maxTries int           // default 3
	window   time.Duration // default 5m
	now      func() time.Time
	// Foreground reports the current foreground package (correlation).
	Foreground func() string
	// PackageStarts reports recent package start times (correlation).
	PackageStarts func() map[string]time.Time

	policy envspec.MutationPolicy
	envID  string

	mu     sync.Mutex
	comps  map[string]*Component
	states map[string]*compState
}

func newSupervisor(hooks SupervisorHooks) *Supervisor {
	return &Supervisor{hooks: hooks, maxTries: 3, window: 5 * time.Minute,
		now:    time.Now,
		comps:  map[string]*Component{},
		states: map[string]*compState{}}
}

// SetLimits overrides the repair budget (RepairPolicy).
func (s *Supervisor) SetLimits(maxAttempts int, window time.Duration) {
	if maxAttempts > 0 {
		s.maxTries = maxAttempts
	}
	if window > 0 {
		s.window = window
	}
}

// SetMutationPolicy installs the experiment-hold gate consulted
// before every Repair this supervisor runs itself (the Ensure path).
// Core-driven repairs are additionally gated by core before Repair
// is invoked. A denial leaves the component untouched (ErrHeld).
func (s *Supervisor) SetMutationPolicy(p envspec.MutationPolicy, envID string) {
	s.mu.Lock()
	s.policy, s.envID = p, envID
	s.mu.Unlock()
}

func (s *Supervisor) mutationAllowed() (bool, string) {
	s.mu.Lock()
	p, id := s.policy, s.envID
	s.mu.Unlock()
	if p == nil {
		return true, ""
	}
	return p.AllowMutation(id, "repair")
}

// Register adds a component.
func (s *Supervisor) Register(c Component) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cc := c
	s.comps[c.Name] = &cc
	s.states[c.Name] = &compState{
		status:      envspec.ComponentState{Name: c.Name, State: adapter.ComponentOK, Since: s.now()},
		failNearPkg: map[string][]time.Time{},
	}
}

// Names lists registered components in stable order.
func (s *Supervisor) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.comps))
	for n := range s.comps {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Statuses snapshots component states for Info.
func (s *Supervisor) Statuses() []envspec.ComponentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]envspec.ComponentState, 0, len(s.states))
	for _, name := range s.sortedNamesLocked() {
		out = append(out, s.states[name].status)
	}
	return out
}

func (s *Supervisor) sortedNamesLocked() []string {
	out := make([]string, 0, len(s.states))
	for n := range s.states {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// setState moves a component to ch.To and reports the transition; ch
// carries the reason and evidence, setState fills in the rest. Called
// with s.mu held. Staying in the same state reports nothing.
func (s *Supervisor) setState(name string, ch adapter.ComponentChange) {
	st := s.states[name]
	if ch.Error != "" {
		st.status.LastFailure = ch.Error
	}
	if st.status.State == ch.To {
		return
	}
	ch.Component, ch.From, ch.Repairs = name, st.status.State, st.status.Repairs
	st.status.State = ch.To
	st.status.Since = s.now()
	if s.hooks.Transition != nil {
		s.hooks.Transition(ch)
	}
}

func (s *Supervisor) component(name string) (*Component, *compState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.comps[name]
	return c, s.states[name], ok
}

// Check probes one component and returns its condition for core's
// activity engine. A first consecutive failure reports Unknown, not
// False: background probes debounce (a single failure is often a
// transient — guest busy under instrumentation), and core only
// repairs on False. Ensure (pre-op) stays immediate. A suspect
// component reports False with the suspect reason and is never
// repaired until ResetSuspect.
func (s *Supervisor) Check(ctx context.Context, name string) (envspec.Condition, error) {
	c, st, ok := s.component(name)
	if !ok {
		return envspec.Condition{Name: name, Status: envspec.ConditionUnknown, Reason: "unmanaged"}, nil
	}
	cond := envspec.Condition{Name: name, ObservedAt: s.now()}
	s.mu.Lock()
	suspect := st.status.State == adapter.ComponentSuspect
	suspectPkg := st.suspectPkg
	s.mu.Unlock()
	if suspect {
		cond.Status, cond.Reason = envspec.ConditionFalse, ReasonDiesOnStart
		cond.Detail = fmt.Sprintf("%s keeps getting killed near package %s (possible anti-tamper); not repairing so the evidence survives", name, suspectPkg)
		return cond, nil
	}
	if err := c.Probe(ctx); err != nil {
		s.mu.Lock()
		st.consecFails++
		st.lastErr = err.Error()
		fails := st.consecFails
		s.mu.Unlock()
		if fails < 2 {
			cond.Status, cond.Reason = envspec.ConditionUnknown, ReasonProbeFailed
		} else {
			cond.Status, cond.Reason = envspec.ConditionFalse, ReasonProbeFailed
		}
		cond.Detail = err.Error()
		return cond, nil
	}
	s.mu.Lock()
	st.consecFails = 0
	st.lastErr = ""
	if st.status.State == adapter.ComponentRepairing || st.status.State == adapter.ComponentDegraded {
		s.setState(name, adapter.ComponentChange{To: adapter.ComponentOK, Reason: ReasonRecovered})
	}
	s.mu.Unlock()
	cond.Status, cond.Reason = envspec.ConditionTrue, "ok"
	return cond, nil
}

// CaptureBeforeRepair records evidence for a pending repair (core's
// Probe.BeforeRepair). Its result is cited on the transition.
func (s *Supervisor) CaptureBeforeRepair(name string) string {
	_, st, ok := s.component(name)
	if !ok || s.hooks.Capture == nil {
		return ""
	}
	s.mu.Lock()
	lastErr := st.lastErr
	s.mu.Unlock()
	pkg := ""
	if s.Foreground != nil {
		pkg = s.Foreground()
	}
	return s.hooks.Capture(FailureInfo{Component: name, Error: lastErr, Package: pkg})
}

// RepairNow runs one component's repair path: suspect correlation,
// budget, repair-the-instrument-only, re-probe. Evidence capture is
// the caller's step (CaptureBeforeRepair); captureID is cited on the
// transitions. The mutation policy is consulted first: a denial
// returns ErrHeld and mutates nothing.
func (s *Supervisor) RepairNow(ctx context.Context, name, captureID string) error {
	c, st, ok := s.component(name)
	if !ok {
		return nil
	}
	if allowed, reason := s.mutationAllowed(); !allowed {
		return fmt.Errorf("%w: %s repair held: %s", ErrHeld, name, reason)
	}
	s.mu.Lock()
	perr := st.lastErr
	if st.status.State == adapter.ComponentSuspect {
		pkg := st.suspectPkg
		s.mu.Unlock()
		return fmt.Errorf("%s keeps getting killed near package %s (possible anti-tamper); not repairing so the evidence survives", name, pkg)
	}
	now := s.now()
	cutoff := now.Add(-s.window)
	kept := st.attempts[:0]
	for _, t := range st.attempts {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	st.attempts = kept
	s.mu.Unlock()

	pkg := ""
	if s.Foreground != nil {
		pkg = s.Foreground()
	}
	// Correlate: failures within 10s of the same package starting,
	// twice, becomes suspect and repairs stop for that package.
	if s.PackageStarts != nil && name == "frida" {
		for p, started := range s.PackageStarts() {
			if gap := now.Sub(started); gap >= 0 && gap <= 10*time.Second {
				s.mu.Lock()
				st.failNearPkg[p] = append(st.failNearPkg[p], now)
				recent := 0
				for _, t := range st.failNearPkg[p] {
					if now.Sub(t) < 10*time.Minute {
						recent++
					}
				}
				if recent >= 2 {
					st.suspectPkg = p
					s.setState(name, adapter.ComponentChange{To: adapter.ComponentSuspect, Reason: ReasonDiesOnStart,
						Error: perr, Capture: captureID, Package: p})
					s.mu.Unlock()
					return fmt.Errorf("%s keeps getting killed when %s starts (possible anti-tamper); left down so the evidence survives", name, p)
				}
				s.mu.Unlock()
			}
		}
	}

	s.mu.Lock()
	if len(st.attempts) >= s.maxTries {
		s.setState(name, adapter.ComponentChange{To: adapter.ComponentDegraded, Reason: ReasonRepairBudgetExhausted,
			Error: perr, Capture: captureID, Package: pkg})
		s.mu.Unlock()
		return fmt.Errorf("%s is degraded after %d repairs in %s: %s (a reboot might help but repairs never reboot by default)",
			name, s.maxTries, s.window, perr)
	}
	st.attempts = append(st.attempts, now)
	st.status.Repairs++
	s.setState(name, adapter.ComponentChange{To: adapter.ComponentRepairing, Reason: ReasonProbeFailed,
		Error: perr, Capture: captureID, Package: pkg})
	s.mu.Unlock()

	// Repair the instrument, never the target (Component.Repair contract).
	if err := c.Repair(ctx); err != nil {
		s.mu.Lock()
		s.setState(name, adapter.ComponentChange{To: adapter.ComponentDegraded, Reason: ReasonRepairFailed,
			Error: err.Error(), Capture: captureID})
		s.mu.Unlock()
		return fmt.Errorf("%s repair failed: %w", name, err)
	}
	if err := c.Probe(ctx); err != nil {
		s.mu.Lock()
		s.setState(name, adapter.ComponentChange{To: adapter.ComponentDegraded, Reason: ReasonRepairIneffective,
			Error: err.Error(), Capture: captureID})
		s.mu.Unlock()
		return fmt.Errorf("%s still broken after repair: %w", name, err)
	}
	s.mu.Lock()
	st.consecFails = 0
	st.lastErr = ""
	s.setState(name, adapter.ComponentChange{To: adapter.ComponentOK, Reason: ReasonRepaired, Capture: captureID})
	s.mu.Unlock()
	return nil
}

// Ensure probes a component before a dependent operation and repairs
// it first when broken, so agents don't debug frida-server. It is
// immediate (no debounce) and passes the same capture-before-repair
// and mutation-policy gates as the core-driven path. A degraded or
// suspect component fails fast; an unmanaged name is a no-op.
func (s *Supervisor) Ensure(ctx context.Context, name string) error {
	c, st, ok := s.component(name)
	if !ok {
		return nil
	}
	if err := c.Probe(ctx); err == nil {
		s.mu.Lock()
		st.consecFails = 0
		st.lastErr = ""
		if st.status.State != adapter.ComponentSuspect && st.status.State != adapter.ComponentOK {
			s.setState(name, adapter.ComponentChange{To: adapter.ComponentOK, Reason: ReasonRecovered})
		}
		s.mu.Unlock()
		return nil
	} else {
		s.mu.Lock()
		st.lastErr = err.Error()
		s.mu.Unlock()
		capID := s.CaptureBeforeRepair(name)
		return s.RepairNow(ctx, name, capID)
	}
}

// ResetSuspect clears a suspect component (new package lifetime).
func (s *Supervisor) ResetSuspect(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.states[name]; ok && st.status.State == adapter.ComponentSuspect {
		st.suspectPkg = ""
		st.failNearPkg = map[string][]time.Time{}
		st.attempts = nil
		s.setState(name, adapter.ComponentChange{To: adapter.ComponentOK, Reason: ReasonSuspectReset})
	}
}
