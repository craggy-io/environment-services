package androiddev

// Live archive transfer: round-trips trees through a real device's tar
// over adb. Gated: CRAGGY_TEST_ANDROID_XFER=1, with CRAGGY_ADB_SERIAL
// naming the device (default emulator-5554). Works on emulators and
// physical devices; everything is written under /data/local/tmp and
// removed afterwards.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/internal/adb"
)

func liveTransferDevice(t *testing.T) (*Device, context.Context) {
	t.Helper()
	if os.Getenv("CRAGGY_TEST_ANDROID_XFER") != "1" {
		t.Skip("set CRAGGY_TEST_ANDROID_XFER=1 (and CRAGGY_ADB_SERIAL) to run against a real device")
	}
	serial := os.Getenv("CRAGGY_ADB_SERIAL")
	if serial == "" {
		serial = "emulator-5554"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return New(ClientTransport{C: adb.New()}, Config{Serial: serial}), ctx
}

// liveScratch returns a fresh absolute guest dir, removed at cleanup.
func liveScratch(t *testing.T, ctx context.Context, d *Device) string {
	t.Helper()
	dir := fmt.Sprintf("/data/local/tmp/craggy-xfer-live-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _, _ = d.Shell(context.Background(), "rm -rf "+adbQuote(dir)) })
	return dir
}

func liveTree() map[string][]byte {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	big := make([]byte, 4<<20)
	rand.New(rand.NewSource(7)).Read(big)
	return map[string][]byte{
		"a.txt":              []byte("hello device\n"),
		"sub/deep/bytes.bin": all,
		"with space/c d.txt": []byte("quoted path"),
		"empty.txt":          {},
		"big.bin":            big,
	}
}

func pullTree(t *testing.T, ctx context.Context, d *Device, src string) map[string][]byte {
	t.Helper()
	rc, err := d.GetArchive(ctx, src)
	if err != nil {
		t.Fatalf("get %s: %v", src, err)
	}
	files := untar(t, rc)
	if err := rc.Close(); err != nil {
		t.Fatalf("get %s: close: %v", src, err)
	}
	return files
}

func sameTree(t *testing.T, got, want map[string][]byte) {
	t.Helper()
	if len(got) != len(want) {
		names := make([]string, 0, len(got))
		for p := range got {
			names = append(names, p)
		}
		t.Fatalf("pulled %d files %v, want %d", len(got), names, len(want))
	}
	for p, w := range want {
		g, ok := got[p]
		if !ok || !bytes.Equal(g, w) {
			t.Fatalf("%s: got %d bytes (present=%v), want %d", p, len(g), ok, len(w))
		}
	}
}

func TestLiveArchiveRoundTrip(t *testing.T) {
	d, ctx := liveTransferDevice(t)
	root := liveScratch(t, ctx, d)
	tree := liveTree()

	start := time.Now()
	if err := d.PutArchive(ctx, root+"/tree", bytes.NewReader(tarOf(t, tree))); err != nil {
		t.Fatal(err)
	}
	pushed := time.Since(start)
	start = time.Now()
	sameTree(t, pullTree(t, ctx, d, root+"/tree"), tree)
	t.Logf("%s: push %s, pull %s (%d files, 4 MiB largest)", d.Serial(), pushed, time.Since(start), len(tree))

	// The guest really holds the bytes (not just a tar echo).
	if out, code, err := d.Shell(ctx, "wc -c < "+adbQuote(root+"/tree/big.bin")); err != nil || code != 0 || strings.TrimSpace(out) != "4194304" {
		t.Fatalf("guest size of big.bin = %q code=%d err=%v", out, code, err)
	}

	// A single file pulls as one entry named by its base name.
	sameTree(t, pullTree(t, ctx, d, root+"/tree/with space/c d.txt"), map[string][]byte{"c d.txt": []byte("quoted path")})

	// A second push overwrites in place and leaves other files alone.
	if err := d.PutArchive(ctx, root+"/tree", bytes.NewReader(tarOf(t, map[string][]byte{"a.txt": []byte("v2")}))); err != nil {
		t.Fatal(err)
	}
	tree["a.txt"] = []byte("v2")
	sameTree(t, pullTree(t, ctx, d, root+"/tree"), tree)
}

func TestLiveArchiveRelativePathUsesWorkspace(t *testing.T) {
	d, ctx := liveTransferDevice(t)
	rel := fmt.Sprintf("craggy-xfer-live-rel-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _, _ = d.Shell(context.Background(), "rm -rf "+adbQuote(d.guestPath(rel))) })
	if err := d.PutArchive(ctx, rel, bytes.NewReader(tarOf(t, map[string][]byte{"r.txt": []byte("rel")}))); err != nil {
		t.Fatal(err)
	}
	if out, code, err := d.Shell(ctx, "cat "+adbQuote(d.Workspace()+"/"+rel+"/r.txt")); err != nil || code != 0 || out != "rel" {
		t.Fatalf("workspace file = %q code=%d err=%v", out, code, err)
	}
	sameTree(t, pullTree(t, ctx, d, rel), map[string][]byte{"r.txt": []byte("rel")})
}

func TestLiveArchiveMissingSourceFails(t *testing.T) {
	d, ctx := liveTransferDevice(t)
	root := liveScratch(t, ctx, d)
	rc, err := d.GetArchive(ctx, root+"/does-not-exist")
	if err != nil {
		t.Fatalf("get: %v (want the failure from Close)", err)
	}
	_, _ = io.Copy(io.Discard, rc)
	if err := rc.Close(); err == nil {
		t.Fatal("pulling a missing path must fail")
	} else {
		t.Logf("missing source: %v", err)
	}
}
