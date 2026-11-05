package hostenv

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// mapPath turns a caller path into a host path. Only /work-rooted or
// relative paths are accepted, so host paths never leak.
func (i *Instance) mapPath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	var rel string
	switch {
	case p == "/work" || p == "/work/":
		return i.workspace, nil
	case strings.HasPrefix(p, "/work/"):
		rel = p[len("/work/"):]
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("hostenv: path %q is outside the workspace; use /work…", p)
	default:
		rel = p
	}
	full := filepath.Join(i.workspace, filepath.FromSlash(rel))
	cleanFull := filepath.Clean(full)
	cleanWS := filepath.Clean(i.workspace)
	if cleanFull != cleanWS && !strings.HasPrefix(cleanFull, cleanWS+string(os.PathSeparator)) {
		return "", fmt.Errorf("hostenv: path %q escapes the workspace", p)
	}
	return cleanFull, nil
}

func (i *Instance) buildEnv(c adapter.ExecCmd) []string {
	envmap := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		envmap[strings.ToUpper(k)] = v
	}
	var extra map[string]string
	var pathPrepend []string
	if i.spec.Host != nil {
		extra = i.spec.Host.Env
		for _, p := range i.spec.Host.Path {
			pathPrepend = append(pathPrepend, os.ExpandEnv(p))
		}
	}
	for k, v := range extra {
		envmap[strings.ToUpper(k)] = os.ExpandEnv(v)
	}
	i.mu.Lock()
	for k, v := range i.credEnv {
		envmap[strings.ToUpper(k)] = v
	}
	i.mu.Unlock()
	for k, v := range c.Env {
		envmap[strings.ToUpper(k)] = v
	}
	if len(pathPrepend) > 0 {
		envmap["PATH"] = strings.Join(pathPrepend, string(os.PathListSeparator)) +
			string(os.PathListSeparator) + envmap["PATH"]
	}
	out := make([]string, 0, len(envmap))
	for k, v := range envmap {
		out = append(out, k+"="+v)
	}
	return out
}

func (i *Instance) lookPath(name string, environ []string) (string, error) {
	if filepath.IsAbs(name) || strings.ContainsAny(name, `/\`) {
		return name, nil
	}
	var pathVal string
	for _, kv := range environ {
		if strings.HasPrefix(strings.ToUpper(kv), "PATH=") {
			pathVal = kv[5:]
		}
	}
	for _, dir := range filepath.SplitList(pathVal) {
		if dir == "" {
			continue
		}
		for _, ext := range executableExts(name) {
			cand := filepath.Join(dir, name+ext)
			if fi, err := os.Stat(cand); err == nil && !fi.IsDir() {
				return cand, nil
			}
		}
	}
	return osexec.LookPath(name)
}

func executableExts(name string) []string {
	if runtime.GOOS != "windows" {
		return []string{""}
	}
	if filepath.Ext(name) != "" {
		return []string{""}
	}
	exts := []string{".exe", ".bat", ".cmd", ".com"}
	if v := os.Getenv("PATHEXT"); v != "" {
		exts = nil
		for _, e := range strings.Split(v, ";") {
			exts = append(exts, strings.ToLower(e))
		}
	}
	return append([]string{""}, exts...)
}

// Exec runs any command as the manager's user, in the job and
// workspace. ExecCmd.User is ignored (host processes run as us).
func (i *Instance) Exec(ctx context.Context, op adapter.OpContext, c adapter.ExecCmd) (*adapter.Process, error) {
	if len(c.Argv) == 0 {
		return nil, fmt.Errorf("hostenv: empty argv")
	}
	environ := i.buildEnv(c)
	bin, err := i.lookPath(c.Argv[0], environ)
	if err != nil {
		return nil, fmt.Errorf("hostenv: find %q: %w", c.Argv[0], err)
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
	cmd := osexec.Command(bin, c.Argv[1:]...)
	cmd.Env = environ
	workdir := i.workspace
	if c.Dir != "" {
		if mapped, err := i.mapPath(c.Dir); err == nil {
			workdir = mapped
		}
	}
	cmd.Dir = workdir
	cmd.SysProcAttr = sysProcAttr()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stop()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stop()
		return nil, err
	}
	var stdin io.WriteCloser
	if c.Stdin != nil {
		w, err := cmd.StdinPipe()
		if err != nil {
			stop()
			return nil, err
		}
		stdin = w
		go func() {
			_, _ = io.Copy(w, c.Stdin)
			_ = w.Close()
		}()
	}
	if err := cmd.Start(); err != nil {
		stop()
		return nil, err
	}
	_ = i.job.assign(cmd)
	i.recordProc(cmd.Process.Pid)
	i.mu.Lock()
	i.running[cmd.Process.Pid] = cmd
	i.mu.Unlock()

	wait := func() (int, error) {
		werr := cmd.Wait()
		i.mu.Lock()
		delete(i.running, cmd.Process.Pid)
		i.mu.Unlock()
		stop()
		code := cmd.ProcessState.ExitCode()
		if code < 0 {
			code = 1
		}
		if werr != nil && cmd.ProcessState.Exited() {
			werr = nil
		}
		return code, werr
	}
	// Context cancellation kills only this process.
	go func() {
		<-execCtx.Done()
		if cmd.Process != nil && cmd.ProcessState == nil {
			_ = killProcess(cmd.Process.Pid)
		}
	}()
	p := adapter.NewProcess(stdout, stderr, stdin, wait, func(sig string) error {
		if cmd.Process == nil {
			return nil
		}
		return killProcess(cmd.Process.Pid)
	})
	p.SetPID(cmd.Process.Pid)
	return p, nil
}

func (i *Instance) recordProc(pid int) {
	rec := procRec{PID: pid}
	if t, ok := processCreationTime(pid); ok {
		rec.Created = t.UnixNano()
	}
	i.mu.Lock()
	i.procs = append(i.procs, rec)
	i.mu.Unlock()
	_ = i.saveState()
}

// --- Transferrer (local tar) / Manifestor / FSLister / CredInjector ---

func (i *Instance) PutArchive(ctx context.Context, op adapter.OpContext, dst string, tarStream io.Reader) error {
	full, err := i.mapPath(dst)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(full, 0o755); err != nil {
		return err
	}
	_, err = xfer.ReadTar(ctx, tarStream, &hostRecvSink{root: full}, xfer.Options{})
	return err
}

type hostRecvSink struct{ root string }

func (s *hostRecvSink) Add(path string, mode int64, r io.Reader) error {
	if path == xfer.DeleteListName {
		b, _ := io.ReadAll(r)
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
	return xfer.DirSink(s.root).Add(path, mode, r)
}

func (i *Instance) GetArchive(ctx context.Context, op adapter.OpContext, src string) (io.ReadCloser, error) {
	full, err := i.mapPath(src)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	var source envspec.Source
	if fi.IsDir() {
		source = xfer.DirSource(full)
	} else {
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		source = envspec.MemSource(map[string][]byte{filepath.Base(full): data})
	}
	var buf bytes.Buffer
	if _, err := xfer.WriteTar(ctx, source, &buf, xfer.Options{}); err != nil {
		return nil, err
	}
	return io.NopCloser(&buf), nil
}

// Manifest computes manifests in-process (no helper needed).
func (i *Instance) Manifest(ctx context.Context, op adapter.OpContext, root string) ([]envspec.Entry, error) {
	full, err := i.mapPath(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		f, err := os.Open(full)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		d, n, err := xfer.DigestOf(f)
		return []envspec.Entry{{Path: filepath.Base(full), Mode: 0o100000 | int64(fi.Mode().Perm()), Size: n, Digest: d}}, err
	}
	return xfer.LocalManifest(full)
}

func entryMode(fi os.FileInfo, isDir, isLink bool) int64 {
	mode := int64(fi.Mode().Perm())
	switch {
	case isDir:
		mode |= 0o040000
	case isLink:
		mode = 0o120000
	default:
		mode |= 0o100000
	}
	return mode
}

func (i *Instance) StatFS(ctx context.Context, op adapter.OpContext, path string) (envspec.Entry, error) {
	full, err := i.mapPath(path)
	if err != nil {
		return envspec.Entry{}, err
	}
	fi, err := os.Lstat(full)
	if err != nil {
		return envspec.Entry{}, err
	}
	return envspec.Entry{Path: path, Mode: entryMode(fi, fi.IsDir(), fi.Mode()&os.ModeSymlink != 0), Size: fi.Size()}, nil
}

func (i *Instance) ListFS(ctx context.Context, op adapter.OpContext, path string) ([]envspec.Entry, error) {
	full, err := i.mapPath(path)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return nil, err
	}
	var out []envspec.Entry
	for _, de := range ents {
		fi, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, envspec.Entry{Path: de.Name(),
			Mode: entryMode(fi, de.IsDir(), de.Type()&os.ModeSymlink != 0), Size: fi.Size()})
	}
	return out, nil
}

// InjectEnv stores env vars applied on exec (adapter.CredInjector).
func (i *Instance) InjectEnv(name, value string) {
	i.mu.Lock()
	i.credEnv[name] = value
	i.mu.Unlock()
}

// InjectFile writes to .secrets beside the workspace for absolute
// in-env paths, or a /work path, current-user readable only.
func (i *Instance) InjectFile(ctx context.Context, path string, value []byte) error {
	var full string
	p := strings.ReplaceAll(path, "\\", "/")
	if strings.HasPrefix(p, "/work") || !strings.HasPrefix(p, "/") {
		mapped, err := i.mapPath(p)
		if err != nil {
			return err
		}
		full = mapped
	} else {
		full = filepath.Join(i.root, ".secrets", filepath.Base(p))
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, value, 0o600)
}

var (
	_ adapter.Adapter           = (*Adapter)(nil)
	_ adapter.Creator           = (*Adapter)(nil)
	_ adapter.Checker           = (*Adapter)(nil)
	_ adapter.Coster            = (*Adapter)(nil)
	_ adapter.Instance          = (*Instance)(nil)
	_ adapter.Executor          = (*Instance)(nil)
	_ adapter.Transferrer       = (*Instance)(nil)
	_ adapter.Manifestor        = (*Instance)(nil)
	_ adapter.FSLister          = (*Instance)(nil)
	_ adapter.CredInjector      = (*Instance)(nil)
	_ adapter.Arched            = (*Instance)(nil)
	_ adapter.CollectorProvider = (*Instance)(nil)
)
