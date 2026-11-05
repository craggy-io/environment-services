package xfer_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	osexec "os/exec"
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

// TestAgentProtocol builds the real craggy-xfer binary for this platform and
// drives manifest/recv/send through the client.
func TestAgentProtocol(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "craggy-xfer-test.exe")
	build := osexec.Command("go", "build", "-o", bin, "github.com/craggy-io/environment-services/cmd/xfer-agent")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build agent: %v\n%s", err, out)
	}
	execFn := func(ctx context.Context, argv []string, stdin io.Reader) (io.Reader, func() (int, error), error) {
		cmd := osexec.CommandContext(ctx, bin, argv[1:]...)
		if stdin != nil {
			cmd.Stdin = stdin
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, nil, err
		}
		return out, func() (int, error) {
			err := cmd.Wait()
			return cmd.ProcessState.ExitCode(), err
		}, nil
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f1.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	man, err := xfer.AgentManifest(ctx, execFn, []string{bin}, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(man) != 1 || man[0].Path != "f1.txt" || man[0].Digest == "" {
		t.Fatalf("manifest = %+v", man)
	}
	// recv a new file, manifest again, send it back.
	var tarBuf bytes.Buffer
	if _, err := xfer.WriteTar(ctx, envspec.MemSource(map[string][]byte{"sub/f2.txt": []byte("two")}), &tarBuf, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := xfer.AgentRecv(ctx, execFn, []string{bin}, root, false, &tarBuf); err != nil {
		t.Fatal(err)
	}
	r, wait, err := xfer.AgentSend(ctx, execFn, []string{bin}, root, []string{"sub/f2.txt"})
	if err != nil {
		t.Fatal(err)
	}
	sink := envspec.NewMemSink()
	if _, err := xfer.ReadTar(ctx, r, sink, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	if code, err := wait(); err != nil || code != 0 {
		t.Fatalf("agent send wait = %d %v", code, err)
	}
	if string(sink.Files["sub/f2.txt"]) != "two" {
		t.Fatalf("send payload = %v", sink.Files)
	}
	// recv with delete list.
	if err := os.WriteFile(filepath.Join(root, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	var delBuf bytes.Buffer
	delSrc := envspec.MemSource(map[string][]byte{xfer.DeleteListName: []byte("stale.txt\n")})
	if _, err := xfer.WriteTar(ctx, delSrc, &delBuf, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := xfer.AgentRecv(ctx, execFn, []string{bin}, root, true, &delBuf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale.txt should be deleted, stat err = %v", err)
	}
}

func TestEmbeddedAgentPresent(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		b, ok := xfer.AgentBinary(arch)
		if !ok || len(b) < 1<<20 {
			t.Fatalf("embedded agent for %s missing or tiny (ok=%v len=%d)", arch, ok, len(b))
		}
		if _, ok := xfer.AgentDigest(arch); !ok {
			t.Fatalf("digest missing for %s", arch)
		}
	}
}

// Benchmark-sized tree transfer through tar (scaled: the §5.4 gate's full
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
