// Package hostenv is the host adapter: processes run directly on the
// machine in a private workspace and a Windows Job Object (process
// group on Unix), behind the same adapter contract as every other
// environment. It is for trusted tooling, not samples. Host paths
// never leave the library: callers name /work-rooted or relative
// paths.
//
// Identity is the deterministic job name craggy-<envID> plus the
// workspace under HostWork/<envID>. The per-instance JSON record in
// InstancesDir is adapter-local recovery data (spec + recorded
// processes); the core's store remains the lifecycle authority.
// A recorded process counts only when both PID and creation time
// match, because operating systems reuse PIDs.
//
// Observe maps facts to presence explicitly: the workspace gone is
// Absent; the workspace present but the named job not reopenable is
// Unknown (the processes may have outlived the job's last handle —
// that is never read as absence, and no empty job is recreated to
// pretend otherwise).
package hostenv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

// Adapter runs host environments.
type Adapter struct {
	HostWork     string // workspace root
	InstancesDir string // <manager Dir>/host/instances for recovery records
}

// New builds the host adapter.
func New(hostWork, instancesDir string) *Adapter {
	return &Adapter{HostWork: hostWork, InstancesDir: instancesDir}
}

// Default builds a host adapter from a manager Dir + HostWork.
func Default(dir, hostWork string) *Adapter {
	if hostWork == "" {
		hostWork = filepath.Join(dir, "hostwork")
	}
	return New(hostWork, filepath.Join(dir, "host", "instances"))
}

func (a *Adapter) Describe() adapter.Description {
	return adapter.Description{
		Name:         "hostenv",
		Kinds:        []envspec.Kind{envspec.Host},
		Relationship: adapter.Created,
		Version:      "1",
	}
}

// Check verifies the workspace root is usable (adapter.Checker).
func (a *Adapter) Check(ctx context.Context) error {
	if a.HostWork == "" {
		return fmt.Errorf("hostenv: HostWork not configured")
	}
	return os.MkdirAll(a.HostWork, 0o755)
}

func (a *Adapter) Validate(ctx context.Context, op adapter.OpContext, s envspec.Spec) error {
	if s.Network != "" && s.Network != envspec.NetworkOpen {
		return fmt.Errorf("%w: host environments have the host's network and accept only network \"open\", not %q; use a container profile for network isolation",
			envspec.ErrInvalid, s.Network)
	}
	return nil
}

func (a *Adapter) Cost(s envspec.Spec) adapter.Cost {
	return adapter.Cost{Pool: envspec.PoolHost, CPUs: s.Resources.CPUs, MemoryMB: s.Resources.MemoryMB, DiskMB: s.Resources.DiskMB}
}

// --- instance record + paths ---

type instanceState struct {
	Env       string       `json:"env"`
	JobName   string       `json:"job_name"`
	Workspace string       `json:"workspace"` // host path; stays inside the library
	Spec      envspec.Spec `json:"spec"`
	Procs     []procRec    `json:"procs,omitempty"`
}

type procRec struct {
	PID     int   `json:"pid"`
	Created int64 `json:"created"` // UnixNano
}

func (a *Adapter) rootFor(envID string) string { return filepath.Join(a.HostWork, envID) }

// workspaceFor is the deterministic workspace path, derivable from
// the env ID alone so Observe works without a live instance.
func (a *Adapter) workspaceFor(envID string) string {
	return filepath.Join(a.rootFor(envID), "work")
}

func (a *Adapter) statePath(envID string) string {
	return filepath.Join(a.InstancesDir, envID+".json")
}

func (a *Adapter) loadState(envID string) (instanceState, error) {
	var st instanceState
	b, err := os.ReadFile(a.statePath(envID))
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Env == "" {
		st.Env = envID
	}
	if st.JobName == "" {
		st.JobName = adapter.NameFor(envID)
	}
	if st.Workspace == "" {
		st.Workspace = a.workspaceFor(envID)
	}
	return st, nil
}

// verifiedProcs filters recorded processes to those verifiably the
// same processes (PID + creation time both match).
func verifiedProcs(recs []procRec) []procRec {
	var out []procRec
	for _, p := range recs {
		if p.Created == 0 {
			continue
		}
		if t, ok := processCreationTime(p.PID); ok && t.UnixNano() == p.Created {
			out = append(out, p)
		}
	}
	return out
}

// --- Observe / Discover ---

// Observe reports confirmed facts about ref. Workspace gone is
// Absent; workspace present but the named job not reopenable (with
// any recorded processes verified by PID+creation) is Unknown.
func (a *Adapter) Observe(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Observation, error) {
	envID := ref.EnvID
	if envID == "" {
		envID = op.EnvID
	}
	obs := adapter.Observation{Ref: ref, Workspace: "/work",
		Endpoints: map[string]string{"os": runtime.GOOS, "arch": runtime.GOARCH}}
	if obs.Ref.EnvID == "" {
		obs.Ref.EnvID = envID
	}
	if obs.Ref.Name == "" {
		obs.Ref.Name = adapter.NameFor(envID)
	}
	if envID == "" {
		obs.Presence, obs.Detail = adapter.Unknown, "no env id in ref or op context"
		return obs, nil
	}
	ws := a.workspaceFor(envID)
	st, stErr := a.loadState(envID)
	if stErr == nil && st.Workspace != "" {
		ws = st.Workspace
		if st.JobName != "" {
			obs.Ref.Name = st.JobName
			obs.Ref.External = st.JobName
		}
	}
	if _, err := os.Stat(ws); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			obs.Presence = adapter.Absent
			return obs, nil
		}
		obs.Presence, obs.Detail = adapter.Unknown, fmt.Sprintf("stat workspace: %v", err)
		return obs, nil
	}
	// Workspace present. The named job is the second identity leg.
	j, err := openJob(obs.Ref.Name)
	if err != nil {
		live := 0
		if stErr == nil {
			live = len(verifiedProcs(st.Procs))
		}
		obs.Presence = adapter.Unknown
		obs.Detail = fmt.Sprintf("workspace present (%d recorded processes verified) but job %q cannot be reopened: %v",
			live, obs.Ref.Name, err)
		return obs, nil
	}
	j.close()
	obs.Presence = adapter.Present
	return obs, nil
}

// Discover lists recorded instances in this manager's InstancesDir.
func (a *Adapter) Discover(ctx context.Context, scope adapter.Scope) ([]adapter.Candidate, error) {
	matches, err := filepath.Glob(filepath.Join(a.InstancesDir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []adapter.Candidate
	for _, m := range matches {
		envID := strings.TrimSuffix(filepath.Base(m), ".json")
		if scope.EnvID != "" && envID != scope.EnvID {
			continue
		}
		st, err := a.loadState(envID)
		if err != nil {
			continue
		}
		if _, err := os.Stat(st.Workspace); err != nil {
			continue // workspace gone: nothing to rediscover
		}
		out = append(out, adapter.Candidate{Ref: adapter.Ref{
			EnvID: st.Env, External: st.JobName, Name: st.JobName,
		}})
	}
	return out, nil
}

// --- Create / Open ---

// Create makes the workspace and the named job craggy-<envID>. A
// retry for the same env rediscovers the earlier result via Open
// instead of creating a second resource.
func (a *Adapter) Create(ctx context.Context, op adapter.OpContext, s envspec.Spec, progress func(envspec.Progress)) (adapter.Instance, error) {
	id := op.EnvID
	if id == "" {
		return nil, fmt.Errorf("%w: hostenv create needs an env id", envspec.ErrInvalid)
	}
	if progress != nil {
		progress(envspec.Progress{Op: "provision", Stage: "create", EnvID: id})
	}
	if _, err := os.Stat(a.workspaceFor(id)); err == nil {
		if _, err := a.loadState(id); err == nil {
			return a.Open(ctx, op, adapter.Ref{EnvID: id, External: adapter.NameFor(id), Name: adapter.NameFor(id)})
		}
	}
	root := a.rootFor(id)
	ws := a.workspaceFor(id)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, ".secrets"), 0o755); err != nil {
		return nil, err
	}
	j, err := createJob(adapter.NameFor(id), s.Resources.MemoryMB, 256)
	if err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	inst := &Instance{ad: a, envID: id, spec: s, root: root, workspace: ws, job: j,
		credEnv: map[string]string{}, running: map[int]*osexec.Cmd{}}
	if err := inst.saveState(); err != nil {
		j.close()
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("hostenv: record instance: %w", err)
	}
	return inst, nil
}

// Open re-materialises an instance from its record: workspace must
// exist and the named job must reopen. No empty job is recreated —
// a job that cannot be reopened is an error here and Unknown in
// Observe.
func (a *Adapter) Open(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Instance, error) {
	envID := ref.EnvID
	if envID == "" {
		envID = op.EnvID
	}
	if envID == "" {
		return nil, fmt.Errorf("%w: hostenv open needs an env id", envspec.ErrInvalid)
	}
	st, err := a.loadState(envID)
	if err != nil {
		return nil, fmt.Errorf("hostenv: load instance record for %s: %w", envID, err)
	}
	if _, err := os.Stat(st.Workspace); err != nil {
		return nil, fmt.Errorf("hostenv: workspace for %s gone: %w", envID, err)
	}
	j, err := openJob(st.JobName)
	if err != nil {
		return nil, fmt.Errorf("hostenv: reopen job for %s: %w", envID, err)
	}
	inst := &Instance{ad: a, envID: st.Env, spec: st.Spec, root: filepath.Dir(st.Workspace),
		workspace: st.Workspace, job: j, credEnv: map[string]string{}, running: map[int]*osexec.Cmd{}}
	for _, p := range verifiedProcs(st.Procs) {
		inst.procs = append(inst.procs, p)
		j.adopt(p.PID)
	}
	return inst, nil
}

// --- Instance ---

// Instance is one host environment: a workspace and a job.
type Instance struct {
	ad        *Adapter
	envID     string
	spec      envspec.Spec
	root      string // <HostWork>/<env>
	workspace string // <root>/work
	job       *job

	mu      sync.Mutex
	procs   []procRec
	credEnv map[string]string
	running map[int]*osexec.Cmd
}

func (i *Instance) Ref() adapter.Ref {
	name := adapter.NameFor(i.envID)
	if i.job != nil && i.job.name != "" {
		name = i.job.name
	}
	return adapter.Ref{EnvID: i.envID, External: name, Name: name}
}

// LocalWorkspace is the real directory. Adapter-internal (collectors
// and tests); never surfaced to callers.
func (i *Instance) LocalWorkspace() string { return i.workspace }

func (i *Instance) saveState() error {
	i.mu.Lock()
	st := instanceState{Env: i.envID, JobName: i.job.name, Workspace: i.workspace, Spec: i.spec, Procs: i.procs}
	i.mu.Unlock()
	if err := os.MkdirAll(i.ad.InstancesDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(i.ad.statePath(i.envID), b, 0o644)
}

// LiveProcs returns recorded processes verified alive (PID+creation).
func (i *Instance) LiveProcs() []int {
	i.mu.Lock()
	recs := append([]procRec(nil), i.procs...)
	i.mu.Unlock()
	var out []int
	for _, p := range verifiedProcs(recs) {
		out = append(out, p.PID)
	}
	return out
}

// Accounting returns job CPU time and process count (env.resources).
func (i *Instance) Accounting() (time.Duration, uint32, error) {
	return i.job.accounting()
}

// Dispose ends the environment per disp: Destroy terminates the job
// (the whole process tree), deletes the workspace and the record;
// RetainWorkspace terminates the job and deletes the record but
// keeps the workspace on disk for inspection.
func (i *Instance) Dispose(ctx context.Context, op adapter.OpContext, disp adapter.Disposition) error {
	switch disp {
	case adapter.Destroy, adapter.RetainWorkspace, adapter.Disconnect, adapter.ResetToPool:
	default:
		return fmt.Errorf("%w: hostenv disposition %q", adapter.ErrUnsupported, disp)
	}
	_ = i.job.terminate()
	i.job.close()
	if disp == adapter.RetainWorkspace || disp == adapter.Disconnect {
		_ = os.Remove(i.ad.statePath(i.envID))
		return nil
	}
	// Destroy (host instances are created resources; ResetToPool is
	// not offered — no Resetter — so it cannot arrive from the core).
	var err error
	for try := 0; try < 20; try++ {
		if err = os.RemoveAll(i.root); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond) // let processes exit so files unlock on Windows
	}
	_ = os.Remove(i.ad.statePath(i.envID))
	if err != nil {
		return err
	}
	return os.RemoveAll(i.root)
}
