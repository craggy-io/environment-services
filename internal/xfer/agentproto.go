package xfer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/craggy-io/environment-services/envspec"
)

// The craggy-xfer helper protocol (§5.3). One invocation handles one
// command over stdin/stdout:
//
//	manifest <root>          → entry lines, then "."
//	recv <root> [mirror]     ← tar stream until EOF; a ".craggy-delete"
//	                           entry in the tar lists paths to remove
//	send <root>              ← path lines until "."; → tar of just those
//
// Entry line: "<mode-octal> <size> <digest|-> <path>".

const DeleteListName = ".craggy-delete"

// FormatEntry renders one manifest line.
func FormatEntry(e envspec.Entry) string {
	d := e.Digest
	if d == "" {
		d = "-"
	}
	return fmt.Sprintf("%o %d %s %s", e.Mode, e.Size, d, e.Path)
}

// ParseEntry parses one manifest line.
func ParseEntry(line string) (envspec.Entry, error) {
	parts := strings.SplitN(strings.TrimSpace(line), " ", 4)
	if len(parts) < 4 {
		return envspec.Entry{}, fmt.Errorf("xfer: bad manifest line %q", line)
	}
	mode, err := strconv.ParseInt(parts[0], 8, 64)
	if err != nil {
		return envspec.Entry{}, err
	}
	size, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return envspec.Entry{}, err
	}
	e := envspec.Entry{Path: parts[3], Mode: mode, Size: size}
	if parts[2] != "-" {
		e.Digest = parts[2]
	}
	return e, nil
}

// LocalManifest builds a manifest of a local directory, hashing in place.
// It backs the agent, host environments (no helper needed) and fs.changes.
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

// AgentManifest runs `manifest <root>` in an environment via execFn. argv
// is the helper invocation prefix, e.g. []string{"/data/local/tmp/craggy-xfer"}.
func AgentManifest(ctx context.Context, execFn ExecFunc, argv []string, root string) ([]envspec.Entry, error) {
	stdout, wait, err := execFn(ctx, append(append([]string{}, argv...), "manifest", root), nil)
	if err != nil {
		return nil, err
	}
	var out []envspec.Entry
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "." {
			break
		}
		if line == "" {
			continue
		}
		e, err := ParseEntry(line)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if code, err := wait(); err != nil || code != 0 {
		return nil, fmt.Errorf("xfer: agent manifest exit %d: %v", code, err)
	}
	return out, nil
}

// AgentRecv streams a delta tar into the agent's recv command.
func AgentRecv(ctx context.Context, execFn ExecFunc, argv []string, root string, mirror bool, tarStream io.Reader) error {
	args := append(append([]string{}, argv...), "recv", root)
	if mirror {
		args = append(args, "mirror")
	}
	_, wait, err := execFn(ctx, args, tarStream)
	if err != nil {
		return err
	}
	if code, err := wait(); err != nil || code != 0 {
		return fmt.Errorf("xfer: agent recv exit %d: %v", code, err)
	}
	return nil
}

// AgentSend requests just the named paths and returns the tar stream.
func AgentSend(ctx context.Context, execFn ExecFunc, argv []string, root string, paths []string) (io.Reader, func() (int, error), error) {
	var list strings.Builder
	for _, p := range paths {
		list.WriteString(p)
		list.WriteByte('\n')
	}
	list.WriteString(".\n")
	stdout, wait, err := execFn(ctx, append(append([]string{}, argv...), "send", root), strings.NewReader(list.String()))
	if err != nil {
		return nil, nil, err
	}
	return stdout, wait, nil
}
