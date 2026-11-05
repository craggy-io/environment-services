package docker_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adapters/docker"
	"github.com/craggy-io/environment-services/internal/core"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Live Docker tests run only when asked: set CRAGGY_TEST_DOCKER=1
// (daemon must answer — failures are real failures, not skips).
// TestLiveStartBackend additionally needs CRAGGY_TEST_DOCKER_BOOT=1
// on a machine where Docker Desktop is stopped.

func liveAdapter(t *testing.T) *docker.Adapter {
	t.Helper()
	if os.Getenv("CRAGGY_TEST_DOCKER") != "1" {
		t.Skip("set CRAGGY_TEST_DOCKER=1 to run live docker tests")
	}
	d, err := docker.New("testmgr")
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}
	if err := d.Check(context.Background()); err != nil {
		t.Fatalf("CRAGGY_TEST_DOCKER=1 but no docker daemon: %v", err)
	}
	return d
}

func tinyProfile() envspec.Profile {
	return envspec.Profile{Name: "tiny", Kind: envspec.Container, Image: "busybox:latest",
		AllowImages: []string{"busybox:*"}, Resources: envspec.Resources{CPUs: 1, MemoryMB: 256},
		MaxResources: envspec.Resources{CPUs: 2, MemoryMB: 1024}, Network: envspec.NetworkNone, TTL: time.Hour}
}

func openManager(t *testing.T, dir string, d *docker.Adapter, profiles ...envspec.Profile) *core.Manager {
	t.Helper()
	m, err := core.Open(context.Background(), core.Config{Dir: dir, ManagerID: "testmgr",
		Adapters: []adapter.Adapter{d}, HealthInterval: time.Hour, ReapInterval: time.Minute,
		Profiles: profiles})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDockerContainerLifecycle(t *testing.T) {
	d := liveAdapter(t)
	dir := t.TempDir()
	m := openManager(t, dir, d, tinyProfile())
	closed := false
	defer func() {
		if !closed {
			_ = m.Close(context.Background(), core.ShutdownDrain)
		}
	}()
	ctx := context.Background()
	owner := envspec.Owner{ID: "agent:test", Session: "dockertest"}
	h, err := m.Provision(ctx, owner, envspec.Spec{Profile: "tiny"})
	if err != nil {
		t.Fatal(err)
	}
	if h.Info().Image == "" {
		t.Fatal("image digest must be recorded")
	}
	// Push, exec (stdout/stderr split), pull.
	if err := h.Push(ctx, envspec.MemSource(map[string][]byte{"a.txt": []byte("alpha")}), "/work"); err != nil {
		t.Fatal(err)
	}
	out, errOut, code, err := h.Exec(ctx, envspec.Cmd{Argv: []string{"sh", "-c", "cat /work/a.txt; echo oops 1>&2; cp /work/a.txt /work/b.txt"}})
	if err != nil || code != 0 {
		t.Fatalf("exec %d %v", code, err)
	}
	if string(out) != "alpha" {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.Contains(string(errOut), "oops") {
		t.Fatalf("stderr not split: %q", errOut)
	}
	sink := envspec.NewMemSink()
	if err := h.Pull(ctx, "/work", sink); err != nil {
		t.Fatal(err)
	}
	if string(sink.Files["a.txt"]) != "alpha" || string(sink.Files["b.txt"]) != "alpha" {
		t.Fatalf("pull files=%v", sink.Files)
	}
	// Network None: no external reachability.
	_, _, code, _ = h.Exec(ctx, envspec.Cmd{Argv: []string{"sh", "-c", "wget -T 3 -qO- http://1.1.1.1 || exit 42"}})
	if code != 42 {
		t.Fatalf("network-none container reached the internet (exit %d)", code)
	}
	// Reopen under a fresh manager: the env is recovered by identity.
	envID := h.ID()
	if err := m.Close(ctx, core.ShutdownDisconnect); err != nil {
		t.Fatal(err)
	}
	closed = true
	m2 := openManager(t, dir, d, tinyProfile())
	defer func() { _ = m2.Close(ctx, core.ShutdownDrain) }()
	got, err := m2.Get(envID)
	if err != nil {
		t.Fatalf("recover after manager restart: %v", err)
	}
	if out, _, _, err := got.Exec(ctx, envspec.Cmd{Argv: []string{"cat", "/work/a.txt"}}); err != nil || string(out) != "alpha" {
		t.Fatalf("recovered env exec: %q %v", out, err)
	}
	if err := m2.Release(ctx, envID); err != nil {
		t.Fatal(err)
	}
}

func TestDockerBuildProfile(t *testing.T) {
	d := liveAdapter(t)
	m := openManager(t, t.TempDir(), d)
	defer func() { _ = m.Close(context.Background(), core.ShutdownDrain) }()
	tool := envspec.Profile{Name: "mod.echotool", Kind: envspec.Container,
		AllowImages: []string{"busybox:*"}, Resources: envspec.Resources{MemoryMB: 256},
		Prepare: true,
		Build: &envspec.Build{Context: fstest.MapFS{
			"Dockerfile": {Data: []byte("FROM busybox:latest\nRUN mkdir -p /work && echo built > /work/marker.txt\n")},
		}}}
	if err := m.RegisterProfiles(tool); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := m.Provision(ctx, envspec.Owner{ID: "agent:test"}, envspec.Spec{Profile: "mod.echotool"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, code, err := h.Exec(ctx, envspec.Cmd{Argv: []string{"cat", "/work/marker.txt"}})
	if err != nil || code != 0 || !strings.Contains(string(out), "built") {
		t.Fatalf("built image content = %q code=%d %v", out, code, err)
	}
	if st := m.ProfileStatus("mod.echotool"); !st.Ready {
		t.Fatalf("profile not ready: %+v", st)
	}
	digest, err := core.BuildDigest(tool)
	if err != nil || len(digest) != 12 {
		t.Fatalf("digest %q %v", digest, err)
	}
	if tag := core.ImageTagFor(tool.Name, digest); tag != "craggy/mod.echotool:"+digest {
		t.Fatal(tag)
	}
	_ = m.Release(ctx, h.ID())
}

func TestDockerRestoredFilesOwnedByApplication(t *testing.T) {
	d := liveAdapter(t)
	m := openManager(t, t.TempDir(), d, envspec.Profile{
		Name: "unprivileged", Kind: envspec.Container, Image: "debian:bookworm-slim", AllowImages: []string{"debian:*"},
		Network: envspec.NetworkNone, TTL: time.Hour, Params: map[string]string{"container.user": "nobody"},
	})
	defer func() { _ = m.Close(context.Background(), core.ShutdownDrain) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	h, err := m.Provision(ctx, envspec.Owner{ID: "test:restore"}, envspec.Spec{Profile: "unprivileged"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Release(context.Background(), h.ID()) }()
	if _, err = m.Push(ctx, h, envspec.MemSource(map[string][]byte{"project/database": []byte("saved")}), "/tmp/analysis", xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	_, stderr, code, err := h.Exec(ctx, envspec.Cmd{Argv: []string{"sh", "-c", "echo edited >> /tmp/analysis/project/database && rm /tmp/analysis/project/database && rmdir /tmp/analysis/project /tmp/analysis"}})
	if err != nil || code != 0 {
		t.Fatalf("application cannot edit/clean restored project: %v code=%d %s", err, code, stderr)
	}
	// A destination rejected before tar consumption must unblock its producer.
	failed, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	_, err = m.Push(failed, h, envspec.MemSource(map[string][]byte{"big": make([]byte, 1<<20)}), "/proc/no-such-directory", xfer.Options{})
	if err == nil {
		t.Fatalf("rejected transfer did not fail: %v / %v", err, failed.Err())
	}
}

// TestDockerXfer80kGate is the transfer gate against a real
// container: 80,000 files pushed and pulled through the archive API.
func TestDockerXfer80kGate(t *testing.T) {
	if testing.Short() {
		t.Skip("80k docker gate skipped in -short")
	}
	d := liveAdapter(t)
	m := openManager(t, t.TempDir(), d, envspec.Profile{Name: "tiny", Kind: envspec.Container, Image: "busybox:latest",
		AllowImages: []string{"busybox:*"}, Resources: envspec.Resources{CPUs: 2, MemoryMB: 1024},
		MaxResources: envspec.Resources{CPUs: 4, MemoryMB: 4096}, Network: envspec.NetworkNone, TTL: time.Hour})
	defer func() { _ = m.Close(context.Background(), core.ShutdownDrain) }()
	ctx := context.Background()
	h, err := m.Provision(ctx, envspec.Owner{ID: "agent:bench"}, envspec.Spec{Profile: "tiny"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Release(ctx, h.ID()) }()

	files := make(map[string][]byte, 80000)
	for i := 0; i < 80000; i++ {
		want := 32 + (i*37)%1500
		pat := []byte("file-" + itoa(i) + "-")
		files["src/pkg"+pad(i/1000, 3)+"/mod"+pad((i/100)%10, 3)+"/F"+pad(i, 5)+".java"] =
			repeatTo(pat, want)
	}
	t0 := time.Now()
	st, err := m.Push(ctx, h, envspec.MemSource(files), "/work/tree", xfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pushT := time.Since(t0)
	t.Logf("container push: %d files %d bytes in %s", st.Files, st.Bytes, pushT)
	if st.Files != 80000 {
		t.Fatalf("push files = %d", st.Files)
	}
	if pushT > 30*time.Second {
		t.Fatalf("gate: container push took %s (>30s)", pushT)
	}

	t0 = time.Now()
	sink := envspec.NewMemSink()
	st, err = m.Pull(ctx, h, "/work/tree", sink, xfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pullT := time.Since(t0)
	t.Logf("container pull: %d files %d bytes in %s", st.Files, st.Bytes, pullT)
	if len(sink.Files) != 80000 {
		t.Fatalf("pulled %d files", len(sink.Files))
	}
	if pullT > 60*time.Second {
		t.Fatalf("gate: container pull took %s (>60s)", pullT)
	}
	for _, k := range []string{"src/pkg000/mod000/F00000.java", "src/pkg079/mod009/F79999.java"} {
		if string(sink.Files[k]) != string(files[k]) {
			t.Fatalf("content mismatch for %s", k)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func pad(n, width int) string {
	s := itoa(n)
	for len(s) < width {
		s = "0" + s
	}
	return s
}

func repeatTo(pat []byte, n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, pat...)
	}
	return out[:n]
}

func TestLiveStartBackend(t *testing.T) {
	if os.Getenv("CRAGGY_TEST_DOCKER_BOOT") != "1" {
		t.Skip("set CRAGGY_TEST_DOCKER_BOOT=1 to start Docker Desktop")
	}
	d, err := docker.New("testmgr")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := d.Check(ctx); err == nil {
		t.Skip("Docker is already running")
	}
	start := time.Now()
	if err := d.StartBackend(ctx, nil); err != nil {
		t.Fatalf("StartBackend: %v", err)
	}
	t.Logf("Docker answered after %s", time.Since(start).Round(time.Second))
	if err := d.Check(ctx); err != nil {
		t.Fatalf("Check after start: %v", err)
	}
	if memMB, cpus, err := d.Capacity(ctx); err != nil || memMB == 0 || cpus == 0 {
		t.Fatalf("Capacity = %d MB, %d CPUs, %v", memMB, cpus, err)
	}
}
