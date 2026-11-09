package environments_test

import (
	"context"
	"reflect"
	"runtime"
	"sort"
	"testing"
	"testing/fstest"
	"time"

	"github.com/craggy-io/environment-services/environments"
	"github.com/craggy-io/environment-services/envspec"
)

// TestManagerMethodSet pins Manager's exported method set to the
// execution plan §3.2 list (with ProfileStatus, the authoritative
// readiness answer; without Warnings — there is no legacy to warn
// about). Adding or removing a Manager method must update the plan
// and this list together.
func TestManagerMethodSet(t *testing.T) {
	want := []string{
		"AcquireService", "Android", "BuildDigest", "Close", "Drivers",
		"EnsureProfileBackend", "Events", "EventsSince", "Get",
		"HostSnapshot", "List", "ManagerID", "Observation", "Profile",
		"ProfileStatus", "Profiles", "Provision", "RecentEvents",
		"RedactString", "Release", "RenewLease", "StockProfiles",
		"StopService",
	}
	typ := reflect.TypeOf(&environments.Manager{})
	var got []string
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Manager methods = %v, want exactly %v", got, want)
	}
}

func TestBuildDigestPackageLevel(t *testing.T) {
	profile := envspec.Profile{
		Name: "digest-test",
		Build: &envspec.Build{
			Context: fstest.MapFS{
				"Dockerfile": {Data: []byte("FROM scratch\n")},
				"input.txt":  {Data: []byte("payload")},
			},
			Args: map[string]string{"MODE": "test"},
		},
	}

	digest, err := environments.BuildDigest(profile)
	if err != nil {
		t.Fatalf("BuildDigest: %v", err)
	}
	if len(digest) != 12 {
		t.Fatalf("BuildDigest = %q, want 12 hex characters", digest)
	}
	again, err := environments.BuildDigest(profile)
	if err != nil || again != digest {
		t.Fatalf("repeat BuildDigest = %q, %v; want %q", again, err, digest)
	}
	if digest, err := environments.BuildDigest(envspec.Profile{Name: "prebuilt", Image: "alpine:3.20"}); err != nil || digest != "" {
		t.Fatalf("BuildDigest(prebuilt) = %q, %v; want empty, nil", digest, err)
	}
}

func openTestManager(t *testing.T) *environments.Manager {
	t.Helper()
	ctx := context.Background()
	mgr, err := environments.Open(ctx, environments.Config{Dir: t.TempDir(), ManagerID: "mgr_test"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = mgr.Close(cctx, environments.ShutdownDrain)
	})
	return mgr
}

// TestOpenHostSmoke exercises the assembled stack end to end on the
// always-present host adapter: provision, push, exec, pull, events,
// release, and the facade's read models.
func TestOpenHostSmoke(t *testing.T) {
	mgr := openTestManager(t)
	ctx := context.Background()

	var hasHost bool
	for _, d := range mgr.Drivers() {
		if d.Name == "hostenv" {
			hasHost = true
		}
	}
	if !hasHost {
		t.Fatalf("Drivers() = %+v, want a hostenv driver (host is always registered)", mgr.Drivers())
	}
	if mgr.ManagerID() != "mgr_test" {
		t.Fatalf("ManagerID = %q, want mgr_test", mgr.ManagerID())
	}
	if len(mgr.StockProfiles()) == 0 || len(mgr.Profiles()) < len(mgr.StockProfiles()) {
		t.Fatalf("profiles: stock=%d registered=%d", len(mgr.StockProfiles()), len(mgr.Profiles()))
	}
	if _, ok := mgr.Profile("host-tools"); !ok {
		t.Fatal("Profile(host-tools) missing")
	}
	if st := mgr.ProfileStatus("host-tools"); !st.Ready {
		t.Fatalf("ProfileStatus(host-tools) = %+v, want ready", st)
	}
	if snap := mgr.HostSnapshot(); snap.CPUs == 0 && snap.MemTotalMB == 0 {
		t.Fatalf("HostSnapshot zero after Open: %+v", snap)
	}

	owner := envspec.Owner{ID: "user:test", Session: "s1"}
	h, err := mgr.Provision(ctx, owner, envspec.Spec{Profile: "host-tools"},
		environments.WithRequestKey("smoke"))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	// Same owner+key+spec returns the same environment.
	h2, err := mgr.Provision(ctx, owner, envspec.Spec{Profile: "host-tools"},
		environments.WithRequestKey("smoke"))
	if err != nil || h2.ID() != h.ID() {
		t.Fatalf("idempotent Provision = %v, %v; want same env %s", h2, err, h.ID())
	}
	if got, err := mgr.Get(h.ID()); err != nil || got.ID() != h.ID() {
		t.Fatalf("Get = %v, %v", got, err)
	}
	if err := mgr.RenewLease(h); err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if list := mgr.List(environments.Filter{Live: true}); len(list) != 1 || list[0].ID != h.ID() {
		t.Fatalf("List(Live) = %+v", list)
	}

	if err := h.Push(ctx, envspec.MemSource(map[string][]byte{"in.txt": []byte("hello\n")}), "/work"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	argv := []string{"sh", "-c", "cat in.txt"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "type in.txt"}
	}
	out, _, code, err := h.Exec(ctx, envspec.Cmd{Argv: argv})
	if err != nil || code != 0 || string(out) != "hello\n" && string(out) != "hello\r\n" {
		t.Fatalf("Exec = %q code=%d err=%v", out, code, err)
	}
	sink := envspec.NewMemSink()
	if err := h.Pull(ctx, "/work", sink); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	found := false
	for name := range sink.Files {
		if name == "in.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Pull files = %v, want in.txt", sink.Files)
	}

	evs, err := mgr.EventsSince(ctx, 0, 100)
	if err != nil || len(evs) == 0 {
		t.Fatalf("EventsSince = %d, %v", len(evs), err)
	}
	if recent, err := mgr.RecentEvents(ctx, 5); err != nil || len(recent) == 0 || len(recent) > 5 {
		t.Fatalf("RecentEvents = %d, %v", len(recent), err)
	}
	ectx, cancel := context.WithCancel(ctx)
	n := 0
	for _, err := range mgr.Events(ectx, 0) {
		if err != nil {
			break
		}
		n++
		if n >= 3 {
			break
		}
	}
	cancel()
	if n == 0 {
		t.Fatal("Events stream yielded nothing")
	}

	// A runtime pin on a real collector: env.resources starts as an
	// activity, reports running, and stops on unpin.
	obs := mgr.Observation()
	unpin, err := obs.Pin(envspec.StreamEnvResources, h.ID(), nil)
	if err != nil {
		t.Fatalf("Pin(env.resources, host env): %v", err)
	}
	startDeadline := time.Now().Add(3 * time.Second)
	for !obs.ConfiguredState(envspec.StreamEnvResources, h.ID(), nil).Running && time.Now().Before(startDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := obs.ConfiguredState(envspec.StreamEnvResources, h.ID(), nil); !st.Running {
		t.Fatalf("ConfiguredState after Pin = %+v, want running", st)
	}
	if got := obs.Running(); len(got) == 0 {
		t.Fatal("Running() empty while a pin is live")
	}
	unpin()
	deadline := time.Now().Add(3 * time.Second)
	for obs.ConfiguredState(envspec.StreamEnvResources, h.ID(), nil).Running && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if obs.ConfiguredState(envspec.StreamEnvResources, h.ID(), nil).Running {
		t.Fatal("collector still running after unpin")
	}

	// Android() on a host env must refuse, not panic.
	if _, err := mgr.Android(h.ID()); err == nil {
		t.Fatal("Android(host env) succeeded, want error")
	}

	if err := mgr.Release(ctx, h.ID()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if list := mgr.List(environments.Filter{Live: true}); len(list) != 0 {
		t.Fatalf("List(Live) after Release = %+v", list)
	}
	// The released holder handle is stale.
	if _, _, _, err := h.Exec(ctx, envspec.Cmd{Argv: argv}); err == nil {
		t.Fatal("Exec on released handle succeeded, want stale-handle error")
	}
}

// TestObservationHostAndUnknown pins the Observation edge behaviour
// that needs no collector: host.metrics is always running, unknown
// streams and sources fail synchronously.
func TestObservationHostAndUnknown(t *testing.T) {
	mgr := openTestManager(t)
	obs := mgr.Observation()

	unpin, err := obs.Pin(envspec.StreamHostMetrics, "host", nil)
	if err != nil || unpin == nil {
		t.Fatalf("Pin(host.metrics, host) unpin nil=%v err=%v", unpin == nil, err)
	}
	unpin()
	if _, err := obs.Pin("no.such@1", "host", nil); err == nil {
		t.Fatal("Pin(unknown, host) succeeded")
	}
	if _, err := obs.Pin("no.such@1", "env_missing", nil); err == nil {
		t.Fatal("Pin(unknown, missing env) succeeded")
	}
	if got := obs.Running(); len(got) != 0 {
		t.Fatalf("Running = %+v, want none", got)
	}
	// The sampler publishes once per second; poll for its first event.
	var evs []envspec.SignalEvent
	var q envspec.Quality
	deadline := time.Now().Add(3 * time.Second)
	for {
		var err error
		evs, q, err = obs.Query(envspec.Selector{Streams: []string{envspec.StreamHostMetrics}},
			time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if len(evs) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(evs) == 0 || q.Received == 0 {
		t.Fatalf("Query host.metrics = %d events q=%+v, want sampler output", len(evs), q)
	}
	cap, err := obs.Capture(envspec.Selector{Streams: []string{envspec.StreamHostMetrics}},
		time.Now().Add(-time.Minute), time.Now())
	if err != nil || cap.ID() == "" || cap.Source() == nil {
		t.Fatalf("Capture = %v, %v", cap, err)
	}
	cap.Close()
}

// TestRenewLeaseRejectsForeignHandle: only handles this manager
// issued can be renewed through it.
func TestRenewLeaseRejectsForeignHandle(t *testing.T) {
	mgr := openTestManager(t)
	if err := mgr.RenewLease(foreignHandle{}); err == nil {
		t.Fatal("RenewLease(foreign handle) succeeded")
	}
}

type foreignHandle struct{}

func (foreignHandle) ID() string         { return "env_foreign" }
func (foreignHandle) Info() envspec.Info { return envspec.Info{ID: "env_foreign"} }
func (foreignHandle) Exec(context.Context, envspec.Cmd) ([]byte, []byte, int, error) {
	return nil, nil, 0, nil
}
func (foreignHandle) Push(context.Context, envspec.Source, string) error { return nil }
func (foreignHandle) Pull(context.Context, string, envspec.Sink) error   { return nil }
func (foreignHandle) Release(context.Context) error                      { return nil }

// TestEnvCapDepsSurface checks the capability deps can drive the
// by-ID operations envcap needs (full handler coverage is S9's gate).
func TestEnvCapDepsSurface(t *testing.T) {
	mgr := openTestManager(t)
	deps := environments.NewEnvCapDeps(mgr)
	ctx := context.Background()
	owner := envspec.Owner{ID: "user:test", Session: "s2", Job: "job1"}
	h, err := deps.Provision(ctx, owner, envspec.Spec{Profile: "host-tools"})
	if err != nil {
		t.Fatalf("deps.Provision: %v", err)
	}
	unborrow, err := deps.Borrow(ctx, h.ID(), "job1")
	if err != nil {
		t.Fatalf("deps.Borrow: %v", err)
	}
	if err := deps.RenewBorrow(h.ID(), "job1"); err != nil {
		t.Fatalf("deps.RenewBorrow: %v", err)
	}
	unborrow()
	argv := []string{"sh", "-c", "echo deps"}
	if runtime.GOOS == "windows" {
		argv = []string{"cmd", "/c", "echo deps"}
	}
	out, _, code, err := deps.Exec(ctx, h.ID(), envspec.Cmd{Argv: argv})
	if err != nil || code != 0 || len(out) == 0 {
		t.Fatalf("deps.Exec = %q code=%d err=%v", out, code, err)
	}
	if st, err := deps.Push(ctx, h.ID(), envspec.MemSource(map[string][]byte{"a.txt": []byte("a")}), "/work"); err != nil || st.Files == 0 {
		t.Fatalf("deps.Push = %+v, %v", st, err)
	}
	if entries, err := deps.Manifest(ctx, h.ID(), "/work"); err != nil || len(entries) == 0 {
		t.Fatalf("deps.Manifest = %v, %v", entries, err)
	}
	if err := deps.CloseJob(ctx, "job1"); err != nil {
		t.Fatalf("deps.CloseJob: %v", err)
	}
	// TypeSecret against a host env refuses (not an Android device)
	// before touching the vault.
	if err := deps.TypeSecret(ctx, h.ID(), "lease_missing", "password"); err == nil {
		t.Fatal("deps.TypeSecret(host env) succeeded")
	}
}
