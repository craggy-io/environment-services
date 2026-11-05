package androiddev

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
)

// WaitDevice waits until adb reports the serial as a device.
func WaitDevice(ctx context.Context, tr Transport, serial string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		devs, err := tr.Devices(ctx)
		if err == nil {
			for _, dev := range devs {
				if dev.Serial == serial && dev.State == "device" {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("android: device %s never came online", serial)
}

func (d *Device) waitDevice(ctx context.Context, timeout time.Duration) error {
	if d.cfg.WaitDevice != nil {
		return d.cfg.WaitDevice(ctx, d.cfg.Serial, timeout)
	}
	return WaitDevice(ctx, d.tr, d.cfg.Serial, timeout)
}

// RootADB restarts adbd as root and returns once a shell on the
// restarted daemon answers uid=0. adbd restarts asynchronously and
// the adb server keeps listing the old transport as "device" for a
// moment, so the device being online proves nothing: a shell started
// in that window lands on the dying daemon and returns no output at
// all. Only `id` reporting uid=0 does.
func RootADB(ctx context.Context, tr Transport, serial string, timeout time.Duration) error {
	msg, err := tr.Root(ctx, serial)
	if err != nil {
		return fmt.Errorf("android: adb root on %s: %w", serial, err)
	}
	if strings.Contains(msg, "cannot run as root") {
		return fmt.Errorf("android: adb root refused on %s: %s (production images need Magisk root)", serial, strings.TrimSpace(msg))
	}
	deadline := time.Now().Add(timeout)
	last := "no answer"
	for {
		tryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, _, err := tr.Run(tryCtx, serial, "id")
		cancel()
		switch {
		case err == nil && strings.Contains(out, "uid=0"):
			return nil
		case err != nil:
			last = err.Error()
		case strings.TrimSpace(out) != "":
			last = strings.TrimSpace(out)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("android: adb root on %s: no root shell within %s (last: %s)", serial, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// captureBeforeRepair is the supervisor's evidence hook: a guest-side
// bundle under the workspace for the manager's capture to cite, plus
// the owner's CaptureFn (recorder pin) when wired.
func (d *Device) captureBeforeRepair(f FailureInfo) string {
	_, _, _ = d.Shell(context.Background(),
		"logcat -d -t '01-01 00:00:00.000' > /data/local/tmp/work/pre-repair-"+f.Component+".logcat 2>/dev/null; "+
			"ls -t /data/tombstones 2>/dev/null | head -3 > /data/local/tmp/work/pre-repair-"+f.Component+".tombstones")
	if d.cfg.CaptureFn != nil {
		return d.cfg.CaptureFn(f)
	}
	return ""
}

// registerComponents builds the managed components (adb, root,
// frida, proxy-ca) for this device's configuration.
func (d *Device) registerComponents() {
	sup := d.sup
	sup.Register(Component{Name: "adb",
		Probe: func(c context.Context) error {
			out, code, err := d.Shell(c, "echo ok")
			if err != nil {
				return err
			}
			if code != 0 || !strings.Contains(out, "ok") {
				return fmt.Errorf("adb echo round-trip failed")
			}
			return nil
		},
		Repair: func(c context.Context) error {
			// Reconnect the transport; restart the server only if
			// every device is offline.
			devs, err := d.tr.Devices(c)
			allOffline := err != nil
			if err == nil && len(devs) > 0 {
				allOffline = true
				for _, dev := range devs {
					if dev.State == "device" {
						allOffline = false
					}
				}
			}
			if allOffline {
				_ = d.tr.KillServer(c)
				time.Sleep(time.Second)
				// Next request auto-starts the server.
				_, _ = d.tr.Version(c)
			}
			return d.waitDevice(c, time.Minute)
		},
	})

	if d.Rooted() {
		sup.Register(Component{Name: "root",
			Probe: func(c context.Context) error {
				out, _, err := d.RootShell(c, "id")
				if err != nil {
					return err
				}
				if !strings.Contains(out, "uid=0") {
					return fmt.Errorf("not root: %s", strings.TrimSpace(out))
				}
				return nil
			},
			Repair: func(c context.Context) error {
				if d.cfg.Root == envspec.RootADB {
					return RootADB(c, d.tr, d.cfg.Serial, time.Minute)
				}
				// Magisk: re-grant shell via su if present; never reboot.
				out, _, err := d.Shell(c, "su -c id")
				if err != nil {
					return err
				}
				if !strings.Contains(out, "uid=0") {
					return fmt.Errorf("magisk su not granting root: %s", strings.TrimSpace(out))
				}
				return nil
			},
		})
	}

	if d.cfg.Frida != nil && d.Rooted() {
		sup.Register(Component{Name: "frida",
			Probe:  func(c context.Context) error { return d.probeFrida(c) },
			Repair: func(c context.Context) error { return d.repairFrida(c) },
		})
	}

	if d.cfg.ProxyCA {
		sup.Register(Component{Name: "proxy-ca",
			Probe: func(c context.Context) error {
				if d.cfg.ProxyCAPEM == "" {
					return nil
				}
				hash := caSubjectHash(d.cfg.ProxyCAPEM)
				if hash == "" {
					return nil
				}
				out, _, err := d.Shell(c, "ls /system/etc/security/cacerts/"+hash+".0 /apex/com.android.conscrypt/cacerts/"+hash+".0 2>/dev/null")
				if err != nil {
					return err
				}
				if !strings.Contains(out, hash+".0") {
					return fmt.Errorf("proxy CA %s.0 missing from system store", hash)
				}
				return nil
			},
			Repair: func(c context.Context) error { return d.installProxyCA(c) },
		})
	}
}

// Probes exposes the components as core activity probes: Check is
// the supervisor's debounced probe, BeforeRepair captures evidence,
// Repair runs the supervisor's budget/suspect-gated repair. Core
// consults its MutationPolicy before invoking Repair; the
// supervisor consults its own policy (SetMutationPolicy) too, so
// the Ensure path is gated even without core.
func (d *Device) Probes() []adapter.Probe {
	names := d.sup.Names()
	out := make([]adapter.Probe, 0, len(names))
	for _, name := range names {
		name := name
		var capID string
		out = append(out, adapter.Probe{
			Name: name,
			// Interval zero: core's health interval governs cadence
			// (the supervisor's debounce is in probe count, not time).
			Check: func(ctx context.Context) (envspec.Condition, error) {
				return d.sup.Check(ctx, name)
			},
			BeforeRepair: func(ctx context.Context) error {
				capID = d.sup.CaptureBeforeRepair(name)
				return nil
			},
			Repair: func(ctx context.Context) error {
				return d.sup.RepairNow(ctx, name, capID)
			},
		})
	}
	return out
}

// Components reports managed-component health for Info.
func (d *Device) Components() []envspec.ComponentState { return d.sup.Statuses() }

// EnsureComponent probes (and repairs, evidence-first) a component
// before a dependent operation.
func (d *Device) EnsureComponent(ctx context.Context, name string) error {
	return d.sup.Ensure(ctx, name)
}

// Supervisor returns the component supervisor (tests/hooks).
func (d *Device) Supervisor() *Supervisor { return d.sup }

// SetMutationPolicy installs the experiment-hold gate for repairs
// this device initiates itself (EnsureComponent). Adapters call it
// when environments.Open wires Config.MutationPolicy and the env ID.
func (d *Device) SetMutationPolicy(p envspec.MutationPolicy, envID string) {
	d.sup.SetMutationPolicy(p, envID)
}
