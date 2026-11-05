package xfer

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/craggy-io/environment-services/envspec"
)

// --- DirSource / DirSink (local folder; tests and core's own use) ---

type dirSource struct{ root string }

// DirSource exposes a local folder as a Source.
func DirSource(path string) envspec.Source { return dirSource{root: path} }

func (d dirSource) Entries(ctx context.Context) iter.Seq2[envspec.Entry, error] {
	return func(yield func(envspec.Entry, error) bool) {
		var entries []envspec.Entry
		err := walkTree(d.root, func(e envspec.Entry) { entries = append(entries, e) })
		if err != nil {
			yield(envspec.Entry{}, err)
			return
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		for _, e := range entries {
			if !yield(e, nil) {
				return
			}
		}
	}
}

func (d dirSource) Open(ctx context.Context, e envspec.Entry) (io.ReadCloser, error) {
	full := filepath.Join(d.root, filepath.FromSlash(e.Path))
	if entryIsSymlink(e) {
		target, err := os.Readlink(full)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(strings.NewReader(filepath.ToSlash(target))), nil
	}
	return os.Open(full)
}

// walkTree emits entries for a local tree (shared with the agent + fschanges).
func walkTree(root string, emit func(envspec.Entry)) error {
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		emit(entryFor(root, filepath.Base(root), info))
		return nil
	}
	return filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := de.Info()
		if err != nil {
			return err
		}
		// WalkDir's Info follows no links for symlink entries; ensure lstat mode.
		if de.Type()&fs.ModeSymlink != 0 {
			emit(envspec.Entry{Path: filepath.ToSlash(rel), Mode: 0o120000, Size: fi.Size()})
			return nil
		}
		emit(entryFor(p, filepath.ToSlash(rel), fi))
		return nil
	})
}

func entryFor(full, rel string, fi fs.FileInfo) envspec.Entry {
	mode := int64(fi.Mode().Perm())
	switch {
	case fi.IsDir():
		mode |= 0o040000
	case fi.Mode()&fs.ModeSymlink != 0:
		mode = 0o120000
	default:
		mode |= 0o100000
	}
	return envspec.Entry{Path: rel, Mode: mode, Size: fi.Size()}
}

type dirSink struct {
	root string
	mu   sync.Mutex
}

// DirSink writes into a local folder. It is a ConcurrentSink and a
// DigestSink (present-and-matching files are left alone).
func DirSink(path string) envspec.Sink { return &dirSink{root: path} }

func (d *dirSink) localPath(p string) (string, error) {
	clean := filepath.Clean("/" + filepath.FromSlash(p))
	if strings.Contains(clean, "..") {
		return "", fmt.Errorf("xfer: path %q escapes sink", p)
	}
	return filepath.Join(d.root, clean), nil
}

func (d *dirSink) Add(path string, mode int64, r io.Reader) error {
	full, err := d.localPath(path)
	if err != nil {
		return err
	}
	e := envspec.Entry{Path: path, Mode: mode}
	if entryIsSymlink(e) {
		target, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		_ = os.Remove(full)
		return os.Symlink(string(target), full)
	}
	d.mu.Lock()
	err = os.MkdirAll(filepath.Dir(full), 0o755)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".xfer-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	perm := os.FileMode(mode & 0o7777)
	if perm == 0 {
		perm = 0o644
	}
	_ = os.Chmod(tmp.Name(), perm)
	if err := os.Rename(tmp.Name(), full); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func (d *dirSink) Concurrent() bool { return true }

func (d *dirSink) Has(digest string) bool { return false } // per-path check below

func (d *dirSink) AddDigest(path string, mode int64, digest string, size int64) error {
	full, err := d.localPath(path)
	if err != nil {
		return err
	}
	// Only accept if the file already there matches the digest.
	f, err := os.Open(full)
	if err != nil {
		return fmt.Errorf("xfer: AddDigest %s: %w", path, err)
	}
	defer f.Close()
	got, _, err := DigestOf(f)
	if err != nil {
		return err
	}
	if got != digest {
		return fmt.Errorf("xfer: AddDigest %s: digest mismatch", path)
	}
	return nil
}

// SingleFileSource is a one-entry Source (a blob).
func SingleFileSource(name string, content []byte) envspec.Source {
	return envspec.MemSource(map[string][]byte{name: content})
}
