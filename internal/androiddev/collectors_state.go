package androiddev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// --- android.state@1 ---

const stateStream = envspec.StreamAndroidState

var stateParts = []string{envspec.StatePartStorage, envspec.StatePartPackage, envspec.StatePartAppOps, envspec.StatePartURIGrants}

const (
	// MaxStateBytes caps the bytes retained per snapshot. A larger
	// output is retained as a prefix and marked Truncated.
	MaxStateBytes = 4 << 20
	// MaxStorageEntries caps the entries one storage listing retains.
	MaxStorageEntries = 20000
	// MaxStorageHashBytes is the largest file the storage listing
	// hashes; a larger file is listed with hash "~".
	MaxStorageHashBytes = 64 << 20
	maxStatePackages    = 8
	storageBatchArgs    = 256
	storageBatchBytes   = 32 << 10
)

var packageNamePatt = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z0-9_]+)+$`)

func parseStateParams(raw json.RawMessage) (envspec.StateParams, error) {
	var p envspec.StateParams
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("%s params: %w", stateStream, err)
		}
	}
	if len(p.Packages) == 0 || len(p.Packages) > maxStatePackages {
		return p, fmt.Errorf("%s params: packages must name 1..%d packages", stateStream, maxStatePackages)
	}
	for _, pkg := range p.Packages {
		if !packageNamePatt.MatchString(pkg) {
			return p, fmt.Errorf("%s params: %q is not a package name", stateStream, pkg)
		}
	}
	if len(p.Parts) == 0 {
		p.Parts = stateParts
	}
	for _, part := range p.Parts {
		if !slices.Contains(stateParts, part) {
			return p, fmt.Errorf("%s params: unknown part %q (want %s)", stateStream, part, strings.Join(stateParts, ", "))
		}
	}
	if p.IntervalSeconds < 0 {
		return p, fmt.Errorf("%s params: interval_seconds cannot be negative", stateStream)
	}
	return p, nil
}

// AndroidState reads app private storage, permission and AppOps state
// and the URI grant table as root, receipt-grade: exact bytes, digests,
// boot id and the installed code identity of each package.
type stateCollector struct {
	dev      *Device
	Interval time.Duration
}

func (a *stateCollector) Name() string { return "android.state" }
func (a *stateCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: stateStream, Description: "App private storage, permission/AppOps state and URI grants of named packages, with exact bytes and digests", Cost: "medium",
		Schema: signalcore.SchemaFor(envspec.StateEvent{})}}
}
func (a *stateCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(stateStream, "android")
	}
	if !e.Root {
		return rootHint(stateStream, e.Profile)
	}
	return nil
}
func (a *stateCollector) Params() *jsonschema.Schema {
	one, maxPkgs := 1, maxStatePackages
	parts := make([]any, len(stateParts))
	for i, p := range stateParts {
		parts[i] = p
	}
	return objSchema(map[string]*jsonschema.Schema{
		"packages":         {Type: "array", Items: strSchema(), MinItems: &one, MaxItems: &maxPkgs},
		"parts":            {Type: "array", Items: &jsonschema.Schema{Type: "string", Enum: parts}},
		"interval_seconds": {Type: "integer"},
	}, "packages")
}
func (a *stateCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := a.dev
	p, err := parseStateParams(params)
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
	w := newStateWatch(e, out, p)
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

// stateWatch is AndroidState's per-run state; cycle runs on one
// goroutine.
type stateWatch struct {
	e      *Device
	out    signalcore.Emitter
	p      envspec.StateParams
	boot   string
	last   map[string]stateEmitted // part key -> last emitted snapshot
	errs   map[string]string       // collector_error text last emitted, by step
	codes  map[string]codeIdentity // apk path -> identity cached by its stat
	hashes map[string]string       // storage file key -> sha256
}

// stateEmitted is what was last emitted for one part.
type stateEmitted struct {
	key  string // change-detection key (content + identity)
	sha  string
	when time.Time
}

type codeIdentity struct{ stat, sha string }

func newStateWatch(e *Device, out signalcore.Emitter, p envspec.StateParams) *stateWatch {
	return &stateWatch{e: e, out: out, p: p, last: map[string]stateEmitted{}, errs: map[string]string{},
		codes: map[string]codeIdentity{}, hashes: map[string]string{}}
}

func (w *stateWatch) emit(ev envspec.StateEvent) {
	w.emitAt(ev, time.Now())
}

func (w *stateWatch) emitAt(ev envspec.StateEvent, at time.Time) {
	ev.BootID = w.boot
	w.out.Emit(envspec.SignalEvent{Stream: stateStream, Time: at, Data: ev})
}

// report emits a collector_error when step fails with a new error
// text; success clears it so a recurrence is reported again.
func (w *stateWatch) report(step string, err error) {
	if err == nil {
		delete(w.errs, step)
		return
	}
	msg := step + ": " + err.Error()
	if w.errs[step] == msg {
		return
	}
	w.errs[step] = msg
	w.emit(envspec.StateEvent{Kind: "collector_error", Error: msg})
}

func (w *stateWatch) wants(part string) bool { return slices.Contains(w.p.Parts, part) }

// cycle reads every requested part once.
func (w *stateWatch) cycle(ctx context.Context) {
	boot, err := readBootID(ctx, w.e)
	w.boot = boot
	w.report("boot id", err)
	live := map[string]bool{}
	for _, pkg := range w.p.Packages {
		id := w.identity(ctx, pkg)
		for _, part := range []string{envspec.StatePartStorage, envspec.StatePartPackage, envspec.StatePartAppOps} {
			if !w.wants(part) {
				continue
			}
			ev := w.read(ctx, part, pkg, live)
			ev.UID, ev.CodePath, ev.CodeSHA256, ev.CodeError = id.UID, id.CodePath, id.CodeSHA256, id.CodeError
			w.publish(ev)
		}
	}
	if w.wants(envspec.StatePartURIGrants) {
		w.publish(w.read(ctx, envspec.StatePartURIGrants, "", live))
	}
	for k := range w.hashes {
		if !live[k] {
			delete(w.hashes, k)
		}
	}
}

// publish emits a part's read as a snapshot, or as unchanged when it
// repeats the last snapshot of that part.
func (w *stateWatch) publish(ev envspec.StateEvent) {
	k := ev.Part + "\x00" + ev.Package
	key := changeKey(ev)
	prev, seen := w.last[k]
	if seen && prev.key == key {
		w.emit(envspec.StateEvent{Kind: "unchanged", Part: ev.Part, Package: ev.Package, Format: ev.Format, Command: ev.Command,
			SHA256: prev.sha, ReadError: ev.ReadError, Since: prev.when.UTC().Format(time.RFC3339Nano),
			UID: ev.UID, CodePath: ev.CodePath, CodeSHA256: ev.CodeSHA256, CodeError: ev.CodeError})
		return
	}
	ev.Kind, ev.Baseline = "snapshot", !seen
	at := time.Now()
	w.last[k] = stateEmitted{key: key, sha: ev.SHA256, when: at}
	w.emitAt(ev, at)
}

// appopsVolatile matches the relative times `appops get` prints; they
// change every read without the state changing.
var appopsVolatile = regexp.MustCompile(`; (time|rejectTime|duration)=[^;\n]*| \(running\)`)

// changeKey identifies a read for change detection: its content (with
// AppOps' relative times removed) and the package identity. The
// retained bytes and their digest are always exact.
func changeKey(ev envspec.StateEvent) string {
	content := ev.Bytes
	if ev.Format == envspec.AppOpsFormat {
		content = appopsVolatile.ReplaceAll(content, nil)
	}
	sum := sha256.Sum256(content)
	return strings.Join([]string{hex.EncodeToString(sum[:]), strconv.FormatBool(ev.Truncated), ev.ReadError,
		strconv.Itoa(ev.UID), ev.CodePath, ev.CodeSHA256, ev.CodeError}, "\x00")
}

// read runs one part; live collects the storage hash keys still in use.
func (w *stateWatch) read(ctx context.Context, part, pkg string, live map[string]bool) envspec.StateEvent {
	ev := envspec.StateEvent{Part: part, Package: pkg}
	var b []byte
	var truncated bool
	var err error
	switch part {
	case envspec.StatePartStorage:
		ev.Format, ev.Command = envspec.StorageFormat, "find -xdev; stat; sha256sum (as root) over "+strings.Join(storageRoots(pkg), " ")
		b, truncated, err = w.storageListing(ctx, pkg, live)
	case envspec.StatePartPackage:
		ev.Format, ev.Command = envspec.PackageFormat, "dumpsys package "+pkg
		b, truncated, err = guestOutput(ctx, w.e, guestCmd{Argv: []string{"dumpsys", "package", pkg}, User: "root"}, MaxStateBytes)
	case envspec.StatePartAppOps:
		ev.Format, ev.Command = envspec.AppOpsFormat, "appops get "+pkg
		b, truncated, err = guestOutput(ctx, w.e, guestCmd{Argv: []string{"appops", "get", pkg}, User: "root"}, MaxStateBytes)
	case envspec.StatePartURIGrants:
		ev.Format, ev.Command = envspec.URIGrantsFormat, "dumpsys activity -a permissions"
		b, truncated, err = guestOutput(ctx, w.e, guestCmd{Argv: []string{"dumpsys", "activity", "-a", "permissions"}, User: "root"}, MaxStateBytes)
	}
	if err != nil {
		ev.ReadError = err.Error()
		return ev
	}
	sum := sha256.Sum256(b)
	ev.Bytes, ev.Truncated, ev.SHA256 = b, truncated, hex.EncodeToString(sum[:])
	if ev.Bytes == nil {
		ev.Bytes = []byte{}
	}
	return ev
}

// packageIdentity is a package's uid and installed code at one read.
type packageIdentity struct {
	UID                  int
	CodePath, CodeSHA256 string
	CodeError            string
}

// identity resolves a package's uid and installed base.apk digest. The
// digest is recomputed only when the apk's size, mtime or inode change.
func (w *stateWatch) identity(ctx context.Context, pkg string) packageIdentity {
	var id packageIdentity
	var errs []string
	uid, err := packageUID(ctx, w.e, pkg)
	if err != nil {
		errs = append(errs, err.Error())
	}
	id.UID = uid
	path, err := packageCodePath(ctx, w.e, pkg)
	if err == nil {
		id.CodePath = path
		id.CodeSHA256, err = w.codeSHA256(ctx, path)
	}
	if err != nil {
		errs = append(errs, err.Error())
	}
	id.CodeError = strings.Join(errs, "; ")
	return id
}

func (w *stateWatch) codeSHA256(ctx context.Context, path string) (string, error) {
	st, _, err := guestOutput(ctx, w.e, guestCmd{Argv: []string{"stat", "-c", "%s %Y %i", path}, User: "root"}, 256)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	stat := strings.TrimSpace(string(st))
	if c, ok := w.codes[path]; ok && c.stat == stat {
		return c.sha, nil
	}
	sha, err := guestSHA256(ctx, w.e, path)
	if err != nil {
		delete(w.codes, path)
		return "", err
	}
	w.codes[path] = codeIdentity{stat: stat, sha: sha}
	return sha, nil
}

// packageUID reads a package's uid from `pm list packages -U`, whose
// filter is a substring match: only the exact name counts.
func packageUID(ctx context.Context, e *Device, pkg string) (int, error) {
	b, _, err := guestOutput(ctx, e, guestCmd{Argv: []string{"pm", "list", "packages", "-U", pkg}}, 64<<10)
	if err != nil {
		return 0, fmt.Errorf("pm list packages -U %s: %w", pkg, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) != 2 || f[0] != "package:"+pkg || !strings.HasPrefix(f[1], "uid:") {
			continue
		}
		// A shared or multi-user uid prints as "uid:10123,10124"; the first is user 0's.
		first, _, _ := strings.Cut(strings.TrimPrefix(f[1], "uid:"), ",")
		if uid, err := strconv.Atoi(first); err == nil {
			return uid, nil
		}
	}
	return 0, fmt.Errorf("pm list packages -U %s: package not listed", pkg)
}

// storageRoots are a package's private storage roots for user 0:
// credential-encrypted, device-encrypted and the app-specific external
// directories (read at their /data/media backing, not through FUSE).
func storageRoots(pkg string) []string {
	return []string{"/data/data/" + pkg, "/data/user_de/0/" + pkg,
		"/data/media/0/Android/data/" + pkg, "/data/media/0/Android/media/" + pkg, "/data/media/0/Android/obb/" + pkg}
}

// Storage listing scripts. Paths reach them as argv (quoted per
// argument), and each prints exactly one line per path, so a file name
// holding a newline cannot forge or shift entries.
const (
	// listRootScript prints a root's tree NUL-separated, or exits 3 when
	// the root is absent.
	listRootScript = `r=$1; [ -e "$r" ] || [ -L "$r" ] || exit 3; exec find "$r" -xdev -print0`
	// statPathsScript prints "mode uid gid nlink size mtime ctime inode
	// label" per path, or "?" when the path is gone.
	statPathsScript = `for f; do stat -c '%f %u %g %h %s %Y %Z %i %C' -- "$f" 2>/dev/null || echo '?'; done`
	// hashPathsScript prints a regular file's sha256, "-" for a path no
	// longer a regular file, or "!" when it could not be read.
	hashPathsScript = `for f; do if [ -f "$f" ] && [ ! -L "$f" ]; then s=$(sha256sum < "$f" 2>/dev/null) && echo "${s%% *}" || echo '!'; else echo '-'; fi; done`
)

// storageEntry is one listed path.
type storageEntry struct {
	path  string
	gone  bool
	mode  uint64
	stat  []string // mode uid gid nlink size mtime ctime inode [label]
	size  int64
	hash  string
	label string
}

func (s storageEntry) regular() bool { return s.mode&0o170000 == 0o100000 }

func (s storageEntry) hashKey() string { return s.path + "\x00" + strings.Join(s.stat[:8], " ") }

// storageListing lists the package's private storage as root, in the
// envspec.StorageFormat:
//
//	craggy.android-storage@1
//	root present|absent <path>
//	entry <mode hex> <uid> <gid> <nlink> <size> <mtime> <ctime> <inode> <sha256|-|!|~> <label|-> <path>
//	gone <path>
//
// Paths are escaped (EscapeStoragePath); entries are sorted bytewise
// under each root. Hash "-" is not a regular file, "!" unreadable, "~"
// larger than MaxStorageHashBytes. A listing past MaxStorageEntries is
// cut there and reported truncated.
func (w *stateWatch) storageListing(ctx context.Context, pkg string, live map[string]bool) ([]byte, bool, error) {
	var b bytes.Buffer
	b.WriteString(envspec.StorageFormat + "\n")
	total, truncated := 0, false
	for _, root := range storageRoots(pkg) {
		names, present, err := listRoot(ctx, w.e, root)
		if err != nil {
			return nil, false, err
		}
		state := "absent"
		if present {
			state = "present"
		}
		fmt.Fprintf(&b, "root %s %s\n", state, EscapeStoragePath(root))
		if room := MaxStorageEntries - total; len(names) > room {
			names, truncated = names[:max(room, 0)], true
		}
		total += len(names)
		entries, err := statPaths(ctx, w.e, names)
		if err != nil {
			return nil, false, err
		}
		if err := w.hashEntries(ctx, entries, live); err != nil {
			return nil, false, err
		}
		for _, en := range entries {
			if en.gone {
				fmt.Fprintf(&b, "gone %s\n", EscapeStoragePath(en.path))
				continue
			}
			label := en.label
			if label == "" {
				label = "-"
			}
			fmt.Fprintf(&b, "entry %s %s %s %s\n", strings.Join(en.stat[:8], " "), en.hash, label, EscapeStoragePath(en.path))
		}
		if b.Len() > MaxStateBytes {
			return b.Bytes()[:MaxStateBytes], true, nil
		}
	}
	return b.Bytes(), truncated, nil
}

// listRoot returns a root's paths, sorted bytewise.
func listRoot(ctx context.Context, e *Device, root string) ([]string, bool, error) {
	proc, err := e.execGuest(ctx, guestCmd{Argv: []string{"sh", "-c", listRootScript, "_", root}, User: "root"})
	if err != nil {
		return nil, false, fmt.Errorf("find %s: exec: %w", root, err)
	}
	stderr := make(chan []byte, 1)
	go func() {
		b, _ := readPrefix(proc.Stderr, 4<<10)
		stderr <- b
	}()
	out, rerr := readPrefix(proc.Stdout, MaxStateBytes*4)
	code, werr := proc.Wait()
	msg := strings.TrimSpace(string(<-stderr))
	switch {
	case werr != nil:
		return nil, false, withStderr(fmt.Errorf("find %s: wait: %w", root, werr), msg)
	case code == 3:
		return nil, false, nil
	case code != 0:
		return nil, false, withStderr(fmt.Errorf("find %s: exit status %d", root, code), msg)
	case rerr != nil:
		return nil, false, fmt.Errorf("find %s: read stdout: %w", root, rerr)
	}
	var names []string
	for _, n := range bytes.Split(out, []byte{0}) {
		if len(n) > 0 {
			names = append(names, string(n))
		}
	}
	sort.Strings(names)
	return names, true, nil
}

// statPaths stats paths in batches, one output line per path.
func statPaths(ctx context.Context, e *Device, paths []string) ([]storageEntry, error) {
	entries := make([]storageEntry, 0, len(paths))
	err := batchLines(ctx, e, statPathsScript, paths, func(path, line string) error {
		if line == "?" {
			entries = append(entries, storageEntry{path: path, gone: true})
			return nil
		}
		f := strings.Fields(line)
		if len(f) < 8 {
			return fmt.Errorf("stat %s: unparsable line %q", EscapeStoragePath(path), line)
		}
		mode, err := strconv.ParseUint(f[0], 16, 32)
		if err != nil {
			return fmt.Errorf("stat %s: mode %q: %w", EscapeStoragePath(path), f[0], err)
		}
		for _, n := range f[1:8] {
			if _, err := strconv.ParseUint(n, 10, 64); err != nil {
				return fmt.Errorf("stat %s: unparsable line %q", EscapeStoragePath(path), line)
			}
		}
		size, _ := strconv.ParseInt(f[4], 10, 64)
		en := storageEntry{path: path, mode: mode, stat: f[:8], size: size, hash: "-"}
		if len(f) > 8 {
			en.label = strings.Join(f[8:], " ")
		}
		entries = append(entries, en)
		return nil
	})
	return entries, err
}

// hashEntries fills each regular file's hash, reusing the digest of a
// file whose stat is unchanged since the last cycle.
func (w *stateWatch) hashEntries(ctx context.Context, entries []storageEntry, live map[string]bool) error {
	var need []string
	idx := map[string]int{}
	for i := range entries {
		en := &entries[i]
		if en.gone || !en.regular() {
			continue
		}
		if en.size > MaxStorageHashBytes {
			en.hash = "~"
			continue
		}
		k := en.hashKey()
		live[k] = true
		if sha, ok := w.hashes[k]; ok {
			en.hash = sha
			continue
		}
		idx[en.path] = i
		need = append(need, en.path)
	}
	return batchLines(ctx, w.e, hashPathsScript, need, func(path, line string) error {
		en := &entries[idx[path]]
		switch {
		case line == "-" || line == "!":
			en.hash = line
		case len(line) == 64 && isHex(line):
			en.hash = strings.ToLower(line)
			w.hashes[en.hashKey()] = en.hash
		default:
			return fmt.Errorf("sha256sum %s: unexpected output %q", EscapeStoragePath(path), line)
		}
		return nil
	})
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// batchLines runs script as root over paths in argv batches; it must
// print exactly one line per path, handed to fn in order.
func batchLines(ctx context.Context, e *Device, script string, paths []string, fn func(path, line string) error) error {
	for start := 0; start < len(paths); {
		end, size := start, 0
		for end < len(paths) && end-start < storageBatchArgs && (end == start || size+len(paths[end]) <= storageBatchBytes) {
			size += len(paths[end]) + 3
			end++
		}
		batch := paths[start:end]
		argv := append([]string{"sh", "-c", script, "_"}, batch...)
		out, _, err := guestOutput(ctx, e, guestCmd{Argv: argv, User: "root"}, MaxStateBytes)
		if err != nil {
			return fmt.Errorf("storage listing: %w", err)
		}
		lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		if len(lines) != len(batch) {
			return fmt.Errorf("storage listing: %d output lines for %d paths", len(lines), len(batch))
		}
		for i, line := range lines {
			if err := fn(batch[i], strings.TrimRight(line, "\r")); err != nil {
				return err
			}
		}
		start = end
	}
	return nil
}

// EscapeStoragePath escapes a path for one envspec.StorageFormat field: bytes
// at or below space, at or above 0x7f, and '%' become %XX.
func EscapeStoragePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c <= ' ' || c >= 0x7f || c == '%' {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
