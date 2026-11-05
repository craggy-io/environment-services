package environments

import (
	"context"
	"fmt"
	"sync"

	"github.com/craggy-io/environment-services/envcap"
	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/core"
	"github.com/craggy-io/environment-services/internal/vault"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// EnvCapDeps is the capability-internal surface over a Manager: the
// consumer facade (Manager) plus the operations only the capability
// layer inside the trust boundary may use — attach, borrows, scope
// closure, transfer/reset, by-ID exec/transfer/filesystem access and
// secret typing. envcap declares its own Deps.Manager interface
// against these methods; it never imports this package's internals,
// and the secret values behind TypeSecret never leave this module's
// internal packages.
//
// Every by-ID operation re-resolves the holder handle first, so the
// same lease fencing applies as through a handle.
type EnvCapDeps struct {
	m *Manager

	mu   sync.Mutex
	pins map[string]*pinEntry // pin id → record + unpin (envcapimpl.go)
}

// envcap.Manager is satisfied here, never the other way round:
// envcap must not import this assembly package.
var _ envcap.Manager = (*EnvCapDeps)(nil)

// NewEnvCapDeps returns the capability deps for m, ready to assign
// to envcap.Deps.Manager.
func NewEnvCapDeps(m *Manager) *EnvCapDeps {
	return &EnvCapDeps{m: m, pins: map[string]*pinEntry{}}
}

func (d *EnvCapDeps) handle(id string) (core.Handle, error) { return d.m.core.Get(id) }

// --- consumer facade, delegated ---

func (d *EnvCapDeps) Provision(ctx context.Context, owner envspec.Owner, spec envspec.Spec, opts ...ProvisionOption) (envspec.Handle, error) {
	return d.m.Provision(ctx, owner, spec, opts...)
}

func (d *EnvCapDeps) Get(id string) (envspec.Handle, error) { return d.m.Get(id) }

func (d *EnvCapDeps) Info(id string) (envspec.Info, error) {
	info, err := d.m.core.Info(id)
	return info, xlate(err)
}

func (d *EnvCapDeps) Release(ctx context.Context, id string) error { return d.m.Release(ctx, id) }

func (d *EnvCapDeps) List(f Filter) []envspec.Info { return d.m.List(f) }

func (d *EnvCapDeps) Profiles() []envspec.Profile { return d.m.Profiles() }

func (d *EnvCapDeps) Profile(name string) (envspec.Profile, bool) { return d.m.Profile(name) }

func (d *EnvCapDeps) ProfileStatus(name string) envspec.ProfileStatus { return d.m.ProfileStatus(name) }

func (d *EnvCapDeps) EnsureProfileBackend(ctx context.Context, name string, progress func(envspec.Progress)) error {
	return d.m.EnsureProfileBackend(ctx, name, progress)
}

func (d *EnvCapDeps) EventsSince(ctx context.Context, since uint64, limit int) ([]envspec.LifecycleEvent, error) {
	return d.m.EventsSince(ctx, since, limit)
}

func (d *EnvCapDeps) RecentEvents(ctx context.Context, limit int) ([]envspec.LifecycleEvent, error) {
	return d.m.RecentEvents(ctx, limit)
}

func (d *EnvCapDeps) AcquireService(ctx context.Context, owner envspec.Owner, request envspec.ServiceRequest) (envspec.ServiceLease, error) {
	return d.m.AcquireService(ctx, owner, request)
}

func (d *EnvCapDeps) StopService(ctx context.Context, id, generation string) error {
	return d.m.StopService(ctx, id, generation)
}

func (d *EnvCapDeps) Android(id string) (envcap.AndroidDevice, error) {
	dev, err := d.m.Android(id)
	if err != nil {
		return nil, xlate(err)
	}
	return dev, nil
}

func (d *EnvCapDeps) Observation() Observation { return d.m.Observation() }

func (d *EnvCapDeps) HostSnapshot() envspec.HostSnapshot { return d.m.HostSnapshot() }

func (d *EnvCapDeps) RedactString(s string) string { return d.m.RedactString(s) }

func (d *EnvCapDeps) Drivers() []Driver { return d.m.Drivers() }

func (d *EnvCapDeps) ManagerID() string { return d.m.ManagerID() }

// --- capability-internal operations ---

// Attach connects to an independently existing resource (a physical
// Android device) under the same durable-operation discipline as
// Provision. Release detaches; it never destroys the resource.
func (d *EnvCapDeps) Attach(ctx context.Context, owner envspec.Owner, spec envspec.Spec, opts ...ProvisionOption) (envspec.Handle, error) {
	h, err := d.m.core.Attach(ctx, owner, spec, coreOpts(opts)...)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Borrow attaches a job to an environment so destruction defers
// until the job finishes. The returned func drops the borrow
// (idempotent); ctx end also drops it.
func (d *EnvCapDeps) Borrow(ctx context.Context, id, job string) (func(), error) {
	release, err := d.m.core.Borrow(ctx, id, job)
	return release, xlate(err)
}

// RenewBorrow extends one job borrow.
func (d *EnvCapDeps) RenewBorrow(id, job string) error { return d.m.core.RenewBorrow(id, job) }

// CloseSession closes a session scope and releases its session-held
// environments.
func (d *EnvCapDeps) CloseSession(ctx context.Context, session string) error {
	return d.m.core.CloseSession(ctx, session)
}

// CloseJob closes a job scope, drops its borrows and releases its
// environments.
func (d *EnvCapDeps) CloseJob(ctx context.Context, job string) error {
	return d.m.core.CloseJob(ctx, job)
}

// PrepareProfile runs a profile's preparation now (image build/pull
// or adapter preparation), streaming progress.
func (d *EnvCapDeps) PrepareProfile(ctx context.Context, name string, progress func(envspec.Progress)) error {
	return d.m.core.PrepareProfile(ctx, name, progress)
}

// Transfer hands an environment to a new owner and returns the new
// holder handle; the previous holder's handle goes stale.
func (d *EnvCapDeps) Transfer(id string, to envspec.Owner) (envspec.Handle, error) {
	h, err := d.m.core.Transfer(id, to)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Reset returns an environment to its post-provision state under a
// durable reset operation.
func (d *EnvCapDeps) Reset(ctx context.Context, id string) error {
	h, err := d.handle(id)
	if err != nil {
		return err
	}
	return d.m.core.Reset(ctx, h)
}

// Exec runs one argv command to completion (bounded output tails).
func (d *EnvCapDeps) Exec(ctx context.Context, id string, cmd envspec.Cmd) (stdout, stderr []byte, exit int, err error) {
	h, err := d.handle(id)
	if err != nil {
		return nil, nil, 0, err
	}
	return d.m.core.ExecCapture(ctx, h, cmd)
}

// Push copies src into the environment at dst and reports what moved.
func (d *EnvCapDeps) Push(ctx context.Context, id string, src envspec.Source, dst string) (envspec.TransferStats, error) {
	h, err := d.handle(id)
	if err != nil {
		return envspec.TransferStats{}, xlate(err)
	}
	st, err := d.m.core.Push(ctx, h, src, dst, xfer.Options{})
	return st, xlate(err)
}

// Pull copies src out of the environment into dst and reports what moved.
func (d *EnvCapDeps) Pull(ctx context.Context, id, src string, dst envspec.Sink) (envspec.TransferStats, error) {
	h, err := d.handle(id)
	if err != nil {
		return envspec.TransferStats{}, xlate(err)
	}
	st, err := d.m.core.Pull(ctx, h, src, dst, xfer.Options{})
	return st, xlate(err)
}

// Snapshot records a named snapshot where the adapter supports it.
func (d *EnvCapDeps) Snapshot(ctx context.Context, id, name string) error {
	h, err := d.handle(id)
	if err != nil {
		return xlate(err)
	}
	return xlate(d.m.core.Snapshot(ctx, h, name))
}

// Restore rolls an environment back to a named snapshot.
func (d *EnvCapDeps) Restore(ctx context.Context, id, name string) error {
	h, err := d.handle(id)
	if err != nil {
		return xlate(err)
	}
	return xlate(d.m.core.Restore(ctx, h, name))
}

// Snapshots lists an environment's snapshots.
func (d *EnvCapDeps) Snapshots(ctx context.Context, id string) ([]string, error) {
	h, err := d.handle(id)
	if err != nil {
		return nil, xlate(err)
	}
	names, err := d.m.core.Snapshots(ctx, h)
	return names, xlate(err)
}

// Stat describes one path inside an environment.
func (d *EnvCapDeps) Stat(ctx context.Context, id, path string) (envspec.Entry, error) {
	h, err := d.handle(id)
	if err != nil {
		return envspec.Entry{}, xlate(err)
	}
	en, err := d.m.core.Stat(ctx, h, path)
	return en, xlate(err)
}

// ListFS lists a directory inside an environment.
func (d *EnvCapDeps) ListFS(ctx context.Context, id, path string, recursive bool, limit int) ([]envspec.Entry, error) {
	h, err := d.handle(id)
	if err != nil {
		return nil, xlate(err)
	}
	entries, err := d.m.core.ListFS(ctx, h, path, recursive, limit)
	return entries, xlate(err)
}

// Manifest returns the file manifest under root inside an environment.
func (d *EnvCapDeps) Manifest(ctx context.Context, id, root string) ([]envspec.Entry, error) {
	h, err := d.handle(id)
	if err != nil {
		return nil, err
	}
	return d.m.core.Manifest(ctx, h, root)
}

// TypeSecret types one leased secret field into an Android device's
// focused input. The lease is named by ID and the value is revealed
// only inside the vault lease's Use callback, so no secret value
// crosses this surface.
func (d *EnvCapDeps) TypeSecret(ctx context.Context, envID, leaseID, field string) error {
	dev, err := d.m.Android(envID)
	if err != nil {
		return xlate(err)
	}
	lease, err := d.m.vault.LeaseByID(ctx, leaseID)
	if err != nil {
		return xlate(err)
	}
	if field == "" {
		field = "password"
	}
	return xlate(lease.Use(field, func(s vault.Secret) error {
		if s.Reveal() == "" {
			return fmt.Errorf("%w: credential has no %q field to type", envspec.ErrInvalid, field)
		}
		return dev.Text(ctx, s.Reveal())
	}))
}
