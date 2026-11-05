package envtest_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/environments"
	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/envtest"
)

func TestFakeObservationPublishQueryPinCapture(t *testing.T) {
	var obs environments.Observation = envtest.NewFakeObservation()
	fake := obs.(*envtest.FakeObservation)
	ctx := context.Background()
	now := time.Now()

	if _, err := fake.Pin("android.logcat@1", "env_1", nil); err == nil {
		t.Fatal("Pin of unregistered stream succeeded")
	}
	fake.Publish(envspec.SignalEvent{Stream: "android.logcat@1", Source: "env_1",
		Collector: "logcat", Time: now, Data: map[string]any{"line": "a"}})
	fake.Publish(envspec.SignalEvent{Stream: "android.logcat@1", Source: "env_1",
		Collector: "logcat", Time: now, Data: map[string]any{"line": "b"}})
	fake.Publish(envspec.SignalEvent{Stream: "host.metrics@1", Source: "host", Time: now,
		Data: envspec.HostSnapshot{CPUs: 4}})

	unpin, err := fake.Pin("android.logcat@1", "env_1", nil)
	if err != nil {
		t.Fatalf("Pin: %v", err)
	}
	if st := fake.ConfiguredState("android.logcat@1", "env_1", nil); !st.Running {
		t.Fatalf("ConfiguredState = %+v, want running", st)
	}
	if got := fake.Running(); len(got) != 1 || got[0].Source != "env_1" {
		t.Fatalf("Running = %+v", got)
	}
	cov := fake.ConfiguredCoverage(envspec.Selector{Streams: []string{"android.logcat@1"}, Sources: []string{"env_1"}},
		now.Add(-time.Minute), now)
	if len(cov) != 1 || cov[0].Status != "covered" {
		t.Fatalf("ConfiguredCoverage = %+v", cov)
	}

	evs, q, err := fake.Query(envspec.Selector{Streams: []string{"android.*@1"}},
		now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || len(evs) != 2 || q.Received != 2 || !q.LossFree() {
		t.Fatalf("Query = %d %+v %v", len(evs), q, err)
	}
	if evs[0].Seq == 0 || evs[1].Seq <= evs[0].Seq {
		t.Fatalf("Publish did not fill monotonic seq: %d, %d", evs[0].Seq, evs[1].Seq)
	}

	before := fake.Running()
	unpin()
	unpin() // idempotent
	if st := fake.ConfiguredState("android.logcat@1", "env_1", nil); st.Running {
		t.Fatal("still running after unpin")
	}
	if _, err := fake.PinFresh(ctx, "android.logcat@1", "env_1", nil); err != nil {
		t.Fatalf("PinFresh: %v", err)
	}
	after := fake.Running()
	if len(after) != 1 || after[0].ID == before[0].ID {
		t.Fatalf("PinFresh kept collector identity: before=%+v after=%+v", before, after)
	}
	if after[0].Name != "android.logcat@1" {
		t.Fatalf("Running Name = %q, want the stream name", after[0].Name)
	}

	cap, err := fake.Capture(envspec.Selector{Sources: []string{"env_1"}},
		now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil || cap.ID() == "" {
		t.Fatalf("Capture = %v, %v", cap, err)
	}
	files := map[string][]byte{}
	for e, err := range cap.Source().Entries(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		rc, err := cap.Source().Open(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		files[e.Path] = b
	}
	if _, ok := files["range.json"]; !ok {
		t.Fatalf("capture files = %v, want range.json", files)
	}
	if _, ok := files["env_1/android.logcat@1.jsonl"]; !ok {
		t.Fatalf("capture files = %v, want the logcat segment", files)
	}
	cap.Close()
}

func TestFakeServiceProvider(t *testing.T) {
	p := envtest.NewFakeServiceProvider()
	ctx := context.Background()
	lease, err := p.AcquireService(ctx, envspec.Owner{ID: "user:test"},
		envspec.ServiceRequest{Key: "k", Service: envspec.ServiceSpec{Name: "ghidra", Version: "1"}})
	if err != nil {
		t.Fatalf("AcquireService: %v", err)
	}
	info := lease.ServiceInfo()
	if info.State != "running" || lease.ID() != info.Environment {
		t.Fatalf("lease = %s info=%+v", lease.ID(), info)
	}
	unlock, err := lease.Lock(ctx)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	unlock()
	if err := lease.Push(ctx, envspec.MemSource(map[string][]byte{"a": []byte("a")}), "/work"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := p.StopService(ctx, info.ID, "stale-gen"); err == nil {
		t.Fatal("StopService with stale generation succeeded")
	}
	if err := p.StopService(ctx, info.ID, info.Generation); err != nil {
		t.Fatalf("StopService: %v", err)
	}
	if err := lease.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, _, _, err := lease.Exec(ctx, envspec.Cmd{Argv: []string{"true"}}); err == nil {
		t.Fatal("Exec after stop+release succeeded")
	}
	if err := p.StopService(ctx, "svc_missing", "gen-1"); err == nil {
		t.Fatal("StopService(unknown) succeeded")
	}
}
