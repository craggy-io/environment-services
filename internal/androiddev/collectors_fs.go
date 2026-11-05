package androiddev

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/signalcore"
)

// --- fs.changes@2: receipt-grade file-object observer ---
//
// Where fs.changes@1 diffs driver manifests (digests only), fs.changes@2
// lists the watched roots as root with full inode and path identity —
// mode, owner, gid, nlink, size, mtime, ctime, inode, SELinux label and
// a SHA-256 per regular file — in the same craggy.android-storage@1
// format the android.state@1 storage part uses, so the platform's
// ParseStorage/DiffStorage read it and a gate sees exactly which object
// (by inode) changed and its before/after content. A cycle emits a
// snapshot when a root's listing changed since the last one emitted, and
// an "unchanged" event (naming the earlier snapshot's digest) otherwise,
// so any capture window holds the state at its edges.

const fsStateStream = envspec.StreamFSObserve

// FSState lists watched roots as root every cycle, receipt-grade.
type fsStateCollector struct {
	dev      *Device
	Interval time.Duration
}

func (f *fsStateCollector) Name() string { return "fs.observe" }
func (f *fsStateCollector) Streams() []signalcore.StreamDef {
	return []signalcore.StreamDef{{Name: fsStateStream, Description: "Watched file trees listed as root with inode/path identity and per-file digests", Cost: "medium",
		Schema: signalcore.SchemaFor(envspec.FSStateEvent{})}}
}
func (f *fsStateCollector) Applies(e signalcore.EnvInfo) error {
	if e.Kind != "android" {
		return kindOnly(fsStateStream, "android")
	}
	if !e.Root {
		return rootHint(fsStateStream, e.Profile)
	}
	return nil
}
func (f *fsStateCollector) Params() *jsonschema.Schema {
	one := 1
	return objSchema(map[string]*jsonschema.Schema{
		"roots":            {Type: "array", Items: strSchema(), MinItems: &one, Description: "absolute paths to list as root"},
		"interval_seconds": {Type: "integer"},
	}, "roots")
}

const maxFSRoots = 16

func parseFSStateParams(raw json.RawMessage) (envspec.FSStateParams, error) {
	var p envspec.FSStateParams
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return p, fmt.Errorf("%s params: %w", fsStateStream, err)
		}
	}
	if len(p.Roots) == 0 || len(p.Roots) > maxFSRoots {
		return p, fmt.Errorf("%s params: roots must name 1..%d absolute paths", fsStateStream, maxFSRoots)
	}
	for _, r := range p.Roots {
		if !strings.HasPrefix(r, "/") {
			return p, fmt.Errorf("%s params: %q is not an absolute path", fsStateStream, r)
		}
	}
	if p.IntervalSeconds < 0 {
		return p, fmt.Errorf("%s params: interval_seconds cannot be negative", fsStateStream)
	}
	return p, nil
}

func (f *fsStateCollector) Run(ctx context.Context, params json.RawMessage, out signalcore.Emitter) error {
	e := f.dev
	p, err := parseFSStateParams(params)
	if err != nil {
		return err
	}
	iv := f.Interval
	if p.IntervalSeconds > 0 {
		iv = time.Duration(p.IntervalSeconds) * time.Second
	}
	if iv <= 0 {
		iv = 5 * time.Second
	}
	w := &fsWatch{e: e, out: out, roots: p.Roots, last: map[string]fsEmitted{}, hashes: map[string]string{}}
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

// fsWatch is FSState's per-run state; cycle runs on one goroutine.
type fsWatch struct {
	e      *Device
	out    signalcore.Emitter
	roots  []string
	boot   string
	last   map[string]fsEmitted // root -> last emitted snapshot
	hashes map[string]string    // storage file key -> sha256 (reused across cycles)
}

type fsEmitted struct {
	sha  string
	when time.Time
}

func (w *fsWatch) cycle(ctx context.Context) {
	w.boot, _ = readBootID(ctx, w.e)
	live := map[string]bool{}
	for _, root := range w.roots {
		w.publish(ctx, root, live)
	}
	for k := range w.hashes {
		if !live[k] {
			delete(w.hashes, k)
		}
	}
}

// publish lists one root and emits a snapshot, or unchanged when the
// listing repeats the last one.
func (w *fsWatch) publish(ctx context.Context, root string, live map[string]bool) {
	b, truncated, err := w.listing(ctx, root, live)
	ev := envspec.FSStateEvent{Root: root, BootID: w.boot, Format: envspec.StorageFormat,
		Command: "find -xdev; stat; sha256sum (as root) over " + root}
	if err != nil {
		ev.ReadError = err.Error()
	} else {
		sum := sha256.Sum256(b)
		ev.Bytes, ev.Truncated, ev.SHA256 = b, truncated, hex.EncodeToString(sum[:])
		if ev.Bytes == nil {
			ev.Bytes = []byte{}
		}
	}
	key := w.changeKey(ev)
	prev, seen := w.last[root]
	if seen && prev.sha == key {
		w.out.Emit(envspec.SignalEvent{Stream: fsStateStream, Data: envspec.FSStateEvent{Kind: "unchanged", Root: root, BootID: w.boot,
			Format: ev.Format, Command: ev.Command, SHA256: prev.sha, ReadError: ev.ReadError,
			Since: prev.when.UTC().Format(time.RFC3339Nano)}})
		return
	}
	ev.Kind, ev.Baseline = "snapshot", !seen
	w.last[root] = fsEmitted{sha: key, when: time.Now()}
	w.out.Emit(envspec.SignalEvent{Stream: fsStateStream, Data: ev})
}

// changeKey identifies a read for change detection: the content digest
// and the read-error (the retained bytes and digest stay exact).
func (w *fsWatch) changeKey(ev envspec.FSStateEvent) string {
	if ev.ReadError != "" {
		return "err:" + ev.ReadError
	}
	return ev.SHA256
}

// listing lists one root in the craggy.android-storage@1 format, reusing
// the android.state storage helpers (listRoot/statPaths/hashEntries).
func (w *fsWatch) listing(ctx context.Context, root string, live map[string]bool) ([]byte, bool, error) {
	var b bytes.Buffer
	b.WriteString(envspec.StorageFormat + "\n")
	names, present, err := listRoot(ctx, w.e, root)
	if err != nil {
		return nil, false, err
	}
	state := "absent"
	if present {
		state = "present"
	}
	fmt.Fprintf(&b, "root %s %s\n", state, EscapeStoragePath(root))
	truncated := false
	if len(names) > MaxStorageEntries {
		names, truncated = names[:MaxStorageEntries], true
	}
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
	return b.Bytes(), truncated, nil
}

// hashEntries fills each regular file's hash, reusing the digest of a
// file whose stat is unchanged since the last cycle (same cache shape as
// the android.state storage listing).
func (w *fsWatch) hashEntries(ctx context.Context, entries []storageEntry, live map[string]bool) error {
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
