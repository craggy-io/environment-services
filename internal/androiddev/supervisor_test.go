package androiddev

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

type compScript struct {
	probeErr error
	repairs  int
	probeSeq []error // consumed per Probe call; then probeErr
}

func (c *compScript) component(name string) Component {
	return Component{Name: name,
		Probe: func(ctx context.Context) error {
			if len(c.probeSeq) > 0 {
				err := c.probeSeq[0]
				c.probeSeq = c.probeSeq[1:]
				return err
			}
			return c.probeErr
		},
		Repair: func(ctx context.Context) error {
			c.repairs++
			c.probeErr = nil
			return nil
		}}
}

func TestSupervisorCheckDebouncesThenReportsFalse(t *testing.T) {
	s := newSupervisor(SupervisorHooks{})
	cs := &compScript{probeErr: errors.New("down")}
	s.Register(cs.component("frida"))
	cond, _ := s.Check(context.Background(), "frida")
	if cond.Status != envspec.ConditionUnknown {
		t.Fatalf("first failure = %s, want Unknown (debounce)", cond.Status)
	}
	cond, _ = s.Check(context.Background(), "frida")
	if cond.Status != envspec.ConditionFalse || cond.Reason != ReasonProbeFailed {
		t.Fatalf("second failure = %+v, want False/probe_failed", cond)
	}
}

func TestSupervisorRepairBudgetExhausts(t *testing.T) {
	s := newSupervisor(SupervisorHooks{})
	s.SetLimits(2, time.Minute)
	// Probe always fails, repair "succeeds" but probe still fails afterwards.
	s.Register(Component{Name: "frida",
		Probe:  func(ctx context.Context) error { return errors.New("still down") },
		Repair: func(ctx context.Context) error { return nil }})
	ctx := context.Background()
	_, _ = s.Check(ctx, "frida")
	_, _ = s.Check(ctx, "frida")
	for i := 0; i < 2; i++ {
		if err := s.RepairNow(ctx, "frida", "cap"); err == nil {
			t.Fatalf("repair %d succeeded despite dead probe", i)
		}
	}
	err := s.RepairNow(ctx, "frida", "cap")
	if err == nil || !contains(err.Error(), "degraded after 2 repairs") {
		t.Fatalf("third repair = %v, want budget-exhausted", err)
	}
	st := s.Statuses()
	if len(st) != 1 || st[0].State != adapter.ComponentDegraded {
		t.Fatalf("state = %+v, want degraded", st)
	}
}

func TestSupervisorSuspectStopsRepairing(t *testing.T) {
	now := time.Now()
	s := newSupervisor(SupervisorHooks{})
	s.now = func() time.Time { return now }
	repairs := 0
	s.Register(Component{Name: "frida",
		Probe:  func(ctx context.Context) error { return errors.New("killed") },
		Repair: func(ctx context.Context) error { repairs++; return errors.New("cannot") }})
	s.PackageStarts = func() map[string]time.Time { return map[string]time.Time{"lab.target": now} }
	ctx := context.Background()
	// Two failures each within 10s of lab.target starting -> suspect.
	_ = s.RepairNow(ctx, "frida", "")
	_ = s.RepairNow(ctx, "frida", "")
	st := s.Statuses()
	if len(st) != 1 || st[0].State != adapter.ComponentSuspect {
		t.Fatalf("state = %+v, want suspect", st)
	}
	before := repairs
	cond, _ := s.Check(ctx, "frida")
	if cond.Status != envspec.ConditionFalse || cond.Reason != ReasonDiesOnStart {
		t.Fatalf("suspect check = %+v", cond)
	}
	if err := s.RepairNow(ctx, "frida", ""); err == nil {
		t.Fatal("suspect repair ran")
	}
	if repairs != before {
		t.Fatalf("repairs ran while suspect: %d -> %d", before, repairs)
	}
	s.ResetSuspect("frida")
	if s.Statuses()[0].State != adapter.ComponentOK {
		t.Fatalf("after reset = %+v", s.Statuses())
	}
}

type denyPolicy struct{ asked []string }

func (p *denyPolicy) AllowMutation(envID, kind string) (bool, string) {
	p.asked = append(p.asked, kind)
	return false, "experiment in progress"
}

func TestSupervisorMutationPolicyHoldsRepair(t *testing.T) {
	s := newSupervisor(SupervisorHooks{})
	pol := &denyPolicy{}
	s.SetMutationPolicy(pol, "env_1")
	cs := &compScript{probeErr: errors.New("down")}
	s.Register(cs.component("frida"))
	err := s.RepairNow(context.Background(), "frida", "")
	if !errors.Is(err, ErrHeld) {
		t.Fatalf("repair under hold = %v, want ErrHeld", err)
	}
	if cs.repairs != 0 {
		t.Fatalf("repair ran under hold (%d)", cs.repairs)
	}
	// Ensure is gated the same way.
	if err := s.Ensure(context.Background(), "frida"); !errors.Is(err, ErrHeld) {
		t.Fatalf("ensure under hold = %v, want ErrHeld", err)
	}
	if len(pol.asked) == 0 || pol.asked[0] != "repair" {
		t.Fatalf("policy asked = %v", pol.asked)
	}
}

func TestSupervisorCaptureRunsBeforeRepair(t *testing.T) {
	var order []string
	s := newSupervisor(SupervisorHooks{Capture: func(f FailureInfo) string {
		order = append(order, "capture:"+f.Component)
		return "cap_1"
	}})
	s.Register(Component{Name: "root",
		Probe: func(ctx context.Context) error {
			if len(order) > 0 && order[len(order)-1] == "repair" {
				return nil
			}
			return errors.New("not root")
		},
		Repair: func(ctx context.Context) error { order = append(order, "repair"); return nil }})
	if err := s.Ensure(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "capture:root" || order[1] != "repair" {
		t.Fatalf("order = %v, want capture before repair", order)
	}
	if s.Statuses()[0].State != adapter.ComponentOK || s.Statuses()[0].Repairs != 1 {
		t.Fatalf("state = %+v", s.Statuses())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
