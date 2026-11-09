package envcap

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

type profileOut struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	Description string   `json:"description,omitempty"`
	Networks    []string `json:"networks,omitempty"`
	MaxCPUs     float64  `json:"max_cpus,omitempty"`
	MaxMemoryMB int64    `json:"max_memory_mb,omitempty"`
	Ready       *bool    `json:"ready,omitempty"`
	NotReady    string   `json:"not_ready,omitempty"`
}

func (s *set) build() []capability {
	caps := []capability{}
	add := func(c capability) { caps = append(caps, c) }

	add(capability{Name: "env.status", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "Start here. Everything you hold in one compact answer: environments, collectors, pins, rules, credential leases. Pass env to narrow it to one environment.",
		Input:       inputSchema[statusIn](),
		Handler:     s.handleStatus})
	add(capability{Name: "env.profiles.list", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "List environment profiles with when to choose each one. ready:false gives the reason in not_ready. Android-physical is for env.attach; other profiles are for env.provision.",
		Handler:     s.handleProfilesList})
	add(capability{Name: "env.provision", Version: 1, Effects: "long", Tags: []string{"agent"},
		Description: "Create a ready-to-use environment from a profile. One call: booted, set up, nothing else to do. Optional key makes retries safe. Android emulators boot headless (background); pass foreground:true only when a person needs to watch the emulator window. A provision that fails leaves nothing to release.",
		Input:       inputSchema[provisionIn](),
		Check:       s.checkManager,
		Handler:     s.handleProvision})
	add(capability{Name: "env.attach", Version: 1, Effects: "long", Tags: []string{"agent"},
		Description: "Lease an existing USB Android device by ADB serial. By default no root or Frida setup is requested. To instrument a rooted device, pass root:magisk (or adb) and frida:true; this may push and start frida-server. Releasing disconnects without wiping apps or data. Optional key makes retries safe.",
		Input:       inputSchema[attachIn](),
		Check:       s.checkManager,
		Handler:     s.handleAttach})
	add(capability{Name: "env.list", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "List the environments you hold (the same set env.status reports).", Handler: s.handleList})
	add(capability{Name: "env.get", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "One environment's state, workspace and component health.",
		Input:       inputSchema[envIDIn](), Handler: s.handleGet})
	add(capability{Name: "env.release", Version: 1, Effects: "mutating", Tags: []string{"agent"},
		Description: "Release an environment. Created environments are destroyed; attached physical devices are disconnected without wiping their apps or data. Idempotent.",
		Input:       inputSchema[envIDIn](), Handler: s.handleRelease})
	add(capability{Name: "env.keepalive", Version: 1, Effects: "mutating", Tags: []string{"agent"},
		Description: "Extend an environment's holder lease. This is the only renewal: using an environment does not renew it.",
		Input:       inputSchema[envIDIn](), Handler: s.handleKeepalive})
	add(capability{Name: "env.exec", Version: 1, Effects: "long", Tags: []string{"agent", "exec"},
		Description: "Run a command in an environment to completion. Output also streams to env.process@1 under the returned proc id; the inline tail caps at 64KiB and full output is stored as artifacts when larger.",
		Input:       inputSchema[execIn](),
		Handler:     s.handleExec})
	add(capability{Name: "env.fs.list", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "List files in an environment path (bounded, with totals).",
		Input:       inputSchema[fsListIn](), Handler: s.handleFSList})
	add(capability{Name: "env.fs.stat", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "Stat one environment path.",
		Input:       inputSchema[fsStatIn](), Handler: s.handleFSStat})
	add(capability{Name: "env.push", Version: 1, Effects: "long", Tags: []string{"agent"},
		Description: "Copy an artifact tree into an environment path. Defaults to in/ in the workspace.",
		Input:       inputSchema[pushIn](), Handler: s.handlePush})
	add(capability{Name: "env.pull", Version: 1, Effects: "long", Tags: []string{"agent"},
		Description: "Copy an environment path out into a new artifact.",
		Input:       inputSchema[pullIn](), Handler: s.handlePull})
	add(capability{Name: "env.snapshot.save", Version: 1, Effects: "long", Tags: []string{"agent"},
		Description: "Save an environment snapshot (emulator snapshot / container commit).",
		Input:       inputSchema[snapIn](), Handler: s.handleSnapshotSave})
	add(capability{Name: "env.snapshot.load", Version: 1, Effects: "long", Tags: []string{"agent"},
		Description: "Restore an environment snapshot.",
		Input:       inputSchema[snapIn](), Handler: s.handleSnapshotLoad})
	add(capability{Name: "env.snapshot.list", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "List an environment's snapshots.",
		Input:       inputSchema[envIDIn](), Handler: s.handleSnapshotList})

	caps = append(caps, s.buildAndroid()...)
	caps = append(caps, s.buildVault()...)
	caps = append(caps, s.buildSignals()...)
	caps = append(caps, s.buildRules()...)
	add(capability{Name: "host.status", Version: 1, Effects: "readonly", Tags: []string{"agent"},
		Description: "Host load and free capacity per pool.", Handler: s.handleHostStatus})
	return caps
}

func (s *set) checkManager(ctx context.Context) error { return requireManager(s) }

type statusIn struct {
	Env string `json:"env,omitempty" jsonschema:"only this environment (env_…); omit for everything you hold"`
}

// heldEnvs lists the live environments the caller holds: owned by it
// or held by its session. env.status and env.list answer from this
// one set, so they never disagree.
func (s *set) heldEnvs(owner envspec.Owner) []agentEnvInfo {
	envs := []agentEnvInfo{}
	seen := map[string]bool{}
	add := func(f ListFilter) {
		f.Live = true
		for _, info := range s.deps.Manager.ListEnvs(f) {
			if !seen[info.ID] {
				seen[info.ID] = true
				envs = append(envs, viewInfo(info))
			}
		}
	}
	if owner.ID != "" {
		add(ListFilter{Owner: owner.ID})
	}
	if owner.Session != "" {
		add(ListFilter{Session: owner.Session})
	}
	return envs
}

func (s *set) handleStatus(ctx context.Context, call *call) (any, error) {
	if err := requireManager(s); err != nil {
		return nil, err
	}
	var in statusIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	owner, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	envs := s.heldEnvs(owner)
	if in.Env != "" {
		envs = slices.DeleteFunc(envs, func(v agentEnvInfo) bool { return v.Env != in.Env })
		if len(envs) == 0 {
			return nil, &agentError{Code: "not_found",
				Message: fmt.Sprintf("environment %s is not among yours (released, failed or never yours)", in.Env),
				Hint:    "env.status with no arguments lists what you hold"}
		}
	}
	seen := map[string]bool{}
	for _, v := range envs {
		seen[v.Env] = true
	}
	out := map[string]any{"envs": envs}

	var cols []map[string]any
	for _, rc := range s.deps.Manager.RunningCollectors() {
		if in.Env != "" && rc.Source != in.Env {
			continue
		}
		cols = append(cols, map[string]any{"collector": rc.ID, "name": rc.Name, "env": rc.Source, "streams": rc.Streams})
	}
	out["collectors"] = cols
	var pins []map[string]any
	for id := range seen {
		for _, p := range s.deps.Manager.Pins(id) {
			pins = append(pins, pinView(p, s.deps.Manager.StreamState(p.Stream, p.Env, p.Params)))
		}
	}
	out["pins"] = pins

	var rs []map[string]any
	for _, ri := range s.deps.Manager.ListRules() {
		m := map[string]any{"rule": ri.Rule, "instance": ri.Instance, "version": ri.Version, "state": ri.State, "triggers": ri.Triggers}
		if !ri.LastTrigger.IsZero() {
			m["last_trigger"] = ri.LastTrigger.Format(time.RFC3339)
		}
		rs = append(rs, m)
	}
	out["rules"] = rs
	var trigs []map[string]any
	for _, t := range s.deps.Manager.RecentRuleTriggers(time.Now().Add(-time.Hour), "") {
		trigs = append(trigs, map[string]any{"rule": t.Rule, "kind": t.Kind, "time": t.Time.Format(time.RFC3339), "capture": t.CaptureID})
	}
	if len(trigs) > 0 {
		out["recent_triggers"] = trigs
	}
	if leases, err := s.deps.Manager.ListCredLeases(ctx); err == nil {
		var ls []map[string]any
		for _, l := range leases {
			heldByEnv := strings.HasPrefix(l.Holder, "env:") && seen[strings.TrimPrefix(l.Holder, "env:")]
			if heldByEnv || in.Env == "" && l.Owner == owner.ID {
				ls = append(ls, map[string]any{"lease": l.Lease, "kind": l.Kind, "holder": l.Holder})
			}
		}
		out["cred_leases"] = ls
	}
	return out, nil
}

func (s *set) handleProfilesList(ctx context.Context, call *call) (any, error) {
	if err := requireManager(s); err != nil {
		return nil, err
	}
	out := []profileOut{}
	for _, p := range s.deps.Manager.Profiles() {
		po := profileOut{Name: p.Name, Kind: string(p.Kind), Description: p.Description,
			MaxCPUs: p.MaxResources.CPUs, MaxMemoryMB: p.MaxResources.MemoryMB}
		for _, n := range p.AllowNetworks {
			po.Networks = append(po.Networks, string(n))
		}
		if po.Networks == nil && p.Network != "" {
			po.Networks = []string{string(p.Network)}
		}
		st := s.deps.Manager.ProfileStatus(p.Name)
		ready := st.Ready
		po.Ready = &ready
		if !ready {
			if p.Name == "android-physical" {
				po.NotReady = st.NotReady
			} else {
				po.NotReady = notReadyText(st.NotReady)
			}
		}
		out = append(out, po)
	}
	return map[string]any{"profiles": out}, nil
}

// notReadyText says what a not-ready profile lacks and that nothing
// but a provision is needed: the first env.provision builds its base.
func notReadyText(why string) string {
	if strings.Contains(why, "still preparing") || strings.Contains(why, "preparing") {
		return why + "; env.provision waits for it"
	}
	return why + "; the next env.provision of this profile builds it first, which takes minutes"
}

type provisionIn struct {
	Profile    string       `json:"profile,omitempty" jsonschema:"profile name from env.profiles.list, e.g. android or linux-tools"`
	Key        string       `json:"key,omitempty" jsonschema:"idempotency key: repeating a provision with the same key returns the same environment"`
	NoWait     bool         `json:"no_wait,omitempty" jsonschema:"fail at once instead of queueing when the host is at capacity"`
	Foreground bool         `json:"foreground,omitempty" jsonschema:"show the Android emulator window; the default boots headless"`
	Overrides  *overridesIn `json:"overrides,omitempty" jsonschema:"adjustments to the profile's spec"`
	Spec       *specJSON    `json:"spec,omitempty" jsonschema:"full ad-hoc spec (advanced)"`
}

type overridesIn struct {
	Image            string            `json:"image,omitempty" jsonschema:"image to run instead of the profile's"`
	Network          string            `json:"network,omitempty" jsonschema:"network policy; must be one the profile allows"`
	PublishedTCPPort uint16            `json:"published_tcp_port,omitempty" jsonschema:"container TCP port exposed on an ephemeral host loopback port"`
	CPUs             float64           `json:"cpus,omitempty"`
	MemoryMB         int64             `json:"memory_mb,omitempty"`
	DiskMB           int64             `json:"disk_mb,omitempty"`
	TTLSeconds       int64             `json:"ttl_seconds,omitempty" jsonschema:"holder lease TTL in seconds"`
	Labels           map[string]string `json:"labels,omitempty"`
	Signals          []string          `json:"signals,omitempty" jsonschema:"streams to record from boot, e.g. net.flows@1"`
}

type specJSON struct {
	Kind             string                  `json:"kind,omitempty"`
	Image            string                  `json:"image,omitempty"`
	CPUs             float64                 `json:"cpus,omitempty"`
	MemoryMB         int64                   `json:"memory_mb,omitempty"`
	DiskMB           int64                   `json:"disk_mb,omitempty"`
	Network          string                  `json:"network,omitempty"`
	PublishedTCPPort uint16                  `json:"published_tcp_port,omitempty"`
	TTLSeconds       int64                   `json:"ttl_seconds,omitempty"`
	Labels           map[string]string       `json:"labels,omitempty"`
	Creds            []envspec.CredRequest   `json:"creds,omitempty"`
	Signals          []envspec.SignalRequest `json:"signals,omitempty"`
	Android          *envspec.AndroidOptions `json:"android,omitempty"`
	Params           map[string]string       `json:"params,omitempty"`
}

func (ov *overridesIn) apply(spec *envspec.Spec) {
	if ov == nil {
		return
	}
	if ov.Image != "" {
		spec.Image = ov.Image
	}
	if ov.Network != "" {
		spec.Network = envspec.Network(ov.Network)
	}
	if ov.PublishedTCPPort != 0 {
		spec.PublishedTCPPort = ov.PublishedTCPPort
	}
	if ov.CPUs > 0 {
		spec.Resources.CPUs = ov.CPUs
	}
	if ov.MemoryMB > 0 {
		spec.Resources.MemoryMB = ov.MemoryMB
	}
	if ov.DiskMB > 0 {
		spec.Resources.DiskMB = ov.DiskMB
	}
	if ov.TTLSeconds > 0 {
		spec.TTL = time.Duration(ov.TTLSeconds) * time.Second
	}
	if len(ov.Labels) > 0 {
		spec.Labels = ov.Labels
	}
	for _, stream := range ov.Signals {
		spec.Signals = append(spec.Signals, envspec.SignalRequest{Stream: stream})
	}
}

func (s *set) handleProvision(ctx context.Context, call *call) (any, error) {
	if err := requireManager(s); err != nil {
		return nil, err
	}
	var in provisionIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	owner, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	if in.Profile == "android-physical" {
		return nil, &agentError{Code: "invalid", Message: "use env.attach with a physical device serial"}
	}
	if in.Profile != "" && in.Spec == nil {
		if _, ok := s.deps.Manager.Profile(in.Profile); !ok {
			return nil, &agentError{Code: "invalid",
				Message: fmt.Sprintf("unknown profile %q", in.Profile),
				Hint:    "env.profiles.list lists the profiles and when to choose each"}
		}
	}
	spec := envspec.Spec{Profile: in.Profile, Key: in.Key}
	if in.Spec != nil {
		spec.Kind = envspec.Kind(in.Spec.Kind)
		spec.Image = in.Spec.Image
		spec.Resources = envspec.Resources{CPUs: in.Spec.CPUs, MemoryMB: in.Spec.MemoryMB, DiskMB: in.Spec.DiskMB}
		spec.Network = envspec.Network(in.Spec.Network)
		spec.PublishedTCPPort = in.Spec.PublishedTCPPort
		if in.Spec.TTLSeconds > 0 {
			spec.TTL = time.Duration(in.Spec.TTLSeconds) * time.Second
		}
		spec.Labels = in.Spec.Labels
		spec.Creds = in.Spec.Creds
		spec.Signals = in.Spec.Signals
		spec.Android = in.Spec.Android
		spec.Params = in.Spec.Params
	}
	in.Overrides.apply(&spec)
	if in.Foreground {
		if spec.Params == nil {
			spec.Params = map[string]string{}
		}
		spec.Params["avd.foreground"] = "true"
	}
	// The durable operation is started inside ProvisionEnv before
	// anything is dispatched; its id is stamped distinctly below.
	res, err := s.deps.Manager.ProvisionEnv(ctx, ProvisionRequest{
		Owner: owner, Spec: spec, Progress: call.Progress, NoWait: in.NoWait,
	})
	if err != nil {
		return nil, err
	}
	call.opid = res.OperationID
	out := map[string]any{"env": res.Env, "info": viewInfo(res.Info)}
	return s.stampLifecycle(ctx, out, res), nil
}

type attachIn struct {
	Serial string `json:"serial" jsonschema:"ADB serial of an authorised physical Android device"`
	Key    string `json:"key,omitempty" jsonschema:"idempotency key: repeating attach with the same key returns the same lease"`
	NoWait bool   `json:"no_wait,omitempty" jsonschema:"fail instead of queueing when the host is at capacity"`
	Root   string `json:"root,omitempty" jsonschema:"explicit root mode: none (default), magisk, or adb"`
	Frida  bool   `json:"frida,omitempty" jsonschema:"opt in to pushing and starting managed frida-server on this device"`
}

func (s *set) handleAttach(ctx context.Context, call *call) (any, error) {
	if err := requireManager(s); err != nil {
		return nil, err
	}
	var in attachIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	serial := strings.TrimSpace(in.Serial)
	if serial == "" || strings.HasPrefix(serial, "emulator-") {
		return nil, &agentError{Code: "invalid", Message: "serial must name an authorised physical Android device"}
	}
	root := envspec.RootMode(in.Root)
	if root == "" {
		root = envspec.RootNone
	}
	if root != envspec.RootNone && root != envspec.RootMagisk && root != envspec.RootADB {
		return nil, &agentError{Code: "invalid", Message: "root must be none, magisk or adb"}
	}
	if in.Frida && root == envspec.RootNone {
		return nil, &agentError{Code: "invalid", Message: "frida requires an explicit magisk or adb root mode"}
	}
	android := &envspec.AndroidOptions{Root: root}
	if in.Frida {
		android.Frida = &envspec.FridaOptions{}
	}
	owner, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	res, err := s.deps.Manager.AttachEnv(ctx, ProvisionRequest{Owner: owner,
		Spec: envspec.Spec{Profile: "android-physical", Key: in.Key,
			Params: map[string]string{"adb.serial": serial}, Android: android},
		Progress: call.Progress, NoWait: in.NoWait})
	if err != nil {
		return nil, err
	}
	call.opid = res.OperationID
	out := map[string]any{"env": res.Env, "info": viewInfo(res.Info)}
	return s.stampLifecycle(ctx, out, res), nil
}

func (s *set) handleList(ctx context.Context, call *call) (any, error) {
	if err := requireManager(s); err != nil {
		return nil, err
	}
	owner, err := s.owner(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"envs": s.heldEnvs(owner)}, nil
}

type envIDIn struct {
	Env string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.attach, env.list or env.status"`
}

func (s *set) handleGet(ctx context.Context, call *call) (any, error) {
	var in envIDIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	info, _, err := s.envOwned(ctx, in.Env)
	if err != nil {
		return nil, err
	}
	return map[string]any{"info": viewInfo(info)}, nil
}

func (s *set) handleRelease(ctx context.Context, call *call) (any, error) {
	var in envIDIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	if !envspec.HasPrefix(in.Env, "env_") {
		return nil, &agentError{Code: "invalid", Message: fmt.Sprintf("%q is not an environment ID (want env_…)", in.Env)}
	}
	if _, _, err := s.envOwned(ctx, in.Env); err != nil {
		// Not held: release is still a no-op success when the env is
		// gone entirely (idempotent), an error when it is someone's.
		if _, ierr := s.deps.Manager.EnvInfo(in.Env); ierr != nil {
			return map[string]any{"released": in.Env}, nil
		}
		return nil, err
	}
	res, err := s.deps.Manager.ReleaseEnv(ctx, in.Env)
	if err != nil {
		return nil, err
	}
	call.opid = res.OperationID
	out := map[string]any{"released": in.Env}
	return s.stampLifecycle(ctx, out, res), nil
}

func (s *set) handleKeepalive(ctx context.Context, call *call) (any, error) {
	var in envIDIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	if _, _, err := s.envOwned(ctx, in.Env); err != nil {
		return nil, err
	}
	if err := s.deps.Manager.RenewLease(in.Env); err != nil {
		return nil, err
	}
	return map[string]any{"kept_alive": in.Env}, nil
}

type execIn struct {
	Env            string            `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Argv           []string          `json:"argv,omitempty" jsonschema:"command and arguments, run without a shell"`
	Cmd            string            `json:"cmd,omitempty" jsonschema:"shell command line, run as sh -c; give argv or cmd"`
	EnvVars        map[string]string `json:"env_vars,omitempty" jsonschema:"extra environment variables"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
}

func (s *set) handleExec(ctx context.Context, call *call) (any, error) {
	var in execIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	info, owner, err := s.envOwned(ctx, in.Env)
	if err != nil {
		return nil, err
	}
	argv := in.Argv
	if len(argv) == 0 && in.Cmd != "" {
		argv = []string{"sh", "-c", in.Cmd}
	}
	if len(argv) == 0 {
		return nil, &agentError{Code: "invalid", Message: "env.exec needs argv or cmd",
			Hint: `env.exec {"env":"` + in.Env + `","argv":["ls","-l"]}`}
	}
	cmd := envspec.Cmd{Argv: argv, Env: in.EnvVars}
	if in.TimeoutSeconds > 0 {
		cmd.Timeout = time.Duration(in.TimeoutSeconds) * time.Second
	}
	// Job-scoped exec borrows the holder lease for its duration.
	if job := s.borrowJob(ctx, owner); job != "" {
		release, berr := s.deps.Manager.Borrow(ctx, in.Env, job)
		if berr != nil {
			return nil, berr
		}
		defer release()
	}
	call.Progress(envspec.Progress{Op: "exec", Stage: "running", EnvID: in.Env, Message: "process started"})
	res, err := s.deps.Manager.ExecEnv(ctx, in.Env, cmd)
	if err != nil {
		return nil, err
	}
	call.Progress(envspec.Progress{Op: "exec", Stage: "complete", Done: 1, Total: 1, EnvID: in.Env,
		ProcID: res.ProcID, StdoutBytes: int64(len(res.Stdout)), StderrBytes: int64(len(res.Stderr)),
		Message: "process completed"})
	out := map[string]any{
		"env": in.Env, "proc": res.ProcID, "exit": res.Exit,
		"stdout": string(tailCapped(res.Stdout)), "stderr": string(tailCapped(res.Stderr)),
		"stdout_bytes": len(res.Stdout), "stderr_bytes": len(res.Stderr),
	}
	// Android's am/pm/cmd report many failures on their output while
	// still exiting 0; surface a structured ok/error there.
	if info.Kind == envspec.Android {
		reason, ok := androidExecOutcome(argv, res.Exit, res.Stdout, res.Stderr)
		out["ok"] = ok
		if !ok && reason != "" {
			out["error"] = reason
		}
	}
	if len(res.Stdout) > 64<<10 || len(res.Stderr) > 64<<10 {
		out["truncated"] = true
		if s.deps.Artifacts != nil {
			if refs, aerr := s.storeExecOutput(ctx, in.Env, info.Image, res); aerr == nil {
				out["stdout_artifact"] = refs["stdout"].ID
				out["stderr_artifact"] = refs["stderr"].ID
			}
		}
	}
	if ctx.Err() != nil && res.Exit == 0 {
		out["cancelled"] = true
	}
	return out, nil
}

func (s *set) storeExecOutput(ctx context.Context, envID, image string, res ExecResult) (map[string]Ref, error) {
	op := s.operation(ctx, "env.exec@1", "", envID, nil)
	return s.deps.Artifacts.Run(ctx, op, func(ctx context.Context, run ArtifactRun) error {
		_ = run.Record(ctx, Evidence{Environment: envID, Image: image, Collector: res.ProcID})
		if err := run.Stage(ctx, "stdout", func(sink envspec.Sink) error {
			return sink.Add("stdout.txt", 0o100644, bytes.NewReader(res.Stdout))
		}); err != nil {
			return err
		}
		if err := run.Keep("stdout"); err != nil {
			return err
		}
		if err := run.Stage(ctx, "stderr", func(sink envspec.Sink) error {
			return sink.Add("stderr.txt", 0o100644, bytes.NewReader(res.Stderr))
		}); err != nil {
			return err
		}
		return run.Keep("stderr")
	})
}

func tailCapped(b []byte) []byte {
	if len(b) <= 64<<10 {
		return b
	}
	return b[len(b)-(64<<10):]
}

type fsListIn struct {
	Env       string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Path      string `json:"path,omitempty" jsonschema:"directory to list; defaults to the workspace"`
	Recursive bool   `json:"recursive,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	Limit     int    `json:"limit,omitempty" jsonschema:"items per page, at most 200"`
}

type fsStatIn struct {
	Env  string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Path string `json:"path"`
}

func (s *set) handleFSList(ctx context.Context, call *call) (any, error) {
	var in fsListIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	info, _, err := s.envOwned(ctx, in.Env)
	if err != nil {
		return nil, err
	}
	path := in.Path
	if path == "" {
		path = info.Workspace
	}
	entries, err := s.deps.Manager.ListFS(ctx, in.Env, path, in.Recursive, 0)
	if err != nil {
		return nil, err
	}
	total := len(entries)
	byExt := map[string]int{}
	var totalBytes int64
	for _, en := range entries {
		totalBytes += en.Size
		ext := "(none)"
		if i := strings.LastIndex(en.Path, "."); i >= 0 && !en.IsDir() {
			ext = en.Path[i:]
		}
		byExt[ext]++
	}
	limit := in.Limit
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	page := entries
	if in.Offset > 0 {
		if in.Offset >= len(page) {
			page = nil
		} else {
			page = page[in.Offset:]
		}
	}
	if len(page) > limit {
		page = page[:limit]
	}
	var items []map[string]any
	for _, en := range page {
		items = append(items, map[string]any{"path": en.Path, "dir": en.IsDir(), "size": en.Size})
	}
	return map[string]any{"env": in.Env, "path": path, "total": total, "total_bytes": totalBytes,
		"by_extension": byExt, "items": items, "offset": in.Offset, "has_more": in.Offset+len(page) < total}, nil
}

func (s *set) handleFSStat(ctx context.Context, call *call) (any, error) {
	var in fsStatIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	if _, _, err := s.envOwned(ctx, in.Env); err != nil {
		return nil, err
	}
	en, err := s.deps.Manager.Stat(ctx, in.Env, in.Path)
	if err != nil {
		return nil, err
	}
	return map[string]any{"path": en.Path, "dir": en.IsDir(),
		"symlink": en.Mode&0o170000 == 0o120000, "size": en.Size, "mode": fmt.Sprintf("%o", en.Mode)}, nil
}

type pushIn struct {
	Env      string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Artifact string `json:"artifact,omitempty" jsonschema:"artifact id to copy in"`
	Path     string `json:"path,omitempty" jsonschema:"destination; defaults to in/ in the workspace"`
	// Content/FileName are a small-inline escape for tests and tiny
	// files when no artifact bridge is wired.
	Content  string `json:"content,omitempty" jsonschema:"small inline file content instead of an artifact"`
	FileName string `json:"file_name,omitempty" jsonschema:"file name for content"`
}

func (s *set) handlePush(ctx context.Context, call *call) (any, error) {
	var in pushIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	info, _, err := s.envOwned(ctx, in.Env)
	if err != nil {
		return nil, err
	}
	dst := in.Path
	if dst == "" {
		dst = strings.TrimSuffix(info.Workspace, "/") + "/in"
	}
	var st envspec.TransferStats
	if in.Content != "" || in.FileName != "" {
		name := in.FileName
		if name == "" {
			name = "content.txt"
		}
		st, err = s.deps.Manager.Push(ctx, in.Env, envspec.MemSource(map[string][]byte{name: []byte(in.Content)}), dst)
		if err != nil {
			return nil, err
		}
	} else {
		if s.deps.Artifacts == nil || in.Artifact == "" {
			return nil, &agentError{Code: "unavailable", Message: "env.push needs an artifact (or inline content)", Hint: "pass an artifact id"}
		}
		op := s.operation(ctx, "env.push@1", "", in.Env, []string{in.Artifact})
		_, err = s.deps.Artifacts.Run(ctx, op, func(ctx context.Context, run ArtifactRun) error {
			src, err := run.Source(ctx, in.Artifact)
			if err != nil {
				return err
			}
			st, err = s.deps.Manager.Push(ctx, in.Env, src, dst)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"env": in.Env, "path": dst, "files": st.Files, "bytes": st.Bytes, "skipped": st.Skipped}, nil
}

type pullIn struct {
	Env  string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Path string `json:"path" jsonschema:"file or directory to copy out"`
	Name string `json:"name,omitempty" jsonschema:"name of the new artifact"`
}

func (s *set) handlePull(ctx context.Context, call *call) (any, error) {
	var in pullIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	info, _, err := s.envOwned(ctx, in.Env)
	if err != nil {
		return nil, err
	}
	if s.deps.Artifacts == nil {
		sink := envspec.NewMemSink()
		st, err := s.deps.Manager.Pull(ctx, in.Env, in.Path, sink)
		if err != nil {
			return nil, err
		}
		return map[string]any{"env": in.Env, "path": in.Path, "files": st.Files, "bytes": st.Bytes,
			"note": "no artifact bridge wired; bytes were pulled and counted, not returned"}, nil
	}
	name := in.Name
	if name == "" {
		name = "pull"
	}
	var st envspec.TransferStats
	op := s.operation(ctx, "env.pull@1", "", in.Env, nil)
	refs, err := s.deps.Artifacts.Run(ctx, op, func(ctx context.Context, run ArtifactRun) error {
		if err := run.Record(ctx, Evidence{Environment: in.Env, Image: info.Image}); err != nil {
			return err
		}
		stageErr := run.Stage(ctx, name, func(sink envspec.Sink) error {
			var err error
			st, err = s.deps.Manager.Pull(ctx, in.Env, in.Path, sink)
			return err
		})
		// Keep what was pulled even when the pull failed midway: the
		// partial tree is evidence, and Run reports it as such.
		if kerr := run.Keep(name); stageErr == nil {
			stageErr = kerr
		}
		return stageErr
	})
	out := map[string]any{"env": in.Env, "files": st.Files, "bytes": st.Bytes, "skipped": st.Skipped}
	if err != nil {
		if partial, ok := PartialOutputsOf(err); ok {
			if ref, kept := partial[name]; kept {
				out["artifact"] = ref.ID
				out["partial"] = true
				out["warning"] = trimStack(err.Error())
				return out, nil
			}
		}
		return nil, err
	}
	out["artifact"] = refs[name].ID
	return out, nil
}

type snapIn struct {
	Env  string `json:"env" jsonschema:"environment id (env_…) from env.provision, env.list or env.status"`
	Name string `json:"name" jsonschema:"snapshot name"`
}

func (s *set) handleSnapshotSave(ctx context.Context, call *call) (any, error) {
	var in snapIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	if _, _, err := s.envOwned(ctx, in.Env); err != nil {
		return nil, err
	}
	if err := s.deps.Manager.Snapshot(ctx, in.Env, in.Name); err != nil {
		return nil, err
	}
	return map[string]any{"env": in.Env, "snapshot": in.Name, "saved": true}, nil
}

func (s *set) handleSnapshotLoad(ctx context.Context, call *call) (any, error) {
	var in snapIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	if _, _, err := s.envOwned(ctx, in.Env); err != nil {
		return nil, err
	}
	if err := s.deps.Manager.Restore(ctx, in.Env, in.Name); err != nil {
		return nil, err
	}
	return map[string]any{"env": in.Env, "snapshot": in.Name, "restored": true}, nil
}

func (s *set) handleSnapshotList(ctx context.Context, call *call) (any, error) {
	var in envIDIn
	if err := decode(call, &in); err != nil {
		return nil, err
	}
	if _, _, err := s.envOwned(ctx, in.Env); err != nil {
		return nil, err
	}
	names, err := s.deps.Manager.Snapshots(ctx, in.Env)
	if err != nil {
		return nil, err
	}
	return map[string]any{"env": in.Env, "snapshots": names}, nil
}

func (s *set) handleHostStatus(ctx context.Context, call *call) (any, error) {
	if err := requireManager(s); err != nil {
		return nil, err
	}
	snap := s.deps.Manager.HostSnapshot()
	pools := []map[string]any{}
	for _, p := range s.deps.Manager.PoolCapacities() {
		pools = append(pools, map[string]any{
			"pool": string(p.Pool), "cpus": p.CPUs, "memory_mb": p.MemoryMB,
			"reserved_cpus": p.ReservedCPUs, "reserved_memory_mb": p.ReservedMemoryMB,
			"free_cpus": p.FreeCPUs, "free_mb": p.FreeMB,
		})
	}
	return map[string]any{"host": snap, "pools": pools}, nil
}
