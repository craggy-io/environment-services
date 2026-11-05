package androiddev

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// --- android.memory@1 ---

const memoryStream = envspec.StreamAndroidMemory

const (
	// MaxMemoryMapsBytes caps the retained /proc/<pid>/maps bytes.
	MaxMemoryMapsBytes = 4 << 20
	// MaxMemoryMappings caps the mappings read per cycle.
	MaxMemoryMappings = 512
	// MaxMemoryMappingBytes caps one mapping's retained bytes;
	// MaxMemoryTotalBytes caps one cycle's retained mapping bytes.
	MaxMemoryMappingBytes = 1 << 20
	MaxMemoryTotalBytes   = 8 << 20

	defaultMemoryMappingBytes = 256 << 10
	defaultMemoryTotalBytes   = 2 << 20
	memoryPageBytes           = 4096
)

// memReadScript copies whole pages of one mapping out of
// /proc/<pid>/mem and base64-encodes them on the device, so the
// bytes survive transports that are not binary-clean. Arguments:
// pid, skip pages, count pages. Mappings are page-aligned, so the
// page granularity loses nothing at the edges of the read.
const memReadScript = `dd if=/proc/$1/mem bs=4096 skip=$2 count=$3 2>/dev/null | base64`

func parseMemoryParams(raw json.RawMessage) (envspec.MemoryParams, error) {
	var p envspec.MemoryParams
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("%s params: %w", memoryStream, err)
		}
	}
	if !packageNamePatt.MatchString(p.Package) {
		return p, fmt.Errorf("%s params: %q is not a package name", memoryStream, p.Package)
	}
	if p.IntervalSeconds < 0 {
		return p, fmt.Errorf("%s params: interval_seconds cannot be negative", memoryStream)
	}
	if p.MaxMappingBytes == 0 {
		p.MaxMappingBytes = defaultMemoryMappingBytes
	}
	if p.MaxTotalBytes == 0 {
		p.MaxTotalBytes = defaultMemoryTotalBytes
	}
	if p.MaxMappingBytes < memoryPageBytes || p.MaxMappingBytes > MaxMemoryMappingBytes {
		return p, fmt.Errorf("%s params: max_mapping_bytes must be %d..%d", memoryStream, memoryPageBytes, MaxMemoryMappingBytes)
	}
	if p.MaxTotalBytes < p.MaxMappingBytes || p.MaxTotalBytes > MaxMemoryTotalBytes {
		return p, fmt.Errorf("%s params: max_total_bytes must be max_mapping_bytes..%d", memoryStream, MaxMemoryTotalBytes)
	}
	return p, nil
}

// AndroidMemory reads a package process's memory maps and mapping
// contents as root, receipt-grade: exact bytes, digests, boot id
// and process-instance identity. It is strictly read-only.
type memoryCollector struct {
	dev      *Device
	Interval time.Duration
}

func (a *memoryCollector) Name() string { return "android.memory" }
func (a *memoryCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: memoryStream, Description: "Read-only /proc maps and mapping-content snapshots of one package's main process, with exact bytes and digests", Cost: "medium",
		Schema: signalcore.SchemaFor(envspec.MemoryEvent{})}}
}
func (a *memoryCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(memoryStream, "android")
	}
	if !e.Root {
		return rootHint(memoryStream, e.Profile)
	}
	return nil
}
func (a *memoryCollector) Params() *jsonschema.Schema {
	return objSchema(map[string]*jsonschema.Schema{
		"package":           strSchema(),
		"interval_seconds":  {Type: "integer"},
		"max_mapping_bytes": {Type: "integer"},
		"max_total_bytes":   {Type: "integer"},
	}, "package")
}
func (a *memoryCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := a.dev
	p, err := parseMemoryParams(params)
	if err != nil {
		return err
	}
	iv := a.Interval
	if p.IntervalSeconds > 0 {
		iv = time.Duration(p.IntervalSeconds) * time.Second
	}
	if iv <= 0 {
		iv = 5 * time.Second
	}
	w := newMemoryWatch(e, out, p)
	w.cycle(ctx)
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.cycle(ctx)
		}
	}
}

// memoryWatch is AndroidMemory's per-run state; cycle runs on one
// goroutine.
type memoryWatch struct {
	e       *Device
	out     signalcore.Emitter
	p       envspec.MemoryParams
	boot    string
	process string // pid + "\x00" + start of the instance snapshots belong to
	last    map[string]stateEmitted
	errs    map[string]string
}

func newMemoryWatch(e *Device, out signalcore.Emitter, p envspec.MemoryParams) *memoryWatch {
	return &memoryWatch{e: e, out: out, p: p, last: map[string]stateEmitted{}, errs: map[string]string{}}
}

func (w *memoryWatch) emitAt(ev envspec.MemoryEvent, at time.Time) {
	ev.BootID = w.boot
	ev.Package = w.p.Package
	w.out.Emit(envspec.SignalEvent{Stream: memoryStream, Time: at, Data: ev})
}

func (w *memoryWatch) report(step string, err error) {
	if err == nil {
		delete(w.errs, step)
		return
	}
	msg := step + ": " + err.Error()
	if w.errs[step] == msg {
		return
	}
	w.errs[step] = msg
	w.emitAt(envspec.MemoryEvent{Kind: "collector_error", Error: msg}, time.Now())
}

// publish emits a part's read as a snapshot, or as unchanged when
// it repeats the last snapshot of that part.
func (w *memoryWatch) publish(key string, ev envspec.MemoryEvent) {
	prev, seen := w.last[key]
	if seen && ev.ReadError == "" && prev.sha == ev.SHA256 {
		ev.Kind = "unchanged"
		ev.Bytes = nil
		ev.Since = prev.when.UTC().Format(time.RFC3339Nano)
		w.emitAt(ev, time.Now())
		return
	}
	ev.Kind, ev.Baseline = "snapshot", !seen
	at := time.Now()
	w.last[key] = stateEmitted{key: ev.ReadError, sha: ev.SHA256, when: at}
	w.emitAt(ev, at)
}

// cycle resolves the process once and reads maps plus every
// readable private mapping.
func (w *memoryWatch) cycle(ctx context.Context) {
	boot, err := readBootID(ctx, w.e)
	w.boot = boot
	w.report("boot id", err)

	pid, start, err := w.resolveProcess(ctx)
	if err != nil {
		w.process = ""
		w.last = map[string]stateEmitted{}
		w.publish("maps", envspec.MemoryEvent{Part: "maps", ReadError: err.Error()})
		return
	}
	instance := strconv.Itoa(pid) + "\x00" + start
	if instance != w.process {
		w.process = instance
		w.last = map[string]stateEmitted{}
	}

	maps, truncated, err := guestOutput(ctx, w.e, guestCmd{Argv: []string{"cat", "/proc/" + strconv.Itoa(pid) + "/maps"}, User: "root"}, MaxMemoryMapsBytes)
	if err != nil {
		w.publish("maps", envspec.MemoryEvent{Part: "maps", PID: pid, Start: start, ReadError: err.Error()})
		return
	}
	mappings := parseMaps(string(maps))
	ev := envspec.MemoryEvent{Part: "maps", PID: pid, Start: start, Bytes: maps, Truncated: truncated}
	ev.SHA256 = sha256Hex(maps)

	total, omitted := 0, 0
	var reads []envspec.MemoryEvent
	for _, m := range mappings {
		if !m.readablePrivate() {
			continue
		}
		if len(reads)+omitted >= MaxMemoryMappings || total >= w.p.MaxTotalBytes {
			omitted++ // a readable private mapping the caps leave unread
			continue
		}
		reads = append(reads, w.readMapping(ctx, pid, start, m, w.p.MaxTotalBytes-total))
		if r := reads[len(reads)-1]; r.ReadError == "" {
			total += len(r.Bytes)
		}
	}
	ev.OmittedMappings = omitted
	w.publish("maps", ev)
	for _, r := range reads {
		w.publish(fmt.Sprintf("mapping\x00%x-%x", r.Address, r.End), r)
	}
}

func (w *memoryWatch) readMapping(ctx context.Context, pid int, start string, m memMapping, budget int) envspec.MemoryEvent {
	ev := envspec.MemoryEvent{Part: "mapping", PID: pid, Start: start, Address: m.begin, End: m.end, Perms: m.perms, Offset: m.offset, Path: m.path}
	size := m.end - m.begin
	want := int(size)
	if want > w.p.MaxMappingBytes {
		want = w.p.MaxMappingBytes
	}
	if want > budget {
		want = budget
	}
	pages := want / memoryPageBytes
	if pages == 0 {
		ev.ReadError = "mapping smaller than one retained page under the configured caps"
		return ev
	}
	want = pages * memoryPageBytes
	ev.Truncated = uint64(want) < size
	capOut := base64.StdEncoding.EncodedLen(want) + (want/57+2)*2 + 4096
	b64, trunc, err := guestOutput(ctx, w.e, guestCmd{
		Argv: []string{"sh", "-c", memReadScript, "_", strconv.Itoa(pid), strconv.FormatUint(m.begin/memoryPageBytes, 10), strconv.Itoa(pages)},
		User: "root"}, capOut)
	if err != nil {
		ev.ReadError = err.Error()
		return ev
	}
	if trunc {
		ev.ReadError = "encoded memory read exceeded its transport cap"
		return ev
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, string(b64)))
	if err != nil {
		ev.ReadError = "decode encoded memory read: " + err.Error()
		return ev
	}
	if len(decoded) != want {
		ev.ReadError = fmt.Sprintf("short memory read: got %d of %d bytes", len(decoded), want)
		return ev
	}
	ev.Bytes = decoded
	ev.SHA256 = sha256Hex(decoded)
	return ev
}

// resolveProcess finds the package's main process (cmdline exactly
// the package name) and its /proc starttime instance identity.
func (w *memoryWatch) resolveProcess(ctx context.Context) (int, string, error) {
	out, _, err := guestOutput(ctx, w.e, guestCmd{Argv: []string{"pidof", w.p.Package}, User: "root"}, 4096)
	if err != nil {
		return 0, "", fmt.Errorf("process absent: pidof %s: %w", w.p.Package, err)
	}
	var candidates []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			candidates = append(candidates, pid)
		}
	}
	if len(candidates) == 0 {
		return 0, "", fmt.Errorf("process absent: pidof %s returned no pid", w.p.Package)
	}
	pid := 0
	for _, cand := range candidates {
		b, _, err := guestOutput(ctx, w.e, guestCmd{Argv: []string{"cat", "/proc/" + strconv.Itoa(cand) + "/cmdline"}, User: "root"}, 4096)
		if err != nil {
			continue
		}
		name, _, _ := strings.Cut(string(b), "\x00")
		if name == w.p.Package {
			pid = cand
			break
		}
	}
	if pid == 0 {
		return 0, "", fmt.Errorf("process absent: no pidof candidate runs %s as its main process", w.p.Package)
	}
	stat, _, err := guestOutput(ctx, w.e, guestCmd{Argv: []string{"cat", "/proc/" + strconv.Itoa(pid) + "/stat"}, User: "root"}, 4096)
	if err != nil {
		return 0, "", fmt.Errorf("stat pid %d: %w", pid, err)
	}
	start, err := procStartTime(string(stat))
	if err != nil {
		return 0, "", fmt.Errorf("stat pid %d: %w", pid, err)
	}
	return pid, start, nil
}

// procStartTime extracts field 22 (starttime) from /proc/<pid>/stat.
func procStartTime(stat string) (string, error) {
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return "", fmt.Errorf("malformed stat")
	}
	fields := strings.Fields(stat[i+1:])
	// fields[0] is field 3 (state); starttime is field 22.
	if len(fields) < 20 {
		return "", fmt.Errorf("malformed stat: %d trailing fields", len(fields))
	}
	return fields[19], nil
}

type memMapping struct {
	begin, end, offset uint64
	perms, path        string
}

// readablePrivate reports whether the mapping is readable and
// private. Kernel pseudo-mappings a memory read cannot serve are
// excluded.
func (m memMapping) readablePrivate() bool {
	if len(m.perms) != 4 || m.perms[0] != 'r' || m.perms[3] != 'p' {
		return false
	}
	return m.path != "[vvar]" && m.path != "[vdso]"
}

func parseMaps(maps string) []memMapping {
	var out []memMapping
	for _, line := range strings.Split(maps, "\n") {
		m, ok := parseMapsLine(line)
		if ok {
			out = append(out, m)
		}
	}
	return out
}

func parseMapsLine(line string) (memMapping, bool) {
	fields := strings.Fields(line)
	if len(fields) < 5 {
		return memMapping{}, false
	}
	lo, hi, ok := strings.Cut(fields[0], "-")
	if !ok {
		return memMapping{}, false
	}
	begin, err1 := strconv.ParseUint(lo, 16, 64)
	end, err2 := strconv.ParseUint(hi, 16, 64)
	offset, err3 := strconv.ParseUint(fields[2], 16, 64)
	if err1 != nil || err2 != nil || err3 != nil || end <= begin {
		return memMapping{}, false
	}
	path := ""
	if len(fields) > 5 {
		path = fields[5]
	}
	return memMapping{begin: begin, end: end, offset: offset, perms: fields[1], path: path}, true
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
