package docker

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/xfer"
)

func newTestAdapter() (*Adapter, *fakeEngine) {
	f := newFakeEngine()
	return &Adapter{cli: f, ManagerID: "mgr-1", SidecarImage: "mitmproxy/mitmproxy:latest"}, f
}

const testEnvID = "env_01a18cca-55a3-7000-a5cf-71f7040a672c"

func testSpec() envspec.Spec {
	return envspec.Spec{Kind: envspec.Container, Profile: "tiny", Image: "busybox:latest",
		Network: envspec.NetworkNone, Resources: envspec.Resources{CPUs: 1, MemoryMB: 256},
		Labels: map[string]string{"team": "red"}}
}

func TestCreateUsesCraggyIdentitiesAndIsIdempotent(t *testing.T) {
	a, f := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID, Generation: 1}
	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := inst.Ref()
	wantName := adapter.NameFor(testEnvID)
	if ref.Name != wantName || !strings.HasPrefix(ref.Name, "craggy-") {
		t.Fatalf("ref name = %q, want %q", ref.Name, wantName)
	}
	fc, err := f.find(ref.External)
	if err != nil {
		t.Fatal(err)
	}
	labels := fc.cfg.Labels
	for k, want := range map[string]string{
		"craggy.env": testEnvID, "craggy.manager": "mgr-1", "craggy.profile": "tiny", "craggy.label.team": "red",
	} {
		if labels[k] != want {
			t.Errorf("label %s = %q, want %q (all: %v)", k, labels[k], want, labels)
		}
	}
	for k := range labels {
		if !strings.HasPrefix(k, "craggy.") {
			t.Errorf("non-craggy identity label created: %s", k)
		}
	}
	if !f.networks[networkNameFor(testEnvID)] {
		t.Errorf("per-env network %q not created: %v", networkNameFor(testEnvID), f.networks)
	}
	if fc.hostCfg == nil || len(fc.hostCfg.CapDrop) != 1 || fc.hostCfg.CapDrop[0] != "ALL" {
		t.Errorf("cap-drop not set: %+v", fc.hostCfg)
	}

	// Retry (crash-after-create rediscovery): same container, no second create.
	again, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref().External != ref.External {
		t.Fatalf("retry created a second container: %q vs %q", again.Ref().External, ref.External)
	}
	if f.creates != 1 {
		t.Fatalf("creates = %d, want 1", f.creates)
	}
}

func TestObservePresenceMapping(t *testing.T) {
	a, f := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}

	// Nothing created: definitive 404 -> Absent (gate §10 #5 counterpart).
	obs, err := a.Observe(ctx, op, adapter.Ref{EnvID: testEnvID})
	if err != nil || obs.Presence != adapter.Absent {
		t.Fatalf("missing container: presence=%s err=%v, want Absent", obs.Presence, err)
	}

	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	obs, err = a.Observe(ctx, op, inst.Ref())
	if err != nil || obs.Presence != adapter.Present {
		t.Fatalf("created container: presence=%s err=%v, want Present", obs.Presence, err)
	}
	if obs.Image != "sha256:busyboxid" {
		t.Errorf("image = %q", obs.Image)
	}
	// By deterministic name only (crash before tx2 recorded the ID, §10 #3).
	obs, err = a.Observe(ctx, op, adapter.Ref{EnvID: testEnvID, Name: adapter.NameFor(testEnvID)})
	if err != nil || obs.Presence != adapter.Present || obs.Ref.External != inst.Ref().External {
		t.Fatalf("by name: %+v %v", obs, err)
	}

	// Backend down: Unknown, never Absent (§10 #5), and Open fails.
	f.down = true
	obs, err = a.Observe(ctx, op, inst.Ref())
	if obs.Presence != adapter.Unknown {
		t.Fatalf("backend down: presence=%s, want Unknown (err %v)", obs.Presence, err)
	}
	if obs.Detail == "" {
		t.Error("backend down: Detail must say why Unknown")
	}
	if _, err := a.Open(ctx, op, inst.Ref()); err == nil {
		t.Error("Open with backend down must fail")
	}
	if err := a.Check(ctx); err == nil {
		t.Error("Check with backend down must fail")
	}
	f.down = false

	// Other inspect failure is also Unknown, not Absent.
	f.inspectErr = errors.New("daemon exploded")
	obs, _ = a.Observe(ctx, op, inst.Ref())
	if obs.Presence != adapter.Unknown {
		t.Fatalf("inspect failure: presence=%s, want Unknown", obs.Presence)
	}
	f.inspectErr = nil
}

func TestDisposeRemovesContainerSidecarsAndNetwork(t *testing.T) {
	a, f := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}
	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// A sidecar for the same env is removed with its target.
	f.seed(adapter.NameFor(testEnvID)+"-proxy", map[string]string{
		LabelEnv: testEnvID, LabelManager: "mgr-1", LabelSidecar: "proxy"})

	if err := inst.Dispose(ctx, op, adapter.Disconnect); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Disconnect on created container = %v, want ErrUnsupported", err)
	}
	if err := inst.Dispose(ctx, op, adapter.Destroy); err != nil {
		t.Fatal(err)
	}
	obs, _ := a.Observe(ctx, op, inst.Ref())
	if obs.Presence != adapter.Absent {
		t.Fatalf("after destroy: presence=%s, want Absent (release verify, §10 #6)", obs.Presence)
	}
	if len(f.containers) != 0 {
		t.Fatalf("containers left: %d", len(f.containers))
	}
	if f.networks[networkNameFor(testEnvID)] {
		t.Fatal("network left behind")
	}
	// Destroy again: already-gone is success (idempotent cleanup).
	if err := inst.Dispose(ctx, op, adapter.Destroy); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
}

func TestDestroyFailureSurfacesForRetry(t *testing.T) {
	a, f := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}
	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.removeErr = errors.New("container is restarting")
	err = inst.Dispose(ctx, op, adapter.Destroy)
	if err == nil {
		t.Fatal("failed destroy must return an error so the release op stays pending (§10 #6)")
	}
	obs, _ := a.Observe(ctx, op, inst.Ref())
	if obs.Presence != adapter.Present {
		t.Fatalf("after failed destroy: presence=%s, want Present", obs.Presence)
	}
	f.removeErr = nil
	if err := inst.Dispose(ctx, op, adapter.Destroy); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverScopedToManager(t *testing.T) {
	a, f := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}
	if _, err := a.Create(ctx, op, testSpec(), nil); err != nil {
		t.Fatal(err)
	}
	otherEnv := "env_01a18cca-55a9-7000-8d1e-4be20c91f3a0"
	f.seed(adapter.NameFor(otherEnv), map[string]string{LabelEnv: otherEnv, LabelManager: "mgr-2"})
	f.seed(adapter.NameFor(testEnvID)+"-proxy", map[string]string{
		LabelEnv: testEnvID, LabelManager: "mgr-1", LabelSidecar: "proxy"})
	f.seed("unrelated", map[string]string{"app": "other"})

	cands, err := a.Discover(ctx, adapter.Scope{ManagerID: "mgr-1"})
	if err != nil {
		t.Fatal(err)
	}
	var ours, foreign int
	for _, c := range cands {
		if c.Foreign {
			foreign++
			if c.Ref.EnvID != otherEnv {
				t.Errorf("foreign candidate = %+v", c)
			}
		} else {
			ours++
			if c.Ref.EnvID != testEnvID {
				t.Errorf("own candidate = %+v", c)
			}
		}
	}
	if ours != 1 || foreign != 1 {
		t.Fatalf("discover: ours=%d foreign=%d, want 1/1 (sidecars and unlabelled excluded): %+v", ours, foreign, cands)
	}
	// Env-scoped discover finds only that env.
	cands, err = a.Discover(ctx, adapter.Scope{ManagerID: "mgr-1", EnvID: testEnvID})
	if err != nil || len(cands) != 1 || cands[0].Ref.EnvID != testEnvID {
		t.Fatalf("env-scoped discover: %+v %v", cands, err)
	}
}

func TestOpenRematerialisesAfterRestart(t *testing.T) {
	a, _ := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}
	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh adapter over the same engine (manager restart).
	a2 := &Adapter{cli: a.cli, ManagerID: "mgr-1"}
	reopened, err := a2.Open(ctx, op, inst.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Ref() != inst.Ref() {
		t.Fatalf("reopened ref %+v != %+v", reopened.Ref(), inst.Ref())
	}
	// Open by name when the external ID was never recorded.
	byName, err := a2.Open(ctx, op, adapter.Ref{EnvID: testEnvID})
	if err != nil || byName.Ref().External != inst.Ref().External {
		t.Fatalf("open by name: %+v %v", byName, err)
	}
}

func TestExecAgainstFakeEngine(t *testing.T) {
	a, _ := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}
	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	di := inst.(*Instance)
	out, serr, code, err := di.execCapture(ctx, op, []string{"echo", "hello-docker"}, "")
	if err != nil || code != 0 || string(out) != "hello-docker\n" || len(serr) != 0 {
		t.Fatalf("exec echo: %q %q code=%d err=%v", out, serr, code, err)
	}
	_, _, code, err = di.execCapture(ctx, op, []string{"false"}, "")
	if err != nil || code != 1 {
		t.Fatalf("exec false: code=%d err=%v", code, err)
	}
}

func TestValidateRejectsPrivileged(t *testing.T) {
	a, _ := newTestAdapter()
	s := testSpec()
	s.Params = map[string]string{"container.privileged": "true"}
	err := a.Validate(context.Background(), adapter.OpContext{}, s)
	if !errors.Is(err, envspec.ErrInvalid) {
		t.Fatalf("validate privileged = %v, want ErrInvalid", err)
	}
}

func TestEnsureImageBuildAndPull(t *testing.T) {
	a, f := newTestAdapter()
	ctx := context.Background()
	// Pull path.
	ref, id, err := a.EnsureImage(ctx, envspec.Profile{Name: "tiny", Image: "alpine:3.20"}, "", nil)
	if err != nil || ref != "alpine:3.20" || id == "" {
		t.Fatalf("pull ensure: %q %q %v", ref, id, err)
	}
	// Build path: content-tagged craggy/ ref, built once.
	p := envspec.Profile{Name: "mod.echotool", Build: &envspec.Build{Context: fstest.MapFS{
		"Dockerfile": {Data: []byte("FROM busybox:latest\n")},
	}}}
	ref, id, err = a.EnsureImage(ctx, p, "abc123def456", nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref != "craggy/mod.echotool:abc123def456" {
		t.Fatalf("build ref = %q, want core-compatible content tag", ref)
	}
	if _, exists, _ := a.ImageExists(ctx, ref); !exists {
		t.Fatal("built image must exist")
	}
	if f.images[ref] != id {
		t.Fatalf("image id mismatch: %q vs %q", f.images[ref], id)
	}
}

func TestNoSnapshotStub(t *testing.T) {
	// The old driver's Snapshotter.Load was a stub erroring "provision
	// a new env"; the adapter must not pretend to be a Snapshotter.
	a, _ := newTestAdapter()
	inst, err := a.Create(context.Background(), adapter.OpContext{EnvID: testEnvID}, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inst.(adapter.Snapshotter); ok {
		t.Fatal("docker instance must not implement adapter.Snapshotter (stub Load deleted)")
	}
}

func TestStartFailureReturnsPartialInstance(t *testing.T) {
	a, f := newTestAdapter()
	f.startErr = errors.New("oci runtime error")
	inst, err := a.Create(context.Background(), adapter.OpContext{EnvID: testEnvID}, testSpec(), nil)
	if err == nil {
		t.Fatal("start failure must error")
	}
	if inst == nil {
		t.Fatal("start failure after container creation must hand back the instance for cleanup")
	}
	if f.creates != 1 {
		t.Fatalf("creates = %d", f.creates)
	}
}

func TestCollectorsBoundAndRegistered(t *testing.T) {
	a, f := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}
	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cp, ok := inst.(adapter.CollectorProvider)
	if !ok {
		t.Fatal("docker instance must provide collectors")
	}
	want := map[string]bool{
		envspec.StreamContainerLogs: false, envspec.StreamEnvResources: false,
		envspec.StreamFSChanges: false, envspec.StreamFSActivity: false,
		envspec.StreamEnvProcess: false, envspec.StreamNetPCAP: false,
		envspec.StreamNetPackets: false, envspec.StreamNetFlows: false,
		envspec.StreamNetDNS: false, envspec.StreamNetTLS: false, envspec.StreamNetHTTP: false,
	}
	for _, c := range cp.Collectors() {
		for _, sd := range c.Streams() {
			if _, known := want[sd.Name]; known {
				want[sd.Name] = true
			}
		}
	}
	for stream, found := range want {
		if !found {
			t.Errorf("stream %s has no docker collector", stream)
		}
	}
	if info := cp.CollectorEnvInfo(); info.Kind != string(envspec.Container) || info.ID != testEnvID {
		t.Errorf("CollectorEnvInfo = %+v", info)
	}

	// Logs collector against the fake framed stream.
	fc, _ := f.find(inst.Ref().External)
	fc.logBuf = append(frame(1, "hello\n"), frame(2, "oops\n")...)
	var col signalcore.Collector
	for _, c := range cp.Collectors() {
		if c.Name() == "container.logs" {
			col = c
		}
	}
	var events []envspec.SignalEvent
	err = col.Run(ctx, nil, signalcore.EmitterFunc(func(ev envspec.SignalEvent) { events = append(events, ev) }))
	if err == nil {
		t.Fatal("logs collector must return an error when the stream ends (run-once)")
	}
	if len(events) != 2 {
		t.Fatalf("log events = %d: %+v", len(events), events)
	}
	if ll, _ := events[0].Data.(envspec.LogLine); ll.Text != "hello" || ll.Stream != "stdout" {
		t.Errorf("event 0 = %+v", events[0].Data)
	}
	if ll, _ := events[1].Data.(envspec.LogLine); ll.Text != "oops" || ll.Stream != "stderr" {
		t.Errorf("event 1 = %+v", events[1].Data)
	}
}

func TestResourcesCollectorSample(t *testing.T) {
	a, _ := newTestAdapter()
	op := adapter.OpContext{EnvID: testEnvID}
	inst, err := a.Create(context.Background(), op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rc := &resourcesCollector{inst: inst.(*Instance), Interval: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var events []envspec.SignalEvent
	done := make(chan error, 1)
	go func() {
		done <- rc.Run(ctx, nil, signalcore.EmitterFunc(func(ev envspec.SignalEvent) { events = append(events, ev) }))
	}()
	if err := <-done; err != nil {
		t.Fatalf("resources run: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no resource samples")
	}
	d, _ := events[0].Data.(envspec.ResourcesData)
	if d.Procs != 3 || d.MemMB != 50 || d.MemLimitMB != 512 {
		t.Fatalf("sample = %+v", d)
	}
}

func TestArchiveRoundTripStripsBaseDir(t *testing.T) {
	a, _ := newTestAdapter()
	ctx := context.Background()
	op := adapter.OpContext{EnvID: testEnvID}
	inst, err := a.Create(ctx, op, testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tr := inst.(adapter.Transferrer)
	var buf bytes.Buffer
	if _, err := xfer.WriteTar(ctx, envspec.MemSource(map[string][]byte{"sub/a.txt": []byte("alpha")}), &buf, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := tr.PutArchive(ctx, op, "/work", &buf); err != nil {
		t.Fatal(err)
	}
	rc, err := tr.GetArchive(ctx, op, "/work")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	sink := envspec.NewMemSink()
	if _, err := xfer.ReadTar(ctx, rc, sink, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	// Docker archives include the base dir ("work/…"); the adapter
	// must hand the core src-relative entries.
	if string(sink.Files["sub/a.txt"]) != "alpha" {
		t.Fatalf("pulled files = %v, want sub/a.txt without base prefix", sink.Files)
	}
}
