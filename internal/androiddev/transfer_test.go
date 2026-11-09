package androiddev

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/xfer"
)

func tarOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	if _, err := xfer.WriteTar(context.Background(), envspec.MemSource(files), &b, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func untar(t *testing.T, r io.Reader) map[string][]byte {
	t.Helper()
	sink := envspec.NewMemSink()
	if _, err := xfer.ReadTar(context.Background(), r, sink, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	return sink.Files
}

// tarScript matches only the archive scripts so other Shell traffic keeps
// the default scripted answers.
func tarScript(script string, fn streamFn) func(string) streamFn {
	return func(cmd string) streamFn {
		if strings.Contains(cmd, script) {
			return fn
		}
		return nil
	}
}

func TestPutArchiveStreamsTarIntoResolvedDestination(t *testing.T) {
	var got []byte
	f := &fakeTransport{stream: tarScript("tar -x -f - -C", func(stdin io.Reader, _ io.Writer) (string, int) {
		got, _ = io.ReadAll(stdin)
		return "", 0
	})}
	d := New(f, Config{Serial: "emulator-test"})
	want := map[string][]byte{"a.txt": []byte("one"), "sub/b.txt": []byte("two")}
	if err := d.PutArchive(context.Background(), "proj", bytes.NewReader(tarOf(t, want))); err != nil {
		t.Fatal(err)
	}
	if len(f.commands) != 1 || !strings.HasSuffix(f.commands[0], " _ /data/local/tmp/work/proj") {
		t.Fatalf("command = %q, want the relative destination resolved against the workspace as $1", f.commands)
	}
	files := untar(t, bytes.NewReader(got))
	if string(files["a.txt"]) != "one" || string(files["sub/b.txt"]) != "two" || len(files) != 2 {
		t.Fatalf("guest tar received %v", files)
	}
}

func TestPutArchiveReportsGuestTarFailure(t *testing.T) {
	f := &fakeTransport{stream: tarScript("tar -x -f - -C", func(stdin io.Reader, _ io.Writer) (string, int) {
		_, _ = io.Copy(io.Discard, stdin)
		return "tar: /data/app: Permission denied", 1
	})}
	d := New(f, Config{Serial: "emulator-test"})
	err := d.PutArchive(context.Background(), "/data/app", bytes.NewReader(tarOf(t, map[string][]byte{"x": []byte("x")})))
	if err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err = %v, want exit status and guest stderr", err)
	}
}

func TestGetArchiveStreamsGuestTar(t *testing.T) {
	archive := tarOf(t, map[string][]byte{"log.txt": []byte("hello"), "d/e.bin": {0, 1, 2, 0xff}})
	f := &fakeTransport{stream: tarScript("tar -c -f -", func(_ io.Reader, stdout io.Writer) (string, int) {
		_, _ = stdout.Write(archive)
		return "", 0
	})}
	d := New(f, Config{Serial: "emulator-test"})
	rc, err := d.GetArchive(context.Background(), "/sdcard/out")
	if err != nil {
		t.Fatal(err)
	}
	files := untar(t, rc)
	if err := rc.Close(); err != nil {
		t.Fatalf("close = %v", err)
	}
	if string(files["log.txt"]) != "hello" || !bytes.Equal(files["d/e.bin"], []byte{0, 1, 2, 0xff}) {
		t.Fatalf("pulled %v", files)
	}
	if !strings.HasSuffix(f.commands[0], " _ /sdcard/out") {
		t.Fatalf("command = %q, want the absolute source as $1", f.commands[0])
	}
}

func TestGetArchiveCloseReportsGuestTarFailure(t *testing.T) {
	archive := tarOf(t, map[string][]byte{"ok.txt": []byte("ok")})
	f := &fakeTransport{stream: tarScript("tar -c -f -", func(_ io.Reader, stdout io.Writer) (string, int) {
		_, _ = stdout.Write(archive)
		return "tar: secret.db: Permission denied", 1
	})}
	d := New(f, Config{Serial: "emulator-test"})
	rc, err := d.GetArchive(context.Background(), "files")
	if err != nil {
		t.Fatal(err)
	}
	_ = untar(t, rc)
	if err := rc.Close(); err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("close = %v, want the guest tar failure", err)
	}
}
