package androiddev

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ulikunitz/xz"
)

func xzBytes(t *testing.T, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := xz.WriterConfig{CheckSum: xz.CRC32}.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fakeELF(size int) []byte {
	b := make([]byte, size)
	copy(b, "\x7fELF-fake-frida-server")
	return b
}

// Auto-download: fetch, decompress, place under the name binary
// looks for; cached afterwards (one HTTP hit). Error paths fail closed.
func TestDownloadFridaServer(t *testing.T) {
	var hits atomic.Int32
	good := xzBytes(t, fakeELF(2<<20))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.HasSuffix(r.URL.Path, "/9.9.9/frida-server-9.9.9-android-x86_64.xz"):
			w.Write(good)
		case strings.Contains(r.URL.Path, "/1.0.0/"):
			w.Write(xzBytes(t, []byte("not an elf, just text padding padding padding")))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	old := fridaReleaseBase
	fridaReleaseBase = srv.URL
	defer func() { fridaReleaseBase = old }()

	d := &FridaServer{StateDir: t.TempDir(), AutoDownload: true}
	ctx := context.Background()
	path, err := d.download(ctx, "x86_64", "9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := filepath.Join("android", "frida-server", "frida-server-9.9.9-android-x86_64")
	if !strings.HasSuffix(path, wantSuffix) {
		t.Fatalf("path = %s, want suffix %s", path, wantSuffix)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 2<<20 || !bytes.HasPrefix(data, []byte{0x7f, 'E', 'L', 'F'}) {
		t.Fatalf("downloaded payload wrong: len=%d err=%v", len(data), err)
	}
	if p, v, ok := d.binary("x86_64", "9.9.9"); !ok || p != path || v != "9.9.9" {
		t.Fatalf("fridaBinary after download = %q %q %v", p, v, ok)
	}
	// Second call is a cache hit.
	if _, err := d.download(ctx, "x86_64", "9.9.9"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (cached)", hits.Load())
	}
	// 404 and non-ELF payloads fail closed, leaving no file.
	if _, err := d.download(ctx, "x86_64", "8.8.8"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404 case err = %v", err)
	}
	if _, err := d.download(ctx, "x86_64", "1.0.0"); err == nil || !strings.Contains(err.Error(), "ELF") {
		t.Fatalf("non-ELF case err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "frida-server-1.0.0-android-x86_64")); !os.IsNotExist(err) {
		t.Fatal("non-ELF payload left a file behind")
	}
}

// Real download from the Frida releases (needs network + host client).
// Gated: CRAGGY_TEST_FRIDA_DL=1.
func TestDownloadFridaServerReal(t *testing.T) {
	if os.Getenv("CRAGGY_TEST_FRIDA_DL") != "1" {
		t.Skip("set CRAGGY_TEST_FRIDA_DL=1 to download a real frida-server")
	}
	ver := hostFridaVersion()
	if ver == "" {
		t.Skip("host frida client not on PATH")
	}
	d := &FridaServer{StateDir: t.TempDir(), AutoDownload: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	path, err := d.download(ctx, "x86_64", ver)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() < 50<<20 {
		t.Fatalf("real frida-server suspiciously small: %d bytes", st.Size())
	}
	t.Logf("downloaded %s (%d bytes)", path, st.Size())
}

func TestNoFridaServerNamesTheSearchedDir(t *testing.T) {
	d := &FridaServer{StateDir: "state"}
	dir := filepath.Join("state", "android", "frida-server")
	if err := d.missingError("x86_64", "17.17.0"); err == nil || !strings.Contains(err.Error(), dir) ||
		!strings.Contains(err.Error(), "frida-server-17.17.0-android-x86_64") {
		t.Fatalf("missing binary message = %v, want the searched dir %s and the file name", err, dir)
	}
	if err := d.missingError("x86_64", ""); err == nil || !strings.Contains(err.Error(), "no version to fetch") {
		t.Fatalf("unknown version message = %v", err)
	}
}
