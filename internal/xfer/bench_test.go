package xfer_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// The transfer benchmark: a synthetic 80,000-file tree through the tar
// pipeline into a ConcurrentSink, plus a 1%-change delta. Run with:
//
//	go test ./xfer -run TestXfer80kGate -v
//
// Gates (on this machine class): full push < 30s, full pull < 60s, delta
// manifest+diff for 1% < 5s. The assertion is loose enough for CI while
// still catching per-file round-trip regressions; timings are logged.
func TestXfer80kGate(t *testing.T) {
	if testing.Short() {
		t.Skip("80k gate skipped in -short")
	}
	ctx := context.Background()
	files := make(map[string][]byte, 80000)
	var total int64
	for i := 0; i < 80000; i++ {
		want := 32 + (i*37)%1500
		pat := []byte(fmt.Sprintf("file-%d-", i))
		seed := bytes.Repeat(pat, want/len(pat)+2)
		content := seed[:want]
		files[fmt.Sprintf("src/pkg%03d/mod%03d/F%05d.java", i/1000, (i/100)%10, i)] = content
		total += int64(len(content))
	}
	src := envspec.MemSource(files)

	t0 := time.Now()
	var buf bytes.Buffer
	st, err := xfer.WriteTar(ctx, src, &buf, xfer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pushElapsed := time.Since(t0)
	t.Logf("full push (tar write): %d files %d bytes in %s", st.Files, st.Bytes, pushElapsed)
	if st.Files != 80000 {
		t.Fatalf("push files = %d", st.Files)
	}
	if pushElapsed > 30*time.Second {
		t.Fatalf("gate: full push took %s (>30s)", pushElapsed)
	}

	t0 = time.Now()
	sink := envspec.NewMemSink()
	st, err = xfer.ReadTar(ctx, &buf, sink, xfer.Options{Workers: 16})
	if err != nil {
		t.Fatal(err)
	}
	pullElapsed := time.Since(t0)
	t.Logf("full pull (concurrent sink): %d files %d bytes in %s", st.Files, st.Bytes, pullElapsed)
	if len(sink.Files) != 80000 {
		t.Fatalf("pulled %d files", len(sink.Files))
	}
	if pullElapsed > 60*time.Second {
		t.Fatalf("gate: full pull took %s (>60s)", pullElapsed)
	}

	// Delta with 1% changed.
	t0 = time.Now()
	srcMan, err := xfer.Manifest(ctx, sink.Source())
	if err != nil {
		t.Fatal(err)
	}
	changed := make(map[string][]byte, len(files))
	n := 0
	for k, v := range files {
		changed[k] = v
		n++
		if n <= 800 {
			changed[k] = append(append([]byte{}, v...), '!')
		}
	}
	dstMan, err := xfer.Manifest(ctx, envspec.MemSource(changed))
	if err != nil {
		t.Fatal(err)
	}
	send, del := xfer.Diff(dstMan, srcMan, true)
	deltaElapsed := time.Since(t0)
	if len(send) < 790 || len(send) > 810 {
		t.Fatalf("delta send set = %d, want ~800", len(send))
	}
	if len(del) != 0 {
		t.Fatalf("delta deletes = %v", del)
	}
	var deltaBuf bytes.Buffer
	only := map[string]bool{}
	for _, e := range send {
		only[e.Path] = true
	}
	if _, err := xfer.WriteTarSubset(ctx, envspec.MemSource(changed), only, &deltaBuf, xfer.Options{}); err != nil {
		t.Fatal(err)
	}
	deltaElapsed = time.Since(t0)
	t.Logf("delta (manifest+diff+subset tar, 1%% changed): %d files in %s", len(send), deltaElapsed)
	if deltaElapsed > 15*time.Second {
		t.Fatalf("delta path too slow: %s", deltaElapsed)
	}
}

func BenchmarkWriteTar(b *testing.B) {
	ctx := context.Background()
	files := map[string][]byte{}
	for i := 0; i < 2000; i++ {
		files[fmt.Sprintf("d/f%04d.txt", i)] = bytes.Repeat([]byte("x"), 300)
	}
	src := envspec.MemSource(files)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		if _, err := xfer.WriteTar(ctx, src, &buf, xfer.Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
