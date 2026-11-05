package hostenv_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adapters/hostenv"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/xfer"
)

func echoArgv(text string) []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd", "/c", "echo " + text}
	}
	return []string{"sh", "-c", "echo " + text}
}

func sleepArgv() []string {
	if runtime.GOOS == "windows" {
		return []string{"ping", "-n", "30", "127.0.0.1"}
	}
	return []string{"sleep", "30"}
}

func runCapture(t *testing.T, inst adapter.Instance, argv []string) (string, string, int) {
	t.Helper()
	ex, ok := inst.(adapter.Executor)
	if !ok {
		t.Fatal("hostenv instance must be an Executor")
	}
	p, err := ex.Exec(context.Background(), adapter.OpContext{EnvID: inst.Ref().EnvID}, adapter.ExecCmd{Argv: argv})
	if err != nil {
		t.Fatal(err)
	}
	outCh := make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(p.Stdout); outCh <- b }()
	errB, _ := io.ReadAll(p.Stderr)
	out := <-outCh
	code, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return string(out), string(errB), code
}

func hostSpec() envspec.Spec {
	return envspec.Spec{Kind: envspec.Host, Profile: "host-tools", Network: envspec.NetworkOpen,
		Resources: envspec.Resources{MemoryMB: 1024}}
}

func TestHostLifecycleOpenDestroy(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "state")
	work := filepath.Join(root, "work")
	ad := hostenv.Default(dir, work)
	ctx := context.Background()
	if err := ad.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if err := ad.Validate(ctx, adapter.OpContext{}, envspec.Spec{Network: envspec.NetworkNone}); !errors.Is(err, envspec.ErrInvalid) {
		t.Fatalf("host validate must reject non-open networks with ErrInvalid, got %v", err)
	}
	if d := ad.Describe(); d.Relationship != adapter.Created || len(d.Kinds) != 1 || d.Kinds[0] != envspec.Host {
		t.Fatalf("Describe = %+v", d)
	}
	if c := ad.Cost(hostSpec()); c.Pool != envspec.PoolHost {
		t.Fatalf("Cost pool = %s", c.Pool)
	}

	id := envspec.NewID("env_")
	op := adapter.OpContext{EnvID: id, Generation: 1}

	// Observe before create: workspace gone -> Absent.
	obs, err := ad.Observe(ctx, op, adapter.Ref{EnvID: id})
	if err != nil || obs.Presence != adapter.Absent {
		t.Fatalf("observe before create: %s %v, want Absent", obs.Presence, err)
	}

	inst, err := ad.Create(ctx, op, hostSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref().Name != adapter.NameFor(id) || !strings.HasPrefix(inst.Ref().Name, "craggy-") {
		t.Fatalf("job identity = %q, want %q", inst.Ref().Name, adapter.NameFor(id))
	}
	hi := inst.(*hostenv.Instance)
	if _, err := os.Stat(hi.LocalWorkspace()); err != nil {
		t.Fatalf("workspace missing: %v", err)
	}
	obs, err = ad.Observe(ctx, op, inst.Ref())
	if err != nil || obs.Presence != adapter.Present {
		t.Fatalf("observe after create: %s %v, want Present", obs.Presence, err)
	}
	if obs.Workspace != "/work" {
		t.Errorf("observation workspace = %q (host path must not leak)", obs.Workspace)
	}

	// Create is idempotent (rediscovery by deterministic identity).
	again, err := ad.Create(ctx, op, hostSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref() != inst.Ref() {
		t.Fatalf("second Create ref %+v != %+v", again.Ref(), inst.Ref())
	}

	out, _, code := runCapture(t, inst, echoArgv("hello-host"))
	if code != 0 || !strings.Contains(out, "hello-host") {
		t.Fatalf("echo: %q code=%d", out, code)
	}

	// Transfer in via Transferrer; workspace escape must fail.
	tr, ok := inst.(adapter.Transferrer)
	if !ok {
		t.Fatal("hostenv instance must be a Transferrer")
	}
	src := envspec.MemSource(map[string][]byte{"t.txt": []byte("data")})
	pr, pw := io.Pipe()
	go func() {
		_, werr := xfer.WriteTar(ctx, src, pw, xfer.Options{})
		_ = pw.CloseWithError(werr)
	}()
	if err := tr.PutArchive(ctx, op, "/work/in", pr); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(hi.LocalWorkspace(), "in", "t.txt"))
	if err != nil || string(b) != "data" {
		t.Fatalf("pushed file: %q %v", b, err)
	}
	rc, err := tr.GetArchive(ctx, op, "/work/in")
	if err != nil {
		t.Fatal(err)
	}
	sink := envspec.NewMemSink()
	if _, err := xfer.ReadTar(ctx, rc, sink, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if string(sink.Files["t.txt"]) != "data" {
		t.Fatalf("pulled files: %v", sink.Files)
	}
	mf := inst.(adapter.Manifestor)
	if _, err := mf.Manifest(ctx, op, "../../etc"); err == nil {
		t.Fatal("paths outside the workspace must be rejected")
	}
	man, err := mf.Manifest(ctx, op, "/work")
	if err != nil || len(man) == 0 {
		t.Fatalf("manifest: %+v %v", man, err)
	}
	lf := inst.(adapter.FSLister)
	if ents, err := lf.ListFS(ctx, op, "/work"); err != nil || len(ents) == 0 {
		t.Fatalf("list: %+v %v", ents, err)
	}
	if en, err := lf.StatFS(ctx, op, "/work/in/t.txt"); err != nil || en.Size != 4 {
		t.Fatalf("stat: %+v %v", en, err)
	}

	// Start a long process; Open in a fresh adapter; PID+creation
	// matching keeps it; Destroy kills the tree.
	ex := inst.(adapter.Executor)
	p2, err := ex.Exec(ctx, op, adapter.ExecCmd{Argv: sleepArgv()})
	if err != nil {
		t.Fatal(err)
	}
	pid := p2.PID()
	if pid == 0 {
		t.Fatal("host exec must expose a PID")
	}
	go func() { _, _ = io.Copy(io.Discard, p2.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, p2.Stderr) }()

	ad2 := hostenv.Default(dir, work)
	cands, err := ad2.Discover(ctx, adapter.Scope{})
	if err != nil || len(cands) != 1 || cands[0].Ref.EnvID != id || cands[0].Foreign {
		t.Fatalf("discover = %+v %v", cands, err)
	}
	opened, err := ad2.Open(ctx, op, cands[0].Ref)
	if err != nil {
		t.Fatal(err)
	}
	live := opened.(*hostenv.Instance).LiveProcs()
	found := false
	for _, lp := range live {
		if lp == pid {
			found = true
		}
	}
	if !found {
		t.Fatalf("reopened live procs %v missing %d (PID+creation identity)", live, pid)
	}
	if err := opened.Dispose(ctx, op, adapter.Destroy); err != nil {
		t.Fatal(err)
	}
	waitGone(t, pid)
	if _, err := os.Stat(hi.LocalWorkspace()); !os.IsNotExist(err) {
		t.Fatalf("workspace must be deleted, stat err = %v", err)
	}
	obs, err = ad.Observe(ctx, op, adapter.Ref{EnvID: id})
	if err != nil || obs.Presence != adapter.Absent {
		t.Fatalf("observe after destroy: %s %v, want Absent", obs.Presence, err)
	}
	if cands, _ := ad2.Discover(ctx, adapter.Scope{}); len(cands) != 0 {
		t.Fatalf("discover after destroy = %+v", cands)
	}
	_, _ = p2.Wait()
}

func TestRetainWorkspaceKeepsFilesAndKillsProcs(t *testing.T) {
	root := t.TempDir()
	ad := hostenv.Default(filepath.Join(root, "state"), filepath.Join(root, "work"))
	ctx := context.Background()
	id := envspec.NewID("env_")
	op := adapter.OpContext{EnvID: id, Generation: 1}
	inst, err := ad.Create(ctx, op, hostSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	hi := inst.(*hostenv.Instance)
	keep := filepath.Join(hi.LocalWorkspace(), "keep.txt")
	if err := os.WriteFile(keep, []byte("evidence"), 0o644); err != nil {
		t.Fatal(err)
	}
	ex := inst.(adapter.Executor)
	p, err := ex.Exec(ctx, op, adapter.ExecCmd{Argv: sleepArgv()})
	if err != nil {
		t.Fatal(err)
	}
	pid := p.PID()
	go func() { _, _ = io.Copy(io.Discard, p.Stdout) }()
	go func() { _, _ = io.Copy(io.Discard, p.Stderr) }()

	if err := inst.Dispose(ctx, op, adapter.RetainWorkspace); err != nil {
		t.Fatal(err)
	}
	waitGone(t, pid)
	b, err := os.ReadFile(keep)
	if err != nil || string(b) != "evidence" {
		t.Fatalf("retained workspace file: %q %v", b, err)
	}
	_, _ = p.Wait()
}

func TestHostCollectors(t *testing.T) {
	root := t.TempDir()
	ad := hostenv.Default(filepath.Join(root, "state"), filepath.Join(root, "work"))
	ctx := context.Background()
	id := envspec.NewID("env_")
	op := adapter.OpContext{EnvID: id, Generation: 1}
	inst, err := ad.Create(ctx, op, hostSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inst.Dispose(ctx, op, adapter.Destroy) }()
	cp, ok := inst.(adapter.CollectorProvider)
	if !ok {
		t.Fatal("hostenv instance must provide collectors")
	}
	streams := map[string]bool{}
	var byName = map[string]signalcore.Collector{}
	for _, c := range cp.Collectors() {
		byName[c.Name()] = c
		for _, sd := range c.Streams() {
			streams[sd.Name] = true
		}
	}
	for _, want := range []string{envspec.StreamEnvResources, envspec.StreamFSChanges, envspec.StreamEnvProcess} {
		if !streams[want] {
			t.Errorf("stream %s has no host collector", want)
		}
	}
	if info := cp.CollectorEnvInfo(); info.Kind != string(envspec.Host) || info.ID != id {
		t.Errorf("CollectorEnvInfo = %+v", info)
	}

	// fs.changes over the real workspace: baseline, then a create.
	hi := inst.(*hostenv.Instance)
	col := byName["fs.changes"]
	cctx, cancel := context.WithCancel(ctx)
	events := make(chan envspec.SignalEvent, 16)
	done := make(chan error, 1)
	go func() {
		done <- col.Run(cctx, nil, signalcore.EmitterFunc(func(ev envspec.SignalEvent) { events <- ev }))
	}()
	// Give the collector a baseline cycle, then change a file. The
	// default interval is 5s, so poll generously but bounded.
	time.Sleep(300 * time.Millisecond)
	target := filepath.Join(hi.LocalWorkspace(), "new.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(15 * time.Second)
	sawCreate := false
	for !sawCreate {
		select {
		case ev := <-events:
			if ch, _ := ev.Data.(envspec.FSChange); ch.Change == "created" && strings.HasSuffix(ch.Path, "new.txt") {
				sawCreate = true
			}
		case <-deadline:
			t.Fatal("fs.changes never reported the created file")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("fs.changes run: %v", err)
	}

	// Resources collector emits a sample promptly; ctx end is clean.
	rctx, rcancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer rcancel()
	var samples int
	err = byName["env.resources"].Run(rctx, nil, signalcore.EmitterFunc(func(ev envspec.SignalEvent) {
		if ev.Stream == envspec.StreamEnvResources {
			samples++
		}
	}))
	if err != nil {
		t.Fatalf("resources run: %v", err)
	}
	if samples == 0 {
		t.Fatal("resources collector emitted nothing")
	}
}

func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after dispose", pid)
}
