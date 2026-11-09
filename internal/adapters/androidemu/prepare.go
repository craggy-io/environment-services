package androidemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/androiddev"
)

// DefaultABI maps the emulator host architecture to the compatible Android
// system-image ABI. An explicit profile ABI always takes precedence.
func DefaultABI(goarch string) string {
	if goarch == "arm64" {
		return "arm64-v8a"
	}
	return "x86_64"
}

func (d *Adapter) defaultABI() string {
	hostArch := d.HostArch
	if hostArch == "" {
		hostArch = runtime.GOARCH
	}
	return DefaultABI(hostArch)
}

// ImagePackage is the sdkmanager package path for a variant.
func ImagePackage(api int, kind envspec.ImageKind, abi string) string {
	flavour := "google_apis"
	switch kind {
	case envspec.GooglePlay:
		flavour = "google_apis_playstore"
	case envspec.AOSP:
		flavour = "default"
	}
	if abi == "" {
		abi = DefaultABI(runtime.GOARCH)
	}
	return fmt.Sprintf("system-images;android-%d;%s;%s", api, flavour, abi)
}

// VariantKey identifies one base AVD per (image, root, magisk) tuple.
func VariantKey(o *envspec.AndroidOptions) string {
	if o == nil {
		o = &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB}
	}
	api := o.API
	if api == 0 {
		api = 34
	}
	abi := o.ABI
	if abi == "" {
		abi = DefaultABI(runtime.GOARCH)
	}
	img := o.Image
	if img == "" {
		img = envspec.GoogleAPIs
	}
	return fmt.Sprintf("craggy-%s-%d-%s-%s", strings.ReplaceAll(string(img), "_", "-"), api, abi, o.Root)
}

func (d *Adapter) variantOptions(p envspec.Profile) *envspec.AndroidOptions {
	o := &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB}
	if p.Android != nil {
		cp := *p.Android
		o = &cp
		if o.API == 0 {
			o.API = 34
		}
		if o.Image == "" {
			o.Image = envspec.GoogleAPIs
		}
	}
	if o.ABI == "" {
		o.ABI = d.defaultABI()
	}
	return o
}

func (d *Adapter) imageInstalled(o *envspec.AndroidOptions) bool {
	pkg := ImagePackage(o.API, o.Image, o.ABI)
	dir := filepath.Join(d.SDK, filepath.FromSlash(strings.ReplaceAll(pkg, ";", "/")))
	_, err := os.Stat(filepath.Join(dir, "system.img"))
	if err == nil {
		return true
	}
	_, err = os.Stat(filepath.Join(dir, "package.xml"))
	return err == nil
}

func (d *Adapter) avdExists(key string) bool {
	_, err := os.Stat(filepath.Join(d.avdHome(), key+".avd", "config.ini"))
	return err == nil
}

func (d *Adapter) cleanSnapshotExists(key string) bool {
	_, err := os.Stat(filepath.Join(d.avdHome(), key+".avd", "snapshots", "clean"))
	return err == nil
}

// ProfilePrepared reports whether a profile's SDK image, base AVD and
// clean snapshot all exist.
func (d *Adapter) ProfilePrepared(p envspec.Profile) error {
	o := d.variantOptions(p)
	if !d.imageInstalled(o) {
		return fmt.Errorf("system image %s not installed", ImagePackage(o.API, o.Image, o.ABI))
	}
	key := VariantKey(o)
	if !d.avdExists(key) {
		return fmt.Errorf("base AVD %s not created", key)
	}
	if !d.cleanSnapshotExists(key) {
		return fmt.Errorf("base AVD %s has no clean snapshot", key)
	}
	return nil
}

// PrepareProfile installs the system image (sdkmanager, progress by
// stage), creates the base AVD, boots it once to set up root/Frida/CA,
// and snapshots `clean`. Slow parts happen exactly once per variant.
func (d *Adapter) PrepareProfile(ctx context.Context, p envspec.Profile, progress func(envspec.Progress)) error {
	if err := d.Check(ctx); err != nil {
		return err
	}
	o := d.variantOptions(p)
	report := func(stage, msg string) {
		if progress != nil {
			progress(envspec.Progress{Op: "provision", Stage: stage, Message: msg})
		}
	}
	key := VariantKey(o)
	if !d.imageInstalled(o) {
		pkg := ImagePackage(o.API, o.Image, o.ABI)
		report("sdk-install", "installing "+pkg+" (several GB)")
		out, err := runTool(ctx, d.sdkManager(), []string{"--sdk_root=" + d.SDK, "--install", pkg}, sdkEnv(d.SDK, d.avdHome()))
		if err != nil {
			return fmt.Errorf("android: sdkmanager install %s: %w\n%s", pkg, err, tailLines(string(out), 20))
		}
	}
	if !d.avdExists(key) {
		report("base-avd", "creating base AVD "+key)
		pkg := ImagePackage(o.API, o.Image, o.ABI)
		out, err := runToolInput(ctx, d.avdManager(), []string{
			"create", "avd", "--force",
			"--name", key, "--package", pkg, "--device", "pixel_6",
		}, "no\n", sdkEnv(d.SDK, d.avdHome()))
		if err != nil {
			return fmt.Errorf("android: avdmanager create %s: %w\n%s", key, err, tailLines(string(out), 20))
		}
		// Sensible base config: enough RAM/disk for analysis images.
		d.tuneAVDConfig(key)
	}
	if d.cleanSnapshotExists(key) {
		return nil
	}

	// One slow boot to prepare the clean snapshot.
	report("base-avd", "first boot of "+key+" to set up root, Frida and the proxy CA")
	if o.Root == envspec.RootMagisk {
		if err := d.prepareMagisk(ctx, key, o, progress, report); err != nil {
			return err
		}
	}
	bootCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	inst, err := d.bootEmulator(bootCtx, "env_prepare_"+key, key, envspec.Spec{Android: o}, bootOptions{writable: true, noSnapshot: true}, progress)
	if err != nil {
		return err
	}
	defer func() { _ = inst.destroy(context.Background()) }()

	if err := d.setupBase(ctx, inst, o, report); err != nil {
		return err
	}
	report("base-avd", "saving clean snapshot")
	if _, err := d.emu(ctx, inst.serial, "avd snapshot save clean"); err != nil {
		return fmt.Errorf("android: save clean snapshot: %w", err)
	}
	_, _ = d.emu(ctx, inst.serial, "avd snapshot list")
	return nil
}

// setupBase performs in-guest first-boot preparation: root (adb), push the
// frida-server binary, install the proxy CA when configured. Everything is
// best-effort except root for rooted variants; failures surface as
// component state later rather than blocking the base.
func (d *Adapter) setupBase(ctx context.Context, inst *Instance, o *envspec.AndroidOptions, report func(string, string)) error {
	serial := inst.serial
	report("base-avd", "waiting for package manager")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, code, err := d.transport().Run(ctx, serial, "pm list packages 2>/dev/null | head -1")
		if err == nil && code == 0 && strings.Contains(out, "package:") {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("android: package manager never answered on %s", serial)
		}
	}
	if o.Root == envspec.RootADB {
		report("components", "adb root")
		if err := androiddev.RootADB(ctx, d.transport(), serial, 2*time.Minute); err != nil {
			return err
		}
	}
	if o.Root == envspec.RootMagisk {
		report("components", "Magisk daemon + app")
		if err := d.setupMagisk(ctx, inst); err != nil {
			return err
		}
	}
	inst.dev.EnsureFridaBinary(ctx)
	if d.ProxyCAPEM != "" {
		_ = inst.dev.EnsureProxyCA(ctx)
	}
	// Workspace root used by transfer.
	_, _, _ = d.transport().Run(ctx, serial, "mkdir -p /data/local/tmp/work")
	return nil
}

// The Magisk ramdisk patch lives in magisk.go.

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
