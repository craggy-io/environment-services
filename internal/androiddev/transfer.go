package androiddev

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Archive transfer streams tar through the device's own tar over the
// binary-safe adb shell (shell v2: no pty, separate stderr, exit code).
// The path arrives as $1 so it is never re-quoted inside the script.
const (
	putArchiveScript = `mkdir -p "$1" && tar -x -f - -C "$1"`
	getArchiveScript = `if [ -d "$1" ]; then cd "$1" && tar -c -f - .; ` +
		`else cd "$(dirname "$1")" && tar -c -f - "$(basename "$1")"; fi`
)

// archiveTrailerMax bounds how much unread tar output Close drains (the
// end-of-archive padding) before it stops the guest tar instead.
const archiveTrailerMax = 1 << 20

// guestPath resolves a relative guest path against the workspace.
func (d *Device) guestPath(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return d.Workspace() + "/" + p
}

// PutArchive extracts a tar stream into dst, creating it first.
func (d *Device) PutArchive(ctx context.Context, dst string, tarStream io.Reader) error {
	dst = d.guestPath(dst)
	proc, err := d.ExecArgv(ctx, []string{"sh", "-c", putArchiveScript, "craggy-tar", dst}, "", "", nil, tarStream, 0)
	if err != nil {
		return fmt.Errorf("android: extract into %s: exec: %w", dst, err)
	}
	stderr := collectStderr(proc.Stderr)
	_, _ = io.Copy(io.Discard, proc.Stdout)
	code, werr := proc.Wait()
	if err := exitStatusError(code, werr, stderr()); err != nil {
		return fmt.Errorf("android: extract into %s: %w", dst, err)
	}
	return nil
}

// GetArchive streams src as tar: a directory's entries are relative to
// it, a single file is one entry named by its base name. Close reports a
// failed guest tar.
func (d *Device) GetArchive(ctx context.Context, src string) (io.ReadCloser, error) {
	src = d.guestPath(src)
	proc, err := d.ExecArgv(ctx, []string{"sh", "-c", getArchiveScript, "craggy-tar", src}, "", "", nil, nil, 0)
	if err != nil {
		return nil, fmt.Errorf("android: archive %s: exec: %w", src, err)
	}
	return &guestArchive{proc: proc, stderr: collectStderr(proc.Stderr), src: src}, nil
}

// guestArchive is the stdout of a guest `tar -c`.
type guestArchive struct {
	proc   *guestProc
	stderr func() string
	src    string
}

func (a *guestArchive) Read(p []byte) (int, error) { return a.proc.Stdout.Read(p) }

// Close drains the end-of-archive trailer the tar reader leaves unread
// (stopping the guest tar if far more remains) and reports its exit.
func (a *guestArchive) Close() error {
	if n, _ := io.CopyN(io.Discard, a.proc.Stdout, archiveTrailerMax); n == archiveTrailerMax {
		_ = a.proc.Kill()
	}
	code, werr := a.proc.Wait()
	if err := exitStatusError(code, werr, a.stderr()); err != nil {
		return fmt.Errorf("android: archive %s: %w", a.src, err)
	}
	return nil
}
