package environments

import (
	"context"
	"fmt"
	"iter"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/core"
)

// Close stops the manager. ShutdownDrain releases every live
// environment first (bounded by ctx; unfinished releases remain
// durable operations the next Open resumes); ShutdownDisconnect
// leaves resources running. The signal hub, vault and host sampler
// stop with the manager. Close is idempotent.
func (m *Manager) Close(ctx context.Context, mode ShutdownMode) error {
	m.closeOnce.Do(func() {
		cm := core.ShutdownDisconnect
		if mode == ShutdownDrain {
			cm = core.ShutdownDrain
		}
		m.closeErr = m.core.Close(ctx, cm)
		m.hub.Close()
		if err := m.vault.Close(); m.closeErr == nil {
			m.closeErr = err
		}
		m.cancelMon()
	})
	return m.closeErr
}

// Provision creates an environment for owner and returns its holder
// handle last, after identity verification, credential delivery and
// required-signal barriers. The durable operation is recorded before
// anything is dispatched, so a crash mid-provision recovers on the
// next Open.
func (m *Manager) Provision(ctx context.Context, owner envspec.Owner, spec envspec.Spec, opts ...ProvisionOption) (envspec.Handle, error) {
	h, err := m.core.Provision(ctx, owner, spec, coreOpts(opts)...)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Get returns a holder handle for an environment, fenced by its
// current lease. A handle obtained before reassignment is stale and
// every use of it fails.
func (m *Manager) Get(id string) (envspec.Handle, error) {
	h, err := m.core.Get(id)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// Release releases an environment by ID: activities stop, credential
// leases are returned, the resource is destroyed (or reset to its
// warm pool, or detached when attached), and absence is verified
// before the release completes.
func (m *Manager) Release(ctx context.Context, id string) error {
	return m.core.Release(ctx, id)
}

// List returns environment infos matching f.
func (m *Manager) List(f Filter) []envspec.Info {
	return m.core.List(core.Filter{Owner: f.Owner, Session: f.Session, Kind: f.Kind, State: f.State, Live: f.Live})
}

// Profiles lists registered profiles (stock, file and Config).
func (m *Manager) Profiles() []envspec.Profile { return m.core.Profiles() }

// Profile returns one registered profile.
func (m *Manager) Profile(name string) (envspec.Profile, bool) { return m.core.Profile(name) }

// ProfileStatus is the authoritative readiness answer for one
// profile: whether it can provision right now and, if not, why.
func (m *Manager) ProfileStatus(name string) envspec.ProfileStatus {
	return m.core.ProfileStatus(name)
}

// EnsureProfileBackend starts a profile's runtime backend if it is
// stopped (for example a stopped container daemon), streaming
// progress. It never provisions an environment.
func (m *Manager) EnsureProfileBackend(ctx context.Context, name string, progress func(envspec.Progress)) error {
	return m.core.EnsureProfileBackend(ctx, name, progress)
}

// Events streams persisted lifecycle events after seq, following new
// ones until ctx ends (at-least-once; keep your own cursor).
func (m *Manager) Events(ctx context.Context, since uint64) iter.Seq2[envspec.LifecycleEvent, error] {
	return m.core.Events(ctx, since)
}

// EventsSince reads one page of lifecycle events after seq without
// following.
func (m *Manager) EventsSince(ctx context.Context, since uint64, limit int) ([]envspec.LifecycleEvent, error) {
	return m.core.EventsSince(ctx, since, limit)
}

// RecentEvents reads the most recent lifecycle events, oldest first.
func (m *Manager) RecentEvents(ctx context.Context, limit int) ([]envspec.LifecycleEvent, error) {
	return m.core.RecentEvents(ctx, limit)
}

// AcquireService acquires a borrow on a managed service, starting
// its environment on first use. Sharing is scoped by owner, session,
// request key and the complete service definition.
func (m *Manager) AcquireService(ctx context.Context, owner envspec.Owner, request envspec.ServiceRequest) (envspec.ServiceLease, error) {
	return m.core.AcquireService(ctx, owner, request)
}

// StopService stops one service generation. It is generation-fenced:
// a stale generation fails with envspec.ErrConflict and stops
// nothing.
func (m *Manager) StopService(ctx context.Context, id, generation string) error {
	return m.core.StopService(ctx, id, generation)
}

// RenewLease extends the holder lease behind h. It is the only
// holder-renewal path: using a handle never renews it. h must be a
// handle this manager issued.
func (m *Manager) RenewLease(h envspec.Handle) error {
	ch, ok := h.(core.Handle)
	if !ok {
		return fmt.Errorf("%w: handle was not issued by an environments.Manager", envspec.ErrInvalid)
	}
	return m.core.RenewLease(ch)
}

// Android returns the device surface of an Android environment. The
// environment must exist and be running; every device call
// re-validates the holder lease, so a released or reassigned
// environment stops answering.
func (m *Manager) Android(id string) (AndroidDevice, error) {
	h, err := m.core.Get(id)
	if err != nil {
		return nil, err
	}
	d := &androidDevice{m: m, h: h}
	if _, err := d.device(); err != nil {
		return nil, err
	}
	return d, nil
}

// Observation returns the signal observation surface: pins, queries,
// coverage and captures over everything this manager records.
func (m *Manager) Observation() Observation { return &observation{m: m} }

// HostSnapshot returns the most recent host sample (zero until the
// first sample lands, normally before Open returns).
func (m *Manager) HostSnapshot() envspec.HostSnapshot {
	s, _ := m.mon.Latest()
	return s
}

// RedactString scrubs known secret values from s. Use it on any
// guest-derived text before showing it outside the trust boundary.
func (m *Manager) RedactString(s string) string {
	if m.redactor == nil {
		return s
	}
	return m.redactor.RedactString(s)
}

// Drivers lists the runtime adapters registered at Open.
func (m *Manager) Drivers() []Driver {
	descs := m.core.Drivers()
	out := make([]Driver, 0, len(descs))
	for _, d := range descs {
		out = append(out, Driver{Name: d.Name, Kinds: d.Kinds, Version: d.Version})
	}
	return out
}

// StockProfiles returns the built-in profiles every Manager starts
// with, before file and Config profiles.
func (m *Manager) StockProfiles() []envspec.Profile { return core.StockProfiles() }

// BuildDigest returns the content digest naming a profile's built
// image, or an error for a profile with no build.
func (m *Manager) BuildDigest(p envspec.Profile) (string, error) { return core.BuildDigest(p) }

// BuildDigest is the package-level form of Manager.BuildDigest for
// callers that only need the digest (it is a pure function of the
// profile's build definition).
func BuildDigest(p envspec.Profile) (string, error) { return core.BuildDigest(p) }

// ManagerID returns this manager's identity (Config.ManagerID, or
// the ID minted at Open).
func (m *Manager) ManagerID() string { return m.core.ManagerID() }
