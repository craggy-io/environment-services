package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/moby/moby/client"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Exec runs through the attach API with multiplexed streams demuxed here.
func (i *Instance) Exec(ctx context.Context, op adapter.OpContext, c adapter.ExecCmd) (*adapter.Process, error) {
	i.mu.Lock()
	envv := []string{}
	for k, v := range i.credEnv {
		envv = append(envv, k+"="+v)
	}
	i.mu.Unlock()
	for k, v := range c.Env {
		envv = append(envv, k+"="+v)
	}
	dir := c.Dir
	if dir == "" {
		dir = "/work"
	} else if !strings.HasPrefix(dir, "/") {
		dir = "/work/" + dir
	}
	execCtx := ctx
	var cancel context.CancelFunc
	if c.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, c.Timeout)
	}
	stop := func() {
		if cancel != nil {
			cancel()
		}
	}
	created, err := i.ad.cli.ExecCreate(execCtx, i.id, client.ExecCreateOptions{
		User:         c.User,
		TTY:          c.TTY,
		AttachStdin:  c.Stdin != nil,
		AttachStdout: true,
		AttachStderr: !c.TTY,
		Env:          envv,
		WorkingDir:   dir,
		Cmd:          c.Argv,
	})
	if err != nil {
		stop()
		return nil, fmt.Errorf("docker: exec create: %w", engineErr(err))
	}
	att, err := i.ad.cli.ExecAttach(execCtx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		stop()
		return nil, fmt.Errorf("docker: exec attach: %w", engineErr(err))
	}
	hijacked := att.HijackedResponse
	if c.Stdin != nil {
		go func() {
			_, _ = io.Copy(hijacked.Conn, c.Stdin)
			_ = hijacked.CloseWrite()
		}()
	}
	var stdout, stderr io.Reader
	var stdin io.WriteCloser
	demuxDone := make(chan struct{})
	if c.TTY {
		stdout = hijacked.Reader
		close(demuxDone)
	} else {
		outR, outW := io.Pipe()
		errR, errW := io.Pipe()
		go func() {
			defer close(demuxDone)
			derr := demuxStdStream(hijacked.Reader, outW, errW)
			_ = outW.CloseWithError(derr)
			_ = errW.CloseWithError(derr)
		}()
		stdout, stderr = outR, errR
	}
	if c.Stdin == nil {
		stdin = connWriter{&hijacked}
	}
	wait := func() (int, error) {
		defer stop()
		// Poll inspect until the exec stops running, then let the
		// demuxer drain to EOF before closing: closing the hijack
		// first truncates buffered output still in flight.
		for {
			insp, err := i.ad.cli.ExecInspect(context.Background(), created.ID, client.ExecInspectOptions{})
			if err != nil {
				hijacked.Close()
				return 0, engineErr(err)
			}
			if !insp.Running {
				select {
				case <-demuxDone:
				case <-time.After(10 * time.Second):
				}
				hijacked.Close()
				return insp.ExitCode, nil
			}
			select {
			case <-execCtx.Done():
				hijacked.Close()
				return 0, execCtx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return adapter.NewProcess(stdout, stderr, stdin, wait, func(sig string) error {
		// Docker exec has no signal API; best effort is closing the hijack.
		hijacked.Close()
		return nil
	}), nil
}

type connWriter struct{ h *client.HijackedResponse }

func (w connWriter) Write(p []byte) (int, error) { return w.h.Conn.Write(p) }
func (w connWriter) Close() error                { return w.h.CloseWrite() }

// execCapture runs a short command to completion (adapter-internal).
func (i *Instance) execCapture(ctx context.Context, op adapter.OpContext, argv []string, user string) (stdout, stderr []byte, code int, err error) {
	p, err := i.Exec(ctx, op, adapter.ExecCmd{Argv: argv, User: user})
	if err != nil {
		return nil, nil, 0, err
	}
	var outBuf, errBuf bytes.Buffer
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&outBuf, p.Stdout); done <- struct{}{} }()
	go func() {
		if p.Stderr != nil {
			_, _ = io.Copy(&errBuf, p.Stderr)
		}
		done <- struct{}{}
	}()
	code, err = p.Wait()
	<-done
	<-done
	return outBuf.Bytes(), errBuf.Bytes(), code, err
}

// --- Transferrer (docker archive API) ---

func (i *Instance) PutArchive(ctx context.Context, op adapter.OpContext, dst string, tarStream io.Reader) error {
	// Docker extracts into an existing dir; create it first.
	_, stderr, code, err := i.execCapture(ctx, op, []string{"mkdir", "-p", dst}, "")
	if err != nil || code != 0 {
		return fmt.Errorf("docker: prepare archive destination %s (exit %d): %s: %v", dst, code, strings.TrimSpace(string(stderr)), err)
	}
	_, err = i.ad.cli.CopyToContainer(ctx, i.id, client.CopyToContainerOptions{
		// Docker resolves Config.User and applies that ownership to
		// extraction. Archive headers from storage have no UID/GID;
		// leaving this false makes restored mutable application
		// projects root-owned and unwritable.
		DestinationPath: dst, Content: tarStream, AllowOverwriteDirWithFile: true, CopyUIDGID: true,
	})
	return engineErr(err)
}

func (i *Instance) GetArchive(ctx context.Context, op adapter.OpContext, src string) (io.ReadCloser, error) {
	res, err := i.ad.cli.CopyFromContainer(ctx, i.id, client.CopyFromContainerOptions{SourcePath: src})
	if err != nil {
		return nil, engineErr(err)
	}
	// Docker's archive includes the copied path's base name; the
	// Transferrer contract (and the core's pull) expects entries
	// relative to src, so normalise here in the owning adapter.
	return stripTopDir(res.Content, src), nil
}

type stripReadCloser struct {
	io.Reader
	pipe *io.PipeReader
	src  io.ReadCloser
}

func (s *stripReadCloser) Close() error {
	_ = s.pipe.Close()
	return s.src.Close()
}

// stripTopDir rewrites a tar stream: archives made from a path
// include the base name (or "."), which should not appear in sink
// trees. Kept simple by streaming through a pipe.
func stripTopDir(r io.ReadCloser, srcPath string) io.ReadCloser {
	base := strings.TrimRight(srcPath, "/")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == "" || base == "." {
		return r
	}
	pr, pw := io.Pipe()
	go func() {
		tr := tar.NewReader(r)
		tw := tar.NewWriter(pw)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			name := hdr.Name
			if name == base || name == base+"/" {
				// Keep a lone file under its own name; drop a lone dir header.
				if hdr.Typeflag == tar.TypeDir {
					continue
				}
			} else if strings.HasPrefix(name, base+"/") {
				// Directory archive: sink trees are relative to the
				// pulled root, so strip the base prefix.
				name = strings.TrimPrefix(name, base+"/")
			}
			nh := *hdr
			nh.Name = name
			if err := tw.WriteHeader(&nh); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := io.Copy(tw, tr); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		_ = tw.Close()
		_ = pw.Close()
	}()
	return &stripReadCloser{Reader: pr, pipe: pr, src: r}
}

// --- CredInjector ---

// InjectEnv stores env vars applied on exec, never baked into config.
func (i *Instance) InjectEnv(name, value string) {
	i.mu.Lock()
	i.credEnv[name] = value
	i.mu.Unlock()
}

// InjectFile writes a tmpfs secret file under /run/secrets, mode 0400.
func (i *Instance) InjectFile(ctx context.Context, path string, value []byte) error {
	if !strings.HasPrefix(path, "/run/secrets/") {
		path = "/run/secrets/" + path[strings.LastIndex(path, "/")+1:]
	}
	dir := path[:strings.LastIndex(path, "/")]
	name := path[strings.LastIndex(path, "/")+1:]
	var buf bytes.Buffer
	if _, err := xfer.WriteTar(ctx, envspec.MemSource(map[string][]byte{name: value}), &buf, xfer.Options{}); err != nil {
		return err
	}
	if _, err := i.ad.cli.CopyToContainer(ctx, i.id, client.CopyToContainerOptions{
		DestinationPath: dir, Content: &buf, AllowOverwriteDirWithFile: true,
	}); err != nil {
		return engineErr(err)
	}
	if p, err := i.Exec(ctx, adapter.OpContext{EnvID: i.envID}, adapter.ExecCmd{Argv: []string{"chmod", "400", path}, User: "root"}); err == nil {
		drainWait(p)
	}
	return nil
}

var (
	_ adapter.Adapter           = (*Adapter)(nil)
	_ adapter.Creator           = (*Adapter)(nil)
	_ adapter.Checker           = (*Adapter)(nil)
	_ adapter.BackendStarter    = (*Adapter)(nil)
	_ adapter.CapacityReporter  = (*Adapter)(nil)
	_ adapter.ImageBuilder      = (*Adapter)(nil)
	_ adapter.Coster            = (*Adapter)(nil)
	_ adapter.Instance          = (*Instance)(nil)
	_ adapter.Executor          = (*Instance)(nil)
	_ adapter.Transferrer       = (*Instance)(nil)
	_ adapter.CredInjector      = (*Instance)(nil)
	_ adapter.CollectorProvider = (*Instance)(nil)
	_ engine                    = (*client.Client)(nil)
)
