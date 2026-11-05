package signalcore_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// dyingCollector starts and fails at once.
type dyingCollector struct{}

func (c *dyingCollector) Name() string { return "test.dying" }
func (c *dyingCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: "test.dying@1", Cost: "med"}}
}
func (c *dyingCollector) Params() *jsonschema.Schema { return nil }
func (c *dyingCollector) Run(context.Context, json.RawMessage, signalcore.Emitter) error {
	return errors.New("trace_pipe closed")
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// runFor drives one Hub.Run until stop is called, the way core's activity
// reconciler will. The returned func blocks until Run has returned.
func runFor(t *testing.T, hub *signalcore.Hub, col signalcore.Collector, source string, params json.RawMessage) (stop func() error) {
	t.Helper()
	before := len(hub.Running())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := hub.Run(ctx, col, source, params)
		done <- err
	}()
	// Wait until this run is visible so clock moves count as covered time.
	deadline := time.Now().Add(2 * time.Second)
	for len(hub.Running()) <= before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(hub.Running()) <= before {
		t.Fatal("run never became active")
	}
	return func() error { cancel(); return <-done }
}

func TestCoverageAndFailures(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)}
	tick := &tickCollector{}
	hub := signalcore.NewHub(signalcore.WithCollectors(tick, &dyingCollector{}), signalcore.WithClock(clk.now))
	defer hub.Close()
	t0 := clk.now()
	stop := runFor(t, hub, tick, "env_a", nil)
	clk.add(10 * time.Minute)
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	t1 := clk.now()
	clk.add(5 * time.Minute)

	sel := envspec.Selector{Streams: []string{"test.tick@1"}, Sources: []string{"env_a"}}
	status := func(from, to time.Time) envspec.Coverage {
		t.Helper()
		got := hub.Coverage(sel, from, to)
		if len(got) != 1 {
			t.Fatalf("coverage = %+v", got)
		}
		return got[0]
	}
	if c := status(t0.Add(-10*time.Minute), t0); c.Status != "not_running" {
		t.Errorf("before the run = %+v", c)
	}
	if c := status(t0, t1); c.Status != "covered" {
		t.Errorf("during the run = %+v", c)
	}
	if c := status(t0, t1.Add(5*time.Minute)); c.Status != "partial" || c.Ran != 10*time.Minute {
		t.Errorf("straddling = %+v", c)
	}
	// A named source nothing ever ran on is answered, as not_running.
	if got := hub.Coverage(envspec.Selector{Streams: []string{"test.tick@1"}, Sources: []string{"env_never"}}, t0, t1); len(got) != 1 || got[0].Status != "not_running" {
		t.Errorf("never-ran source = %+v", got)
	}

	// A collector that dies records its failure on state and coverage.
	from := clk.now()
	if _, err := hub.Run(context.Background(), &dyingCollector{}, "env_a", nil); err == nil {
		t.Fatal("dying run must return its error")
	}
	if st := hub.State("test.dying@1", "env_a"); st.Running || st.LastFailure != "trace_pipe closed" {
		t.Fatalf("dying state = %+v", st)
	}
	clk.add(time.Second)
	cov := hub.Coverage(envspec.Selector{Streams: []string{"test.dying@1"}, Sources: []string{"env_a"}}, from, clk.now())
	if len(cov) != 1 || cov[0].Failure != "trace_pipe closed" {
		t.Fatalf("dying coverage = %+v", cov)
	}
}

func TestConfiguredCoverageCannotBorrowAnotherInstance(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	tick := &tickCollector{}
	hub := signalcore.NewHub(signalcore.WithCollectors(tick), signalcore.WithClock(clk.now))
	defer hub.Close()
	a, b := json.RawMessage(`{"package":"a"}`), json.RawMessage(`{"package":"b"}`)
	start := clk.now()
	stopA := runFor(t, hub, tick, "env_a", a)
	defer stopA()
	clk.add(time.Second)
	if got := hub.ConfiguredState("test.tick@1", "env_a", b); got.Running || len(got.Collectors) != 0 {
		t.Fatalf("unstarted instance borrowed readiness: %+v", got)
	}
	if got := hub.ConfiguredState("test.tick@1", "env_a", a); !got.Running || len(got.Collectors) != 1 {
		t.Fatalf("started instance missing: %+v", got)
	}
	sel := envspec.Selector{Streams: []string{"test.tick@1"}, Sources: []string{"env_a"}, Params: b}
	if got := hub.ConfiguredCoverage(sel, start, clk.now()); len(got) != 1 || got[0].Status != "not_running" {
		t.Fatalf("unstarted instance borrowed coverage: %+v", got)
	}
	stopB := runFor(t, hub, tick, "env_a", b)
	defer stopB()
	clk.add(time.Second)
	if got := hub.ConfiguredCoverage(sel, start, clk.now()); len(got) != 1 || got[0].Status != "partial" || got[0].Ran != time.Second {
		t.Fatalf("late instance borrowed earlier coverage: %+v", got)
	}
	if got := hub.Coverage(sel, start, clk.now()); len(got) != 1 || got[0].Status != "covered" {
		t.Fatalf("aggregate inventory changed: %+v", got)
	}
	sel.Collectors = hub.ConfiguredState("test.tick@1", "env_a", b).Collectors
	if got := hub.Coverage(sel, start, clk.now()); len(got) != 1 || got[0].Status != "partial" {
		t.Fatalf("selected collector borrowed another instance: %+v", got)
	}
	sel.Collectors = []string{"missing"}
	if got := hub.ConfiguredCoverage(sel, start, clk.now()); len(got) != 1 || got[0].Status != "not_running" {
		t.Fatalf("missing collector borrowed coverage: %+v", got)
	}
	sel.Collectors = nil
	sel.Params = nil
	if got := hub.ConfiguredCoverage(sel, start, clk.now()); len(got) != 1 || got[0].Status != "not_running" {
		t.Fatalf("default instance borrowed configured coverage: %+v", got)
	}
}
