// Command craggy-xfer is the in-environment helper for fast manifests and
// delta transfer (design §5.3). It is a small static Go binary built for
// linux/amd64 and linux/arm64, embedded by the xfer package and injected
// on first use (on Android it runs from /data/local/tmp).
//
// Usage: craggy-xfer manifest <root> | recv <root> [mirror] | send <root>
package main

import (
	"archive/tar"
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/craggy-io/environment-services/internal/xfer"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: craggy-xfer manifest|recv|send <root> [mirror]")
		os.Exit(2)
	}
	cmd, root := os.Args[1], os.Args[2]
	var err error
	switch cmd {
	case "manifest":
		err = doManifest(root)
	case "recv":
		err = doRecv(root)
	case "send":
		err = doSend(root)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "craggy-xfer:", err)
		os.Exit(1)
	}
}

func doManifest(root string) error {
	entries, err := xfer.LocalManifest(root)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	for _, e := range entries {
		fmt.Fprintln(w, xfer.FormatEntry(e))
	}
	fmt.Fprintln(w, ".")
	return nil
}

func doRecv(root string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	_, err := xfer.ReadTar(context.Background(), os.Stdin, &recvSink{root: root}, xfer.Options{})
	return err
}

// recvSink is a DirSink that honours the delete-list entry.
type recvSink struct{ root string }

func (s *recvSink) Add(path string, mode int64, r io.Reader) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if path == xfer.DeleteListName {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			full := filepath.Join(s.root, filepath.FromSlash(line))
			if strings.HasPrefix(filepath.Clean(full), filepath.Clean(s.root)) {
				_ = os.Remove(full)
			}
		}
		return nil
	}
	return xfer.DirSink(s.root).Add(path, mode, strings.NewReader(string(b)))
}

func doSend(root string) error {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var paths []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "." {
			break
		}
		if line != "" {
			paths = append(paths, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	tw := tar.NewWriter(os.Stdout)
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		fi, err := os.Lstat(full)
		if err != nil {
			continue // vanished between manifest and send; delta is best-effort
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = p
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				continue
			}
			hdr.Linkname = filepath.ToSlash(target)
			hdr.Typeflag = tar.TypeSymlink
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			f, err := os.Open(full)
			if err != nil {
				continue
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
	}
	return tw.Close()
}
