package androidemu

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/craggy-io/environment-services/envspec"
)

// Magisk preparation (RootMagisk): patch the stock ramdisk host-side
// (the same entry changes Magisk's boot_patch.sh performs; see
// ramdisk.go for why magiskboot itself can't do it on modern images),
// boot with `-ramdisk <patched>`, install the Magisk app. The patched
// ramdisk lives beside the AVD as magisk-ramdisk.img.

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (d *Adapter) magiskRamdiskPath(avdName string) string {
	return filepath.Join(d.avdHome(), avdName+".avd", "magisk-ramdisk.img")
}

// extractMagiskAssets pulls the patch-time binaries for arch out of the
// Magisk APK (they ship as lib/<abi>/lib*.so).
func extractMagiskAssets(apkPath, abi string) (map[string][]byte, error) {
	if abi == "" {
		abi = "x86_64"
	}
	zr, err := zip.OpenReader(apkPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	want := map[string]string{
		"lib/" + abi + "/libmagiskboot.so": "magiskboot",
		"lib/" + abi + "/libmagiskinit.so": "magiskinit",
		"lib/" + abi + "/libmagisk.so":     "magisk",
		"lib/" + abi + "/libinit-ld.so":    "init-ld",
	}
	out := map[string][]byte{}
	for _, f := range zr.File {
		name, ok := want[f.Name]
		if !ok {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		out[name] = b
	}
	if len(out) != len(want) {
		return nil, fmt.Errorf("android: Magisk APK is missing patch assets for %s (found %d of %d)", abi, len(out), len(want))
	}
	return out, nil
}

// prepareMagisk patches the variant's ramdisk host-side (see ramdisk.go
// for why magiskboot can't do it on modern images) and leaves
// magisk-ramdisk.img beside the AVD; boots pass it via -ramdisk.
func (d *Adapter) prepareMagisk(ctx context.Context, avdName string, o *envspec.AndroidOptions, progress func(envspec.Progress), report func(string, string)) error {
	if d.MagiskAPK == "" {
		return fmt.Errorf("android: RootMagisk base %s needs a Magisk APK: set Driver.MagiskAPK (path to Magisk.apk) so the base AVD ramdisk can be patched", avdName)
	}
	abi := o.ABI
	if abi == "" {
		abi = "x86_64"
	}
	assets, err := extractMagiskAssets(d.MagiskAPK, abi)
	if err != nil {
		return err
	}
	// Reuse an existing patched ramdisk only if it truly is one.
	if existing, err := os.ReadFile(d.magiskRamdiskPath(avdName)); err == nil {
		if cpioData, _, err := decompressRamdisk(existing); err == nil {
			if entries, err := parseNewc(cpioData); err == nil && verifyPatchedCPIO(entries, assets["magiskinit"]) == nil {
				return nil
			}
		}
		report("base-avd", "existing magisk-ramdisk.img did not verify; re-patching")
	}
	pkg := ImagePackage(o.API, o.Image, o.ABI)
	ramdiskSrc := filepath.Join(d.SDK, filepath.FromSlash(strings.ReplaceAll(pkg, ";", "/")), "ramdisk.img")
	stock, err := os.ReadFile(ramdiskSrc)
	if err != nil {
		return fmt.Errorf("android: read stock ramdisk: %w", err)
	}
	report("base-avd", "patching ramdisk with Magisk (host-side cpio surgery)")
	cpioData, format, err := decompressRamdisk(stock)
	if err != nil {
		return err
	}
	entries, err := parseNewc(cpioData)
	if err != nil {
		return err
	}
	magiskXZ, err := xzCompressBytes(assets["magisk"])
	if err != nil {
		return err
	}
	initLDXZ, err := xzCompressBytes(assets["init-ld"])
	if err != nil {
		return err
	}
	var origInit []byte
	for _, e := range entries {
		if e.name == "init" {
			origInit = e.data
		}
	}
	if origInit == nil {
		return fmt.Errorf("android: stock ramdisk has no /init to back up")
	}
	origInitXZ, err := xzCompressBytes(origInit)
	if err != nil {
		return err
	}
	patched := patchRamdiskCPIO(entries, assets, magiskXZ, initLDXZ, origInitXZ)
	if err := verifyPatchedCPIO(patched, assets["magiskinit"]); err != nil {
		return err
	}
	// A stock-identical result is the failure mode that must never ship.
	outBytes, err := compressRamdisk(writeNewc(patched), format)
	if err != nil {
		return err
	}
	if bytes.Equal(outBytes, stock) {
		return fmt.Errorf("android: patched ramdisk is byte-identical to stock; patch did not apply")
	}
	if err := os.MkdirAll(filepath.Dir(d.magiskRamdiskPath(avdName)), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(d.magiskRamdiskPath(avdName), outBytes, 0o644); err != nil {
		return err
	}
	report("base-avd", fmt.Sprintf("patched ramdisk saved (%d bytes)", len(outBytes)))
	return nil
}

func xzCompressBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	// CRC32 check, not the library default CRC64: magiskinit's embedded
	// XZ decoder rejects CRC64 streams, which then "decompress" to
	// garbage and the restored init dies with Exec format error.
	w, err := xz.WriterConfig{CheckSum: xz.CRC32}.NewWriter(&buf)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(data); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// setupMagisk runs on the patched boot: install the Magisk app, confirm
// the daemon, then make `su` usable for the adb shell. Magisk v31
// removed the interactive grant prompt (HSUM redesign): access must be
// pre-granted from the app's Superuser tab, so we drive that flow over
// the accessibility tree — launch app, dismiss the notification
// permission, accept "Requires additional setup", back out of the
// release notes, then Superuser tab -> + -> Shell. A running daemon is
// the hard requirement; shell grant failure is reported precisely.
func (d *Adapter) setupMagisk(ctx context.Context, inst *Instance) error {
	serial := inst.serial
	if d.MagiskAPK != "" {
		apk, err := os.Open(d.MagiskAPK)
		if err != nil {
			return err
		}
		err = inst.dev.Install(ctx, apk, true)
		apk.Close()
		if err != nil {
			return fmt.Errorf("android: install Magisk app: %w", err)
		}
		if out, _, _ := d.transport().Run(ctx, serial, "pm path com.topjohnwu.magisk"); !strings.Contains(out, "package:") {
			return fmt.Errorf("android: Magisk app not registered after install (pm path gave %q)", strings.TrimSpace(out))
		}
	}
	ver, _, err := d.transport().Run(ctx, serial, "pidof magiskd; /debug_ramdisk/magisk -v 2>/dev/null || magisk -v 2>/dev/null || echo NO_MAGISK")
	if err != nil {
		return err
	}
	if strings.Contains(ver, "NO_MAGISK") {
		return fmt.Errorf("android: Magisk daemon did not come up after patched boot (probe gave %q)", strings.TrimSpace(ver))
	}
	if err := d.grantShellRoot(ctx, inst); err != nil {
		return err
	}
	return nil
}

var boundsRe = regexp.MustCompile(`bounds="\[(\d+),(\d+)\]\[(\d+),(\d+)\]"`)

// uiDump returns the current accessibility tree XML.
func (d *Adapter) uiDump(ctx context.Context, serial string) (string, error) {
	if _, _, err := d.transport().Run(ctx, serial, "uiautomator dump /sdcard/craggy-ui.xml"); err != nil {
		return "", err
	}
	out, _, err := d.transport().Run(ctx, serial, "cat /sdcard/craggy-ui.xml")
	return out, err
}

// tapText taps the centre of the first node whose text contains want.
// Returns false when no such node is on screen.
func (d *Adapter) tapText(ctx context.Context, inst *Instance, dump, want string) bool {
	for _, node := range strings.Split(dump, "<node") {
		ti := strings.Index(node, `text="`)
		if ti < 0 {
			continue
		}
		rest := node[ti+len(`text="`):]
		end := strings.Index(rest, `"`)
		if end < 0 || !strings.Contains(rest[:end], want) {
			continue
		}
		m := boundsRe.FindStringSubmatch(node)
		if m == nil {
			continue
		}
		x1, _ := strconv.Atoi(m[1])
		y1, _ := strconv.Atoi(m[2])
		x2, _ := strconv.Atoi(m[3])
		y2, _ := strconv.Atoi(m[4])
		_ = inst.dev.Tap(ctx, (x1+x2)/2, (y1+y2)/2)
		return true
	}
	return false
}

// magiskTrace appends grant-flow diagnostics when CRAGGY_MAGISK_TRACE
// names a file (UI automation failures are otherwise invisible).
func magiskTrace(format string, args ...any) {
	path := os.Getenv("CRAGGY_MAGISK_TRACE")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	fmt.Fprintf(f, time.Now().Format("15:04:05")+" "+format+"\n", args...)
	f.Close()
}

// grantShellRoot drives the Magisk app's pre-grant flow (v31: no
// interactive prompt exists anymore) and verifies `su -c id` == uid=0.
func (d *Adapter) grantShellRoot(ctx context.Context, inst *Instance) error {
	serial := inst.serial
	rootWorks := func() bool {
		out, code, err := d.transport().Run(ctx, serial, "su -c id 2>/dev/null")
		return err == nil && code == 0 && strings.Contains(out, "uid=0")
	}
	if rootWorks() {
		return nil
	}
	// Launch the manager; tolerate the window manager still warming up.
	for i := 0; i < 10; i++ {
		_, _, _ = d.transport().Run(ctx, serial, "monkey -p com.topjohnwu.magisk -c android.intent.category.LAUNCHER 1")
		time.Sleep(2 * time.Second)
		if dump, err := d.uiDump(ctx, serial); err == nil && strings.Contains(dump, "com.topjohnwu.magisk") {
			break
		}
	}
	// Walk the first-run flow by named nodes until the Superuser tab is
	// reachable and Shell can be granted. Each round: re-dump, act on
	// the first known element present.
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		if rootWorks() {
			return nil
		}
		dump, err := d.uiDump(ctx, serial)
		if err != nil {
			time.Sleep(time.Second)
			continue
		}
		magiskTrace("screen: notifications=%v setup=%v grant=%v changelog=%v superuser=%v magiskpkg=%v len=%d",
			strings.Contains(dump, "send you notifications"), strings.Contains(dump, "Requires additional setup"),
			strings.Contains(dump, "Grant Superuser"), strings.Contains(dump, "Changelog"),
			strings.Contains(dump, `text="Superuser"`), strings.Contains(dump, "com.topjohnwu.magisk"), len(dump))
		// Additional setup reboots the guest; afterwards the manager is
		// no longer foreground and must be relaunched.
		if !strings.Contains(dump, "com.topjohnwu.magisk") {
			_, _, _ = d.transport().Run(ctx, serial, "monkey -p com.topjohnwu.magisk -c android.intent.category.LAUNCHER 1")
			time.Sleep(3 * time.Second)
			continue
		}
		switch {
		case strings.Contains(dump, "responding"):
			// First-boot ANR dialogs block all launches.
			d.tapText(ctx, inst, dump, "Wait")
		case strings.Contains(dump, "send you notifications"):
			d.tapText(ctx, inst, dump, "t allow")
		case strings.Contains(dump, "Requires additional setup"):
			d.tapText(ctx, inst, dump, "OK")
			time.Sleep(10 * time.Second)
		case strings.Contains(dump, "Grant Superuser"):
			d.tapText(ctx, inst, dump, "com.android.shell")
			time.Sleep(2 * time.Second)
		case strings.Contains(dump, "Changelog") || strings.Contains(dump, "This release brings"):
			_, _, _ = d.transport().Run(ctx, serial, "input keyevent 4")
		case strings.Contains(dump, `text="Superuser"`):
			// Main UI: prefer the + (grant) affordance when present;
			// otherwise open the Superuser tab.
			if !d.tapText(ctx, inst, dump, "Superuser") {
				time.Sleep(time.Second)
			}
			time.Sleep(2 * time.Second)
			if dump2, err := d.uiDump(ctx, serial); err == nil {
				// The grant FAB has no text: a small clickable square
				// bottom-right ABOVE the nav bar (nav items match a
				// looser filter and must be excluded by their row).
				bestY, tapX, tapY := -1, 0, 0
				for _, node := range strings.Split(dump2, "<node") {
					if !strings.Contains(node, "clickable=\"true\"") {
						continue
					}
					m := boundsRe.FindStringSubmatch(node)
					if m == nil {
						continue
					}
					x1, _ := strconv.Atoi(m[1])
					y1, _ := strconv.Atoi(m[2])
					x2, _ := strconv.Atoi(m[3])
					y2, _ := strconv.Atoi(m[4])
					if x1 > 700 && y2 <= 2170 && x2-x1 < 250 && y2-y1 < 250 && y2-y1 > 50 && y1 > bestY {
						bestY, tapX, tapY = y1, (x1+x2)/2, (y1+y2)/2
					}
				}
				if bestY >= 0 {
					magiskTrace("tapping FAB at %d,%d", tapX, tapY)
					_ = inst.dev.Tap(ctx, tapX, tapY)
				} else {
					magiskTrace("no FAB candidate on Superuser screen")
				}
			}
		default:
			// Unknown screen: record its first text node for diagnosis.
			if i := strings.Index(dump, `text="`); i >= 0 {
				rest := dump[i+6:]
				if j := strings.Index(rest, `"`); j >= 0 && j < 80 {
					magiskTrace("unknown screen, first text: %q", rest[:j])
				}
			}
			time.Sleep(time.Second)
		}
		time.Sleep(time.Second)
	}
	if rootWorks() {
		return nil
	}
	return fmt.Errorf("android: Magisk daemon is up but shell root could not be granted via the app flow; grant com.android.shell in the Magisk app's Superuser tab")
}
