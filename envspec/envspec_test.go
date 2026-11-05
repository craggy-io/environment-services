package envspec_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
)

func TestIDs(t *testing.T) {
	a, b := envspec.NewID("env_"), envspec.NewID("env_")
	if a == b {
		t.Fatal("IDs must be unique")
	}
	if !strings.HasPrefix(a, "env_") || len(a) != len("env_")+36 {
		t.Fatalf("bad ID shape: %s", a)
	}
	if !envspec.HasPrefix(a, "env_") || envspec.HasPrefix(a, "lease_") {
		t.Fatal("HasPrefix wrong")
	}
}

func TestThrottleKeepsFirstOfStageAndFlush(t *testing.T) {
	var got []envspec.Progress
	report, flush := envspec.Throttled(func(p envspec.Progress) { got = append(got, p) }, 1000)
	for i := 0; i < 100; i++ {
		report(envspec.Progress{Stage: "pull-image", Done: int64(i)})
	}
	report(envspec.Progress{Stage: "create", Done: 1})
	flush()
	if len(got) == 0 {
		t.Fatal("no progress delivered")
	}
	if got[0].Stage != "pull-image" || got[0].Done != 0 {
		t.Fatalf("first update of stage must pass: %+v", got[0])
	}
	foundCreate := false
	for _, p := range got {
		if p.Stage == "create" {
			foundCreate = true
		}
	}
	if !foundCreate {
		t.Fatal("first update of new stage must pass throttle")
	}
	if len(got) > 101 {
		t.Fatalf("more updates than sent: %d", len(got))
	}
}

func TestMemSourceSinkRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := envspec.MemSource(map[string][]byte{"b.txt": []byte("two"), "a.txt": []byte("one")})
	sink := envspec.NewMemSink()
	var order []string
	for e, err := range src.Entries(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, e.Path)
		rc, err := src.Open(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if err := sink.Add(e.Path, e.Mode, rc); err != nil {
			t.Fatal(err)
		}
		rc.Close()
	}
	if strings.Join(order, ",") != "a.txt,b.txt" {
		t.Fatalf("entries not sorted: %v", order)
	}
	if string(sink.Files["a.txt"]) != "one" || sink.Modes["a.txt"] != 0o100644 {
		t.Fatalf("sink contents wrong: %v %v", sink.Files, sink.Modes)
	}
	// Digest dedup: "one" is already held.
	if err := sink.AddDigest("c.txt", 0o100644, digestOfForTest("one"), 3); err != nil {
		t.Fatalf("AddDigest held content: %v", err)
	}
	if string(sink.Files["c.txt"]) != "one" {
		t.Fatalf("AddDigest recorded wrong content: %q", sink.Files["c.txt"])
	}
	if !sink.Has(digestOfForTest("two")) || sink.Has(digestOfForTest("three")) {
		t.Fatal("Has wrong")
	}
	if err := sink.AddDigest("d.txt", 0o100644, digestOfForTest("three"), 5); err == nil {
		t.Fatal("AddDigest of unheld digest must fail")
	}
	// Sink back out as a Source.
	back := sink.Source()
	var names []string
	for e, err := range back.Entries(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, e.Path)
	}
	if strings.Join(names, ",") != "a.txt,b.txt,c.txt" {
		t.Fatalf("sink source entries: %v", names)
	}
	rc, err := back.Open(ctx, envspec.Entry{Path: "missing"})
	if err == nil {
		rc.Close()
		t.Fatal("Open of missing entry must fail")
	}
}

func digestOfForTest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
