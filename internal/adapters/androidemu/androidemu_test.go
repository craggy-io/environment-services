package androidemu

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/adb"
)

func TestValidateCombos(t *testing.T) {
	d := New(t.TempDir())
	ctx := context.Background()
	playRootADB := envspec.Spec{Kind: envspec.Android, Android: &envspec.AndroidOptions{
		API: 34, Image: envspec.GooglePlay, Root: envspec.RootADB}}
	err := d.Validate(ctx, adapter.OpContext{}, playRootADB)
	if err == nil || !strings.Contains(err.Error(), "RootMagisk") {
		t.Fatalf("play+RootADB err = %v (must name the working alternative)", err)
	}
	if !errors.Is(err, envspec.ErrInvalid) {
		t.Fatalf("want ErrInvalid wrap, got %v", err)
	}
	var he *hintError
	if !errors.As(err, &he) {
		t.Fatalf("want hintError, got %T", err)
	}
	fridaNoRoot := envspec.Spec{Kind: envspec.Android, Android: &envspec.AndroidOptions{
		API: 34, Image: envspec.GooglePlay, Root: envspec.RootNone, Frida: &envspec.FridaOptions{}}}
	if err := d.Validate(ctx, adapter.OpContext{}, fridaNoRoot); err == nil {
		t.Fatal("frida without root must fail validation")
	}
	ok := envspec.Spec{Kind: envspec.Android, Android: &envspec.AndroidOptions{
		API: 34, Image: envspec.GooglePlay, Root: envspec.RootMagisk, Frida: &envspec.FridaOptions{}, ProxyCA: true}}
	if err := d.Validate(ctx, adapter.OpContext{}, ok); err != nil {
		t.Fatal(err)
	}
}

func TestVariantKeyAndImagePackage(t *testing.T) {
	if got := ImagePackage(34, envspec.GoogleAPIs, "x86_64"); got != "system-images;android-34;google_apis;x86_64" {
		t.Fatal(got)
	}
	if got := ImagePackage(34, envspec.GooglePlay, "arm64-v8a"); got != "system-images;android-34;google_apis_playstore;arm64-v8a" {
		t.Fatal(got)
	}
	a := VariantKey(&envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB})
	b := VariantKey(&envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootMagisk})
	if a == b {
		t.Fatal("root method must distinct variants")
	}
	// Full cutover: base AVD names are craggy-* only.
	if !strings.HasPrefix(a, "craggy-") {
		t.Fatalf("variant key = %q, want craggy- prefix", a)
	}
}

func TestDefaultImageABIFollowsHostArchitecture(t *testing.T) {
	for _, test := range []struct{ host, want string }{
		{host: "arm64", want: "arm64-v8a"},
		{host: "amd64", want: "x86_64"},
		{host: "386", want: "x86_64"},
	} {
		if got := DefaultABI(test.host); got != test.want {
			t.Errorf("DefaultABI(%q) = %q, want %q", test.host, got, test.want)
		}
	}
	for _, test := range []struct{ host, want string }{
		{host: "arm64", want: "system-images;android-34;google_apis;arm64-v8a"},
		{host: "amd64", want: "system-images;android-34;google_apis;x86_64"},
	} {
		d := New(t.TempDir())
		d.SDK = t.TempDir()
		d.HostArch = test.host
		err := d.ProfilePrepared(envspec.Profile{Android: &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs}})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("ProfilePrepared host %s error = %v, want package %s", test.host, err, test.want)
		}
	}
}

// fakeTransport is the adapter-level adb fake (Devices/Run scripted).
type bootFake struct {
	devices []adb.Device
	devErr  error
	runs    []string
	emuOut  map[string]string
}

func (f *bootFake) Devices(ctx context.Context) ([]adb.Device, error) { return f.devices, f.devErr }
func (f *bootFake) DiscoverUnused()                                   {}

func newBootFakeAdapter(t *testing.T, f *bootFake) *Adapter {
	t.Helper()
	d := New(t.TempDir())
	d.Transport = &transportOver{f}
	return d
}

func TestObservePresenceMapping(t *testing.T) {
	ctx := context.Background()
	f := &bootFake{devices: []adb.Device{{Serial: "emulator-5554", State: "device"}}}
	d := newBootFakeAdapter(t, f)
	ref := adapter.Ref{EnvID: "env_1", External: "emulator-5554"}
	obs, err := d.Observe(ctx, adapter.OpContext{}, ref)
	if err != nil || obs.Presence != adapter.Present {
		t.Fatalf("present case = %+v err=%v", obs, err)
	}
	if obs.Workspace != "/data/local/tmp/work" || obs.Endpoints["adb"] != "emulator-5554" {
		t.Fatalf("observation facts = %+v", obs)
	}
	// Gone, no live process (pid 0 in no cache) -> Absent.
	f.devices = nil
	obs, _ = d.Observe(ctx, adapter.OpContext{}, ref)
	if obs.Presence != adapter.Absent {
		t.Fatalf("gone case = %s, want Absent", obs.Presence)
	}
	// Backend down -> Unknown, never Absent.
	f.devErr = errors.New("connection refused")
	obs, _ = d.Observe(ctx, adapter.OpContext{}, ref)
	if obs.Presence != adapter.Unknown {
		t.Fatalf("down case = %s, want Unknown", obs.Presence)
	}
	// Discover via down backend errors as backend-down.
	if _, err := d.Discover(ctx, adapter.Scope{}); !errors.Is(err, adapter.ErrBackendDown) {
		t.Fatalf("discover down err = %v", err)
	}
}

func TestDiscoverScopesAndForeign(t *testing.T) {
	ctx := context.Background()
	f := &bootFake{devices: []adb.Device{
		{Serial: "emulator-5554", State: "device"},
		{Serial: "emulator-5556", State: "device"},
		{Serial: "PIXEL123", State: "device"}, // physical: not ours to report
	}}
	d := newBootFakeAdapter(t, f)
	// Cache ties 5554 to env_9.
	inst := &Instance{ad: d, envID: "env_9", serial: "emulator-5554", port: 5554}
	inst.saveCache()
	got, err := d.Discover(ctx, adapter.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("candidates = %+v, want the two emulators", got)
	}
	for _, c := range got {
		switch c.Ref.External {
		case "emulator-5554":
			if c.Foreign || c.Ref.EnvID != "env_9" || c.Ref.Name != "craggy-env_9" {
				t.Fatalf("owned candidate = %+v", c)
			}
		case "emulator-5556":
			if !c.Foreign {
				t.Fatalf("unknown emulator must be Foreign: %+v", c)
			}
		}
	}
	got, _ = d.Discover(ctx, adapter.Scope{EnvID: "env_9"})
	if len(got) != 1 || got[0].Ref.External != "emulator-5554" {
		t.Fatalf("scoped discover = %+v", got)
	}
}

func TestOpenFromCacheAndStaleCacheDropped(t *testing.T) {
	ctx := context.Background()
	f := &bootFake{devices: []adb.Device{{Serial: "emulator-5554", State: "device"}}}
	d := newBootFakeAdapter(t, f)
	variant := &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB, Frida: &envspec.FridaOptions{}}
	seed := &Instance{ad: d, envID: "env_9", serial: "emulator-5554", port: 5554, grpc: 8554,
		pid: 4242, avd: "craggy-google-apis-34-x86_64-adb", variant: variant,
		spec: envspec.Spec{Profile: "android", Android: variant}}
	seed.saveCache()

	inst, err := d.Open(ctx, adapter.OpContext{EnvID: "env_9"}, adapter.Ref{EnvID: "env_9", External: "emulator-5554"})
	if err != nil {
		t.Fatal(err)
	}
	emu := inst.(*Instance)
	if emu.avd != seed.avd || emu.pid != 4242 || emu.Device() == nil || !emu.Device().Rooted() {
		t.Fatalf("opened = %+v dev=%v", emu, emu.Device())
	}
	if emu.Ref().Name != "craggy-env_9" || emu.Ref().External != "emulator-5554" {
		t.Fatalf("ref = %+v", emu.Ref())
	}
	// Probes: rooted + frida options -> adb/root/frida components.
	names := map[string]bool{}
	for _, p := range emu.Probes() {
		names[p.Name] = true
	}
	for _, n := range []string{"adb", "root", "frida"} {
		if !names[n] {
			t.Errorf("probe %s missing from %v", n, names)
		}
	}

	// Stale cache: the recorded PID is this test process (not an
	// emulator) -> reopenLive drops the cache and Create would boot.
	stale := filepath.Join(d.instancesDir(), "env_stale.json")
	b, _ := json.Marshal(instanceCache{Env: "env_stale", PID: os.Getpid(), Port: 5554, Serial: "emulator-5554"})
	if err := os.MkdirAll(d.instancesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := d.reopenLive(ctx, "env_stale"); err == nil {
		t.Fatal("stale cache reopened")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale cache kept: %v", err)
	}
	// Open with no serial anywhere fails cleanly.
	if _, err := d.Open(ctx, adapter.OpContext{}, adapter.Ref{EnvID: "env_none"}); err == nil {
		t.Fatal("open without serial succeeded")
	}
}

func TestParseSnapshotList(t *testing.T) {
	out := "  ID        TAG                 VM SIZE                DATE       VM CLOCK\n--  clean  1.2G ...\n 1  after-login  900M ...\nOK\n"
	got := parseSnapshotList(out)
	if len(got) != 2 || got[0] != "clean" || got[1] != "after-login" {
		t.Fatalf("snapshots = %v", got)
	}
}

func TestCostDefaults(t *testing.T) {
	d := New(t.TempDir())
	c := d.Cost(envspec.Spec{})
	if c.Pool != envspec.PoolHost || c.MemoryMB != 3072+512 {
		t.Fatalf("cost = %+v", c)
	}
	if d.Describe().Relationship != adapter.Created {
		t.Fatalf("describe = %+v", d.Describe())
	}
}
