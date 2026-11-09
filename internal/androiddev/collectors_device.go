package androiddev

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// --- android.logcat@1: binary logcat (-B) parsed in Go ---

// parseLogcatBinary reads Android logger_entry frames:
// u16 payloadLen, u16 hdrLen(=28), i32 pid, i32 tid, u32 sec, u32 nsec,
// u32 logID, u32 uid, then payload: priority byte, tag\0, message\0.
func parseLogcatBinary(r io.Reader, fn func(envspec.LogLine, time.Time)) error {
	br := bufio.NewReader(r)
	hdr := make([]byte, 28)
	for {
		if _, err := io.ReadFull(br, hdr[:4]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		payloadLen := int(binary.LittleEndian.Uint16(hdr[0:2]))
		hdrLen := int(binary.LittleEndian.Uint16(hdr[2:4]))
		if hdrLen < 28 || hdrLen > 256 {
			return fmt.Errorf("logcat -B: bad header length %d", hdrLen)
		}
		if _, err := io.ReadFull(br, hdr[4:hdrLen]); err != nil {
			return err
		}
		pid := int(int32(binary.LittleEndian.Uint32(hdr[4:8])))
		tid := int(int32(binary.LittleEndian.Uint32(hdr[8:12])))
		sec := binary.LittleEndian.Uint32(hdr[12:16])
		nsec := binary.LittleEndian.Uint32(hdr[16:20])
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return err
		}
		if len(payload) < 2 {
			continue
		}
		prio := priorityName(payload[0])
		rest := payload[1:]
		tagEnd := bytes.IndexByte(rest, 0)
		if tagEnd < 0 {
			continue
		}
		tag := string(rest[:tagEnd])
		msg := strings.TrimRight(string(rest[tagEnd+1:]), "\x00\n")
		fn(envspec.LogLine{Text: msg, Tag: tag, Priority: prio, PID: pid, TID: tid},
			time.Unix(int64(sec), int64(nsec)))
	}
}

func priorityName(b byte) string {
	switch b {
	case 2:
		return "VERBOSE"
	case 3:
		return "DEBUG"
	case 4:
		return "INFO"
	case 5:
		return "WARN"
	case 6:
		return "ERROR"
	case 7:
		return "FATAL"
	default:
		return fmt.Sprintf("P%d", b)
	}
}

// Logcat streams android.logcat@1 via logcat -B over exec:.
type logcatCollector struct{ dev *Device }

func (l *logcatCollector) Name() string { return "android.logcat" }
func (l *logcatCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamAndroidLogcat, Description: "Android logcat (binary format parsed in Go)", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.LogLine{})}}
}
func (l *logcatCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(envspec.StreamAndroidLogcat, "android")
	}
	return nil
}
func (l *logcatCollector) Params() *jsonschema.Schema {
	return objSchema(map[string]*jsonschema.Schema{"buffer": strSchema(), "filter": strSchema()})
}
func (l *logcatCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := l.dev
	var p struct {
		Buffer string `json:"buffer"`
		Filter string `json:"filter"`
	}
	_ = json.Unmarshal(params, &p)
	// Device→host clock offset for timestamps.
	offset := sampleClockOffset(ctx, e)
	client, serial := e.tr, e.cfg.Serial
	args := "logcat -B"
	if p.Buffer != "" {
		args += " -b " + p.Buffer
	}
	if p.Filter != "" {
		args += " " + p.Filter
	}
	// Run once: when the stream ends while ctx lives, that is the
	// failure core restarts from (no collector-local retry loop).
	rc, closeFn, err := client.ExecOut(ctx, serial, args, nil)
	if err != nil {
		return fmt.Errorf("android.logcat: %w", err)
	}
	defer closeFn()
	err = parseLogcatBinary(rc, func(line envspec.LogLine, ts time.Time) {
		out.Emit(envspec.SignalEvent{Stream: envspec.StreamAndroidLogcat, Time: ts.Add(offset), Data: line})
	})
	if ctx.Err() != nil {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("logcat stream ended")
	}
	return fmt.Errorf("android.logcat: %w", err)
}

func sampleClockOffset(ctx context.Context, e *Device) time.Duration {
	client, serial := e.tr, e.cfg.Serial
	before := time.Now()
	out, _, err := client.Run(ctx, serial, "date +%s")
	after := time.Now()
	if err != nil {
		return 0
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0
	}
	mid := before.Add(after.Sub(before) / 2)
	return mid.Sub(time.Unix(sec, 0))
}

// --- android.proc@1 ---

// AndroidProc polls `ps -A` at 1s (rooted ftrace is the exact path; the
// poll works everywhere and feeds package-start correlation).
type procCollector struct {
	dev      *Device
	Interval time.Duration
}

func (a *procCollector) Name() string { return "android.proc" }
func (a *procCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamAndroidProc, Description: "Guest process start/exit", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.ProcEvent{})}}
}
func (a *procCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(envspec.StreamAndroidProc, "android")
	}
	return nil
}
func (a *procCollector) Params() *jsonschema.Schema { return nil }
func (a *procCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := a.dev
	iv := a.Interval
	if iv <= 0 {
		iv = time.Second
	}
	noteStart := e
	var prev map[int]string
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		if cur := psSnapshot(ctx, e); cur != nil {
			if prev != nil {
				for pid, name := range cur {
					if _, ok := prev[pid]; !ok {
						out.Emit(envspec.SignalEvent{Stream: envspec.StreamAndroidProc, Data: envspec.ProcEvent{Event: "start", PID: pid, Name: name, Package: packageOf(name)}})
						if noteStart != nil {
							if pkg := packageOf(name); pkg != "" {
								noteStart.NotePackageStart(pkg, time.Now())
							}
						}
					}
				}
				for pid, name := range prev {
					if _, ok := cur[pid]; !ok {
						out.Emit(envspec.SignalEvent{Stream: envspec.StreamAndroidProc, Data: envspec.ProcEvent{Event: "exit", PID: pid, Name: name, Package: packageOf(name)}})
					}
				}
			}
			prev = cur
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// packageOf returns the package an app process name belongs to: its
// first word with any ":subprocess" suffix stripped. Names without a dot
// and paths (native binaries such as /vendor/bin/hw/android.hardware.x)
// are not packages.
func packageOf(procName string) string {
	f := strings.Fields(procName)
	if len(f) == 0 || !strings.Contains(f[0], ".") || strings.Contains(f[0], "/") {
		return ""
	}
	pkg, _, _ := strings.Cut(f[0], ":")
	return pkg
}

func psSnapshot(ctx context.Context, e *Device) map[int]string {
	proc, err := e.execGuest(ctx, guestCmd{Argv: []string{"ps", "-A", "-o", "PID,NAME"}})
	if err != nil {
		return nil
	}
	out := map[int]string{}
	_ = readLines(ctx, proc.Stdout, func(line string) {
		f := strings.Fields(line)
		if len(f) >= 2 {
			if pid, err := strconv.Atoi(f[0]); err == nil {
				out[pid] = f[len(f)-1]
			}
		}
	})
	_, _ = proc.Wait()
	if len(out) == 0 {
		return nil
	}
	return out
}

// --- android.packages@1 ---

// AndroidPackages diffs pm list at 2s.
type packagesCollector struct {
	dev      *Device
	Interval time.Duration
}

func (a *packagesCollector) Name() string { return "android.packages" }
func (a *packagesCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: envspec.StreamAndroidPackage, Description: "Package installs and removals", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.PackageEvent{})}}
}
func (a *packagesCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(envspec.StreamAndroidPackage, "android")
	}
	return nil
}
func (a *packagesCollector) Params() *jsonschema.Schema { return nil }
func (a *packagesCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := a.dev
	iv := a.Interval
	if iv <= 0 {
		iv = 2 * time.Second
	}
	var prev map[string]bool
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		cur := packageSet(ctx, e)
		if cur != nil {
			if prev != nil {
				for p := range cur {
					if !prev[p] {
						out.Emit(envspec.SignalEvent{Stream: envspec.StreamAndroidPackage, Data: envspec.PackageEvent{Event: "installed", Package: p}})
					}
				}
				for p := range prev {
					if !cur[p] {
						out.Emit(envspec.SignalEvent{Stream: envspec.StreamAndroidPackage, Data: envspec.PackageEvent{Event: "removed", Package: p}})
					}
				}
			}
			prev = cur
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func packageSet(ctx context.Context, e *Device) map[string]bool {
	proc, err := e.execGuest(ctx, guestCmd{Argv: []string{"pm", "list", "packages"}})
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	_ = readLines(ctx, proc.Stdout, func(line string) {
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "package:"))
		if p != "" {
			out[p] = true
		}
	})
	_, _ = proc.Wait()
	return out
}

// --- android.crash@2 ---

const crashStream = envspec.StreamAndroidCrash

// MaxTombstoneBytes caps the bytes retained per tombstone (the platform
// parser cap). A larger file is retained as a prefix and marked Truncated.
const MaxTombstoneBytes = 1 << 20 // 1 MiB

const (
	tombstoneDir = "/data/tombstones"
	bootIDPath   = "/proc/sys/kernel/random/boot_id"
)

// AndroidCrash reads the crash buffer and acquires tombstones. The first
// successful listing of /data/tombstones is the baseline: those files are
// reported as preexisting (metadata only). After that, a path that is new
// or whose (size, mtime) changed is read in full as root and emitted with
// its exact bytes and digest, unless the digest equals the one last
// emitted for that path. Failures are events, never empty content.
// Protobuf tombstones (*.pb) are skipped: tombstoned writes the text
// tombstone of the same crash beside each one.
type crashCollector struct {
	dev      *Device
	Interval time.Duration
}

func (a *crashCollector) Name() string { return "android.crash" }
func (a *crashCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: crashStream, Description: "App crashes, ANRs and tombstones with exact bytes and digests (reported, never repaired)", Cost: "low",
		Schema: signalcore.SchemaFor(envspec.CrashEvent{})}}
}
func (a *crashCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(crashStream, "android")
	}
	return nil
}
func (a *crashCollector) Params() *jsonschema.Schema { return nil }
func (a *crashCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := a.dev
	iv := a.Interval
	if iv <= 0 {
		iv = 5 * time.Second
	}
	w := &tombstoneWatch{e: e, out: out, files: map[string]tombstoneState{}, errs: map[string]string{}}
	w.cycle(ctx)
	// Crash buffer (text form is fine here; the logcat collector owns -B).
	// If it dies while ctx lives, the run fails and core restarts the
	// collector (no collector-local retry loop).
	bufErr := make(chan error, 1)
	go func() {
		bufErr <- guestExecLines(ctx, e, "logcat -b crash -v brief", func(line string) {
			ev := parseCrashLine(line)
			ev.BootID = w.bootID()
			w.emit(ev)
		})
	}()
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-bufErr:
			if err == nil {
				err = fmt.Errorf("crash logcat buffer ended")
			}
			return err
		case <-t.C:
			w.cycle(ctx)
		}
	}
}

// tombstoneWatch is AndroidCrash's per-run state. cycle runs on one
// goroutine; bootID is also read by the crash-buffer goroutine.
type tombstoneWatch struct {
	e         *Device
	out       signalcore.Emitter
	boot      atomic.Value // string: boot id read by the latest cycle
	baselined bool
	files     map[string]tombstoneState
	errs      map[string]string // collector_error text last emitted, by failing step
}

// tombstoneState is what was last seen and last emitted for one path.
type tombstoneState struct {
	size, mtime int64
	sha         string // last emitted digest
	readErr     string // last emitted read error at this (size, mtime)
}

func (w *tombstoneWatch) bootID() string {
	s, _ := w.boot.Load().(string)
	return s
}

func (w *tombstoneWatch) emit(ev envspec.CrashEvent) {
	w.out.Emit(envspec.SignalEvent{Stream: crashStream, Data: ev})
}

// cycle is one poll: boot id, listing, then baseline or diff.
func (w *tombstoneWatch) cycle(ctx context.Context) {
	boot, err := readBootID(ctx, w.e)
	w.boot.Store(boot)
	w.report("boot id", err)
	files, err := statTombstones(ctx, w.e)
	w.report("list "+tombstoneDir, err)
	if err != nil {
		return
	}
	if !w.baselined {
		w.baseline(files)
		return
	}
	w.diff(ctx, files)
}

// report emits a collector_error when step fails with a new error text;
// success clears it so a recurrence is reported again.
func (w *tombstoneWatch) report(step string, err error) {
	if err == nil {
		delete(w.errs, step)
		return
	}
	msg := step + ": " + err.Error()
	if w.errs[step] == msg {
		return
	}
	w.errs[step] = msg
	w.emit(envspec.CrashEvent{Kind: "collector_error", Error: msg, BootID: w.bootID()})
}

func (w *tombstoneWatch) baseline(files []envspec.TombstoneFile) {
	for _, f := range files {
		w.files[f.Path] = tombstoneState{size: f.Size, mtime: f.MTime}
		f.Preexisting = true
		w.emitTombstone(f)
	}
	w.baselined = true
}

func (w *tombstoneWatch) diff(ctx context.Context, files []envspec.TombstoneFile) {
	present := map[string]bool{}
	for _, f := range files {
		present[f.Path] = true
		prev, seen := w.files[f.Path]
		sameMeta := seen && prev.size == f.Size && prev.mtime == f.MTime
		if sameMeta && prev.readErr == "" {
			continue
		}
		tf := acquireTombstone(ctx, w.e, f)
		next := tombstoneState{size: f.Size, mtime: f.MTime, sha: prev.sha, readErr: tf.ReadError}
		if tf.ReadError == "" {
			next.sha = tf.SHA256
		}
		w.files[f.Path] = next
		switch {
		case tf.ReadError != "" && sameMeta && prev.readErr == tf.ReadError:
			continue // same failure on the same file: already reported
		case tf.ReadError == "" && seen && prev.sha == tf.SHA256:
			continue // same bytes already emitted for this path
		case tf.ReadError == "":
			resolvePackageCode(ctx, w.e, &tf)
		}
		w.emitTombstone(tf)
	}
	for p := range w.files {
		if !present[p] {
			delete(w.files, p)
		}
	}
}

func (w *tombstoneWatch) emitTombstone(tf envspec.TombstoneFile) {
	ev := envspec.CrashEvent{Kind: "tombstone", Text: tf.Path, BootID: w.bootID(), Tombstone: &tf}
	if len(tf.Bytes) > 0 {
		ev.Process, ev.PID, ev.Signal = parseTombstone(tf.Bytes)
	}
	w.emit(ev)
}

// acquireTombstone reads one tombstone as root, keeping the exact bytes,
// and names the package of the crashing process.
func acquireTombstone(ctx context.Context, e *Device, f envspec.TombstoneFile) envspec.TombstoneFile {
	b, truncated, err := readGuestFile(ctx, e, f.Path, MaxTombstoneBytes)
	if err != nil {
		f.ReadError = err.Error()
		return f
	}
	sum := sha256.Sum256(b)
	f.Bytes, f.Truncated, f.SHA256 = b, truncated, hex.EncodeToString(sum[:])
	process, _, _ := parseTombstone(b)
	f.Package = packageOf(process)
	return f
}

// resolvePackageCode records the installed base.apk path and digest of
// the crashing package, or why they could not be resolved.
func resolvePackageCode(ctx context.Context, e *Device, tf *envspec.TombstoneFile) {
	if tf.Package == "" {
		return
	}
	path, err := packageCodePath(ctx, e, tf.Package)
	if err == nil {
		tf.PackageCodePath = path
		tf.PackageCodeSHA256, err = guestSHA256(ctx, e, path)
	}
	if err != nil {
		tf.PackageCodeError = err.Error()
	}
}

// packageCodePath returns the base.apk `pm path` reports (else its first
// path).
func packageCodePath(ctx context.Context, e *Device, pkg string) (string, error) {
	b, _, err := guestOutput(ctx, e, guestCmd{Argv: []string{"pm", "path", pkg}}, 64<<10)
	if err != nil {
		return "", fmt.Errorf("pm path %s: %w", pkg, err)
	}
	var first string
	for _, line := range strings.Split(string(b), "\n") {
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "package:"))
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/base.apk") {
			return p, nil
		}
		if first == "" {
			first = p
		}
	}
	if first == "" {
		return "", fmt.Errorf("pm path %s: no code path in output", pkg)
	}
	return first, nil
}

// guestSHA256 hashes a guest file device-side as root.
func guestSHA256(ctx context.Context, e *Device, path string) (string, error) {
	b, _, err := guestOutput(ctx, e, guestCmd{Argv: []string{"sha256sum", path}, User: "root"}, 4<<10)
	if err != nil {
		return "", fmt.Errorf("sha256sum %s: %w", path, err)
	}
	f := strings.Fields(string(b))
	if len(f) > 0 && len(f[0]) == 64 {
		sum := strings.ToLower(f[0])
		if _, err := hex.DecodeString(sum); err == nil {
			return sum, nil
		}
	}
	return "", fmt.Errorf("sha256sum %s: unexpected output %q", path, strings.TrimSpace(string(b)))
}

func readBootID(ctx context.Context, e *Device) (string, error) {
	b, _, err := readGuestFile(ctx, e, bootIDPath, 256)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("%s: empty", bootIDPath)
	}
	return id, nil
}

// statTombstonesScript prints "size mtime path" per regular non-.pb file
// in the tombstone dir. An empty dir prints nothing and exits 0; a missing
// or unreadable dir fails with a message on stderr.
const statTombstonesScript = `d=` + tombstoneDir + `; ` +
	`[ -d "$d" ] || { echo "$d: no such directory" >&2; exit 2; }; ` +
	`{ [ -r "$d" ] && [ -x "$d" ]; } || { echo "$d: permission denied" >&2; exit 13; }; ` +
	`set --; for f in "$d"/*; do case "$f" in *.pb) continue ;; esac; [ -f "$f" ] && set -- "$@" "$f"; done; ` +
	`[ $# -eq 0 ] || exec stat -c '%s %Y %n' "$@"`

// statTombstones lists the tombstone dir as root with per-file metadata.
func statTombstones(ctx context.Context, e *Device) ([]envspec.TombstoneFile, error) {
	b, _, err := guestOutput(ctx, e, guestCmd{Argv: []string{"sh", "-c", statTombstonesScript}, User: "root"}, 1<<20)
	if err != nil {
		return nil, err
	}
	var files []envspec.TombstoneFile
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, " ", 3)
		if len(f) != 3 {
			return nil, fmt.Errorf("stat: unparsable line %q", line)
		}
		tf := envspec.TombstoneFile{Path: f[2], Size: -1}
		if n, err := strconv.ParseInt(f[0], 10, 64); err == nil {
			tf.Size = n
		}
		if n, err := strconv.ParseInt(f[1], 10, 64); err == nil {
			tf.MTime = n
		}
		files = append(files, tf)
	}
	return files, nil
}

func parseCrashLine(line string) envspec.CrashEvent {
	ev := envspec.CrashEvent{Kind: "crash", Text: strings.TrimSpace(line), Signal: signalName(line)}
	low := strings.ToLower(line)
	if strings.Contains(low, "anr in") || strings.Contains(low, "not responding") {
		ev.Kind = "anr"
	}
	// "FATAL EXCEPTION: main Process: com.x, PID: 1234"
	if i := strings.Index(line, "Process:"); i >= 0 {
		rest := strings.TrimSpace(line[i+len("Process:"):])
		if j := strings.Index(rest, ","); j >= 0 {
			ev.Process = strings.TrimSpace(rest[:j])
		}
	}
	if i := strings.Index(line, "PID:"); i >= 0 {
		f := strings.Fields(line[i+4:])
		if len(f) > 0 {
			ev.PID, _ = strconv.Atoi(strings.Trim(f[0], ","))
		}
	}
	return ev
}

// parseTombstone reads the crashing process (Cmdline:, else the
// ">>> name <<<" of the first pid line), its pid and the signal.
func parseTombstone(b []byte) (process string, pid int, sig string) {
	for rest := b; len(rest) > 0 && (process == "" || pid == 0 || sig == ""); {
		var raw []byte
		raw, rest, _ = bytes.Cut(rest, []byte("\n"))
		line := strings.TrimSpace(string(raw))
		switch {
		case strings.HasPrefix(line, "Cmdline:"):
			process = strings.TrimSpace(strings.TrimPrefix(line, "Cmdline:"))
		case pid == 0 && strings.HasPrefix(line, "pid:"):
			var name string
			pid, name = parsePIDLine(line)
			if process == "" {
				process = name
			}
		case sig == "" && strings.HasPrefix(line, "signal "):
			sig = signalName(line)
		}
	}
	return process, pid, sig
}

// parsePIDLine parses "pid: 1234, tid: 1250, name: X  >>> proc <<<".
func parsePIDLine(line string) (pid int, process string) {
	if f := strings.Fields(strings.TrimPrefix(line, "pid:")); len(f) > 0 {
		pid, _ = strconv.Atoi(strings.TrimSuffix(f[0], ","))
	}
	if _, rest, ok := strings.Cut(line, ">>>"); ok {
		name, _, _ := strings.Cut(rest, "<<<")
		process = strings.TrimSpace(name)
	}
	return pid, process
}

var knownSignals = []string{"SIGSEGV", "SIGABRT", "SIGBUS", "SIGILL", "SIGTRAP", "SIGFPE", "SIGSYS"}

// signalName returns the "(SIGxxx)" of a signal line, else a known
// signal name mentioned in it.
func signalName(line string) string {
	if _, rest, ok := strings.Cut(line, "(SIG"); ok {
		if name, _, ok := strings.Cut(rest, ")"); ok && name != "" {
			return "SIG" + name
		}
	}
	for _, sig := range knownSignals {
		if strings.Contains(line, sig) {
			return sig
		}
	}
	return ""
}

// readGuestFile reads up to max bytes of a guest file as root; truncated
// reports that the file was longer.
func readGuestFile(ctx context.Context, e *Device, path string, max int) ([]byte, bool, error) {
	b, truncated, err := guestOutput(ctx, e, guestCmd{Argv: []string{"cat", path}, User: "root"}, max)
	if err != nil {
		return nil, false, fmt.Errorf("cat %s: %w", path, err)
	}
	return b, truncated, nil
}

// guestOutput runs c to completion and returns up to max bytes of its
// stdout (truncated: there was more). An exec failure, a wait failure or
// a non-zero exit status is an error carrying the status and stderr.
func guestOutput(ctx context.Context, e *Device, c guestCmd, max int) ([]byte, bool, error) {
	proc, err := e.execGuest(ctx, c)
	if err != nil {
		return nil, false, fmt.Errorf("exec: %w", err)
	}
	stderr := collectStderr(proc.Stderr)
	out, rerr := readPrefix(proc.Stdout, max+1)
	code, werr := proc.Wait()
	if err := exitStatusError(code, werr, stderr()); err != nil {
		return nil, false, err
	}
	if rerr != nil {
		return nil, false, fmt.Errorf("read stdout: %w", rerr)
	}
	if len(out) > max {
		return out[:max], true, nil
	}
	return out, false, nil
}

// readPrefix keeps the first max bytes of r and drains the rest, so the
// writer never blocks. A nil r reads as empty.
func readPrefix(r io.Reader, max int) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, int64(max)))
	if err != nil {
		return b, err
	}
	_, err = io.Copy(io.Discard, r)
	return b, err
}

// collectStderr drains r in the background, keeping its first 4 KiB;
// the returned func waits for the drain and yields the trimmed text.
func collectStderr(r io.Reader) func() string {
	ch := make(chan []byte, 1)
	go func() {
		b, _ := readPrefix(r, 4<<10)
		ch <- b
	}()
	return func() string { return strings.TrimSpace(string(<-ch)) }
}

// exitStatusError is nil for a clean exit; otherwise it names the wait
// failure or the non-zero exit status, carrying stderr.
func exitStatusError(code int, werr error, stderr string) error {
	switch {
	case werr != nil:
		return withStderr(fmt.Errorf("wait: %w", werr), stderr)
	case code != 0:
		return withStderr(fmt.Errorf("exit status %d", code), stderr)
	}
	return nil
}

func withStderr(err error, stderr string) error {
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

// sampleAndroidResources: qemu numbers come from host metrics; the guest
// side is `top -b -n1` excerpt.
func sampleAndroidResources(ctx context.Context, e *Device) (envspec.ResourcesData, error) {
	b, _, err := guestOutput(ctx, e, guestCmd{Argv: []string{"top", "-b", "-n", "1", "-m", "5"}}, 16<<10)
	if err != nil {
		return envspec.ResourcesData{}, fmt.Errorf("top: %w", err)
	}
	return envspec.ResourcesData{Top: firstLines(string(b), 8)}, nil
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}
