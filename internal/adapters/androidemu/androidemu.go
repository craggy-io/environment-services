// Package androidemu is the Android emulator adapter: per-variant
// base AVDs with clean snapshots, read-only multi-instance quickboots
// (console ports 5554–5584), boot observation, snapshots and reset.
// Device access over the serial (shell, packages, frida/root/proxy-ca
// components, collectors) is internal/androiddev; this package owns
// only emulator lifecycle.
//
// Identity: the durable Ref's External ID — the adb serial
// (emulator-<port>) recorded by core in the env DB — is the
// authority for existence and ownership. The JSON files under
// <StateDir>/android/instances/ are a cache only: they remember the
// spec, PID and AVD of a running emulator so Open after a restart
// does not have to re-derive them. Losing the cache degrades Open to
// live probing (console `avd name`, root-mode probe); it can never
// make a foreign emulator ours, and a cache entry without a DB
// record is never adopted.
package androidemu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adb"
	"github.com/craggy-io/environment-services/internal/androiddev"
)

// Adapter is the Android emulator adapter.
type Adapter struct {
	SDK      string // ANDROID_SDK_ROOT; discovered when empty
	AVDHome  string // defaults to <StateDir>/android/avd
	StateDir string // manager Dir; instance cache at android/instances
	// HostArch selects the default system-image ABI when a profile
	// leaves ABI empty. New sets runtime.GOARCH; tests may override.
	HostArch string
	ADB      *adb.Client
	// Transport overrides the adb transport (tests). Nil means ADB.
	// The emulator console (Emu) is taken from Transport when it
	// implements it, else from ADB.
	Transport androiddev.Transport
	// FridaServer locates frida-server binaries (optional; the frida
	// component is degraded with guidance when unset).
	FridaServer *androiddev.FridaServer
	// MagiskAPK enables RootMagisk preparation.
	MagiskAPK string
	// ProxyCAPEM is the egress proxy CA certificate (PEM) to install.
	ProxyCAPEM string
	// GRPCBase is the first emulator gRPC port (default 8554).
	GRPCBase int
	// CaptureFn pins evidence before repairs (manager wires the recorder).
	CaptureFn func(envID string, f androiddev.FailureInfo) string

	mu    sync.Mutex
	ports map[int]string // console port → env ID
	// components is core's component sink (WatchComponents).
	components *componentSink
}

type componentSink struct {
	fn func(envID string, c adapter.ComponentChange)
}

// New builds the adapter over the discovered SDK.
func New(stateDir string) *Adapter {
	return &Adapter{SDK: sdkRoot(), StateDir: stateDir, HostArch: runtime.GOARCH,
		ADB: adb.New(), GRPCBase: 8554, ports: map[int]string{}}
}

func (d *Adapter) transport() androiddev.Transport {
	if d.Transport != nil {
		return d.Transport
	}
	if d.ADB == nil {
		d.ADB = adb.New()
	}
	return androiddev.ClientTransport{C: d.ADB}
}

type emuConsole interface {
	Emu(ctx context.Context, serial, cmd string) (string, error)
}

func (d *Adapter) emu(ctx context.Context, serial, cmd string) (string, error) {
	if e, ok := d.transport().(emuConsole); ok {
		return e.Emu(ctx, serial, cmd)
	}
	if d.ADB != nil {
		return androiddev.ClientTransport{C: d.ADB}.Emu(ctx, serial, cmd)
	}
	return "", fmt.Errorf("android: no emulator console transport")
}

// Describe implements adapter.Adapter.
func (d *Adapter) Describe() adapter.Description {
	return adapter.Description{Name: "android-emu", Kinds: []envspec.Kind{envspec.Android},
		Relationship: adapter.Created, Version: "1"}
}

// WatchComponents implements adapter.ComponentWatcher: every
// supervisor transition of every instance goes to fn.
func (d *Adapter) WatchComponents(fn func(envID string, c adapter.ComponentChange)) (stop func()) {
	sink := &componentSink{fn: fn}
	d.mu.Lock()
	d.components = sink
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		if d.components == sink {
			d.components = nil
		}
		d.mu.Unlock()
	}
}

func (d *Adapter) componentChanged(envID string, c adapter.ComponentChange) {
	d.mu.Lock()
	sink := d.components
	d.mu.Unlock()
	if sink != nil {
		sink.fn(envID, c)
	}
}

func sdkRoot() string {
	for _, k := range []string{"ANDROID_SDK_ROOT", "ANDROID_HOME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

func (d *Adapter) avdHome() string {
	if d.AVDHome != "" {
		return d.AVDHome
	}
	return filepath.Join(d.StateDir, "android", "avd")
}

func (d *Adapter) instancesDir() string {
	return filepath.Join(d.StateDir, "android", "instances")
}

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func (d *Adapter) emulatorBin() string {
	return filepath.Join(d.SDK, "emulator", exe("emulator"))
}

func (d *Adapter) cmdlineTool(name string) string {
	// cmdline-tools;latest/bin, fall back to tools/bin.
	latest := filepath.Join(d.SDK, "cmdline-tools", "latest", "bin", toolFile(name))
	if _, err := os.Stat(latest); err == nil {
		return latest
	}
	matches, _ := filepath.Glob(filepath.Join(d.SDK, "cmdline-tools", "*", "bin", toolFile(name)))
	if len(matches) > 0 {
		return matches[0]
	}
	return filepath.Join(d.SDK, "tools", "bin", toolFile(name))
}

func toolFile(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".bat"
	}
	return name
}

func (d *Adapter) sdkManager() string { return d.cmdlineTool("sdkmanager") }
func (d *Adapter) avdManager() string { return d.cmdlineTool("avdmanager") }

func sdkEnv(sdk, avdHome string) []string {
	env := os.Environ()
	set := func(k, v string) {
		for i, kv := range env {
			if strings.HasPrefix(strings.ToUpper(kv), k+"=") {
				env[i] = k + "=" + v
				return
			}
		}
		env = append(env, k+"="+v)
	}
	set("ANDROID_SDK_ROOT", sdk)
	set("ANDROID_HOME", sdk)
	set("ANDROID_AVD_HOME", avdHome)
	return env
}

func runTool(ctx context.Context, bin string, args []string, environ []string) ([]byte, error) {
	return runToolInput(ctx, bin, args, "", environ)
}

func runToolInput(ctx context.Context, bin string, args []string, stdin string, environ []string) ([]byte, error) {
	cmd := osexec.CommandContext(ctx, bin, args...)
	cmd.Env = environ
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return []byte(buf.String()), err
}

// hintError carries an agent-facing next step beside the cause.
type hintError struct {
	err  error
	hint string
}

func (e *hintError) Error() string { return e.err.Error() + " (hint: " + e.hint + ")" }
func (e *hintError) Unwrap() error { return e.err }

func withHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	return &hintError{err: err, hint: hint}
}

// Check verifies the SDK, emulator binary and (best-effort) acceleration.
func (d *Adapter) Check(ctx context.Context) error {
	if d.SDK == "" {
		return withHint(fmt.Errorf("%w: ANDROID_SDK_ROOT/ANDROID_HOME not set", adapter.ErrBackendDown),
			"install the Android SDK and set ANDROID_SDK_ROOT")
	}
	if _, err := os.Stat(d.emulatorBin()); err != nil {
		return fmt.Errorf("android: emulator binary: %w", err)
	}
	return os.MkdirAll(d.avdHome(), 0o755)
}

// Validate rejects combinations that can't work, naming the alternative.
func (d *Adapter) Validate(ctx context.Context, op adapter.OpContext, s envspec.Spec) error {
	o := s.Android
	if o == nil {
		return nil
	}
	invalid := func(err error, hint string) error {
		return withHint(fmt.Errorf("%w: %s", envspec.ErrInvalid, err), hint)
	}
	if o.Image == envspec.GooglePlay && o.Root == envspec.RootADB {
		return invalid(fmt.Errorf("google_apis_playstore with RootADB cannot work: production builds disable adb root"),
			"use Root: RootMagisk or profile \"android-play-rooted\"")
	}
	if o.ProxyCA && (o.Root == envspec.RootNone || o.Root == "") {
		return invalid(fmt.Errorf("ProxyCA installs a system CA and needs root"),
			"use profile \"android\" or \"android-play-rooted\"")
	}
	if o.Frida != nil && (o.Root == envspec.RootNone || o.Root == "") {
		return invalid(fmt.Errorf("frida-server needs root; this variant is unrooted"),
			"provision with profile \"android\" (or \"android-play-rooted\")")
	}
	return nil
}

// Cost implements adapter.Coster.
func (d *Adapter) Cost(s envspec.Spec) adapter.Cost {
	mem := s.Resources.MemoryMB
	if mem == 0 {
		mem = 3072
	}
	return adapter.Cost{Pool: envspec.PoolHost, CPUs: s.Resources.CPUs, MemoryMB: mem + 512 /* qemu overhead */, DiskMB: s.Resources.DiskMB}
}

func serialPort(serial string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	return n
}

// Observe reports presence from adb state triangulated with the
// emulator process: listed as device is Present; process alive but
// unlisted is Unknown (adb may be restarting); neither is Absent. An
// unreachable adb server is Unknown, never Absent.
func (d *Adapter) Observe(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Observation, error) {
	obs := adapter.Observation{Ref: ref, Workspace: "/data/local/tmp/work"}
	serial := ref.External
	var cache *instanceCache
	if c, err := d.loadCache(ref.EnvID); err == nil {
		cache = c
		if serial == "" {
			serial = c.Serial
		}
	}
	if serial == "" {
		obs.Presence = adapter.Unknown
		obs.Detail = "no serial recorded for env"
		return obs, nil
	}
	obs.Ref.External = serial
	obs.Endpoints = map[string]string{"adb": serial}
	if cache != nil {
		obs.Endpoints["grpc"] = fmt.Sprint(cache.GRPC)
		obs.Endpoints["avd"] = cache.AVD
	}
	devs, err := d.transport().Devices(ctx)
	if err != nil {
		obs.Presence = adapter.Unknown
		obs.Detail = adapter.BackendDown(err).Error()
		return obs, nil
	}
	for _, dev := range devs {
		if dev.Serial == serial && dev.State == "device" {
			obs.Presence = adapter.Present
			return obs, nil
		}
	}
	// Not listed: an emulator process still running on the port means
	// adb has not (re)seen it yet — Unknown, not Absent.
	pid, port := 0, serialPort(serial)
	if cache != nil {
		pid, port = cache.PID, cache.Port
	}
	if emulatorProcess(pid, port) != nil {
		obs.Presence = adapter.Unknown
		obs.Detail = "emulator process alive but adb does not list " + serial
		return obs, nil
	}
	obs.Presence = adapter.Absent
	return obs, nil
}

// Discover lists emulator serials adb can see. Ones the instance
// cache ties to an env are ours (the DB Ref decides adoption);
// the rest are foreign and left alone.
func (d *Adapter) Discover(ctx context.Context, scope adapter.Scope) ([]adapter.Candidate, error) {
	devs, err := d.transport().Devices(ctx)
	if err != nil {
		return nil, adapter.BackendDown(err)
	}
	bySerial := d.cacheBySerial()
	var out []adapter.Candidate
	for _, dev := range devs {
		if !strings.HasPrefix(dev.Serial, "emulator-") {
			continue
		}
		envID := bySerial[dev.Serial]
		if scope.EnvID != "" && envID != scope.EnvID {
			continue
		}
		c := adapter.Candidate{Ref: adapter.Ref{EnvID: envID, External: dev.Serial}, Foreign: envID == ""}
		if envID != "" {
			c.Ref.Name = adapter.NameFor(envID)
		} else {
			c.Detail = "emulator not created by this manager"
		}
		out = append(out, c)
	}
	return out, nil
}

// instanceCache is the on-disk cache of one running emulator's
// facts (see the package doc: cache, never authority).
type instanceCache struct {
	Env      string                  `json:"env"`
	PID      int                     `json:"pid"`
	Port     int                     `json:"port"`
	GRPC     int                     `json:"grpc"`
	Serial   string                  `json:"serial"`
	AVD      string                  `json:"avd"`
	Spec     envspec.Spec            `json:"spec"`
	Variant  *envspec.AndroidOptions `json:"variant"`
	Writable bool                    `json:"writable,omitempty"`
}

func (d *Adapter) cachePath(envID string) string {
	return filepath.Join(d.instancesDir(), envID+".json")
}

func (d *Adapter) loadCache(envID string) (*instanceCache, error) {
	if envID == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(d.cachePath(envID))
	if err != nil {
		return nil, err
	}
	var c instanceCache
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (d *Adapter) cacheBySerial() map[string]string {
	out := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(d.instancesDir(), "*.json"))
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var c instanceCache
		if json.Unmarshal(b, &c) == nil && c.Serial != "" {
			out[c.Serial] = c.Env
		}
	}
	d.mu.Lock()
	for port, envID := range d.ports {
		out[fmt.Sprintf("emulator-%d", port)] = envID
	}
	d.mu.Unlock()
	return out
}
