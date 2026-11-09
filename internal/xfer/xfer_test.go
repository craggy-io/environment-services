package xfer_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/xfer"
)

func TestTarRoundTrip(t *testing.T) {
	ctx := context.Background()
	files := map[string][]byte{
		"a.txt":         []byte("hello"),
		"dir/b.bin":     bytes.Repeat([]byte{1, 2, 3}, 1000),
		"dir/deep/c.md": []byte("# title\n"),
		"empty":         {},
	}
	var buf bytes.Buffer
	st, err := xfer.WriteTar(ctx, envspec.MemSource(files), &buf, xfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != int64(len(files)) {
		t.Fatalf("files = %d", st.Files)
	}
	sink := envspec.NewMemSink()
	st2, err := xfer.ReadTar(ctx, &buf, sink, xfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if st2.Files != st.Files || st2.Bytes != st.Bytes {
		t.Fatalf("stats mismatch: %+v vs %+v", st, st2)
	}
	for name, content := range files {
		if !bytes.Equal(sink.Files[name], content) {
			t.Fatalf("%s round-trip mismatch", name)
		}
	}
}

func TestTarSymlinkAndModes(t *testing.T) {
	ctx := context.Background()
	src := symlinkSource{}
	var buf bytes.Buffer
	if _, err := xfer.WriteTar(ctx, src, &buf, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	sink := envspec.NewMemSink()
	if _, err := xfer.ReadTar(ctx, &buf, sink, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	if string(sink.Files["link"]) != "target/path" {
		t.Fatalf("symlink target = %q", sink.Files["link"])
	}
	if sink.Modes["link"] != 0o120000 {
		t.Fatalf("symlink mode = %o", sink.Modes["link"])
	}
	if sink.Modes["run.sh"]&0o111 == 0 {
		t.Fatalf("executable bit lost: %o", sink.Modes["run.sh"])
	}
}

type symlinkSource struct{}

func (symlinkSource) Entries(ctx context.Context) iter.Seq2[envspec.Entry, error] {
	return func(yield func(envspec.Entry, error) bool) {
		yield(envspec.Entry{Path: "link", Mode: 0o120000, Size: 11}, nil)
		yield(envspec.Entry{Path: "run.sh", Mode: 0o100755, Size: 4}, nil)
	}
}

func (symlinkSource) Open(ctx context.Context, e envspec.Entry) (io.ReadCloser, error) {
	if e.Path == "link" {
		return io.NopCloser(strings.NewReader("target/path")), nil
	}
	return io.NopCloser(strings.NewReader("echo")), nil
}

func TestManifestAndDiff(t *testing.T) {
	ctx := context.Background()
	srcEarly := envspec.MemSource(map[string][]byte{"a": []byte("1"), "b": []byte("22"), "c": []byte("333")})
	man, err := xfer.Manifest(ctx, srcEarly)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range man {
		if e.Digest == "" || !strings.HasPrefix(e.Digest, "sha256:") {
			t.Fatalf("entry %s missing digest", e.Path)
		}
	}
	dst := envspec.MemSource(map[string][]byte{"a": []byte("1"), "b": []byte("CHANGED"), "d": []byte("extra")})
	dstMan, _ := xfer.Manifest(ctx, dst)
	send, del := xfer.Diff(man, dstMan, true)
	var sendPaths []string
	for _, e := range send {
		sendPaths = append(sendPaths, e.Path)
	}
	if strings.Join(sendPaths, ",") != "b,c" {
		t.Fatalf("send = %v, want [b c]", sendPaths)
	}
	if len(del) != 1 || del[0] != "d" {
		t.Fatalf("del = %v", del)
	}
	if _, del2 := xfer.Diff(man, dstMan, false); len(del2) != 0 {
		t.Fatalf("no-mirror del = %v", del2)
	}
}

func TestDirSourceSinkAndDigestSink(t *testing.T) {
	ctx := context.Background()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "x.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "sub", "y.txt"), []byte("more"), 0o644); err != nil {
		t.Fatal(err)
	}
	dstDir := filepath.Join(t.TempDir(), "out")
	var buf bytes.Buffer
	if _, err := xfer.WriteTar(ctx, xfer.DirSource(srcDir), &buf, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := xfer.ReadTar(ctx, &buf, xfer.DirSink(dstDir), xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dstDir, "sub", "y.txt"))
	if err != nil || string(b) != "more" {
		t.Fatalf("round trip file: %q %v", b, err)
	}
	// LocalManifest digests match Manifest of the same dir.
	lm, err := xfer.LocalManifest(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	gm, err := xfer.Manifest(ctx, xfer.DirSource(srcDir))
	if err != nil {
		t.Fatal(err)
	}
	byDigestLocal, byDigestGen := map[string]string{}, map[string]string{}
	for _, e := range lm {
		byDigestLocal[e.Path] = e.Digest
	}
	for _, e := range gm {
		byDigestGen[e.Path] = e.Digest
	}
	for p, d := range byDigestGen {
		if byDigestLocal[p] != d {
			t.Fatalf("digest mismatch for %s: %s vs %s", p, byDigestLocal[p], d)
		}
	}
}

// Benchmark-sized tree transfer through tar (scaled: the gate's full
// 80k run lives in BenchmarkXfer80k; this asserts correctness at 2k files).
func TestTarManyFiles(t *testing.T) {
	ctx := context.Background()
	files := map[string][]byte{}
	for i := 0; i < 2000; i++ {
		files[fmt.Sprintf("pkg%04d/file%04d.txt", i/100, i)] = []byte(fmt.Sprintf("content-%d", i))
	}
	var buf bytes.Buffer
	st, err := xfer.WriteTar(ctx, envspec.MemSource(files), &buf, xfer.Options{})
	if err != nil || st.Files != 2000 {
		t.Fatalf("write: %+v %v", st, err)
	}
	sink := envspec.NewMemSink()
	st, err = xfer.ReadTar(ctx, &buf, sink, xfer.Options{})
	if err != nil || st.Files != 2000 || len(sink.Files) != 2000 {
		t.Fatalf("read: %+v %v got %d", st, err, len(sink.Files))
	}
}
