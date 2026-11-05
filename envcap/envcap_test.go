package envcap_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	registry "github.com/craggy-io/capability-services/registry"

	"github.com/craggy-io/environment-services/envcap"
	"github.com/craggy-io/environment-services/envspec"
)

// --- in-memory fake Artifacts port (Stage/Keep) ---

type fakeArtifacts struct {
	mu    sync.Mutex
	byID  map[string]*envspec.MemSink
	n     int
	evid  []envcap.Evidence
	ops   []envcap.Operation
	keptN []string // names kept, in order
}

func newFakeArtifacts() *fakeArtifacts {
	return &fakeArtifacts{byID: map[string]*envspec.MemSink{}}
}

func (a *fakeArtifacts) seed(files map[string][]byte) string {
	sink := envspec.NewMemSink()
	for name, body := range files {
		_ = sink.Add(name, 0o100644, strings.NewReader(string(body)))
	}
	a.n++
	id := fmt.Sprintf("art_%d", a.n)
	a.byID[id] = sink
	return id
}

func (a *fakeArtifacts) publish(name string, sink *envspec.MemSink) envcap.Ref {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	id := fmt.Sprintf("art_%d", a.n)
	a.byID[id] = sink
	a.keptN = append(a.keptN, name)
	return envcap.Ref{ID: id}
}

func (a *fakeArtifacts) Run(ctx context.Context, op envcap.Operation, fn func(context.Context, envcap.ArtifactRun) error) (map[string]envcap.Ref, error) {
	a.mu.Lock()
	a.ops = append(a.ops, op)
	a.mu.Unlock()
	run := &fakeRun{a: a, staged: map[string]*envspec.MemSink{}, kept: map[string]envcap.Ref{}}
	err := fn(ctx, run)
	if err != nil && len(run.kept) > 0 {
		return run.kept, &envcap.PartialOutputs{Outputs: run.kept, Err: err}
	}
	return run.kept, err
}

type fakeRun struct {
	a      *fakeArtifacts
	staged map[string]*envspec.MemSink
	kept   map[string]envcap.Ref
}

func (r *fakeRun) Source(ctx context.Context, id string) (envspec.Source, error) {
	sink, ok := r.a.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: artifact %s", envspec.ErrNotFound, id)
	}
	return sink.Source(), nil
}

func (r *fakeRun) Stage(ctx context.Context, name string, build func(envspec.Sink) error) error {
	sink := envspec.NewMemSink()
	r.staged[name] = sink
	// The sink retains whatever was added before a build error:
	// that is what Keep then publishes as deliberate partial output.
	return build(sink)
}

func (r *fakeRun) Keep(name string) error {
	sink, ok := r.staged[name]
	if !ok {
		return fmt.Errorf("nothing staged under %q", name)
	}
	r.kept[name] = r.a.publish(name, sink)
	return nil
}

func (r *fakeRun) Record(ctx context.Context, ev envcap.Evidence) error {
	r.a.mu.Lock()
	defer r.a.mu.Unlock()
	r.a.evid = append(r.a.evid, ev)
	return nil
}

// --- in-memory fake Manager ---

var testOwner = envspec.Owner{ID: "agent:test", Session: "s1", Job: "owner-job-not-a-registry-job"}

type fakeManager struct {
	mu       sync.Mutex
	seq      int
	byKey    map[string]string
	envs     map[string]*fakeEnv
	provErr  error
	pins     map[string]envcap.PinRecord
	pinErr   map[string]error
	creds    map[string]fakeCred
	leases   map[string]envcap.CredLease
	events   []envspec.SignalEvent
	execFn   func(envID string, argv []string) envcap.ExecResult
	typed    []string
	borrowed []string
}

type fakeEnv struct {
	info  envspec.Info
	spec  envspec.Spec
	files map[string][]byte // flat "dir/name" → content
	opID  string
}

type fakeCred struct {
	in     envcap.CredInput
	secret map[string]string // held apart; must never be returned
}

func newFakeManager() *fakeManager {
	return &fakeManager{
		byKey:  map[string]string{},
		envs:   map[string]*fakeEnv{},
		pins:   map[string]envcap.PinRecord{},
		pinErr: map[string]error{},
		creds:  map[string]fakeCred{},
		leases: map[string]envcap.CredLease{},
	}
}

var _ envcap.Manager = (*fakeManager)(nil)

func (f *fakeManager) env(id string) (*fakeEnv, error) {
	e, ok := f.envs[id]
	if !ok {
		return nil, fmt.Errorf("%w: environment %s", envspec.ErrNotFound, id)
	}
	return e, nil
}

func (f *fakeManager) ProvisionEnv(ctx context.Context, req envcap.ProvisionRequest) (envcap.EnvResult, error) {
	if f.provErr != nil {
		return envcap.EnvResult{}, f.provErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Spec.Key != "" {
		if id, ok := f.byKey[req.Spec.Key]; ok {
			e := f.envs[id]
			return envcap.EnvResult{Env: id, Info: e.info, OperationID: e.opID, Generation: 1, LeaseEpoch: 1}, nil
		}
	}
	f.seq++
	id := fmt.Sprintf("env_fake_%d", f.seq)
	if req.Progress != nil {
		req.Progress(envspec.Progress{Op: "provision", Stage: "creating", EnvID: id})
	}
	kind := envspec.Container
	if req.Spec.Profile == "android-play" {
		kind = envspec.Android
	}
	e := &fakeEnv{spec: req.Spec, files: map[string][]byte{}, opID: fmt.Sprintf("op_durable_%d", f.seq)}
	e.info = envspec.Info{ID: id, Kind: kind, Profile: req.Spec.Profile, State: envspec.StatePresent,
		Owner: req.Owner, Workspace: "/work", Created: time.Now()}
	f.envs[id] = e
	if req.Spec.Key != "" {
		f.byKey[req.Spec.Key] = id
	}
	return envcap.EnvResult{Env: id, Info: e.info, OperationID: e.opID, Generation: 1, LeaseEpoch: 1}, nil
}

func (f *fakeManager) ReleaseEnv(ctx context.Context, id string) (envcap.EnvResult, error) {
	if _, err := f.env(id); err != nil {
		return envcap.EnvResult{}, err
	}
	delete(f.envs, id)
	return envcap.EnvResult{Env: id, OperationID: "op_release_" + id, Generation: 1, LeaseEpoch: 1}, nil
}

func (f *fakeManager) EnvInfo(id string) (envspec.Info, error) {
	e, err := f.env(id)
	if err != nil {
		return envspec.Info{}, err
	}
	return e.info, nil
}

func (f *fakeManager) EnvIdentity(id string) (envcap.EnvIdentity, error) {
	if _, err := f.env(id); err != nil {
		return envcap.EnvIdentity{}, err
	}
	return envcap.EnvIdentity{Generation: 1, LeaseID: "lease_" + id, LeaseEpoch: 1}, nil
}

func (f *fakeManager) ListEnvs(flt envcap.ListFilter) []envspec.Info {
	out := []envspec.Info{}
	for _, e := range f.envs {
		if flt.Owner != "" && e.info.Owner.ID != flt.Owner {
			continue
		}
		if flt.Session != "" && e.info.Owner.Session != flt.Session {
			continue
		}
		out = append(out, e.info)
	}
	return out
}

func (f *fakeManager) RenewLease(id string) error {
	_, err := f.env(id)
	return err
}

func (f *fakeManager) Borrow(ctx context.Context, id, job string) (func(), error) {
	if _, err := f.env(id); err != nil {
		return nil, err
	}
	f.borrowed = append(f.borrowed, job)
	return func() {}, nil
}

func (f *fakeManager) Profiles() []envspec.Profile {
	return []envspec.Profile{
		{Name: "linux-tools", Kind: envspec.Container, Description: "small linux toolbox"},
		{Name: "android-play", Kind: envspec.Android, Description: "play-store android"},
	}
}

func (f *fakeManager) Profile(name string) (envspec.Profile, bool) {
	for _, p := range f.Profiles() {
		if p.Name == name {
			return p, true
		}
	}
	return envspec.Profile{}, false
}

func (f *fakeManager) ProfileStatus(name string) envspec.ProfileStatus {
	p, ok := f.Profile(name)
	if !ok {
		return envspec.ProfileStatus{Ready: false, NotReady: "unknown profile"}
	}
	return envspec.ProfileStatus{Profile: p, Ready: true}
}

func (f *fakeManager) ExecEnv(ctx context.Context, id string, cmd envspec.Cmd) (envcap.ExecResult, error) {
	e, err := f.env(id)
	if err != nil {
		return envcap.ExecResult{}, err
	}
	if f.execFn != nil {
		return f.execFn(id, cmd.Argv), nil
	}
	if len(cmd.Argv) == 2 && cmd.Argv[0] == "cat" {
		body, ok := e.files[strings.TrimPrefix(cmd.Argv[1], "/work/")]
		if !ok {
			return envcap.ExecResult{ProcID: "proc_1", Stderr: []byte("no such file"), Exit: 1}, nil
		}
		return envcap.ExecResult{ProcID: "proc_1", Stdout: body, Exit: 0}, nil
	}
	return envcap.ExecResult{ProcID: "proc_1", Exit: 0}, nil
}

func copyInto(ctx context.Context, src envspec.Source, add func(path string, body []byte)) (envspec.TransferStats, error) {
	var st envspec.TransferStats
	for en, err := range src.Entries(ctx) {
		if err != nil {
			return st, err
		}
		if en.IsDir() {
			continue
		}
		rc, err := src.Open(ctx, en)
		if err != nil {
			return st, err
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return st, err
		}
		add(en.Path, body)
		st.Files++
		st.Bytes += int64(len(body))
	}
	return st, nil
}

func (f *fakeManager) Push(ctx context.Context, id string, src envspec.Source, dst string) (envspec.TransferStats, error) {
	e, err := f.env(id)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	rel := strings.Trim(strings.TrimPrefix(dst, "/work"), "/")
	return copyInto(ctx, src, func(path string, body []byte) {
		key := path
		if rel != "" {
			key = rel + "/" + path
		}
		e.files[key] = body
	})
}

// pullFail, when > 0, makes Pull fail after that many files.
var pullFailAfter int

func (f *fakeManager) Pull(ctx context.Context, id, src string, dst envspec.Sink) (envspec.TransferStats, error) {
	e, err := f.env(id)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	rel := strings.Trim(strings.TrimPrefix(src, "/work"), "/")
	var st envspec.TransferStats
	names := make([]string, 0, len(e.files))
	for name := range e.files {
		if rel == "" || strings.HasPrefix(name, rel) {
			names = append(names, name)
		}
	}
	for _, name := range names {
		if err := dst.Add(name, 0o100644, strings.NewReader(string(e.files[name]))); err != nil {
			return st, err
		}
		st.Files++
		st.Bytes += int64(len(e.files[name]))
		if pullFailAfter > 0 && int(st.Files) >= pullFailAfter {
			return st, errors.New("guest died mid-pull")
		}
	}
	return st, nil
}

func (f *fakeManager) Stat(ctx context.Context, id, path string) (envspec.Entry, error) {
	e, err := f.env(id)
	if err != nil {
		return envspec.Entry{}, err
	}
	body, ok := e.files[strings.Trim(strings.TrimPrefix(path, "/work"), "/")]
	if !ok {
		return envspec.Entry{}, fmt.Errorf("%w: %s", envspec.ErrNotFound, path)
	}
	return envspec.Entry{Path: path, Mode: 0o100644, Size: int64(len(body))}, nil
}

func (f *fakeManager) ListFS(ctx context.Context, id, path string, recursive bool, limit int) ([]envspec.Entry, error) {
	e, err := f.env(id)
	if err != nil {
		return nil, err
	}
	out := []envspec.Entry{}
	for name, body := range e.files {
		out = append(out, envspec.Entry{Path: name, Mode: 0o100644, Size: int64(len(body))})
	}
	return out, nil
}

func (f *fakeManager) Snapshot(ctx context.Context, id, name string) error {
	_, err := f.env(id)
	return err
}

func (f *fakeManager) Restore(ctx context.Context, id, name string) error {
	_, err := f.env(id)
	return err
}

func (f *fakeManager) Snapshots(ctx context.Context, id string) ([]string, error) {
	if _, err := f.env(id); err != nil {
		return nil, err
	}
	return []string{}, nil
}

func (f *fakeManager) HostSnapshot() envspec.HostSnapshot {
	return envspec.HostSnapshot{CPUs: 8, MemFreeMB: 1024}
}

func (f *fakeManager) PoolCapacities() []envspec.PoolCapacity {
	return []envspec.PoolCapacity{{Pool: envspec.PoolHost, CPUs: 8, MemoryMB: 16384}}
}

// --- fake android/signals/vault/rules ---

type fakeDevice struct{ installed string }

func (d *fakeDevice) Install(ctx context.Context, apk io.Reader, replace bool) error {
	b, err := io.ReadAll(apk)
	d.installed = string(b)
	return err
}
func (d *fakeDevice) Screenshot(ctx context.Context) ([]byte, error) {
	return []byte("PNGDATA"), nil
}
func (d *fakeDevice) Tap(ctx context.Context, x, y int) error { return nil }
func (d *fakeDevice) Swipe(ctx context.Context, a, b, c, e, ms int) error {
	return nil
}
func (d *fakeDevice) KeyEvent(ctx context.Context, code int) error { return nil }
func (d *fakeDevice) Text(ctx context.Context, text string) error  { return nil }

func (f *fakeManager) Android(id string) (envcap.AndroidDevice, error) {
	e, err := f.env(id)
	if err != nil {
		return nil, err
	}
	if e.info.Kind != envspec.Android {
		return nil, fmt.Errorf("%w: not an android env", envspec.ErrInvalid)
	}
	return &fakeDevice{}, nil
}

func (f *fakeManager) TypeSecret(ctx context.Context, envID, leaseID, field string) error {
	if _, err := f.env(envID); err != nil {
		return err
	}
	f.typed = append(f.typed, leaseID+"/"+field) // never the value
	return nil
}

func (f *fakeManager) Streams(envID string) []envcap.StreamInfo {
	out := []envcap.StreamInfo{{Stream: "env.process@1", Description: "process output", Cost: "low"}}
	if envID != "" {
		for _, p := range f.Pins(envID) {
			for i := range out {
				if out[i].Stream == p.Stream {
					out[i].Pins = append(out[i].Pins, p.ID)
					out[i].Running = true
				}
			}
		}
	}
	return out
}

func (f *fakeManager) PinStream(ctx context.Context, ownerID, envID, stream string, params json.RawMessage) (envcap.PinRecord, error) {
	if _, err := f.env(envID); err != nil {
		return envcap.PinRecord{}, err
	}
	if err := f.pinErr[stream]; err != nil {
		return envcap.PinRecord{}, err
	}
	for _, p := range f.pins {
		if p.Env == envID && p.Stream == stream && string(p.Params) == string(params) {
			return p, nil
		}
	}
	rec := envcap.PinRecord{ID: "pin_" + stream, Env: envID, Stream: stream, Params: params, Owner: ownerID, Since: time.Now()}
	f.pins[rec.ID] = rec
	return rec, nil
}

func (f *fakeManager) UnpinStream(pinID string) (envcap.PinRecord, error) {
	rec, ok := f.pins[pinID]
	if !ok {
		return envcap.PinRecord{}, fmt.Errorf("%w: pin %s", envspec.ErrNotFound, pinID)
	}
	delete(f.pins, pinID)
	return rec, nil
}

func (f *fakeManager) Pins(envID string) []envcap.PinRecord {
	out := []envcap.PinRecord{}
	for _, p := range f.pins {
		if envID == "" || p.Env == envID {
			out = append(out, p)
		}
	}
	return out
}

func (f *fakeManager) StreamState(stream, source string, params json.RawMessage) envspec.StreamState {
	for _, p := range f.pins {
		if p.Env == source && p.Stream == stream {
			return envspec.StreamState{Running: true, Collectors: []string{"col_1"}, Since: p.Since}
		}
	}
	return envspec.StreamState{}
}

func (f *fakeManager) RunningCollectors() []envcap.RunningCollector {
	out := []envcap.RunningCollector{}
	for _, p := range f.pins {
		out = append(out, envcap.RunningCollector{ID: "col_1", Name: p.Stream, Source: p.Env, Streams: []string{p.Stream}})
	}
	return out
}

func (f *fakeManager) QuerySignals(sel envspec.Selector, where map[string]any, from, to time.Time) ([]envspec.SignalEvent, envspec.Quality, error) {
	return f.events, envspec.Quality{}, nil
}

func (f *fakeManager) TailSignals(ctx context.Context, sel envspec.Selector, where map[string]any, wait time.Duration, limit int) ([]envspec.SignalEvent, envspec.Quality, error) {
	return f.events, envspec.Quality{}, nil
}

func (f *fakeManager) SignalCoverage(sel envspec.Selector, from, to time.Time) []envspec.Coverage {
	out := []envspec.Coverage{}
	for _, stream := range sel.Streams {
		for _, src := range sel.Sources {
			status := "not_running"
			for _, p := range f.pins {
				if p.Stream == stream && p.Env == src {
					status = "covered"
				}
			}
			out = append(out, envspec.Coverage{Stream: stream, Source: src, Status: status})
		}
	}
	return out
}

type fakeCapture struct{ events []envspec.SignalEvent }

func (c *fakeCapture) ID() string { return "cap_1" }
func (c *fakeCapture) Source() envspec.Source {
	return envspec.MemSource(map[string][]byte{"range.json": []byte("{}")})
}
func (c *fakeCapture) Events() ([]envspec.SignalEvent, envspec.Quality, error) {
	return c.events, envspec.Quality{}, nil
}
func (c *fakeCapture) Close() {}

func (f *fakeManager) CaptureSignals(sel envspec.Selector, where map[string]any, from, to time.Time) (envcap.Capture, error) {
	return &fakeCapture{events: f.events}, nil
}

func (f *fakeManager) ListCreds(ctx context.Context) ([]envcap.CredView, error) {
	out := []envcap.CredView{}
	for id, c := range f.creds {
		out = append(out, envcap.CredView{ID: id, Kind: c.in.Kind, Name: c.in.Name,
			Public: c.in.Public, Labels: c.in.Labels, Available: true})
	}
	return out, nil
}

func (f *fakeManager) LeaseCred(ctx context.Context, req envcap.LeaseCredRequest) (envcap.CredLease, error) {
	c, ok := f.creds[req.Cred]
	if !ok {
		return envcap.CredLease{}, fmt.Errorf("%w: cred %s", envspec.ErrNotFound, req.Cred)
	}
	l := envcap.CredLease{Lease: "lease_1", Cred: req.Cred, Kind: c.in.Kind,
		Holder: req.Holder, Owner: req.Owner, Public: c.in.Public}
	f.leases[l.Lease] = l
	return l, nil
}

func (f *fakeManager) ReleaseCredLease(ctx context.Context, leaseID string) error {
	delete(f.leases, leaseID)
	return nil
}

func (f *fakeManager) ListCredLeases(ctx context.Context) ([]envcap.CredLease, error) {
	out := []envcap.CredLease{}
	for _, l := range f.leases {
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeManager) PutCred(ctx context.Context, in envcap.CredInput) (string, error) {
	id := in.ID
	if id == "" {
		id = "cred_1"
	}
	f.creds[id] = fakeCred{in: in, secret: in.Secret}
	return id, nil
}

func (f *fakeManager) DeleteCred(ctx context.Context, cred string) error {
	delete(f.creds, cred)
	return nil
}

func (f *fakeManager) SetCredPolicy(ctx context.Context, cred string, p envcap.CredPolicy) error {
	return nil
}

func (f *fakeManager) LoadRule(ctx context.Context, source []byte, codeRef string, params map[string]any) (string, error) {
	if strings.Contains(string(source), "badrule") {
		return "", errors.New("compile: undefined badrule")
	}
	return "rule_1", nil
}

func (f *fakeManager) UnloadRule(id string) error { return nil }

func (f *fakeManager) ListRules() []envcap.RuleInfo {
	return []envcap.RuleInfo{{Instance: "rule_1", Rule: "r", Version: 1, State: "active"}}
}

func (f *fakeManager) TestRule(ctx context.Context, source []byte, params map[string]any, events []envspec.SignalEvent) (envcap.RuleTest, error) {
	return envcap.RuleTest{Rule: "r", Emits: 0}, nil
}

func (f *fakeManager) WaitRuleTriggers(ctx context.Context, rule string, wait time.Duration) ([]envcap.RuleTrigger, error) {
	return nil, nil
}

func (f *fakeManager) RecentRuleTriggers(since time.Time, rule string) []envcap.RuleTrigger {
	return nil
}

// --- registry setup + call helper ---

func setupSet(t *testing.T) (*registry.Registry, *fakeManager, *fakeArtifacts) {
	t.Helper()
	pullFailAfter = 0
	mgr := newFakeManager()
	art := newFakeArtifacts()
	reg := registry.New()
	t.Cleanup(reg.Close)
	if err := reg.Add(envcap.Capabilities(envcap.Deps{
		Manager: mgr, Artifacts: art,
		Owner: func(ctx context.Context) (envspec.Owner, error) { return testOwner, nil },
	})...); err != nil {
		t.Fatal(err)
	}
	if err := reg.AddGroup(envcap.Groups()...); err != nil {
		t.Fatal(err)
	}
	return reg, mgr, art
}

func call(t *testing.T, reg *registry.Registry, name string, in map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(in)
	out, err := reg.Invoke(context.Background(), name, b)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("%s decode: %v (%s)", name, err, out)
	}
	return m
}

func TestCapabilitySurfaceComplete(t *testing.T) {
	reg, _, _ := setupSet(t)
	want := []string{
		"env.status", "env.profiles.list", "env.provision", "env.list", "env.get",
		"env.release", "env.keepalive", "env.exec", "env.fs.list", "env.fs.stat",
		"env.push", "env.pull", "env.snapshot.save", "env.snapshot.load", "env.snapshot.list",
		"env.android.install", "env.android.screenshot", "env.android.input",
		"env.android.type-secret", "env.android.frida.hook",
		"vault.creds.list", "vault.lease", "vault.release",
		"vault.creds.put", "vault.creds.delete", "vault.creds.policy.set",
		"signals.streams.list", "signals.query", "signals.tail", "signals.pin", "signals.unpin", "signals.capture",
		"rules.load", "rules.unload", "rules.list", "rules.test", "rules.triggers.wait",
		"host.status",
	}
	have := map[string]registry.Entry{}
	for _, c := range reg.List() {
		have[c.Name] = c
		if c.Description == "" {
			t.Errorf("%s has no description", c.Name)
		}
		if c.Group == "" {
			t.Errorf("%s is in no envcap group", c.Name)
		}
	}
	for _, name := range want {
		c, ok := have[name]
		if !ok {
			t.Errorf("missing capability %s", name)
			continue
		}
		if strings.HasPrefix(name, "vault.creds.put") || strings.HasPrefix(name, "vault.creds.delete") || strings.HasPrefix(name, "vault.creds.policy") {
			found := false
			for _, tg := range c.Tags {
				if tg == "admin" {
					found = true
				}
				if tg == "agent" {
					t.Errorf("%s must not be agent-tagged", name)
				}
			}
			if !found {
				t.Errorf("%s missing admin tag", name)
			}
		}
	}
	if c := have["env.exec"]; !c.HasTag("exec") {
		t.Errorf("env.exec must carry the exec tag")
	}
}

func TestEveryCapabilityDeclaresItsInput(t *testing.T) {
	byName := map[string]registry.Capability{}
	for _, c := range envcap.Capabilities(envcap.Deps{}) {
		byName[c.Name] = c
		in := c.Input
		if in == nil || in.Type != "object" || in.AdditionalProperties == nil {
			b, _ := json.Marshal(in)
			t.Errorf("%s input schema is not a strict object: %s", c.Name, b)
		}
	}
	if p := byName["env.provision"].Input.Properties["foreground"]; p == nil || p.Type != "boolean" {
		t.Errorf("env.provision foreground must be declared boolean, got %+v", p)
	}
	for _, name := range []string{"env.status", "env.release", "env.get", "env.exec"} {
		if byName[name].Input.Properties["env"] == nil {
			t.Errorf("%s must declare its env argument", name)
		}
	}
	if byName["rules.unload"].Input.Properties["rule_instance"] == nil {
		t.Error("rules.unload must take rule_instance, the name rules.load returns")
	}
}

// S9 gate: lifecycle capabilities always stamp the durable
// operation, distinctly from the registry job, env and lease epoch.
func TestLifecycleOperationIDAlwaysSet(t *testing.T) {
	reg, _, art := setupSet(t)
	prov := call(t, reg, "env.provision", map[string]any{"profile": "linux-tools", "key": "run1"})
	envID, _ := prov["env"].(string)
	if envID == "" {
		t.Fatalf("provision = %v", prov)
	}
	opID, _ := prov["operation_id"].(string)
	if opID == "" || opID == prov["job_id"] || opID == envID {
		t.Fatalf("provision operation_id = %q (job %v, env %v)", opID, prov["job_id"], envID)
	}
	if prov["generation"] == nil || prov["lease_epoch"] == nil {
		t.Fatalf("provision missing generation/lease_epoch: %v", prov)
	}
	if prov["job_id"] == testOwner.Job {
		t.Fatalf("job_id fell back to owner.Job; want the registry job")
	}
	// Artifact runs carry IDs (and registry job ids) too.
	inArt := art.seed(map[string][]byte{"sample.txt": []byte("evidence")})
	call(t, reg, "env.push", map[string]any{"env": envID, "artifact": inArt})
	call(t, reg, "env.pull", map[string]any{"env": envID, "path": "/work", "name": "workspace"})
	sawPull := false
	for _, op := range art.ops {
		if op.ID == "" {
			t.Errorf("artifact op %+v has no ID", op)
		}
		if op.JobID == "" || op.JobID == testOwner.Job {
			t.Errorf("artifact op JobID = %q, want the registry job", op.JobID)
		}
		if op.Capability == "env.pull@1" {
			sawPull = true
			if op.Env != envID {
				t.Errorf("pull op Env = %q, want %q", op.Env, envID)
			}
		}
	}
	if !sawPull {
		t.Fatalf("no env.pull@1 operation recorded: %+v", art.ops)
	}
	rel := call(t, reg, "env.release", map[string]any{"env": envID})
	if rel["operation_id"] == nil || rel["operation_id"] == "" {
		t.Fatalf("release has no operation_id: %v", rel)
	}
	// Parent op id propagates through context.
	ctx := envcap.WithParentOperation(context.Background(), "op_parent_9")
	p2 := mustRaw(t, reg, ctx, "env.provision", map[string]any{"profile": "linux-tools", "key": "run2"})
	e2 := p2["env"].(string)
	mustRaw(t, reg, ctx, "env.pull", map[string]any{"env": e2, "path": "/work", "name": "w2"})
	last := art.ops[len(art.ops)-1]
	if last.Parent != "op_parent_9" {
		t.Fatalf("pull op Parent = %q, want op_parent_9", last.Parent)
	}
}

func mustRaw(t *testing.T, reg *registry.Registry, ctx context.Context, name string, in map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(in)
	out, err := reg.Invoke(ctx, name, b)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAgentFlowProvisionExecPushPullStatus(t *testing.T) {
	reg, _, art := setupSet(t)
	prov := call(t, reg, "env.provision", map[string]any{"profile": "linux-tools", "key": "run1"})
	envID, _ := prov["env"].(string)
	again := call(t, reg, "env.provision", map[string]any{"profile": "linux-tools", "key": "run1"})
	if again["env"] != envID {
		t.Fatalf("idempotent retry made a second env: %v vs %v", again["env"], envID)
	}
	inArt := art.seed(map[string][]byte{"sample.txt": []byte("evidence")})
	push := call(t, reg, "env.push", map[string]any{"env": envID, "artifact": inArt})
	if push["files"].(float64) != 1 {
		t.Fatalf("push = %v", push)
	}
	ex := call(t, reg, "env.exec", map[string]any{"env": envID, "argv": []string{"cat", "/work/in/sample.txt"}})
	if ex["stdout"] != "evidence" || ex["exit"].(float64) != 0 || ex["proc"] == "" {
		t.Fatalf("exec = %v", ex)
	}
	pull := call(t, reg, "env.pull", map[string]any{"env": envID, "path": "/work", "name": "workspace"})
	outArt, _ := pull["artifact"].(string)
	if outArt == "" || string(art.byID[outArt].Files["in/sample.txt"]) != "evidence" {
		t.Fatalf("pull = %v", pull)
	}
	if len(art.evid) == 0 || art.evid[0].Environment != envID {
		t.Fatalf("evidence = %+v", art.evid)
	}
	status := call(t, reg, "env.status", map[string]any{})
	if envs, _ := status["envs"].([]any); len(envs) != 1 {
		t.Fatalf("status envs = %v", status)
	}
	raw, _ := json.Marshal(status)
	for _, banned := range []string{"emulator-", "C:\\", "serial", "container_id"} {
		if strings.Contains(string(raw), banned) {
			t.Fatalf("plumbing %q leaked into env.status: %s", banned, raw)
		}
	}
	call(t, reg, "env.release", map[string]any{"env": envID})
	if envs := call(t, reg, "env.list", map[string]any{})["envs"].([]any); len(envs) != 0 {
		t.Fatalf("env.list after release = %v, want empty", envs)
	}
}

// Stage/Keep at the port level: kept outputs publish, staged-only
// outputs do not, and a failure after Keep returns PartialOutputs.
func TestStageKeepPublishesOnlyKept(t *testing.T) {
	ctx := context.Background()
	art := newFakeArtifacts()
	op := envcap.Operation{ID: "op_x", Capability: "test@1"}
	refs, err := art.Run(ctx, op, func(ctx context.Context, run envcap.ArtifactRun) error {
		if err := run.Stage(ctx, "kept", func(sink envspec.Sink) error {
			return sink.Add("a.txt", 0o100644, strings.NewReader("a"))
		}); err != nil {
			return err
		}
		if err := run.Keep("kept"); err != nil {
			return err
		}
		if err := run.Stage(ctx, "dropped", func(sink envspec.Sink) error {
			return sink.Add("b.txt", 0o100644, strings.NewReader("b"))
		}); err != nil {
			return err
		}
		return errors.New("later step failed")
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	partial, ok := envcap.PartialOutputsOf(err)
	if !ok {
		t.Fatalf("err %v does not wrap PartialOutputs", err)
	}
	if len(refs) != 1 || partial["kept"].ID != refs["kept"].ID || partial["dropped"].ID != "" {
		t.Fatalf("partial = %v refs = %v, want only kept published", partial, refs)
	}
	if got := string(art.byID[refs["kept"].ID].Files["a.txt"]); got != "a" {
		t.Fatalf("kept artifact content = %q", got)
	}
	// Clean run: staged-but-unkept still unpublished, nothing partial.
	refs, err = art.Run(ctx, op, func(ctx context.Context, run envcap.ArtifactRun) error {
		return run.Stage(ctx, "scratch", func(sink envspec.Sink) error {
			return sink.Add("c.txt", 0o100644, strings.NewReader("c"))
		})
	})
	if err != nil || len(refs) != 0 {
		t.Fatalf("unkept stage published something: refs=%v err=%v", refs, err)
	}
}

// env.pull keeps the partial tree when the pull dies midway and
// says so instead of failing the whole call.
func TestPullPartialOutputOnError(t *testing.T) {
	reg, _, art := setupSet(t)
	envID := call(t, reg, "env.provision", map[string]any{"profile": "linux-tools"})["env"].(string)
	inArt := art.seed(map[string][]byte{"a.txt": []byte("a"), "b.txt": []byte("b"), "c.txt": []byte("c")})
	call(t, reg, "env.push", map[string]any{"env": envID, "artifact": inArt})
	pullFailAfter = 2
	defer func() { pullFailAfter = 0 }()
	out := call(t, reg, "env.pull", map[string]any{"env": envID, "path": "/work", "name": "ws"})
	if out["partial"] != true || out["artifact"] == nil || out["artifact"] == "" {
		t.Fatalf("pull = %v, want partial artifact", out)
	}
	sink := art.byID[out["artifact"].(string)]
	if len(sink.Files) != 2 {
		t.Fatalf("partial artifact files = %v, want the 2 pulled before failure", sink.Files)
	}
}

func TestVaultCapsNoSecretLeak(t *testing.T) {
	reg, mgr, _ := setupSet(t)
	put := call(t, reg, "vault.creds.put", map[string]any{
		"kind": "google.account", "name": "g",
		"public": map[string]string{"email": "g@example.test"},
		"secret": map[string]string{"password": "NEVER-LEAK-ME"},
	})
	credID := put["cred"].(string)
	for name, in := range map[string]map[string]any{
		"vault.creds.list": {},
		"vault.lease":      {"cred": credID},
	} {
		out := call(t, reg, name, in)
		raw, _ := json.Marshal(out)
		if strings.Contains(string(raw), "NEVER-LEAK-ME") {
			t.Fatalf("secret leaked in %s: %s", name, raw)
		}
	}
	// type-secret carries only ids, never the value.
	envID := call(t, reg, "env.provision", map[string]any{"profile": "android-play"})["env"].(string)
	call(t, reg, "env.android.type-secret", map[string]any{"env": envID, "lease": "lease_1", "field": "password"})
	if len(mgr.typed) != 1 || strings.Contains(mgr.typed[0], "NEVER") {
		t.Fatalf("typed = %v", mgr.typed)
	}
}

func TestUnavailableCarriesReason(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		err    error
		code   registry.Code
		reason string
	}{
		"no capacity":               {err: fmt.Errorf("pool: %w", envcap.ErrNoCapacity), code: registry.Unavailable, reason: registry.ReasonNoCapacity},
		"backend down":              {err: fmt.Errorf("adb: %w", envcap.ErrBackendDown), code: registry.Unavailable, reason: registry.ReasonBackendDown},
		"not ready keeps no reason": {err: fmt.Errorf("%w: base preparing", envspec.ErrNotReady), code: registry.Unavailable},
		"plain failure is internal": {err: errors.New("boot failed"), code: registry.Internal},
		"hint error keeps its hint": {err: &envcap.HintError{Code: "invalid", Msg: "no such profile", Hint: "env.profiles.list lists them", Err: envspec.ErrInvalid}, code: registry.Invalid},
	} {
		t.Run(name, func(t *testing.T) {
			reg, mgr, _ := setupSet(t)
			mgr.provErr = tc.err
			_, err := reg.Invoke(ctx, "env.provision", []byte(`{"profile":"linux-tools"}`))
			if err == nil {
				t.Fatal("provision succeeded")
			}
			if got := registry.CodeOf(err); got != tc.code {
				t.Fatalf("code = %s, want %s (%v)", got, tc.code, err)
			}
			if got := registry.ReasonOf(err); got != tc.reason {
				t.Fatalf("reason = %q, want %q (%v)", got, tc.reason, err)
			}
			if name == "hint error keeps its hint" && !strings.Contains(err.Error(), "Hint: env.profiles.list") {
				t.Fatalf("hint lost: %v", err)
			}
		})
	}
}

func TestErrorsCarryHintsAndStrictInputs(t *testing.T) {
	reg, _, _ := setupSet(t)
	ctx := context.Background()
	_, err := reg.Invoke(ctx, "env.get", []byte(`{"env":"lease_123"}`))
	if err == nil {
		t.Fatal("expected error for wrong ID kind")
	}
	if info := registry.Info(err); info.Code == "" || info.Message == "" {
		t.Fatalf("error shape = %+v (%v)", info, err)
	}
	_, err = reg.Invoke(ctx, "env.provision", []byte(`{"profile":"nope"}`))
	if err == nil || !strings.Contains(err.Error(), "Hint:") {
		t.Fatalf("unknown profile must carry a hint: %v", err)
	}
	_, err = reg.Invoke(ctx, "env.get", []byte(`{"env":123}`))
	if err == nil || registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("bad input type must be invalid, got %v", err)
	}
	_, err = reg.Invoke(ctx, "env.provision", []byte(`{"profile":"linux-tools","foreground":"true"}`))
	if registry.CodeOf(err) != registry.Invalid || !strings.Contains(err.Error(), "foreground") {
		t.Fatalf("string foreground = %v, want invalid naming foreground", err)
	}
}

func TestSignalsPinUnpinAndCoverageNotes(t *testing.T) {
	reg, mgr, _ := setupSet(t)
	ctx := context.Background()
	envID := call(t, reg, "env.provision", map[string]any{"profile": "linux-tools"})["env"].(string)

	q := call(t, reg, "signals.query", map[string]any{"env": envID, "streams": []string{"fs.changes@1"}, "within": "1m"})
	notes, _ := q["notes"].([]any)
	if len(notes) != 1 || !strings.Contains(notes[0].(string), "no collector was running") ||
		!strings.Contains(notes[0].(string), "signals.pin") {
		t.Fatalf("query notes = %v", q["notes"])
	}

	pin := call(t, reg, "signals.pin", map[string]any{"env": envID, "stream": "env.process@1"})
	pinID, _ := pin["pin"].(string)
	if pinID == "" || pin["running"] != true {
		t.Fatalf("pin = %v", pin)
	}
	again := call(t, reg, "signals.pin", map[string]any{"env": envID, "stream": "env.process@1"})
	if again["pin"] != pinID {
		t.Fatalf("repeat pin = %v, want %s", again, pinID)
	}
	status := call(t, reg, "env.status", map[string]any{})
	if raw, _ := json.Marshal(status["pins"]); !strings.Contains(string(raw), pinID) {
		t.Fatalf("env.status does not show the pin: %s", raw)
	}
	un := call(t, reg, "signals.unpin", map[string]any{"pin": pinID})
	if un["stream"] != "env.process@1" || un["until"] == nil {
		t.Fatalf("unpin = %v", un)
	}
	if _, err := reg.Invoke(ctx, "signals.unpin", []byte(`{"pin":"`+pinID+`"}`)); registry.CodeOf(err) != registry.NotFound {
		t.Fatalf("second unpin = %v, want not_found", err)
	}
	mgr.pinErr["android.binder@1"] = fmt.Errorf("%w: binder needs root", envcap.ErrNotApplicable)
	_, err := reg.Invoke(ctx, "signals.pin", []byte(`{"env":"`+envID+`","stream":"android.binder@1"}`))
	if registry.CodeOf(err) != registry.Unavailable || !strings.Contains(err.Error(), "needs root") || !strings.Contains(err.Error(), "Hint:") {
		t.Fatalf("not-applicable pin = %v", err)
	}
	if _, err := reg.Invoke(ctx, "signals.pin", []byte(`{"env":"`+envID+`","stream":"net.*"}`)); registry.CodeOf(err) != registry.Invalid {
		t.Fatalf("glob pin = %v, want invalid", err)
	}
}

// Progress reaches the registry job as envspec.Progress, stage
// counters and structured fields intact (the old ProgressReport
// translation test, one layer up).
func TestProgressCarriesStructuredFields(t *testing.T) {
	reg, mgr, _ := setupSet(t)
	mgr.execFn = func(envID string, argv []string) envcap.ExecResult {
		return envcap.ExecResult{ProcID: "proc_9", Stdout: []byte("12345"), Stderr: []byte("ab"), Exit: 0}
	}
	envID := call(t, reg, "env.provision", map[string]any{"profile": "linux-tools"})["env"].(string)
	ctx := context.Background()
	job, err := reg.Start(ctx, "env.exec", []byte(`{"env":"`+envID+`","argv":["true"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	sawComplete := false
	for ev := range job.Events(ctx) {
		if ev.Update == nil {
			continue
		}
		raw, _ := json.Marshal(ev.Update.Data)
		var p envspec.Progress
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("progress data is not envspec.Progress: %s", raw)
		}
		if p.Stage == "complete" {
			sawComplete = true
			if p.ProcID != "proc_9" || p.StdoutBytes != 5 || p.StderrBytes != 2 || p.EnvID != envID {
				t.Fatalf("complete progress lost fields: %+v", p)
			}
		}
	}
	if !sawComplete {
		t.Fatal("no complete progress event")
	}
}
