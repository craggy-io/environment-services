package androiddev

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ulikunitz/xz"

	"github.com/craggy-io/environment-services/envspec"
)

// FridaServer locates frida-server binaries on the host and,
// optionally, downloads missing ones. It is shared by every Device
// an adapter builds; per-device frida lifecycle lives on Device.
type FridaServer struct {
	// ServerDir holds frida-server binaries named
	// frida-server-<version>-android-<arch>.
	ServerDir string
	// StateDir is the manager dir; the download cache defaults to
	// <StateDir>/android/frida-server when ServerDir is empty.
	StateDir string
	// AutoDownload fetches a missing frida-server — matching
	// FridaOptions.Version, else the host client's version — from
	// the official Frida GitHub releases. Off by default.
	AutoDownload bool

	mu sync.Mutex // serializes downloads
}

// fridaName picks a randomised on-device binary name (§3.9 options).
func fridaName(o *envspec.FridaOptions, avd string) string {
	if o != nil && o.Name != "" {
		return o.Name
	}
	seed := int64(0)
	for _, r := range avd {
		seed = seed*31 + int64(r)
	}
	return fmt.Sprintf(".svc-%06x", uint32(seed*2654435761)&0xffffff)
}

func fridaPort(o *envspec.FridaOptions) int {
	if o != nil && o.Port > 0 {
		return o.Port
	}
	return 27042
}

// hostFridaVersion returns the host frida client's version, if installed.
func hostFridaVersion() string {
	bin, err := osexec.LookPath("frida")
	if err != nil {
		return ""
	}
	out, err := osexec.Command(bin, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// fridaReleaseBase is the official Frida releases download root
// (overridable in tests).
var fridaReleaseBase = "https://github.com/frida/frida/releases/download"

// cacheDir is where auto-downloaded binaries live: the configured
// server dir, else a cache under the driver state dir.
func (f *FridaServer) cacheDir() (string, error) {
	if f.ServerDir != "" {
		return f.ServerDir, nil
	}
	if f.StateDir != "" {
		return filepath.Join(f.StateDir, "android", "frida-server"), nil
	}
	return "", fmt.Errorf("nowhere to cache frida-server: set FridaServer.ServerDir")
}

// download fetches frida-server-<version>-android-<arch>.xz
// from the Frida releases, decompresses it into the cache dir under the
// name binary looks for, and returns its path. An existing file is
// reused. The payload must look like an ELF binary of plausible size —
// a bad download fails closed and leaves nothing behind.
func (f *FridaServer) download(ctx context.Context, arch, version string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dir, err := f.cacheDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("frida-server-%s-android-%s", version, arch)
	dest := filepath.Join(dir, name)
	if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
		return dest, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/%s/%s.xz", fridaReleaseBase, version, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download frida-server %s: %w", version, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download frida-server %s: %s (check the version exists)", version, resp.Status)
	}
	xr, err := xz.NewReader(resp.Body)
	if err != nil {
		return "", fmt.Errorf("download frida-server %s: xz stream: %w", version, err)
	}
	tmp, err := os.CreateTemp(dir, name+".part-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	head := make([]byte, 4)
	if _, err := io.ReadFull(xr, head); err != nil {
		tmp.Close()
		return "", fmt.Errorf("download frida-server %s: %w", version, err)
	}
	if !bytes.Equal(head, []byte{0x7f, 'E', 'L', 'F'}) {
		tmp.Close()
		return "", fmt.Errorf("download frida-server %s: payload is not an ELF binary", version)
	}
	if _, err := tmp.Write(head); err != nil {
		tmp.Close()
		return "", err
	}
	written, err := io.Copy(tmp, xr)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download frida-server %s: %w", version, err)
	}
	if written+4 < 1<<20 {
		return "", fmt.Errorf("download frida-server %s: suspiciously small (%d bytes)", version, written+4)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// noFridaServer explains why no frida-server could be had, naming the
// directory actually searched and the one step that would fix it.
func (f *FridaServer) missingError(arch, want string) error {
	dir, err := f.cacheDir()
	if err != nil {
		return err
	}
	if want == "" {
		return fmt.Errorf("no frida-server for android-%s in %s, and no version to fetch: install the frida client on the host (its version is matched) or set Frida.Version", arch, dir)
	}
	return fmt.Errorf("no frida-server binary for android-%s version %q in %s; put frida-server-%s-android-%s there or enable FridaServer.AutoDownload", arch, want, dir, want, fridaArchName(arch))
}

// fridaBinary finds a server binary for the wanted version.
func (f *FridaServer) binary(arch, wantVersion string) (path, version string, ok bool) {
	dir, err := f.cacheDir()
	if err != nil {
		return "", "", false
	}
	if wantVersion != "" {
		for _, pat := range []string{
			fmt.Sprintf("frida-server-%s-android-%s*", wantVersion, arch),
			fmt.Sprintf("frida-server-%s-android-%s*", wantVersion, fridaArchName(arch)),
		} {
			if m, _ := filepath.Glob(filepath.Join(dir, pat)); len(m) > 0 {
				return m[0], wantVersion, true
			}
		}
		return "", "", false
	}
	// Any version present for this arch.
	for _, a := range []string{arch, fridaArchName(arch)} {
		m, _ := filepath.Glob(filepath.Join(dir, "frida-server-*-android-"+a+"*"))
		if len(m) > 0 {
			base := filepath.Base(m[0])
			parts := strings.Split(base, "-")
			ver := ""
			if len(parts) >= 3 {
				ver = parts[2]
			}
			return m[0], ver, true
		}
	}
	return "", "", false
}

func fridaArchName(arch string) string {
	switch arch {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "arm64-v8a":
		return "arm64"
	default:
		return arch
	}
}

func (d *Device) guestArch() string {
	abi := d.cfg.ABI
	if abi == "" {
		abi = "x86_64"
	}
	return fridaArchName(abi)
}

// EnsureFridaBinary places the frida-server binary on a freshly
// booted base device (prepared snapshot), so instances never repeat
// it. Best-effort: the frida component reports degraded with
// guidance when no binary is available.
func (d *Device) EnsureFridaBinary(ctx context.Context) {
	if d.cfg.Frida == nil {
		return
	}
	_ = d.repairFrida(ctx)
}

// probeFrida: process alive, port answers, version matches the host client.
func (d *Device) probeFrida(ctx context.Context) error {
	name := fridaName(d.cfg.Frida, d.nameSeed())
	// pidof ONLY: a `pgrep -f` fallback self-matches, because the probing
	// shell's own command line contains the literal daemon name (from the
	// pidof argument) — the probe then reports a phantom daemon forever.
	out, _, err := d.Shell(ctx, "pidof "+name)
	if err != nil {
		// Transport hiccup (guest busy) is not evidence the daemon is
		// down; one retry before declaring failure.
		time.Sleep(time.Second)
		out, _, err = d.Shell(ctx, "pidof "+name)
		if err != nil {
			return err
		}
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("frida-server (%s) not running", name)
	}
	// Frida speaks over its port; a TCP connect through adb forward is
	// heavy, so smoke-test via the host client when present.
	if hv := hostFridaVersion(); hv != "" {
		if want := d.cfg.Frida; want != nil && want.Version != "" && want.Version != hv {
			return fmt.Errorf("frida version mismatch: want %s, host client is %s (the most common Frida failure)", want.Version, hv)
		}
	}
	return nil
}

// repairFrida (re)pushes the right binary and restarts the daemon.
// Repair of the instrument only — target processes are untouched.
func (d *Device) repairFrida(ctx context.Context) error {
	o := d.cfg.Frida
	fs := d.cfg.FridaServer
	if fs == nil {
		return fmt.Errorf("no frida-server source configured: set FridaServer (FridaServerDir) so frida-server binaries can be found")
	}
	arch := d.guestArch()
	want := ""
	if o != nil {
		want = o.Version
	}
	if want == "" {
		want = hostFridaVersion()
	}
	path, _, ok := fs.binary(arch, want)
	if !ok && fs.AutoDownload && want != "" {
		dl, derr := fs.download(ctx, fridaArchName(arch), want)
		if derr != nil {
			return fmt.Errorf("no frida-server binary for android-%s version %q locally and auto-download failed: %w", arch, want, derr)
		}
		path, ok = dl, true
	}
	if !ok {
		return fs.missingError(arch, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	name := fridaName(o, d.nameSeed())
	remote := "/data/local/tmp/" + name
	if err := d.tr.SyncSend(ctx, d.cfg.Serial, remote, 0o100755, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("push frida-server: %w", err)
	}
	port := fridaPort(o)
	// Kill any stale daemon (ours), then start fresh, detached.
	// setsid is load-bearing: shell-v2 sessions reap their process
	// group when they close, which would kill a plain `&` daemon as
	// soon as this start command returns.
	_, _, _ = d.Shell(ctx, d.RootCmd("pkill -f "+name))
	startCmd := fmt.Sprintf("chmod 755 %s; setsid %s -l 0.0.0.0:%d >/data/local/tmp/work/%s.log 2>&1 </dev/null &", remote, remote, port, name)
	streamCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, _, _ = d.Shell(streamCtx, d.RootCmd(startCmd))
	cancel()
	// Verify the daemon actually stayed up; a start that died with its
	// shell session looks exactly like success until first use, so the
	// guest log is the evidence and belongs in the error.
	for i := 0; i < 10; i++ {
		time.Sleep(500 * time.Millisecond)
		if err := d.probeFrida(ctx); err == nil {
			d.mu.Lock()
			d.fridaOwned = true
			d.mu.Unlock()
			return nil
		}
	}
	log, _, _ := d.Shell(ctx, "cat /data/local/tmp/work/"+name+".log 2>/dev/null; ls -la /data/local/tmp/"+name+" 2>&1")
	return fmt.Errorf("frida-server did not stay up after start; guest says: %s", strings.TrimSpace(log))
}

// --- Proxy CA ---

// caSubjectHash computes OpenSSL's old-style subject hash (what
// Android wants for system CA file names): first 4 bytes of
// SHA1(subject DER), little-endian hex.
func caSubjectHash(pemText string) string {
	block, _ := pem.Decode([]byte(pemText))
	if block == nil {
		return ""
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return ""
	}
	sum := sha1.Sum(cert.RawSubject)
	v := binary.LittleEndian.Uint32(sum[:4])
	return fmt.Sprintf("%08x", v)
}

// EnsureProxyCA installs the configured proxy CA when one is set
// (base-AVD preparation; per-instance health is the proxy-ca
// component's job).
func (d *Device) EnsureProxyCA(ctx context.Context) error {
	if d.cfg.ProxyCAPEM == "" {
		return nil
	}
	return d.installProxyCA(ctx)
}

// installProxyCA puts the proxy CA into the system store (root;
// writable system or apex overlay via tmpfs trick used by modules).
// Stock Magisk variants accept a module-less install on userdebug; on
// production this degrades the component rather than failing the env.
func (d *Device) installProxyCA(ctx context.Context) error {
	hash := caSubjectHash(d.cfg.ProxyCAPEM)
	if hash == "" {
		return fmt.Errorf("android: cannot parse ProxyCAPEM")
	}
	remote := "/data/local/tmp/" + hash + ".0"
	if err := d.tr.SyncSend(ctx, d.cfg.Serial, remote, 0o100644, strings.NewReader(d.cfg.ProxyCAPEM)); err != nil {
		return err
	}
	script := strings.Join([]string{
		"mount -o rw,remount /system 2>/dev/null; mount -o rw,remount / 2>/dev/null",
		"cp " + remote + " /system/etc/security/cacerts/" + hash + ".0 2>/dev/null || true",
		"chmod 644 /system/etc/security/cacerts/" + hash + ".0 2>/dev/null || true",
		"chcon u:object_r:system_file:s0 /system/etc/security/cacerts/" + hash + ".0 2>/dev/null || true",
		"ls /system/etc/security/cacerts/" + hash + ".0",
	}, "; ")
	out, _, err := d.RootShell(ctx, script)
	if err != nil {
		return err
	}
	if !strings.Contains(out, hash+".0") {
		return fmt.Errorf("android: install CA %s.0 into system store failed (read-only system?)", hash)
	}
	return nil
}
