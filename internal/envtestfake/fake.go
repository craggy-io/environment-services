// Package envtestfake provides scriptable fake adapters — one CREATED
// and one ATTACHED — over a shared in-memory "world", so core tests can
// inject crash-window faults (partial create, destroy that leaves the
// resource present, backend-down discovery, vanishing resources) with
// no Docker daemon and no Android SDK. The world survives across
// manager instances in a test: closing a manager uncleanly and opening
// a new one on the same Dir with the same world simulates a crash.
package envtestfake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Clock is a manually advanced clock for core tests.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

func NewClock() *Clock { return &Clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// World is the fake backend: resources keyed by deterministic name.
type World struct {
	mu        sync.Mutex
	resources map[string]*Resource
	creates   int
}

// Resource is one fake external resource.
type Resource struct {
	Name     string
	EnvID    string
	External string
	Attached bool

	mu          sync.Mutex
	files       map[string][]byte
	modes       map[string]int64
	snaps       map[string]map[string][]byte
	envInjected map[string]string
	resets      int
	disposed    bool
}

func NewWorld() *World { return &World{resources: map[string]*Resource{}} }

// Creates reports how many resources Create actually made.
func (w *World) Creates() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.creates
}

// Count reports live resources.
func (w *World) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.resources)
}

// Get returns a resource by deterministic name.
func (w *World) Get(name string) *Resource {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.resources[name]
}

// Vanish removes a resource behind the manager's back.
func (w *World) Vanish(name string) {
	w.mu.Lock()
	delete(w.resources, name)
	w.mu.Unlock()
}

// Script holds the fault knobs shared by both fakes. Zero is healthy.
type Script struct {
	mu sync.Mutex

	// CreatePartial: Create makes the resource, then returns it with
	// CreateErr (or a generic error) — the crash-after-create window.
	CreatePartial bool
	// CreateErr makes Create/Attach fail (with a resource iff partial).
	CreateErr error
	// CreateGate, when non-nil, blocks Create until closed or ctx ends.
	CreateGate chan struct{}
	// DestroyErr makes Dispose fail; LeavesPresent keeps the resource.
	DestroyErr    error
	LeavesPresent bool
	// DiscoverErr makes Discover (and Observe of unknown refs) report
	// the backend down.
	DiscoverErr error
	// OpenErr makes Open (re-materialisation) fail — a backend that is
	// down at manager restart, so recovery gets no runtime.
	OpenErr error
	// ObserveAbsentAfterN: after N successful Observes of a resource,
	// Observe reports Absent. 0 = never.
	ObserveAbsentAfterN int
	// FailDestroyTimes: Dispose fails this many times, then succeeds.
	FailDestroyTimes int

	observes  map[string]int
	destroys  int
	disposedN int
}

// Set mutates the script under lock (tests flip faults mid-run).
func (s *Script) Set(fn func(*Script)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

// DisposedCount reports successful Dispose calls.
func (s *Script) DisposedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disposedN
}

// Created is the fake CREATED adapter (containers/host-like).
type Created struct {
	World  *World
	Script *Script
	KindV  envspec.Kind

	// OnExec may override instance exec; return (nil, nil) to fall through.
	OnExec func(r *Resource, cmd adapter.ExecCmd) (*adapter.Process, error)
	// TickStream, when set, makes instances expose a tick collector
	// providing this stream (for required-signal barrier tests).
	TickStream string
	TickFail   bool // tick collector Run fails immediately
	// Probe, when set, is exposed by every instance as its probe set
	// (MutationPolicy / repair tests).
	Probe *adapter.Probe

	components *func(envID string, c adapter.ComponentChange)
}

func NewCreated(w *World, kind envspec.Kind) *Created {
	if kind == "" {
		kind = envspec.Container
	}
	return &Created{World: w, Script: &Script{observes: map[string]int{}}, KindV: kind}
}

func (a *Created) script() *Script {
	if a.Script.observes == nil {
		a.Script.observes = map[string]int{}
	}
	return a.Script
}

func (a *Created) Describe() adapter.Description {
	return adapter.Description{Name: "fake-created", Kinds: []envspec.Kind{a.KindV}, Relationship: adapter.Created, Version: "1"}
}

func (a *Created) Validate(ctx context.Context, op adapter.OpContext, s envspec.Spec) error {
	if a.KindV == envspec.Host && s.Network != "" && s.Network != envspec.NetworkOpen {
		return fmt.Errorf("%w: host environments accept only network open", envspec.ErrInvalid)
	}
	if a.KindV == envspec.Android && s.Android != nil && s.Android.Image == envspec.GooglePlay && s.Android.Root == envspec.RootADB {
		return fmt.Errorf("%w: google_apis_playstore with RootADB cannot work", envspec.ErrInvalid)
	}
	return nil
}

func (a *Created) Cost(s envspec.Spec) adapter.Cost {
	pool := envspec.PoolDocker
	if a.KindV != envspec.Container {
		pool = envspec.PoolHost
	}
	return adapter.Cost{Pool: pool, CPUs: s.Resources.CPUs, MemoryMB: s.Resources.MemoryMB, DiskMB: s.Resources.DiskMB}
}

func (a *Created) Create(ctx context.Context, op adapter.OpContext, s envspec.Spec, progress func(envspec.Progress)) (adapter.Instance, error) {
	sc := a.script()
	sc.mu.Lock()
	gate, partial, createErr := sc.CreateGate, sc.CreatePartial, sc.CreateErr
	sc.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	name := adapter.NameFor(op.EnvID)
	// Idempotent by deterministic name: a retry finds the earlier result.
	if r := a.World.Get(name); r != nil {
		return &Instance{a: a, r: r}, nil
	}
	if createErr != nil && !partial {
		return nil, createErr
	}
	r := &Resource{Name: name, EnvID: op.EnvID, External: "fake-" + op.EnvID,
		files: map[string][]byte{}, modes: map[string]int64{},
		snaps: map[string]map[string][]byte{}, envInjected: map[string]string{}}
	a.World.mu.Lock()
	if existing := a.World.resources[name]; existing != nil {
		r = existing
	} else {
		a.World.resources[name] = r
		a.World.creates++
	}
	a.World.mu.Unlock()
	inst := &Instance{a: a, r: r}
	if progress != nil {
		progress(envspec.Progress{Op: "provision", Stage: "create", EnvID: op.EnvID})
	}
	if partial {
		err := createErr
		if err == nil {
			err = errors.New("fake: create died after making the resource")
		}
		return inst, err
	}
	return inst, nil
}

func (a *Created) Open(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Instance, error) {
	if sc := a.script(); sc.OpenErr != nil {
		return nil, adapter.BackendDown(sc.OpenErr)
	}
	r := a.World.Get(ref.Name)
	if r == nil {
		return nil, fmt.Errorf("%w: %s", envspec.ErrNotFound, ref.Name)
	}
	return &Instance{a: a, r: r}, nil
}

func (a *Created) Observe(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Observation, error) {
	sc := a.script()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	if sc.DiscoverErr != nil {
		return adapter.Observation{Ref: ref, Presence: adapter.Unknown, Detail: sc.DiscoverErr.Error()}, nil
	}
	r := a.World.resources[ref.Name]
	if r == nil {
		return adapter.Observation{Ref: ref, Presence: adapter.Absent}, nil
	}
	sc.observes[ref.Name]++
	if sc.ObserveAbsentAfterN > 0 && sc.observes[ref.Name] > sc.ObserveAbsentAfterN {
		return adapter.Observation{Ref: ref, Presence: adapter.Absent}, nil
	}
	return adapter.Observation{Ref: adapter.Ref{EnvID: r.EnvID, External: r.External, Name: r.Name},
		Presence: adapter.Present, Attached: true, Workspace: "/work"}, nil
}

func (a *Created) Discover(ctx context.Context, scope adapter.Scope) ([]adapter.Candidate, error) {
	sc := a.script()
	sc.mu.Lock()
	derr := sc.DiscoverErr
	sc.mu.Unlock()
	if derr != nil {
		return nil, adapter.BackendDown(derr)
	}
	a.World.mu.Lock()
	defer a.World.mu.Unlock()
	var out []adapter.Candidate
	for _, r := range a.World.resources {
		if scope.EnvID != "" && r.EnvID != scope.EnvID {
			continue
		}
		out = append(out, adapter.Candidate{Ref: adapter.Ref{EnvID: r.EnvID, External: r.External, Name: r.Name}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref.Name < out[j].Ref.Name })
	return out, nil
}

// WatchComponents implements adapter.ComponentWatcher.
func (a *Created) WatchComponents(fn func(envID string, c adapter.ComponentChange)) (stop func()) {
	sink := &fn
	a.components = sink
	return func() {
		if a.components == sink {
			a.components = nil
		}
	}
}

// ChangeComponent reports a transition to the watching core (tests).
func (a *Created) ChangeComponent(envID string, c adapter.ComponentChange) {
	if a.components != nil {
		(*a.components)(envID, c)
	}
}

// Check implements adapter.Checker.
func (a *Created) Check(ctx context.Context) error {
	sc := a.script()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.DiscoverErr
}

// EnsureImage / ImageExists implement adapter.ImageBuilder (in-memory).
func (a *Created) EnsureImage(ctx context.Context, p envspec.Profile, digest string, progress func(envspec.Progress)) (string, string, error) {
	ref := p.Image
	if p.Build != nil {
		ref = "craggy/" + strings.ToLower(p.Name) + ":" + digest
	}
	return ref, "sha256:fakeimg-" + digest + ref, nil
}

func (a *Created) ImageExists(ctx context.Context, ref string) (string, bool, error) {
	return "", false, nil
}

// Instance is one fake live resource.
type Instance struct {
	a *Created
	r *Resource
	// Device, when set (attached fake), is the serial identity.
	Device string
	att    *Attached
}

func (i *Instance) Ref() adapter.Ref {
	return adapter.Ref{EnvID: i.r.EnvID, External: i.r.External, Name: i.r.Name}
}

func (i *Instance) script() *Script {
	if i.att != nil {
		return i.att.script()
	}
	return i.a.script()
}

func (i *Instance) world() *World {
	if i.att != nil {
		return i.att.World
	}
	return i.a.World
}

func (i *Instance) Dispose(ctx context.Context, op adapter.OpContext, disp adapter.Disposition) error {
	sc := i.script()
	sc.mu.Lock()
	if sc.FailDestroyTimes > 0 {
		sc.FailDestroyTimes--
		sc.mu.Unlock()
		return errors.New("fake: destroy failed (scripted)")
	}
	derr, leaves := sc.DestroyErr, sc.LeavesPresent
	if derr == nil {
		sc.disposedN++
	}
	sc.mu.Unlock()
	if derr != nil {
		return derr
	}
	if disp == adapter.Disconnect {
		i.r.mu.Lock()
		i.r.Attached = false
		i.r.mu.Unlock()
		return nil
	}
	if leaves {
		return nil // claims success but the resource is still there
	}
	w := i.world()
	w.mu.Lock()
	delete(w.resources, i.r.Name)
	w.mu.Unlock()
	i.r.mu.Lock()
	i.r.disposed = true
	i.r.mu.Unlock()
	return nil
}

// Reset implements adapter.Resetter: back to the "clean" snapshot.
func (i *Instance) Reset(ctx context.Context, op adapter.OpContext) error {
	i.r.mu.Lock()
	defer i.r.mu.Unlock()
	i.r.resets++
	i.r.files = map[string][]byte{}
	if s, ok := i.r.snaps["clean"]; ok {
		for k, v := range s {
			i.r.files[k] = append([]byte(nil), v...)
		}
	}
	return nil
}

// Resets reports Reset calls (tests).
func (r *Resource) Resets() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resets
}

// Files returns a copy of the resource FS (tests).
func (r *Resource) Files() map[string][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]byte, len(r.files))
	for k, v := range r.files {
		out[k] = append([]byte(nil), v...)
	}
	return out
}

// WriteFile seeds a file (tests).
func (r *Resource) WriteFile(path string, data []byte) {
	r.mu.Lock()
	if r.files == nil {
		r.files, r.modes = map[string][]byte{}, map[string]int64{}
	}
	r.files[clean(path)] = data
	r.modes[clean(path)] = 0o100644
	r.mu.Unlock()
}

// InjectedEnv returns env injections (tests).
func (r *Resource) InjectedEnv() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]string{}
	for k, v := range r.envInjected {
		out[k] = v
	}
	return out
}

// --- Instance optional capabilities ---

func (i *Instance) InjectEnv(name, value string) {
	i.r.mu.Lock()
	if i.r.envInjected == nil {
		i.r.envInjected = map[string]string{}
	}
	i.r.envInjected[name] = value
	i.r.mu.Unlock()
}

func (i *Instance) InjectFile(ctx context.Context, path string, value []byte) error {
	i.r.mu.Lock()
	if i.r.files == nil {
		i.r.files, i.r.modes = map[string][]byte{}, map[string]int64{}
	}
	i.r.files[clean(path)] = append([]byte(nil), value...)
	i.r.modes[clean(path)] = 0o100400
	i.r.mu.Unlock()
	return nil
}

func (i *Instance) Save(ctx context.Context, op adapter.OpContext, name string) error {
	i.r.mu.Lock()
	defer i.r.mu.Unlock()
	if i.r.snaps == nil {
		i.r.snaps = map[string]map[string][]byte{}
	}
	cp := make(map[string][]byte, len(i.r.files))
	for k, v := range i.r.files {
		cp[k] = append([]byte(nil), v...)
	}
	i.r.snaps[name] = cp
	return nil
}

func (i *Instance) Load(ctx context.Context, op adapter.OpContext, name string) error {
	i.r.mu.Lock()
	defer i.r.mu.Unlock()
	s, ok := i.r.snaps[name]
	if !ok {
		return fmt.Errorf("snapshot %q: %w", name, envspec.ErrNotFound)
	}
	i.r.files = make(map[string][]byte, len(s))
	for k, v := range s {
		i.r.files[k] = append([]byte(nil), v...)
	}
	return nil
}

func (i *Instance) Snapshots(ctx context.Context, op adapter.OpContext) ([]string, error) {
	i.r.mu.Lock()
	defer i.r.mu.Unlock()
	out := make([]string, 0, len(i.r.snaps))
	for k := range i.r.snaps {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

func (i *Instance) Manifest(ctx context.Context, op adapter.OpContext, root string) ([]envspec.Entry, error) {
	i.r.mu.Lock()
	defer i.r.mu.Unlock()
	prefix := clean(root)
	var out []envspec.Entry
	for p, content := range i.r.files {
		if prefix != "/" && !strings.HasPrefix(p, prefix+"/") && p != prefix {
			continue
		}
		rel := strings.TrimPrefix(p, prefix+"/")
		if prefix == "/" {
			rel = strings.TrimPrefix(p, "/")
		}
		d, _, _ := xfer.DigestOf(bytes.NewReader(content))
		mode := i.r.modes[p]
		if mode == 0 {
			mode = 0o100644
		}
		out = append(out, envspec.Entry{Path: rel, Mode: mode, Size: int64(len(content)), Digest: d})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out, nil
}

func (i *Instance) StatFS(ctx context.Context, op adapter.OpContext, path string) (envspec.Entry, error) {
	i.r.mu.Lock()
	defer i.r.mu.Unlock()
	p := clean(path)
	if data, ok := i.r.files[p]; ok {
		mode := i.r.modes[p]
		if mode == 0 {
			mode = 0o100644
		}
		return envspec.Entry{Path: path, Mode: mode, Size: int64(len(data))}, nil
	}
	for fp := range i.r.files {
		if strings.HasPrefix(fp, p+"/") {
			return envspec.Entry{Path: path, Mode: 0o040755}, nil
		}
	}
	return envspec.Entry{}, fmt.Errorf("stat %s: %w", path, envspec.ErrNotFound)
}

func (i *Instance) ListFS(ctx context.Context, op adapter.OpContext, path string) ([]envspec.Entry, error) {
	man, err := i.Manifest(ctx, op, path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []envspec.Entry
	for _, e := range man {
		if idx := strings.Index(e.Path, "/"); idx >= 0 {
			dir := e.Path[:idx]
			if !seen[dir] {
				seen[dir] = true
				out = append(out, envspec.Entry{Path: dir, Mode: 0o040755})
			}
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// PutArchive / GetArchive implement adapter.Transferrer over the
// in-memory FS via real tar (xfer), like the old envtest fake.
func (i *Instance) PutArchive(ctx context.Context, op adapter.OpContext, dst string, tarStream io.Reader) error {
	data, err := io.ReadAll(tarStream)
	if err != nil {
		return err
	}
	return i.extractTar(dst, data)
}

func (i *Instance) GetArchive(ctx context.Context, op adapter.OpContext, src string) (io.ReadCloser, error) {
	b, err := i.makeTar(src, nil)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (i *Instance) extractTar(dst string, data []byte) error {
	sink := envspec.NewMemSink()
	if _, err := xfer.ReadTar(context.Background(), bytes.NewReader(data), sink, xfer.Options{}); err != nil {
		return err
	}
	i.r.mu.Lock()
	defer i.r.mu.Unlock()
	if i.r.files == nil {
		i.r.files, i.r.modes = map[string][]byte{}, map[string]int64{}
	}
	for p, content := range sink.Files {
		full := clean(strings.TrimSuffix(dst, "/") + "/" + p)
		if p == xfer.DeleteListName {
			for _, line := range strings.Split(string(content), "\n") {
				if line = strings.TrimSpace(line); line != "" {
					delete(i.r.files, clean(strings.TrimSuffix(dst, "/")+"/"+line))
				}
			}
			continue
		}
		i.r.files[full] = content
		i.r.modes[full] = sink.Modes[p]
	}
	return nil
}

func (i *Instance) makeTar(root string, only []string) ([]byte, error) {
	i.r.mu.Lock()
	files := map[string][]byte{}
	prefix := clean(root)
	for p, content := range i.r.files {
		if prefix != "/" && !strings.HasPrefix(p, prefix+"/") && p != prefix {
			continue
		}
		rel := strings.TrimPrefix(p, prefix+"/")
		if prefix == "/" {
			rel = strings.TrimPrefix(p, "/")
		}
		if len(only) > 0 {
			found := false
			for _, o := range only {
				if o == rel {
					found = true
				}
			}
			if !found {
				continue
			}
		}
		files[rel] = content
	}
	i.r.mu.Unlock()
	var buf bytes.Buffer
	_, err := xfer.WriteTar(context.Background(), envspec.MemSource(files), &buf, xfer.Options{})
	return buf.Bytes(), err
}

// Arch implements adapter.Arched.
func (i *Instance) Arch() string { return "amd64" }

// Probes implements adapter.ProbeProvider when Created.Probe is set.
func (i *Instance) Probes() []adapter.Probe {
	if i.a == nil || i.a.Probe == nil {
		return nil
	}
	return []adapter.Probe{*i.a.Probe}
}

// Collectors implements adapter.CollectorProvider when TickStream is set.
func (i *Instance) Collectors() []signalcore.Collector {
	if i.a == nil || i.a.TickStream == "" {
		return nil
	}
	return []signalcore.Collector{&TickCollector{Stream: i.a.TickStream, Fail: i.a.TickFail}}
}

func (i *Instance) CollectorEnvInfo() signalcore.EnvInfo {
	kind := "container"
	if i.a != nil {
		kind = string(i.a.KindV)
	}
	return signalcore.EnvInfo{ID: i.r.EnvID, Kind: kind}
}

// TickCollector emits scripted events; Run blocks until ctx ends.
type TickCollector struct {
	Stream string
	Fail   bool
	// EmitFn is called each tick; nil emits a default tick.
	EmitFn   func(n int, out signalcore.Emitter)
	Interval time.Duration
	Runs     int32 // atomic-ish under mu below
	mu       sync.Mutex
}

func (c *TickCollector) Name() string { return "fake-tick" }
func (c *TickCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: c.Stream, Description: "fake ticks"}}
}
func (c *TickCollector) Params() *jsonschema.Schema { return nil }

func (c *TickCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	c.mu.Lock()
	c.Runs++
	c.mu.Unlock()
	if c.Fail {
		return errors.New("fake: tick collector died")
	}
	iv := c.Interval
	if iv <= 0 {
		iv = 10 * time.Millisecond
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for n := 0; ; n++ {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if c.EmitFn != nil {
				c.EmitFn(n, out)
			} else {
				out.Emit(envspec.SignalEvent{Stream: c.Stream, Data: map[string]any{"tick": n}})
			}
		}
	}
}

func clean(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	parts := []string{}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
		case "..":
			if len(parts) > 0 {
				parts = parts[:len(parts)-1]
			}
		default:
			parts = append(parts, seg)
		}
	}
	return "/" + strings.Join(parts, "/")
}

// finished builds an already-exited process.
func finished(stdout, stderr string, code int) *adapter.Process {
	return adapter.NewProcess(bytes.NewReader([]byte(stdout)), bytes.NewReader([]byte(stderr)),
		nopWriteCloser{}, func() (int, error) { return code, nil }, func(string) error { return nil })
}

func finishedBytes(stdout []byte, code int) *adapter.Process {
	return adapter.NewProcess(bytes.NewReader(stdout), bytes.NewReader(nil),
		nopWriteCloser{}, func() (int, error) { return code, nil }, func(string) error { return nil })
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// Exec implements a tiny shell-less command set: echo, cat, true/false,
// sleep-forever (for service processes), plus tar/agent stdin idioms.
func (i *Instance) Exec(ctx context.Context, op adapter.OpContext, c adapter.ExecCmd) (*adapter.Process, error) {
	if i.a != nil && i.a.OnExec != nil {
		if p, err := i.a.OnExec(i.r, c); p != nil || err != nil {
			return p, err
		}
	}
	args := c.Argv
	if len(args) == 0 {
		return nil, fmt.Errorf("empty argv")
	}
	if c.Stdin != nil {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "tar -x -C") {
			dst := fieldAfter(args, "-C")
			data, err := io.ReadAll(c.Stdin)
			if err != nil {
				return nil, err
			}
			if err := i.extractTar(dst, data); err != nil {
				return finished("", err.Error(), 1), nil
			}
			return finished("", "", 0), nil
		}
		if strings.Contains(joined, "recv") {
			root := args[len(args)-1]
			if root == "mirror" {
				root = args[len(args)-2]
			}
			data, err := io.ReadAll(c.Stdin)
			if err != nil {
				return nil, err
			}
			if err := i.extractTar(root, data); err != nil {
				return finished("", err.Error(), 1), nil
			}
			return finished("", "", 0), nil
		}
		if strings.Contains(joined, "send") {
			root := args[len(args)-1]
			body, _ := io.ReadAll(c.Stdin)
			var paths []string
			for _, line := range strings.Split(string(body), "\n") {
				line = strings.TrimSpace(line)
				if line == "." {
					break
				}
				if line != "" {
					paths = append(paths, line)
				}
			}
			tarBytes, err := i.makeTar(root, paths)
			if err != nil {
				return finished("", err.Error(), 1), nil
			}
			return finishedBytes(tarBytes, 0), nil
		}
	}
	switch args[0] {
	case "echo":
		return finished(strings.Join(args[1:], " ")+"\n", "", 0), nil
	case "true":
		return finished("", "", 0), nil
	case "false":
		return finished("", "", 1), nil
	case "sleep-forever", "serve":
		// A long-running service process: exits when ctx ends or killed.
		done := make(chan struct{})
		var once sync.Once
		stop := func() { once.Do(func() { close(done) }) }
		go func() { <-ctx.Done(); stop() }()
		return adapter.NewProcess(bytes.NewReader(nil), bytes.NewReader(nil), nopWriteCloser{},
			func() (int, error) { <-done; return 0, nil },
			func(string) error { stop(); return nil }), nil
	case "cat":
		if len(args) < 2 {
			return finished("", "cat: no file\n", 1), nil
		}
		path := args[1]
		if !strings.HasPrefix(path, "/") {
			base := c.Dir
			if base == "" {
				base = "/work"
			}
			path = strings.TrimSuffix(base, "/") + "/" + path
		}
		i.r.mu.Lock()
		data, ok := i.r.files[clean(path)]
		i.r.mu.Unlock()
		if !ok {
			return finished("", "cat: "+args[1]+": no such file\n", 1), nil
		}
		return finishedBytes(data, 0), nil
	case "getprop":
		if len(args) > 1 && args[1] == "sys.boot_completed" {
			return finished("1\n", "", 0), nil
		}
		return finished("\n", "", 0), nil
	case "id":
		return finished("uid=0(root) gid=0(root)\n", "", 0), nil
	default:
		if len(args) >= 3 && args[1] == "manifest" {
			man, _ := i.Manifest(ctx, op, args[2])
			var b strings.Builder
			for _, e := range man {
				b.WriteString(xfer.FormatEntry(e) + "\n")
			}
			b.WriteString(".\n")
			return finished(b.String(), "", 0), nil
		}
		return finished("", "fake: unknown command "+args[0]+"\n", 127), nil
	}
}

func fieldAfter(args []string, flag string) string {
	for j, a := range args {
		if a == flag && j+1 < len(args) {
			return strings.Trim(args[j+1], "'\"")
		}
	}
	return "/"
}
