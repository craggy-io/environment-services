package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/store"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Handle is a lease-aware value: env, generation, lease ID and epoch.
// The runtime env object never escapes; every operation re-validates
// the handle via checkHandle, so a handle from before reassignment
// (warm claim, transfer, pool return) is permanently stale.
type Handle struct {
	m          *Manager
	EnvID      string
	Generation int64
	LeaseID    string
	Epoch      int64

	// borrowJob, when set, authorises via a live borrow instead of the
	// holder lease (managed-service borrowers).
	borrowJob string
}

var _ envspec.Handle = Handle{}

// checkHandle is the one epoch fence for Exec/Push/Pull/Release/
// RenewLease/device access. RequirePresent rejects acquiring envs for
// data-plane ops. There is no renew-on-use: checking never extends.
func (m *Manager) checkHandle(h Handle) (store.Env, *runtimeEnv, error) {
	e, err := m.freshEnv(h.EnvID)
	if err != nil {
		return e, nil, err
	}
	if e.Generation != h.Generation {
		return e, nil, fmt.Errorf("%w: generation %d, env is %d", ErrStaleHandle, h.Generation, e.Generation)
	}
	if h.borrowJob != "" {
		bs, berr := m.store.Borrows(h.EnvID)
		if berr != nil {
			return e, nil, berr
		}
		live := false
		for _, b := range bs {
			if b.Job == h.borrowJob && (b.Expires.IsZero() || m.now().Before(b.Expires)) {
				live = true
			}
		}
		if !live {
			return e, nil, fmt.Errorf("%w: borrow %s not live", ErrStaleHandle, h.borrowJob)
		}
	} else {
		lease, lerr := m.store.GetLease(h.EnvID)
		if lerr != nil {
			return e, nil, lerr
		}
		if lease.State != store.LeaseHeld || lease.LeaseID != h.LeaseID || lease.Epoch != h.Epoch {
			return e, nil, fmt.Errorf("%w: lease %s epoch %d not held", ErrStaleHandle, h.LeaseID, h.Epoch)
		}
		if !lease.Expires.IsZero() && m.now().After(lease.Expires) {
			return e, nil, fmt.Errorf("%w: lease expired", ErrStaleHandle)
		}
	}
	r := m.refreshRuntimeEnv(h.EnvID)
	if r == nil || r.generation != h.Generation {
		return e, nil, fmt.Errorf("%w: environment %s not running under this manager", ErrEnvironmentLost, h.EnvID)
	}
	return e, r, nil
}

func (h Handle) ID() string { return h.EnvID }

// Info snapshots the environment.
func (h Handle) Info() envspec.Info {
	info, _ := h.m.Info(h.EnvID)
	return info
}

func operationError(ctx context.Context, err error) error {
	cause := context.Cause(ctx)
	if errors.Is(cause, ErrEnvironmentLost) {
		if err == nil {
			return ErrEnvironmentLost
		}
		return fmt.Errorf("%w: %v", ErrEnvironmentLost, err)
	}
	if errors.Is(cause, ErrClosed) {
		if err == nil {
			return ErrClosed
		}
		return fmt.Errorf("%w: %v", ErrClosed, err)
	}
	return err
}

const inlineCap = 64 << 10

func tailOf(b []byte) []byte {
	if len(b) <= inlineCap {
		return b
	}
	return b[len(b)-inlineCap:]
}

// execProcess starts one streaming command against a checked runtime,
// with credential env merged. Output is teed to env.process@1.
func (m *Manager) execProcess(ctx context.Context, e store.Env, r *runtimeEnv, cmd adapter.ExecCmd) (*adapter.Process, string, error) {
	ex, ok := r.inst.(adapter.Executor)
	if !ok {
		return nil, "", fmt.Errorf("%w: exec on %s", adapter.ErrUnsupported, e.Kind)
	}
	r.mu.Lock()
	if len(r.credEnv) > 0 {
		env := make(map[string]string, len(r.credEnv)+len(cmd.Env))
		for k, v := range r.credEnv {
			env[k] = v
		}
		for k, v := range cmd.Env {
			env[k] = v
		}
		cmd.Env = env
	}
	r.mu.Unlock()
	if cmd.User == "root" {
		if cr, ok := r.inst.(adapter.ComponentReporter); ok {
			_ = cr.EnsureComponent(ctx, "root")
		}
	}
	opCtx, done := r.operationContext(ctx)
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	p, err := ex.Exec(opCtx, oc, cmd)
	if err != nil {
		done()
		return nil, "", operationError(opCtx, err)
	}
	procID := envspec.NewID("proc_")
	out := m.teeProcess(e, p, procID, done, opCtx)
	return out, procID, nil
}

// teeProcess tees output to env.process@1 while passing it through.
func (m *Manager) teeProcess(e store.Env, p *adapter.Process, procID string, done func(), opCtx context.Context) *adapter.Process {
	tee := func(rd io.Reader, stream string) io.Reader {
		if rd == nil {
			return nil
		}
		pr, pw := io.Pipe()
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := rd.Read(buf)
				if n > 0 {
					chunk := append([]byte(nil), buf[:n]...)
					if m.cfg.Signals != nil {
						m.cfg.Signals.Publish(envspec.SignalEvent{
							Stream: envspec.StreamEnvProcess, Source: e.ID, Collector: procID,
							Data: envspec.ProcessData{Proc: procID, Stream: stream, Text: string(chunk)},
						})
					}
					_, _ = pw.Write(chunk)
				}
				if err != nil {
					if errors.Is(err, io.EOF) {
						_ = pw.Close()
					} else {
						pw.CloseWithError(err)
					}
					return
				}
			}
		}()
		return pr
	}
	out := adapter.NewProcess(tee(p.Stdout, "stdout"), tee(p.Stderr, "stderr"), p.Stdin,
		func() (int, error) {
			code, err := p.Wait()
			err = operationError(opCtx, err)
			done()
			return code, err
		}, func(sig string) error { return p.Signal(sig) })
	out.SetPID(p.PID())
	return out
}

// ExecCapture runs one command to completion (bounded tails returned).
func (m *Manager) ExecCapture(ctx context.Context, h Handle, cmd envspec.Cmd) ([]byte, []byte, int, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return nil, nil, -1, err
	}
	return m.execCapture(ctx, e, r, cmd)
}

func (m *Manager) execCapture(ctx context.Context, e store.Env, r *runtimeEnv, cmd envspec.Cmd) ([]byte, []byte, int, error) {
	p, _, err := m.execProcess(ctx, e, r, adapter.ExecCmd{Argv: cmd.Argv, Env: cmd.Env, Timeout: cmd.Timeout})
	if err != nil {
		return nil, nil, -1, err
	}
	var outBuf, errBuf bytes.Buffer
	done := make(chan struct{}, 2)
	for _, s := range []struct {
		r io.Reader
		w *bytes.Buffer
	}{{p.Stdout, &outBuf}, {p.Stderr, &errBuf}} {
		go func(rd io.Reader, w *bytes.Buffer) {
			if rd != nil {
				_, _ = io.Copy(w, rd)
			}
			done <- struct{}{}
		}(s.r, s.w)
	}
	exit, err := p.Wait()
	<-done
	<-done
	return tailOf(outBuf.Bytes()), tailOf(errBuf.Bytes()), exit, err
}

// Exec implements envspec.Handle.
func (h Handle) Exec(ctx context.Context, cmd envspec.Cmd) ([]byte, []byte, int, error) {
	return h.m.ExecCapture(ctx, h, cmd)
}

// ExecStream starts a streaming command (services, capability layer).
func (m *Manager) ExecStream(ctx context.Context, h Handle, cmd adapter.ExecCmd) (*adapter.Process, string, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return nil, "", err
	}
	return m.execProcess(ctx, e, r, cmd)
}

// push moves a Source tree into an env: archive fast path when the
// instance is a Transferrer, else the xfer helper protocol over exec.
func (m *Manager) push(ctx context.Context, e store.Env, r *runtimeEnv, src envspec.Source, dst string, o xfer.Options) (envspec.TransferStats, error) {
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	opCtx, done := r.operationContext(ctx)
	defer done()
	if o.Delta {
		if st, err, handled := m.deltaPush(opCtx, e, r, src, dst, o); handled {
			return st, operationError(opCtx, err)
		}
	}
	if tr, ok := r.inst.(adapter.Transferrer); ok {
		pr, pw := io.Pipe()
		errCh := make(chan error, 1)
		var st envspec.TransferStats
		go func() {
			var err error
			st, err = xfer.WriteTar(opCtx, src, pw, o)
			_ = pw.CloseWithError(err)
			errCh <- err
		}()
		err := tr.PutArchive(opCtx, oc, dst, pr)
		_ = pr.CloseWithError(err)
		if werr := <-errCh; err == nil {
			err = werr
		}
		return st, operationError(opCtx, err)
	}
	execFn, err := m.agentExec(e, r)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	argv, err := m.agentArgv(ctx, e, r)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	var buf bytes.Buffer
	st, err := xfer.WriteTar(opCtx, src, &buf, o)
	if err != nil {
		return st, err
	}
	err = xfer.AgentRecv(opCtx, execFn, argv, dst, o.Mirror, &buf)
	return st, operationError(opCtx, err)
}

func (m *Manager) deltaPush(ctx context.Context, e store.Env, r *runtimeEnv, src envspec.Source, dst string, o xfer.Options) (envspec.TransferStats, error, bool) {
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	dstMan, err, ok := m.manifestOf(ctx, e, r, dst)
	if !ok {
		return envspec.TransferStats{}, nil, false
	}
	if err != nil {
		return envspec.TransferStats{}, err, true
	}
	srcMan, err := xfer.Manifest(ctx, src)
	if err != nil {
		return envspec.TransferStats{}, err, true
	}
	send, del := xfer.Diff(srcMan, dstMan, o.Mirror)
	tr, isTr := r.inst.(adapter.Transferrer)
	if !isTr {
		return envspec.TransferStats{}, nil, false
	}
	only := map[string]bool{}
	for _, e2 := range send {
		only[e2.Path] = true
	}
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	var st envspec.TransferStats
	go func() {
		var err error
		st, err = xfer.WriteTarSubset(ctx, src, only, pw, o)
		if err == nil && len(del) > 0 {
			// Deletions travel as the delete-list entry in a second tar.
			err = writeDeleteList(pw, del)
		}
		_ = pw.CloseWithError(err)
		errCh <- err
	}()
	perr := tr.PutArchive(ctx, oc, dst, pr)
	_ = pr.CloseWithError(perr)
	if werr := <-errCh; perr == nil {
		perr = werr
	}
	st.Skipped = int64(len(srcMan) - len(send))
	return st, perr, true
}

func writeDeleteList(w io.Writer, del []string) error {
	var body bytes.Buffer
	for _, d := range del {
		body.WriteString(d + "\n")
	}
	src := xfer.SingleFileSource(xfer.DeleteListName, body.Bytes())
	_, err := xfer.WriteTar(context.Background(), src, w, xfer.Options{})
	return err
}

// pull reads a tree out of an env into a Sink.
func (m *Manager) pull(ctx context.Context, e store.Env, r *runtimeEnv, src string, dst envspec.Sink, o xfer.Options) (envspec.TransferStats, error) {
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	opCtx, done := r.operationContext(ctx)
	defer done()
	if tr, ok := r.inst.(adapter.Transferrer); ok {
		rc, err := tr.GetArchive(opCtx, oc, src)
		if err != nil {
			return envspec.TransferStats{}, operationError(opCtx, err)
		}
		defer rc.Close()
		st, err := xfer.ReadTar(opCtx, rc, dst, o)
		return st, operationError(opCtx, err)
	}
	execFn, err := m.agentExec(e, r)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	argv, err := m.agentArgv(ctx, e, r)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	rd, wait, err := xfer.AgentSend(opCtx, execFn, argv, src, nil)
	if err != nil {
		return envspec.TransferStats{}, operationError(opCtx, err)
	}
	st, rerr := xfer.ReadTar(opCtx, rd, dst, o)
	code, werr := wait()
	if rerr == nil && werr == nil && code != 0 {
		rerr = fmt.Errorf("agent send exited %d", code)
	}
	if rerr == nil {
		rerr = werr
	}
	return st, operationError(opCtx, rerr)
}

// agentExec adapts instance exec to xfer's ExecFunc. Agent *deployment*
// (pushing the helper binary) is the owning adapter's concern in later
// stages; argv resolution here is the conventional helper path.
func (m *Manager) agentExec(e store.Env, r *runtimeEnv) (xfer.ExecFunc, error) {
	ex, ok := r.inst.(adapter.Executor)
	if !ok {
		return nil, fmt.Errorf("%w: transfer on %s", adapter.ErrUnsupported, e.Kind)
	}
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	return func(ctx context.Context, argv []string, stdin io.Reader) (io.Reader, func() (int, error), error) {
		p, err := ex.Exec(ctx, oc, adapter.ExecCmd{Argv: argv, Stdin: stdin})
		if err != nil {
			return nil, nil, err
		}
		return p.Stdout, p.Wait, nil
	}, nil
}

func (m *Manager) agentArgv(ctx context.Context, e store.Env, r *runtimeEnv) ([]string, error) {
	arch := "amd64"
	if a, ok := r.inst.(adapter.Arched); ok && a.Arch() != "" {
		arch = a.Arch()
	}
	if _, ok := xfer.AgentBinary(arch); !ok {
		return nil, fmt.Errorf("%w: no transfer helper for arch %s", adapter.ErrUnsupported, arch)
	}
	// Conventional helper location; adapters that need the agent path
	// deploy it there during Create (see adapter stages).
	return []string{"/tmp/craggy-xfer"}, nil
}

// manifestOf returns a tree manifest via the instance fast path.
func (m *Manager) manifestOf(ctx context.Context, e store.Env, r *runtimeEnv, root string) ([]envspec.Entry, error, bool) {
	if mf, ok := r.inst.(adapter.Manifestor); ok {
		man, err := mf.Manifest(ctx, adapter.OpContext{EnvID: e.ID, Generation: e.Generation}, root)
		return man, err, true
	}
	return nil, nil, false
}

// Push implements envspec.Handle.
func (h Handle) Push(ctx context.Context, src envspec.Source, dst string) error {
	_, err := h.m.Push(ctx, h, src, dst, xfer.Options{})
	return err
}

// Pull implements envspec.Handle.
func (h Handle) Pull(ctx context.Context, src string, dst envspec.Sink) error {
	_, err := h.m.Pull(ctx, h, src, dst, xfer.Options{})
	return err
}

// Push is Push with transfer options (capability layer).
func (m *Manager) Push(ctx context.Context, h Handle, src envspec.Source, dst string, o xfer.Options) (envspec.TransferStats, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	return m.push(ctx, e, r, src, dst, o)
}

// Pull is Pull with transfer options.
func (m *Manager) Pull(ctx context.Context, h Handle, src string, dst envspec.Sink, o xfer.Options) (envspec.TransferStats, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return envspec.TransferStats{}, err
	}
	return m.pull(ctx, e, r, src, dst, o)
}

// Release implements envspec.Handle: holder-initiated release requires
// the live holder lease (borrower handles cannot release the env).
func (h Handle) Release(ctx context.Context) error {
	if h.borrowJob != "" {
		return fmt.Errorf("%w: borrowers cannot release the environment", envspec.ErrConflict)
	}
	if _, _, err := h.m.checkHandle(h); err != nil {
		// A stale handle's Release is a no-op success for terminal envs,
		// an error while someone else holds it.
		if e, ferr := h.m.freshEnv(h.EnvID); ferr == nil && e.Phase.Terminal() {
			return nil
		}
		return err
	}
	return h.m.Release(ctx, h.EnvID)
}

// RenewLease extends this handle's lease.
func (h Handle) RenewLease() error { return h.m.RenewLease(h) }

// Instance returns the adapter instance behind a checked handle
// (assembly use: AndroidDevice extraction). Holders never see it.
func (m *Manager) Instance(h Handle) (adapter.Instance, error) {
	_, r, err := m.checkHandle(h)
	if err != nil {
		return nil, err
	}
	return r.inst, nil
}

// Snapshot/Restore/Snapshots over a checked handle.
func (m *Manager) Snapshot(ctx context.Context, h Handle, name string) error {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return err
	}
	sn, ok := r.inst.(adapter.Snapshotter)
	if !ok {
		return fmt.Errorf("%w: snapshots on %s", adapter.ErrUnsupported, e.Kind)
	}
	return sn.Save(ctx, adapter.OpContext{EnvID: e.ID, Generation: e.Generation}, name)
}

func (m *Manager) Restore(ctx context.Context, h Handle, name string) error {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return err
	}
	sn, ok := r.inst.(adapter.Snapshotter)
	if !ok {
		return fmt.Errorf("%w: snapshots on %s", adapter.ErrUnsupported, e.Kind)
	}
	return sn.Load(ctx, adapter.OpContext{EnvID: e.ID, Generation: e.Generation}, name)
}

func (m *Manager) Snapshots(ctx context.Context, h Handle) ([]string, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return nil, err
	}
	sn, ok := r.inst.(adapter.Snapshotter)
	if !ok {
		return nil, fmt.Errorf("%w: snapshots on %s", adapter.ErrUnsupported, e.Kind)
	}
	return sn.Snapshots(ctx, adapter.OpContext{EnvID: e.ID, Generation: e.Generation})
}

// Stat/List over a checked handle.
func (m *Manager) Stat(ctx context.Context, h Handle, path string) (envspec.Entry, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return envspec.Entry{}, err
	}
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	if lf, ok := r.inst.(adapter.FSLister); ok {
		return lf.StatFS(ctx, oc, path)
	}
	return envspec.Entry{}, fmt.Errorf("%w: stat on %s", adapter.ErrUnsupported, e.Kind)
}

func (m *Manager) ListFS(ctx context.Context, h Handle, path string, recursive bool, limit int) ([]envspec.Entry, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return nil, err
	}
	oc := adapter.OpContext{EnvID: e.ID, Generation: e.Generation}
	if lf, ok := r.inst.(adapter.FSLister); ok && !recursive {
		return lf.ListFS(ctx, oc, path)
	}
	man, merr, ok := m.manifestOf(ctx, e, r, path)
	if !ok {
		return nil, fmt.Errorf("%w: list on %s", adapter.ErrUnsupported, e.Kind)
	}
	if merr != nil {
		return nil, merr
	}
	if limit > 0 && len(man) > limit {
		man = man[:limit]
	}
	return man, nil
}

// Manifest returns a tree manifest over a checked handle.
func (m *Manager) Manifest(ctx context.Context, h Handle, root string) ([]envspec.Entry, error) {
	e, r, err := m.checkHandle(h)
	if err != nil {
		return nil, err
	}
	man, merr, ok := m.manifestOf(ctx, e, r, root)
	if !ok {
		return nil, fmt.Errorf("%w: manifest on %s", adapter.ErrUnsupported, e.Kind)
	}
	return man, merr
}
