package envspec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"iter"
	"sort"
	"sync"
	"time"
)

// TransferStats counts a completed (or in-progress) transfer.
type TransferStats struct {
	Files        int64         `json:"files"`
	Bytes        int64         `json:"bytes"`
	Skipped      int64         `json:"skipped"`
	SkippedBytes int64         `json:"skipped_bytes"`
	Elapsed      time.Duration `json:"elapsed"`
}

// digestOf hashes b as "sha256:<hex>".
func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MemSink collects files in memory: Files maps path to content (symlink
// targets as content). Add is safe for concurrent use; Has/AddDigest let a
// transfer skip bytes the sink already holds by digest.
type MemSink struct {
	mu    sync.Mutex
	Files map[string][]byte
	Modes map[string]int64
}

// NewMemSink builds an empty MemSink.
func NewMemSink() *MemSink {
	return &MemSink{Files: map[string][]byte{}, Modes: map[string]int64{}}
}

func (m *MemSink) Add(path string, mode int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Files[path] = b
	m.Modes[path] = mode
	return nil
}

// Concurrent reports that Add is safe for concurrent use.
func (m *MemSink) Concurrent() bool { return true }

// Has reports whether the sink already holds content with this digest.
func (m *MemSink) Has(digest string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.Files {
		if digestOf(b) == digest {
			return true
		}
	}
	return false
}

// AddDigest records path as held content identified by digest, without
// moving bytes. It fails when the sink holds no such content.
func (m *MemSink) AddDigest(path string, mode int64, digest string, size int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for p, b := range m.Files {
		if digestOf(b) == digest {
			m.Files[path] = m.Files[p]
			m.Modes[path] = mode
			return nil
		}
	}
	return fmt.Errorf("envspec: digest %s not held", digest)
}

// Source returns this sink's contents as a Source.
func (m *MemSink) Source() Source {
	m.mu.Lock()
	defer m.mu.Unlock()
	files := make(map[string][]byte, len(m.Files))
	modes := make(map[string]int64, len(m.Modes))
	for k, v := range m.Files {
		files[k] = append([]byte(nil), v...)
	}
	for k, v := range m.Modes {
		modes[k] = v
	}
	return &memSource{files: files, modes: modes}
}

type memSource struct {
	files map[string][]byte
	modes map[string]int64
}

// MemSource builds a Source from a path-to-content map (mode 0o100644).
func MemSource(files map[string][]byte) Source {
	modes := make(map[string]int64, len(files))
	for k := range files {
		modes[k] = 0o100644
	}
	return &memSource{files: files, modes: modes}
}

func (m *memSource) Entries(ctx context.Context) iter.Seq2[Entry, error] {
	return func(yield func(Entry, error) bool) {
		names := make([]string, 0, len(m.files))
		for n := range m.files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			mode := m.modes[n]
			if mode == 0 {
				mode = 0o100644
			}
			if !yield(Entry{Path: n, Mode: mode, Size: int64(len(m.files[n]))}, nil) {
				return
			}
		}
	}
}

func (m *memSource) Open(ctx context.Context, e Entry) (io.ReadCloser, error) {
	b, ok := m.files[e.Path]
	if !ok {
		return nil, fmt.Errorf("envspec: no such entry %q", e.Path)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
