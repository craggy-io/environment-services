package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/store"
	"github.com/craggy-io/environment-services/internal/vault"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// injectCreds leases and delivers a spec's credentials. Values move
// only inside Lease.Use: into exec env, an in-env file, or a registered
// account injector. They never appear in specs, signals or logs.
// Existing vault leases for this env (crash resume) are re-delivered
// rather than leased twice.
func (m *Manager) injectCreds(ctx context.Context, e store.Env, r *runtimeEnv, reqs []envspec.CredRequest) error {
	if m.cfg.Vault == nil {
		return WithHint("unavailable", fmt.Errorf("spec asks for %d credential(s) but no vault is configured", len(reqs)),
			"open the manager with a vault, or drop the creds from the request")
	}
	holder := "env:" + e.ID
	existing, _ := m.cfg.Vault.Leases(ctx, holder)
	for i, req := range reqs {
		var lease *vault.Lease
		if i < len(existing) {
			lease = existing[i]
		} else {
			lr := vault.LeaseRequest{
				Cred: req.Cred, Holder: holder, HolderKind: "env:" + string(e.Kind),
				Owner: e.Owner.ID, Purpose: "env provision " + e.ID,
				Select: vault.Selector{Kind: req.Kind, Labels: req.Labels},
			}
			l, err := m.cfg.Vault.Lease(ctx, lr)
			if err != nil {
				m.emit("lease.denied", e.ID, map[string]any{"error": m.redact(err.Error())})
				return fmt.Errorf("credential for %s: %w", e.ID, err)
			}
			lease = l
		}
		if err := m.deliverCred(ctx, e, r, lease, req); err != nil {
			return err
		}
		r.mu.Lock()
		found := false
		for _, id := range r.credLeases {
			if id == lease.ID {
				found = true
			}
		}
		if !found {
			r.credLeases = append(r.credLeases, lease.ID)
		}
		r.mu.Unlock()
	}
	m.refreshRedactor()
	return nil
}

// rehydrateCreds re-delivers env/file credentials from surviving vault
// leases after a restart, without new leases.
func (m *Manager) rehydrateCreds(ctx context.Context, e store.Env, r *runtimeEnv) {
	if m.cfg.Vault == nil || len(e.Spec.Creds) == 0 {
		return
	}
	leases, err := m.cfg.Vault.Leases(ctx, "env:"+e.ID)
	if err != nil || len(leases) == 0 {
		return
	}
	for i, req := range e.Spec.Creds {
		if i >= len(leases) {
			break
		}
		_ = m.deliverCred(ctx, e, r, leases[i], req)
		r.mu.Lock()
		r.credLeases = append(r.credLeases, leases[i].ID)
		r.mu.Unlock()
	}
}

func (m *Manager) deliverCred(ctx context.Context, e store.Env, r *runtimeEnv, lease *vault.Lease, req envspec.CredRequest) error {
	kind, target, _ := strings.Cut(req.Deliver, ":")
	switch kind {
	case "env":
		if target == "" {
			return Invalidf("cred delivery %q: want env:NAME", req.Deliver)
		}
		field := req.Field
		if field == "" {
			field = "password"
			switch lease.Kind() {
			case "api-key":
				field = "api_key"
			}
		}
		return lease.Use(field, func(s vault.Secret) error {
			r.mu.Lock()
			r.credEnv[target] = s.Reveal()
			r.mu.Unlock()
			if ci, ok := r.inst.(adapter.CredInjector); ok {
				ci.InjectEnv(target, s.Reveal())
			}
			return nil
		})
	case "file":
		path := strings.TrimPrefix(req.Deliver, "file:")
		if path == "" || path == req.Deliver {
			return Invalidf("cred delivery %q: want file:/path", req.Deliver)
		}
		field := req.Field
		if field == "" {
			field = "password"
		}
		return lease.Use(field, func(s vault.Secret) error {
			if ci, ok := r.inst.(adapter.CredInjector); ok {
				return ci.InjectFile(ctx, path, []byte(s.Reveal()))
			}
			// Generic: one-file push at mode 0400.
			dir, name := splitPath(path)
			_, err := m.push(ctx, e, r, xfer.SingleFileSource(name, []byte(s.Reveal())), dir, xfer.Options{})
			return err
		})
	case "account":
		inj, ok := m.cfg.Injectors[lease.Kind()]
		if !ok {
			return WithHint("unavailable", fmt.Errorf("no account injector registered for kind %q", lease.Kind()),
				"register one with Config.Injectors, or use the type delivery")
		}
		holder, err := m.store.GetLease(e.ID)
		if err != nil {
			return err
		}
		return inj(ctx, Handle{m: m, EnvID: e.ID, Generation: e.Generation, LeaseID: holder.LeaseID, Epoch: holder.Epoch}, lease)
	case "type":
		return nil // delivered later via device typing; nothing at provision
	default:
		return Invalidf("unknown cred delivery %q (want env:NAME, file:/path, account, type)", req.Deliver)
	}
}

func splitPath(p string) (dir, name string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ".", p
	}
	if i == 0 {
		return "/", p[1:]
	}
	return p[:i], p[i+1:]
}
