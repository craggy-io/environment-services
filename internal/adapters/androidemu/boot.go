package androidemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/androiddev"
)

// Console ports are allocated in pairs from 5554–5584 (even = console).
const (
	portFirst = 5554
	portLast  = 5584
)

func (d *Adapter) allocPort(envID string) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ports == nil {
		d.ports = map[int]string{}
	}
	// Also avoid serials adb already reports.
	inUse := map[int]bool{}
	if devs, err := d.transport().Devices(context.Background()); err == nil {
		for _, dev := range devs {
			if strings.HasPrefix(dev.Serial, "emulator-") {
				if n, err := strconv.Atoi(strings.TrimPrefix(dev.Serial, "emulator-")); err == nil {
					inUse[n] = true
				}
			}
		}
	}
	for p := portFirst; p <= portLast; p += 2 {
		if d.ports[p] == "" && !inUse[p] {
			d.ports[p] = envID
			return p, nil
		}
	}
	return 0, fmt.Errorf("android: no free emulator console port in %d-%d", portFirst, portLast)
}

func (d *Adapter) freePort(p int) {
	d.mu.Lock()
	delete(d.ports, p)
	d.mu.Unlock()
}

func (d *Adapter) tuneAVDConfig(avdName string) {
	cfgPath := filepath.Join(d.avdHome(), avdName+".avd", "config.ini")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return
	}
	set := map[string]string{
		"hw.ramSize": "3072", "disk.dataPartition.size": "8192M",
		"hw.cpu.ncore": "2", "hw.gpu.enabled": "yes",
		"hw.gpu.mode":     "swiftshader_indirect",
		"showDeviceFrame": "no", "hw.keyboard": "yes",
	}
	lines := strings.Split(string(b), "\n")
	seen := map[string]bool{}
	for i, line := range lines {
		k, _, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		if v, ok := set[strings.TrimSpace(k)]; ok {
			lines[i] = strings.TrimSpace(k) + "=" + v
			seen[strings.TrimSpace(k)] = true
		}
	}
	for k, v := range set {
		if !seen[k] {
			lines = append(lines, k+"="+v)
		}
	}
	_ = os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")), 0o644)
}

type bootOptions struct {
	writable   bool // false = -read-only multi-instance
	noSnapshot bool // true = cold boot without quickboot load
	snapshot   string
	foreground bool // true = show the emulator window instead of -no-window
}

const quickbootDeviceWait = 2 * time.Minute

func deviceWaitLimit(bo bootOptions) time.Duration {
	if !bo.writable && !bo.noSnapshot {
		return quickbootDeviceWait
	}
	return 3 * time.Minute
}

// paramTrue reports whether a boolean driver param is set to "true".
func paramTrue(s envspec.Spec, key string) bool { return s.Params[key] == "true" }

// windowArgs is the emulator's display mode: headless unless the caller
// asked for a visible window (Params["avd.foreground"]).
func windowArgs(foreground bool) []string {
	if foreground {
		return nil
	}
	return []string{"-no-window"}
}

// bootEmulator launches one emulator process and waits for boot_completed.
func (d *Adapter) bootEmulator(ctx context.Context, envID, avdName string, s envspec.Spec, bo bootOptions, progress func(envspec.Progress)) (*Instance, error) {
	port, err := d.allocPort(envID)
	if err != nil {
		return nil, err
	}
	serial := fmt.Sprintf("emulator-%d", port)
	grpc := d.GRPCBase + (port-portFirst)/2
	args := []string{"-avd", avdName}
	args = append(args, windowArgs(bo.foreground)...)
	args = append(args,
		"-no-audio", "-no-boot-anim",
		"-gpu", "swiftshader_indirect",
		"-port", strconv.Itoa(port),
		"-grpc", strconv.Itoa(grpc),
		"-no-snapshot-save",
	)
	if !bo.writable {
		args = append(args, "-read-only")
	}
	// Magisk variants boot the patched ramdisk (see magisk.go).
	if rd := d.magiskRamdiskPath(avdName); pathExists(rd) {
		args = append(args, "-ramdisk", rd)
	}
	snap := bo.snapshot
	if snap == "" && !bo.noSnapshot {
		snap = "clean"
	}
	if bo.noSnapshot {
		args = append(args, "-no-snapshot-load")
	} else if snap != "" {
		args = append(args, "-snapshot", snap)
	}
	if s.Resources.MemoryMB > 0 {
		args = append(args, "-memory", strconv.FormatInt(s.Resources.MemoryMB, 10))
	}
	if s.Resources.CPUs > 0 {
		args = append(args, "-cores", strconv.Itoa(int(s.Resources.CPUs)))
	}
	if s.Params["net.tcpdump"] != "" {
		// Pin via Spec.Signals in the manager path (host-side file).
		args = append(args, "-tcpdump", s.Params["net.tcpdump"])
	}
	if progress != nil {
		progress(envspec.Progress{Op: "provision", Stage: "boot", EnvID: envID, Message: "emulator " + strings.Join(args, " ")})
	}
	cmd := osexec.Command(d.emulatorBin(), args...)
	cmd.Env = sdkEnv(d.SDK, d.avdHome())
	// Capture output so an emulator that refuses to start (AVD in use,
	// bad flags) fails in seconds with its own words, not after a
	// 3-minute adb timeout.
	var emuLog syncBuffer
	cmd.Stdout = &emuLog
	cmd.Stderr = &emuLog
	if err := cmd.Start(); err != nil {
		d.freePort(port)
		return nil, fmt.Errorf("android: launch emulator: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	o := s.Android
	inst := &Instance{ad: d, envID: envID, serial: serial, port: port, grpc: grpc,
		pid: cmd.Process.Pid, avd: avdName, spec: s, variant: o, writable: bo.writable}
	inst.wire()
	inst.saveCache()

	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	bootDone := make(chan error, 1)
	go func() { bootDone <- d.waitBootCompleted(waitCtx, inst, deviceWaitLimit(bo), progress) }()
	select {
	case err := <-bootDone:
		if err != nil {
			failure := d.bootFailure(inst, args, &emuLog, err)
			d.cleanupFailedBoot(inst)
			return nil, failure
		}
	case werr := <-exited:
		cancelWait()
		failure := d.bootFailure(inst, args, &emuLog, fmt.Errorf("android: emulator exited before boot completed: %v", werr))
		d.cleanupFailedBoot(inst)
		return nil, failure
	}
	return inst, nil
}

// syncBuffer is a goroutine-safe tail buffer for emulator output.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	const maxTail = 256 << 10
	length := len(p)
	if length >= maxTail {
		s.b.Reset()
		_, _ = s.b.Write(p[length-maxTail:])
		return length, nil
	}
	if excess := s.b.Len() + length - maxTail; excess > 0 {
		current := s.b.String()
		s.b.Reset()
		_, _ = s.b.WriteString(current[excess:])
	}
	_, _ = s.b.Write(p)
	return length, nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// heartbeatEvery is how often a long wait re-reports its stage: well
// inside the registry's 30s stalled threshold.
var heartbeatEvery = 10 * time.Second

// heartbeat reports p now and again every heartbeatEvery, with the time
// spent so far, until stop is called. A cold first boot waits minutes
// with nothing new to say; without this the job reads as stalled.
func heartbeat(progress func(envspec.Progress), p envspec.Progress) (stop func()) {
	if progress == nil {
		return func() {}
	}
	progress(p)
	done, exited := make(chan struct{}), make(chan struct{})
	start := time.Now()
	go func() {
		defer close(exited)
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				q := p
				q.Message = strings.TrimSpace(fmt.Sprintf("%s (%s elapsed)", p.Message, time.Since(start).Round(time.Second)))
				progress(q)
			}
		}
	}()
	return func() { close(done); <-exited }
}

func (d *Adapter) waitBootCompleted(ctx context.Context, inst *Instance, deviceTimeout time.Duration, progress func(envspec.Progress)) error {
	stop := heartbeat(progress, envspec.Progress{Op: "provision", Stage: "wait-device", EnvID: inst.envID,
		Message: "waiting for " + inst.serial + " to come online"})
	err := androiddev.WaitDevice(ctx, d.transport(), inst.serial, deviceTimeout)
	stop()
	if err != nil {
		return err
	}
	stop = heartbeat(progress, envspec.Progress{Op: "provision", Stage: "wait-boot-completed", EnvID: inst.envID,
		Message: "waiting for sys.boot_completed and the package manager"})
	defer stop()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		out, _, err := d.transport().Run(ctx, inst.serial, "getprop sys.boot_completed")
		if err == nil && strings.TrimSpace(out) == "1" {
			// Package manager answering is part of ready.
			if _, code, err := d.transport().Run(ctx, inst.serial, "pm list packages -f 2>/dev/null | head -1"); err == nil && code == 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("android: %s never finished booting", inst.serial)
}

// Create prepares the variant if needed, then quickboots a read-only
// instance from the base AVD's clean snapshot. It is rediscoverable:
// a retry for an env whose emulator is already running (port
// allocation or cache with a live process) re-opens it instead of
// booting a second one.
func (d *Adapter) Create(ctx context.Context, op adapter.OpContext, s envspec.Spec, progress func(envspec.Progress)) (adapter.Instance, error) {
	id := op.EnvID
	if inst, err := d.reopenLive(ctx, id); err == nil && inst != nil {
		return inst, nil
	}
	if err := d.Check(ctx); err != nil {
		return nil, adapter.BackendDown(err)
	}
	o := &envspec.AndroidOptions{API: 34, Image: envspec.GoogleAPIs, Root: envspec.RootADB}
	if s.Android != nil {
		cp := *s.Android
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
	s.Android = o
	key := VariantKey(o)
	if !d.cleanSnapshotExists(key) {
		prof := envspec.Profile{Name: s.Profile, Kind: envspec.Android, Android: o}
		if err := d.PrepareProfile(ctx, prof, progress); err != nil {
			return nil, fmt.Errorf("android: prepare variant: %w", err)
		}
	}
	report := func(stage, msg string) {
		if progress != nil {
			progress(envspec.Progress{Op: "provision", Stage: stage, EnvID: id, Message: msg})
		}
	}
	snapName := "clean"
	if v := s.Params["avd.snapshot"]; v != "" {
		snapName = v
	}
	report("boot", "quickboot from snapshot "+snapName)
	boot := bootOptions{snapshot: snapName, writable: paramTrue(s, "avd.writable"), foreground: paramTrue(s, "avd.foreground")}
	inst, err := bootWithRetry(ctx, boot, report, func() (*Instance, error) {
		return d.bootEmulator(ctx, id, key, s, boot, progress)
	})
	if err != nil {
		return nil, err
	}
	_, _, _ = d.transport().Run(ctx, inst.serial, "mkdir -p /data/local/tmp/work")

	// Components were wired with the device in bootEmulator.
	report("components", "verifying managed components")
	for _, name := range []string{"adb", "root", "frida", "proxy-ca"} {
		if err := inst.EnsureComponent(ctx, name); err != nil {
			// Recorded in component state; ready is not blocked on
			// optional instruments (degraded semantics).
			_ = err
		}
	}
	return inst, nil
}

func shouldRetryQuickboot(ctx context.Context, bo bootOptions, err error) bool {
	if err == nil || ctx.Err() != nil || bo.writable || bo.noSnapshot {
		return false
	}
	var wait *androiddev.DeviceWaitError
	if !errors.As(err, &wait) || wait.State != "offline" {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < quickbootDeviceWait/2 {
		return false
	}
	return true
}

func bootWithRetry(ctx context.Context, bo bootOptions, report func(string, string), launch func() (*Instance, error)) (*Instance, error) {
	inst, err := launch()
	if !shouldRetryQuickboot(ctx, bo, err) {
		return inst, err
	}
	if report != nil {
		report("boot-retry", "adb stayed offline after snapshot quickboot; retrying once from the same snapshot")
	}
	first := err
	inst, err = launch()
	if err != nil {
		return nil, fmt.Errorf("android: snapshot quickboot failed twice (first: %v; retry: %w)", first, err)
	}
	return inst, nil
}

// Open re-materialises an Instance from a Ref after restart. The
// Ref's serial (the DB record) identifies the emulator; the cache
// fills in spec/PID/AVD when present, and without it the AVD name
// and root mode are probed live. An emulator whose process is alive
// opens even if adb does not list it yet (a restarted adb server
// takes seconds to see it again): Observe then reports Unknown
// until adb catches up, never Absent.
func (d *Adapter) Open(ctx context.Context, op adapter.OpContext, ref adapter.Ref) (adapter.Instance, error) {
	inst, err := d.openFromRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	return inst, nil
}

func (d *Adapter) openFromRef(ctx context.Context, ref adapter.Ref) (*Instance, error) {
	serial := ref.External
	cache, _ := d.loadCache(ref.EnvID)
	if serial == "" && cache != nil {
		serial = cache.Serial
	}
	if serial == "" {
		return nil, fmt.Errorf("android: no serial recorded for %s", ref.EnvID)
	}
	inst := &Instance{ad: d, envID: ref.EnvID, serial: serial, port: serialPort(serial)}
	if cache != nil && cache.Serial == serial {
		inst.pid, inst.port, inst.grpc, inst.avd = cache.PID, cache.Port, cache.GRPC, cache.AVD
		inst.spec, inst.variant = cache.Spec, cache.Variant
		inst.writable = cache.Writable || cache.Spec.Params["avd.writable"] == "true"
	} else {
		// No (matching) cache: derive what can be derived live.
		base := d.GRPCBase
		if base == 0 {
			base = 8554
		}
		inst.grpc = base + (inst.port-portFirst)/2
		if out, err := d.emu(ctx, serial, "avd name"); err == nil {
			inst.avd = firstLine(out)
		}
		inst.variant = &envspec.AndroidOptions{Root: probeRootMode(ctx, d.transport(), serial)}
		if inst.variant.Root == envspec.RootNone {
			inst.variant = nil
		}
	}
	if inst.port > 0 {
		d.mu.Lock()
		if d.ports == nil {
			d.ports = map[int]string{}
		}
		d.ports[inst.port] = inst.envID
		d.mu.Unlock()
	}
	inst.wire()
	return inst, nil
}

// reopenLive returns the running instance for envID when one is
// already booted (Create rediscovery): a cache entry whose emulator
// process is alive. A cache entry whose emulator is gone is removed.
func (d *Adapter) reopenLive(ctx context.Context, envID string) (*Instance, error) {
	cache, err := d.loadCache(envID)
	if err != nil {
		return nil, err
	}
	if emulatorProcess(cache.PID, cache.Port) == nil {
		_ = os.Remove(d.cachePath(envID))
		return nil, fmt.Errorf("android: cached emulator for %s is gone", envID)
	}
	return d.openFromRef(ctx, adapter.Ref{EnvID: envID, External: cache.Serial})
}

// probeRootMode classifies a live device's root: adbd-as-root when a
// plain shell is uid 0, Magisk when only `su -c` is.
func probeRootMode(ctx context.Context, tr androiddev.Transport, serial string) envspec.RootMode {
	if out, _, err := tr.Run(ctx, serial, "id"); err == nil && strings.Contains(out, "uid=0") {
		return envspec.RootADB
	}
	if out, _, err := tr.Run(ctx, serial, "su -c id"); err == nil && strings.Contains(out, "uid=0") {
		return envspec.RootMagisk
	}
	return envspec.RootNone
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}
