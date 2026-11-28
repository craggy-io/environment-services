package androidemu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

// imagePackage is the sdkmanager package of a variant as this SDK names
// it. Since API 37 Google names a platform's images by its minor release
// too (system-images;android-37.0;...), and an API level may have several
// (android-36.1): the one installed is used, else the newest-looking name
// to install. Asking for android-37 found no image, installed nothing and
// failed creating the AVD.
func (d *Adapter) imagePackage(o *envspec.AndroidOptions) string {
	pkg := ImagePackage(o.API, o.Image, o.ABI)
	if d.packageInstalled(pkg) {
		return pkg
	}
	parts := strings.Split(pkg, ";")
	matches, _ := filepath.Glob(filepath.Join(d.SDK, "system-images", fmt.Sprintf("android-%d.*", o.API), parts[2], parts[3]))
	sort.Strings(matches)
	for i := len(matches) - 1; i >= 0; i-- {
		release := filepath.Base(filepath.Dir(filepath.Dir(matches[i])))
		named := strings.Join([]string{parts[0], release, parts[2], parts[3]}, ";")
		if d.packageInstalled(named) {
			return named
		}
	}
	if o.API >= 37 {
		return strings.Join([]string{parts[0], fmt.Sprintf("android-%d.0", o.API), parts[2], parts[3]}, ";")
	}
	return pkg
}

// installImage installs a system image. Newer command-line tools carry
// the Android CLI (android sdk install, paths with slashes) and make
// sdkmanager a wrapper of it that takes a semicolon path for four packages
// not found, and still exits 0: the image was never installed and
// avdmanager failed after it. So the CLI is used when it is there, and an
// install that leaves no image is an error either way.
func (d *Adapter) installImage(ctx context.Context, pkg string) error {
	bin, args := d.sdkManager(), []string{"--sdk_root=" + d.SDK, "--install", pkg}
	if cli := d.androidCLI(); cli != "" {
		bin, args = cli, []string{"--sdk=" + d.SDK, "sdk", "install", strings.ReplaceAll(pkg, ";", "/")}
	}
	out, err := runToolInput(ctx, bin, args, "y\ny\ny\ny\ny\n", sdkEnv(d.SDK, d.avdHome()))
	if err != nil {
		return fmt.Errorf("android: install %s: %w\n%s", pkg, err, tailLines(string(out), 20))
	}
	if !d.packageInstalled(pkg) {
		return fmt.Errorf("android: install %s left no image in the SDK\n%s", pkg, tailLines(string(out), 20))
	}
	return nil
}

// androidCLI is the Android CLI of the command-line tools, or "".
func (d *Adapter) androidCLI() string {
	name := "android"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cli := filepath.Join(d.SDK, "cmdline-tools", "latest", "bin", name)
	if info, err := os.Stat(cli); err == nil && !info.IsDir() {
		return cli
	}
	return ""
}

// packageInstalled says whether an sdkmanager package is in the SDK.
func (d *Adapter) packageInstalled(pkg string) bool {
	dir := filepath.Join(d.SDK, filepath.FromSlash(strings.ReplaceAll(pkg, ";", "/")))
	for _, name := range []string{"system.img", "package.xml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return true
		}
	}
	return false
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
	return d.packageInstalled(d.imagePackage(o))
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
		return fmt.Errorf("system image %s not installed", d.imagePackage(o))
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
		pkg := d.imagePackage(o)
		report("sdk-install", "installing "+pkg+" (several GB)")
		if err := d.installImage(ctx, pkg); err != nil {
			return err
		}
	}
	if !d.avdExists(key) {
		report("base-avd", "creating base AVD "+key)
		pkg := d.imagePackage(o)
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
