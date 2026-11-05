package xfer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

const spoolThreshold = 1 << 20 // §5.4: <1MiB in memory, larger spooled

// WriteTar streams src as tar to w, straight from Source.Open with no temp
// file or Materialize copy (§5.2). Entries are written in stream order.
func WriteTar(ctx context.Context, src envspec.Source, w io.Writer, opts Options) (envspec.TransferStats, error) {
	return writeTarEntries(ctx, src, nil, w, opts, "push")
}

// WriteTarSubset streams only the named paths (delta push).
func WriteTarSubset(ctx context.Context, src envspec.Source, only map[string]bool, w io.Writer, opts Options) (envspec.TransferStats, error) {
	return writeTarEntries(ctx, src, only, w, opts, "push")
}

func writeTarEntries(ctx context.Context, src envspec.Source, only map[string]bool, w io.Writer, opts Options, op string) (envspec.TransferStats, error) {
	start := time.Now()
	var st envspec.TransferStats
	tw := tar.NewWriter(w)
	directories := map[string]bool{}
	for e, err := range src.Entries(ctx) {
		if err != nil {
			return st, err
		}
		if only != nil && !only[e.Path] && !e.IsDir() {
			continue
		}
		if err := ctx.Err(); err != nil {
			return st, err
		}
		if err := writeTarParents(tw, e.Path, directories); err != nil {
			return st, err
		}
		hdr, body, err := entryHeader(ctx, src, e)
		if err != nil {
			return st, err
		}
		if err := tw.WriteHeader(hdr); err != nil {
			if body != nil {
				body.Close()
			}
			return st, err
		}
		if e.IsDir() {
			directories[e.Path] = true
		}
		if body != nil {
			n, err := io.Copy(tw, body)
			body.Close()
			if err != nil {
				return st, err
			}
			st.Bytes += n
		}
		if !e.IsDir() {
			st.Files++
		}
		st.Elapsed = time.Since(start)
		opts.report(st, op)
	}
	if err := tw.Close(); err != nil {
		return st, err
	}
	st.Elapsed = time.Since(start)
	opts.report(st, op)
	return st, nil
}

// Explicit parents let the receiver apply its ownership policy to directories
// as well as files. Implicit mkdir during Docker extraction otherwise creates
// root-owned parents even when file ownership is mapped to the application.
func writeTarParents(tw *tar.Writer, name string, seen map[string]bool) error {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
		return fmt.Errorf("invalid transfer path %q", name)
	}
	var parents []string
	for p := path.Dir(name); p != "." && !seen[p]; p = path.Dir(p) {
		parents = append(parents, p)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		p := parents[i]
		if err := tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)}); err != nil {
			return err
		}
		seen[p] = true
	}
	return nil
}

func entryHeader(ctx context.Context, src envspec.Source, e envspec.Entry) (*tar.Header, io.ReadCloser, error) {
	hdr := &tar.Header{Name: e.Path, Mode: e.Mode & 0o7777, ModTime: time.Unix(0, 0)}
	switch {
	case e.IsDir():
		hdr.Typeflag = tar.TypeDir
		if hdr.Mode == 0 {
			hdr.Mode = 0o755
		}
		return hdr, nil, nil
	case entryIsSymlink(e):
		hdr.Typeflag = tar.TypeSymlink
		rc, err := src.Open(ctx, e)
		if err != nil {
			return nil, nil, err
		}
		defer rc.Close()
		target, err := io.ReadAll(io.LimitReader(rc, 64<<10))
		if err != nil {
			return nil, nil, err
		}
		hdr.Linkname = string(target)
		return hdr, nil, nil
	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = e.Size
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		rc, err := src.Open(ctx, e)
		if err != nil {
			return nil, nil, err
		}
		if e.Size < 0 {
			// Unknown size: buffer (sources should report sizes).
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return nil, nil, err
			}
			hdr.Size = int64(len(b))
			return hdr, io.NopCloser(bytes.NewReader(b)), nil
		}
		return hdr, rc, nil
	}
}

type pendingFile struct {
	path string
	mode int64
	data []byte   // small file
	file *os.File // spooled large file
	err  error
}

// ReadTar consumes a tar stream into sink. The reader is single-threaded
// (§5.4): small files buffer in memory, large ones spool to temp files,
// and adds fan out to workers only for a ConcurrentSink.
func ReadTar(ctx context.Context, r io.Reader, sink envspec.Sink, opts Options) (envspec.TransferStats, error) {
	start := time.Now()
	var st envspec.TransferStats
	var mu sync.Mutex
	tr := tar.NewReader(r)

	concurrent := false
	if cs, ok := sink.(ConcurrentSink); ok && cs.Concurrent() {
		concurrent = true
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = 8
	}
	var jobs chan pendingFile
	var wg sync.WaitGroup
	var firstErr error
	if concurrent {
		jobs = make(chan pendingFile, workers*2)
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for pf := range jobs {
					if err := deliver(sink, pf); err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
					}
				}
			}()
		}
	}
	emit := func(pf pendingFile) error {
		if jobs != nil {
			jobs <- pf
			return nil
		}
		return deliver(sink, pf)
	}

	for {
		if err := ctx.Err(); err != nil {
			if jobs != nil {
				close(jobs)
				wg.Wait()
			}
			return st, err
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if jobs != nil {
				close(jobs)
				wg.Wait()
			}
			return st, fmt.Errorf("xfer: tar read: %w", err)
		}
		name := cleanTarName(hdr.Name)
		if name == "" || name == "." {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue // directories are implicit in the Sink tree model
		case tar.TypeSymlink:
			if err := emit(pendingFile{path: name, mode: 0o120000, data: []byte(hdr.Linkname)}); err != nil {
				if jobs != nil {
					close(jobs)
					wg.Wait()
				}
				return st, err
			}
			st.Files++
		case tar.TypeReg, tar.TypeRegA:
			pf := pendingFile{path: name, mode: 0o100000 | (hdr.Mode & 0o7777)}
			if hdr.Size < spoolThreshold {
				b, err := io.ReadAll(tr)
				if err != nil {
					if jobs != nil {
						close(jobs)
						wg.Wait()
					}
					return st, err
				}
				pf.data = b
				st.Bytes += int64(len(b))
			} else {
				f, err := os.CreateTemp("", "craggy-xfer-*")
				if err != nil {
					if jobs != nil {
						close(jobs)
						wg.Wait()
					}
					return st, err
				}
				n, err := io.Copy(f, tr)
				if err != nil {
					f.Close()
					os.Remove(f.Name())
					if jobs != nil {
						close(jobs)
						wg.Wait()
					}
					return st, err
				}
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					f.Close()
					os.Remove(f.Name())
					if jobs != nil {
						close(jobs)
						wg.Wait()
					}
					return st, err
				}
				pf.file = f
				st.Bytes += n
			}
			if err := emit(pf); err != nil {
				if jobs != nil {
					close(jobs)
					wg.Wait()
				}
				return st, err
			}
			st.Files++
		default:
			// Skip exotic types rather than failing the whole transfer.
			st.Skipped++
		}
		st.Elapsed = time.Since(start)
		opts.report(st, "pull")
	}
	if jobs != nil {
		close(jobs)
		wg.Wait()
	}
	st.Elapsed = time.Since(start)
	opts.report(st, "pull")
	return st, firstErr
}

func deliver(sink envspec.Sink, pf pendingFile) error {
	if pf.file != nil {
		defer pf.file.Close()
		defer os.Remove(pf.file.Name())
		return sink.Add(pf.path, pf.mode, pf.file)
	}
	return sink.Add(pf.path, pf.mode, bytes.NewReader(pf.data))
}

func cleanTarName(name string) string {
	// Normalise "./a/b" and "a/b/" forms; reject escapes.
	for len(name) > 0 && (name[0] == '/' || name[0] == '.') {
		if len(name) > 1 && name[0] == '.' && name[1] != '/' {
			break
		}
		name = name[1:]
		if len(name) > 0 && name[0] == '/' {
			name = name[1:]
		}
	}
	if len(name) > 0 && name[len(name)-1] == '/' {
		name = name[:len(name)-1]
	}
	return name
}

// DigestOf hashes r as "sha256:<hex>".
func DigestOf(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", n, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

// Manifest lists src with digests filled in (hashing entries whose digest
// is unknown), sorted by path.
func Manifest(ctx context.Context, src envspec.Source) ([]envspec.Entry, error) {
	var out []envspec.Entry
	for e, err := range src.Entries(ctx) {
		if err != nil {
			return nil, err
		}
		if e.Digest == "" && !e.IsDir() {
			rc, err := src.Open(ctx, e)
			if err != nil {
				return nil, err
			}
			d, n, err := DigestOf(rc)
			rc.Close()
			if err != nil {
				return nil, err
			}
			e.Digest = d
			e.Size = n
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Diff computes a delta between source and destination manifests (§5.3):
// entries to send (new or changed by (path,digest,mode)) and, when mirror
// is set, destination paths to delete.
func Diff(src, dst []envspec.Entry, mirror bool) (send []envspec.Entry, del []string) {
	dstByPath := map[string]envspec.Entry{}
	for _, e := range dst {
		dstByPath[e.Path] = e
	}
	srcPaths := map[string]bool{}
	for _, e := range src {
		srcPaths[e.Path] = true
		d, ok := dstByPath[e.Path]
		if !ok || d.Digest != e.Digest || d.Mode != e.Mode || d.Size != e.Size {
			send = append(send, e)
		}
	}
	if mirror {
		for _, e := range dst {
			if !srcPaths[e.Path] && !e.IsDir() {
				del = append(del, e.Path)
			}
		}
		sort.Strings(del)
	}
	return send, del
}
