// Package environments is the one assembly and manager package: Open
// builds the host sampler, credential vault, signal hub + recorder,
// rule engine and runtime adapters over a state directory, and
// Manager is the whole consumer-facing lifecycle surface. Everything
// below it (durable core, adapters, signal plumbing) is internal;
// records and the holder/service interfaces live in envspec.
//
// There is no legacy compatibility here: identities are craggy.*
// only, state directories are fresh, and foreign resources are never
// adopted.
package environments

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adapters/androidemu"
	"github.com/craggy-io/environment-services/internal/adapters/androidphys"
	"github.com/craggy-io/environment-services/internal/adapters/docker"
	"github.com/craggy-io/environment-services/internal/adapters/hostenv"
	"github.com/craggy-io/environment-services/internal/androiddev"
	"github.com/craggy-io/environment-services/internal/core"
	"github.com/craggy-io/environment-services/internal/hostmon"
	"github.com/craggy-io/environment-services/internal/rules"
	"github.com/craggy-io/environment-services/internal/signalcore"
	"github.com/craggy-io/environment-services/internal/vault"
)

// Config opens a Manager. Only Dir is required.
type Config struct {
	// Dir is the manager state directory (state DB, vault DB, signal
	// segments, host workspaces). Use a fresh directory per manager
	// identity; never share one between managers.
	Dir string
	// ManagerID names this manager in runtime ownership labels.
	// Empty mints a fresh ID per Open; set it when resources must be
	// recognised as this manager's across restarts.
	ManagerID string
	// AndroidSDK is the Android SDK root. The Android adapters
	// (emulator and physical attach) register only when it is set.
	AndroidSDK string
	// FridaServerDir holds frida-server binaries for Android
	// environments; empty uses the manager's download cache.
	FridaServerDir string
	// FridaAutoDownload fetches a missing frida-server (matching the
	// host client version) when FridaServerDir does not have one.
	FridaAutoDownload bool
	// AVDHome is where emulator AVDs live; empty uses
	// <Dir>/android/avd. Set it to share prepared AVDs across
	// manager state directories.
	AVDHome string
	// HostSampler overrides host sampling (tests); nil samples the
	// real host once per second.
	HostSampler func() (envspec.HostSnapshot, error)
	// Profiles are registered in addition to the stock profiles and
	// any profile files under Dir.
	Profiles []envspec.Profile
	// HostPaths are extra disk-free watch paths for the host sampler
	// (Dir is always watched).
	HostPaths []string
	// MutationPolicy gates autonomous repair/reap; nil allows all.
	MutationPolicy envspec.MutationPolicy
	// Logger receives assembly notes (adapter skipped, rule trigger
	// captures); nil discards.
	Logger *slog.Logger
}

// ShutdownMode selects what Close does with live environments.
type ShutdownMode int

const (
	// ShutdownDisconnect stops the manager and leaves resources
	// running for the next Open over the same Dir.
	ShutdownDisconnect ShutdownMode = iota
	// ShutdownDrain releases every live environment first (bounded
	// by the Close context; unfinished releases stay durable
	// operations for the next Open).
	ShutdownDrain
)

// Filter selects environments for List. Zero fields match anything.
type Filter struct {
	Owner   string
	Session string
	Kind    envspec.Kind
	State   envspec.State
	Live    bool // only non-terminal
}

// Driver describes one registered runtime adapter.
type Driver struct {
	Name    string
	Kinds   []envspec.Kind
	Version string
}

// ProvisionOption tunes one Provision call.
type ProvisionOption struct{ opt core.ProvisionOption }

// OnProgress reports provisioning progress (stage-scoped, throttled
// by the manager).
func OnProgress(fn func(envspec.Progress)) ProvisionOption {
	return ProvisionOption{opt: core.OnProgress(fn)}
}

// WithRequestKey makes the call idempotent under (owner, key):
// repeating it returns the existing operation's environment, and a
// different spec under the same key fails with envspec.ErrConflict.
// Equivalent to setting Spec.Key.
func WithRequestKey(key string) ProvisionOption {
	return ProvisionOption{opt: core.WithRequestKey(key)}
}

func coreOpts(opts []ProvisionOption) []core.ProvisionOption {
	out := make([]core.ProvisionOption, 0, len(opts))
	for _, o := range opts {
		if o.opt != nil {
			out = append(out, o.opt)
		}
	}
	return out
}

// Manager owns environments, leases, admission and lifecycle events
// over one state directory. Safe for concurrent use. Exactly one
// Manager per Dir (enforced by a lock file).
type Manager struct {
	core      *core.Manager
	mon       *hostmon.Monitor
	vault     *vault.Vault
	redactor  *vault.Redactor
	hub       *signalcore.Hub
	rec       *signalcore.Recorder
	rules     *rules.Engine
	cancelMon context.CancelFunc

	closeOnce sync.Once
	closeErr  error
}

var _ envspec.ServiceProvider = (*Manager)(nil)

// Open assembles and opens a Manager: host sampler, vault, signal
// hub + recorder, rule engine, then adapters — host always; docker
// when the docker binary (or DOCKER_HOST) is present and the backend
// answers; Android only when Config.AndroidSDK is set. Adapters that
// are absent are skipped with a log note, never an error: profiles
// for a missing kind report not-ready through ProfileStatus.
func Open(ctx context.Context, cfg Config) (*Manager, error) {
	if cfg.Dir == "" {
		return nil, fmt.Errorf("%w: Config.Dir is required", envspec.ErrInvalid)
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	managerID := cfg.ManagerID
	if managerID == "" {
		managerID = envspec.NewID("mgr_")
	}
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}

	paths := append([]string{cfg.Dir}, cfg.HostPaths...)
	mon := hostmon.New(hostmon.Config{Paths: paths, Sampler: cfg.HostSampler})
	monCtx, cancelMon := context.WithCancel(context.Background())
	mon.Start(monCtx)

	v, err := vault.Open(filepath.Join(cfg.Dir, "vault.db"))
	if err != nil {
		cancelMon()
		return nil, err
	}
	redactor := v.Redactor()

	rec := signalcore.NewRecorder(signalcore.RecorderConfig{Dir: filepath.Join(cfg.Dir, "signals")})
	hub := signalcore.NewHub(signalcore.WithRecorder(rec), signalcore.WithRedactor(redactor))

	m := &Manager{mon: mon, vault: v, redactor: redactor, hub: hub, rec: rec, cancelMon: cancelMon}
	m.rules = rules.NewEngine(hub, rec,
		rules.WithTriggerSink(m.triggerSink(log)),
		rules.WithLookups(rules.Lookups{EnvInfo: m.ruleEnvInfo, HostSnapshot: m.ruleHostSnapshot}))

	adapters := []adapter.Adapter{hostenv.Default(cfg.Dir, "")}
	if dd, ok := dockerAdapter(ctx, managerID, mon, log); ok {
		adapters = append(adapters, dd)
	}
	if cfg.AndroidSDK != "" {
		emu := androidemu.New(cfg.Dir)
		emu.SDK = cfg.AndroidSDK
		emu.AVDHome = cfg.AVDHome
		emu.FridaServer = &androiddev.FridaServer{ServerDir: cfg.FridaServerDir, StateDir: cfg.Dir, AutoDownload: cfg.FridaAutoDownload}
		emu.CaptureFn = m.captureBeforeRepair
		phys := androidphys.New()
		phys.FridaServer = emu.FridaServer
		phys.CaptureFn = m.captureBeforeRepair
		adapters = append(adapters, emu, phys)
	} else {
		log.Debug("android adapters skipped: Config.AndroidSDK not set")
	}

	cm, err := core.Open(ctx, core.Config{
		Dir: cfg.Dir, ManagerID: managerID, Adapters: adapters,
		Host: mon, Vault: v, Redactor: redactor, Signals: hub,
		Profiles: cfg.Profiles, MutationPolicy: cfg.MutationPolicy,
	})
	if err != nil {
		hub.Close()
		_ = v.Close()
		cancelMon()
		return nil, err
	}
	m.core = cm
	return m, nil
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// dockerAdapter registers the docker adapter when a client is
// plausibly configured (docker binary on PATH or DOCKER_HOST set),
// whether or not the backend answers right now: a stopped Docker
// stays startable later via EnsureProfileBackend/provision. The
// runtime is never started here; the ping only seeds capacity.
func dockerAdapter(ctx context.Context, managerID string, mon *hostmon.Monitor, log *slog.Logger) (*docker.Adapter, bool) {
	if os.Getenv("DOCKER_HOST") == "" {
		if _, err := osexec.LookPath("docker"); err != nil {
			log.Debug("docker adapter skipped: docker binary not on PATH and DOCKER_HOST unset")
			return nil, false
		}
	}
	dd, err := docker.New(managerID)
	if err != nil {
		log.Debug("docker adapter skipped: client init failed", "err", err)
		return nil, false
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := dd.Check(pingCtx); err != nil {
		log.Debug("docker backend not answering at open; registered for start-on-need", "err", err)
		return dd, true
	}
	if memMB, cpus, err := dd.Capacity(pingCtx); err == nil {
		mon.SetDockerCapacity(memMB, cpus)
	}
	return dd, true
}

// triggerSink persists rule triggers as captures: a trigger with a
// capture window pins that range in the recorder and reports its ID.
func (m *Manager) triggerSink(log *slog.Logger) rules.TriggerSink {
	return func(t rules.Trigger) (string, error) {
		log.Debug("rule triggered", "rule", t.Rule, "kind", t.Kind)
		if t.Capture == nil {
			return "", nil
		}
		sel := envspec.Selector{Streams: t.Capture.Streams, Sources: t.Capture.Sources}
		cap, err := m.rec.Capture(sel, t.Time.Add(-t.Capture.Before), t.Time.Add(t.Capture.After))
		if err != nil {
			return "", err
		}
		return cap.ID(), nil
	}
}

// captureBeforeRepair pins the recent signal range of a failing
// Android env so repair never destroys the evidence of the failure.
func (m *Manager) captureBeforeRepair(envID string, f androiddev.FailureInfo) string {
	now := time.Now()
	cap, err := m.rec.Capture(envspec.Selector{Sources: []string{envID}}, now.Add(-2*time.Minute), now)
	if err != nil {
		return ""
	}
	return cap.ID()
}

func (m *Manager) ruleEnvInfo(id string) (map[string]any, bool) {
	if m.core == nil {
		return nil, false
	}
	info, err := m.core.Info(id)
	if err != nil {
		return nil, false
	}
	return toMap(info), true
}

func (m *Manager) ruleHostSnapshot() map[string]any {
	s, ok := m.mon.Latest()
	if !ok {
		return map[string]any{}
	}
	return toMap(s)
}

// toMap flattens a record to the map shape rule lookups expose.
func toMap(v any) map[string]any {
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return out
}
