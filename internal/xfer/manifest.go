package xfer

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/craggy-io/environment-services/envspec"
)

// DeleteListName is the tar entry a delta push uses to carry the paths
// the receiving Transferrer must remove (one per line).
const DeleteListName = ".craggy-delete"

// LocalManifest builds a manifest of a local directory, hashing in place.
// It backs host environments and fs.changes.
func LocalManifest(root string) ([]envspec.Entry, error) {
	var entries []envspec.Entry
	if err := walkTree(root, func(e envspec.Entry) { entries = append(entries, e) }); err != nil {
		return nil, err
	}
	out := entries[:0]
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e)
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(e.Path))
		if e.Path == filepath.Base(root) && len(entries) == 1 {
			full = root
		}
		if entryIsSymlink(e) {
			target, err := os.Readlink(full)
			if err != nil {
				continue
			}
			e.Digest, _, _ = DigestOf(strings.NewReader(filepath.ToSlash(target)))
			out = append(out, e)
			continue
		}
		f, err := os.Open(full)
		if err != nil {
			continue
		}
		d, n, err := DigestOf(f)
		f.Close()
		if err != nil {
			return nil, err
		}
		e.Digest, e.Size = d, n
		out = append(out, e)
	}
	return out, nil
}
