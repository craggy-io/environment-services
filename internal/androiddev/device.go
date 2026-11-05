package androiddev

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Install pushes an APK stream and installs it with pm. Reinstall keeps
// data (-r), matching agent expectations for iterative analysis.
func (d *Device) Install(ctx context.Context, apk io.Reader, replace bool) error {
	return installAPK(ctx, d.tr, d.cfg.Serial, apk, replace)
}

type apkInstaller interface {
	SyncSend(context.Context, string, string, uint32, io.Reader) error
	Run(context.Context, string, string) (string, int, error)
}

func installAPK(ctx context.Context, client apkInstaller, serial string, apk io.Reader, replace bool) (result error) {
	data, err := io.ReadAll(apk)
	if err != nil {
		return err
	}
	tmp := "/data/local/tmp/craggy-install-" + rand.Text() + ".apk"
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		out, code, err := client.Run(cleanup, serial, "rm -f "+tmp)
		if err != nil || code != 0 {
			result = errors.Join(result, fmt.Errorf("android: remove staged APK (exit %d): %s: %v", code, strings.TrimSpace(out), err))
		}
	}()
	if err := client.SyncSend(ctx, serial, tmp, 0o100644, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("android: push apk: %w", err)
	}
	args := "pm install "
	if replace {
		args += "-r "
	}
	args += tmp + " 2>&1"
	out, code, err := client.Run(ctx, serial, args)
	if err != nil {
		return fmt.Errorf("android: pm install interrupted (exit %d): %s: %w", code, strings.TrimSpace(out), err)
	}
	if code != 0 || !strings.Contains(out, "Success") {
		return fmt.Errorf("android: pm install failed (%d): %s", code, strings.TrimSpace(out))
	}
	return nil
}

// Uninstall removes a package.
func (d *Device) Uninstall(ctx context.Context, pkg string) error {
	out, code, err := d.Shell(ctx, "pm uninstall "+pkg)
	if err != nil {
		return err
	}
	if code != 0 || !strings.Contains(out, "Success") {
		return fmt.Errorf("android: pm uninstall %s (%d): %s", pkg, code, strings.TrimSpace(out))
	}
	return nil
}

// Packages lists installed packages (optionally filtered by substring).
func (d *Device) Packages(ctx context.Context, filter string) ([]string, error) {
	out, _, err := d.Shell(ctx, "pm list packages")
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "package:"))
		if p != "" && (filter == "" || strings.Contains(p, filter)) {
			pkgs = append(pkgs, p)
		}
	}
	return pkgs, nil
}

// Screenshot returns a PNG via exec:screencap (binary-safe).
func (d *Device) Screenshot(ctx context.Context) ([]byte, error) {
	rc, closeFn, err := d.tr.ExecOut(ctx, d.cfg.Serial, "screencap -p", nil)
	if err != nil {
		return nil, fmt.Errorf("android: screencap: %w", err)
	}
	defer closeFn()
	png, err := io.ReadAll(rc)
	if err != nil {
		return nil, err
	}
	if len(png) < 8 || !bytes.HasPrefix(png, []byte{0x89, 'P', 'N', 'G'}) {
		return nil, fmt.Errorf("android: screencap returned %d non-PNG bytes", len(png))
	}
	return png, nil
}

func (d *Device) inputOK(_ string, code int, err error, what string) error {
	if err == nil && code != 0 {
		err = fmt.Errorf("input %s exit %d", what, code)
	}
	return err
}

// Tap sends a tap at x,y.
func (d *Device) Tap(ctx context.Context, x, y int) error {
	_, code, err := d.Shell(ctx, fmt.Sprintf("input tap %d %d", x, y))
	return d.inputOK("", code, err, "tap")
}

// Swipe sends a swipe gesture.
func (d *Device) Swipe(ctx context.Context, x1, y1, x2, y2, durationMS int) error {
	_, code, err := d.Shell(ctx, fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, durationMS))
	return d.inputOK("", code, err, "swipe")
}

// KeyEvent sends an Android keycode (e.g. 66 = ENTER, 4 = BACK).
func (d *Device) KeyEvent(ctx context.Context, code int) error {
	_, ec, err := d.Shell(ctx, fmt.Sprintf("input keyevent %d", code))
	return d.inputOK("", ec, err, "keyevent")
}

// Text types text into the focused field (input text, spaces as %s).
// Secret values reach it only inside a vault Lease.Use callback held
// by the caller (environments/envcap glue); this package never sees
// the vault.
func (d *Device) Text(ctx context.Context, text string) error {
	_, code, err := d.Shell(ctx, "input text "+inputQuote(text))
	return d.inputOK("", code, err, "text")
}

func inputQuote(s string) string {
	r := strings.NewReplacer(" ", "%s", "'", "\\'", "\"", "\\\"", "$", "\\$", "`", "\\`", "&", "\\&", ";", "\\;", "(", "\\(", ")", "\\)", "<", "\\<", ">", "\\>", "|", "\\|", "*", "\\*")
	return "'" + r.Replace(s) + "'"
}

// ForegroundPackage returns the focused app's package (supervisor input).
func (d *Device) ForegroundPackage(ctx context.Context) string {
	out, _, err := d.Shell(ctx, "dumpsys window windows 2>/dev/null | grep -m1 mCurrentFocus")
	if err != nil {
		return ""
	}
	// mCurrentFocus=Window{... u0 com.example/.MainActivity}
	fields := strings.Fields(out)
	for _, f := range fields {
		if strings.Contains(f, "/") {
			pkg, _, _ := strings.Cut(strings.Trim(f, "}"), "/")
			return pkg
		}
	}
	return ""
}

// StartApp launches a package's main activity via monkey.
func (d *Device) StartApp(ctx context.Context, pkg string) error {
	out, _, err := d.Shell(ctx, fmt.Sprintf("monkey -p %s -c android.intent.category.LAUNCHER 1", pkg))
	if err != nil {
		return err
	}
	if strings.Contains(out, "No activities found") {
		return fmt.Errorf("android: no launcher activity in %s", pkg)
	}
	return nil
}

// ForceStop stops a package. The supervisor never calls this during
// repairs (only agents/direct callers may).
func (d *Device) ForceStop(ctx context.Context, pkg string) error {
	_, _, err := d.Shell(ctx, "am force-stop "+pkg)
	return err
}

// Logcat streams raw logcat text (the android.logcat collector uses
// the binary path; this is a convenience for callers).
func (d *Device) Logcat(ctx context.Context, args string) (io.ReadCloser, func() error, error) {
	return d.tr.ExecOut(ctx, d.cfg.Serial, "logcat "+args, nil)
}
