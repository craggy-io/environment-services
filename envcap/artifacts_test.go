package envcap

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

// A staged tree keeps what a plain file walk loses: the directory
// entry, the symlink (target as content, symlink mode) and the exec
// bit ride in the craggy.envspec.modes manifest and in the sink
// modes, so the bridge restores all three on the way out.
func TestStageTreeModesManifestRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := envspec.NewMemSink()
	add := func(path string, mode int64, body string) {
		if err := src.Add(path, mode, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	add("sub", 0o040755, "")
	add("sub/run.sh", 0o100755, "#!/bin/sh\n")
	add("sub/link", 0o120777, "../target.txt")
	add("target.txt", 0o100644, "hello")

	run := &stageRun{staged: map[string]*envspec.MemSink{}}
	if err := stageTree(ctx, run, "tree", src.Source()); err != nil {
		t.Fatal(err)
	}
	sink := run.staged["tree"]
	if sink == nil {
		t.Fatal("nothing staged")
	}
	// Modes survive into the sink as-is (the old copy skipped dirs).
	for path, want := range map[string]int64{
		"sub": 0o040755, "sub/run.sh": 0o100755, "sub/link": 0o120777, "target.txt": 0o100644,
	} {
		if sink.Modes[path] != want {
			t.Errorf("sink mode %s = %o, want %o", path, sink.Modes[path], want)
		}
	}
	if got := string(sink.Files["sub/link"]); got != "../target.txt" {
		t.Errorf("symlink content = %q, want target", got)
	}
	// And the manifest round-trips the same entries.
	entries, err := DecodeModes(sink.Files[ModesManifestName])
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]envspec.Entry{}
	for _, e := range entries {
		byPath[e.Path] = e
	}
	if e := byPath["sub"]; !e.IsDir() {
		t.Errorf("manifest lost the directory entry: %+v", entries)
	}
	if e := byPath["sub/run.sh"]; e.Mode&0o111 == 0 {
		t.Errorf("manifest lost the exec bit: %+v", e)
	}
	if e := byPath["sub/link"]; e.Mode&0o170000 != 0o120000 {
		t.Errorf("manifest lost the symlink mode: %+v", e)
	}
	if _, err := EncodeModes(entries); err != nil {
		t.Fatal(err)
	}
}

// stageRun is the smallest in-memory ArtifactRun for port tests.
type stageRun struct {
	staged map[string]*envspec.MemSink
	kept   []string
}

func (r *stageRun) Source(ctx context.Context, id string) (envspec.Source, error) {
	return nil, errors.New("no sources in stageRun")
}

func (r *stageRun) Stage(ctx context.Context, name string, build func(envspec.Sink) error) error {
	sink := envspec.NewMemSink()
	r.staged[name] = sink
	return build(sink)
}

func (r *stageRun) Keep(name string) error {
	if _, ok := r.staged[name]; !ok {
		return errors.New("nothing staged under " + name)
	}
	r.kept = append(r.kept, name)
	return nil
}

func (r *stageRun) Record(ctx context.Context, ev Evidence) error { return nil }
