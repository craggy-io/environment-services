package core

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/redact"
	"github.com/craggy-io/environment-services/internal/store"
	"github.com/craggy-io/environment-services/internal/xfer"
)

// Credentials is the manager's port to whatever keeps credentials: the
// embedding application implements it. The manager keeps none itself. It
// asks for a lease when an environment is to be given a credential, is
// handed the secret behind a lease only to deliver it, and says when a
// holder is gone.
//
// A holder is "env:<environment id>". A use is one of envspec's Use
// constants, which the application declares to its credential store.
//
// Every method is called with the context of the work it is part of. When
// that context belongs to no call the application can act under (the
// manager's own background work), the method returns ErrNoCall and the
// manager does the work later, from SyncCredentials. Other errors should
// wrap envspec.ErrNotFound (no such credential or lease),
// ErrCredentialUnavailable (the store's limits refuse it for now) or
// ErrCredentialDenied (the credential may not be put to this use).
type Credentials interface {
	// Hold takes a lease of a credential for one use and one holder, on
	// behalf of whoever made the call ctx belongs to. The same holder
	// asking again for the same credential and use is given the lease it
	// already has.
	Hold(ctx context.Context, req CredentialRequest) (CredentialLease, error)
	// Redeem hands over the secret behind one lease, which must be held
	// by holder.
	Redeem(ctx context.Context, lease, holder string) (HeldCredential, error)
	// RedeemOnce is Redeem that also takes one value of a once-only
	// field, such as a backup code: the store hands it over and it is
	// gone.
	RedeemOnce(ctx context.Context, lease, holder, field string) (HeldCredential, error)
	// RedeemHolder hands over the secret behind every lease holder has
	// for the manager's uses; none is not an error.
	RedeemHolder(ctx context.Context, holder string) ([]HeldCredential, error)
	// Drop ends every lease the holders have for the manager's uses.
	// Holders with none are not an error.
	Drop(ctx context.Context, holders []string) error
}

// CredentialRequest asks for a lease: of Cred, or of the least recently
// used credential of Kind carrying Labels.
type CredentialRequest struct {
	Cred    string
	Kind    string
	Labels  map[string]string
	Use     string
	Holder  string
	Purpose string
	TTL     time.Duration // zero: until dropped
}

// CredentialLease is a lease as anyone may see it: no secret.
type CredentialLease struct {
	ID     string
	Cred   string
	Kind   string
	Use    string
	Holder string
	Public map[string]string
}

// HeldCredential is a lease with the secret behind it, by field: what the
// store delivers of the credential, which may be a one-time code in
// place of its seed and never what the store keeps to itself. It is the
// only way a secret value reaches the manager, which delivers it and
// teaches its redactor the value; it is never stored, logged or returned.
type HeldCredential struct {
	CredentialLease
	Secret map[string]string
	// Base64 are the fields of Secret that are a file's content, which
	// may be binary and so is base64.
	Base64 []string
}

// plain is each secret field as it is delivered: a file's content
// decoded. A field that doesn't decode is left out.
func (h HeldCredential) plain() map[string][]byte {
	out := make(map[string][]byte, len(h.Secret))
	for field, value := range h.Secret {
		if !slices.Contains(h.Base64, field) {
			out[field] = []byte(value)
			continue
		}
		if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
			out[field] = raw
		}
	}
	return out
}

var (
	// ErrNoCall is what a Credentials method returns when ctx belongs to
	// no call it can act under. The manager defers the work.
	ErrNoCall = errors.New("credentials: no call is being served")
	// ErrCredentialUnavailable marks a lease the store's limits refuse
	// for now: asking again later can succeed.
	ErrCredentialUnavailable = errors.New("credential unavailable")
	// ErrCredentialDenied marks a credential that may not be put to the
	// use asked for.
	ErrCredentialDenied = errors.New("credential denied")
)

// AccountInjector delivers an "account" credential into an environment.
// It receives a lease-checked handle, the lease and its secret fields.
type AccountInjector func(ctx context.Context, h Handle, lease CredentialLease, secret map[string]redact.Secret) error

// credHolder is how an environment is named as the holder of a lease.
func credHolder(envID string) string { return "env:" + envID }

// deliveryUse is the use a credential request's delivery puts the
// credential to. The delivery decides it, except typing, which is a
// login unless the request says it signs the device in to the account.
func deliveryUse(req envspec.CredRequest) (string, error) {
	kind, _, _ := strings.Cut(req.Deliver, ":")
	var uses []string
	switch kind {
	case "env", "file":
		uses = []string{envspec.UseInject}
	case "account":
		uses = []string{envspec.UseAndroidAccount}
	case "type":
		uses = []string{envspec.UseLogin, envspec.UseAndroidAccount}
	default:
		return "", Invalidf("unknown cred delivery %q (want env:NAME, file:/path, account, type)", req.Deliver)
	}
	switch {
	case req.Use == "":
		return uses[0], nil
	case slices.Contains(uses, req.Use):
		return req.Use, nil
	}
	return "", Invalidf("cred delivery %q is for the use %s, not %q", req.Deliver, strings.Join(uses, " or "), req.Use)
}

// fieldOf is the secret field of a redeemed credential to deliver: the
// one named, or with none named its password, or the only field it
// delivers. A credential that delivers several and has no password needs
// the field named.
func fieldOf(held HeldCredential, named string) (string, error) {
	if named != "" {
		if _, ok := held.Secret[named]; !ok {
			return "", Invalidf("credential %s delivers no %q field; it delivers %s", held.Cred, named, strings.Join(fieldsOf(held), ", "))
		}
		return named, nil
	}
	if _, ok := held.Secret["password"]; ok {
		return "password", nil
	}
	fields := fieldsOf(held)
	if len(fields) == 1 {
		return fields[0], nil
	}
	return "", Invalidf("credential %s delivers %s: name the field", held.Cred, strings.Join(fields, ", "))
}

func fieldsOf(held HeldCredential) []string {
	fields := make([]string, 0, len(held.Secret))
	for field := range held.Secret {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	return fields
}

// learn wraps a redeemed credential's secret as it is delivered, a
// file's content decoded, and teaches the redactor its values for as
// long as the holder has it. Content that isn't text is delivered but
// not looked for in output.
func (m *Manager) learn(held HeldCredential) map[string]redact.Secret {
	plain := held.plain()
	secret := make(map[string]redact.Secret, len(plain))
	known := make(map[string]redact.Secret, len(plain))
	for field, value := range plain {
		secret[field] = redact.NewSecret(string(value))
		if utf8.Valid(value) {
			known[field] = secret[field]
		}
	}
	if m.cfg.Redactor != nil {
		m.cfg.Redactor.Hold(held.Holder, held.Cred, known)
	}
	return secret
}

func noCredentials(what string) error {
	return WithHint("unavailable", fmt.Errorf("%s, but the manager was opened with no credentials port", what),
		"open the manager with Config.Credentials, or drop the creds from the request")
}

// injectCreds leases and delivers a spec's credentials while the call
// that provisions the environment is being served. A request delivered
// by typing is only leased: its secret is redeemed when it is typed.
// Leasing again for the same holder gives the lease it has, so a second
// pass takes nothing twice.
func (m *Manager) injectCreds(ctx context.Context, e store.Env, r *runtimeEnv, reqs []envspec.CredRequest) error {
	if m.cfg.Credentials == nil {
		return noCredentials(fmt.Sprintf("spec asks for %d credential(s)", len(reqs)))
	}
	holder := credHolder(e.ID)
	// A drop still queued for this holder would end the leases taken
	// below. It is ended first, or nothing is leased.
	if err := m.flushCredDrops(ctx); err != nil {
		return credentialNeedsCall(e.ID, err)
	}
	for _, req := range reqs {
		use, err := deliveryUse(req)
		if err != nil {
			return err
		}
		lease, err := m.cfg.Credentials.Hold(ctx, CredentialRequest{Cred: req.Cred, Kind: req.Kind, Labels: req.Labels,
			Use: use, Holder: holder, Purpose: "env provision " + e.ID})
		if err != nil {
			if !errors.Is(err, ErrNoCall) {
				m.emit("lease.denied", e.ID, map[string]any{"use": use, "error": m.redact(err.Error())})
			}
			return credentialNeedsCall(e.ID, err)
		}
		if req.Deliver == "type" {
			continue
		}
		held, err := m.cfg.Credentials.Redeem(ctx, lease.ID, holder)
		if err != nil {
			return credentialNeedsCall(e.ID, err)
		}
		if err := m.deliverCred(ctx, e, r, held, req); err != nil {
			return err
		}
	}
	return nil
}

// credentialNeedsCall names the environment in a port error, and says
// what to do when the error is that no call is being served: a provision
// resumed by the manager itself after a restart can't lease.
func credentialNeedsCall(envID string, err error) error {
	err = fmt.Errorf("credential for %s: %w", envID, err)
	if errors.Is(err, ErrNoCall) {
		return WithHint("unavailable", err,
			"provision again: a credential is leased only while the call that asks for it is being served")
	}
	return err
}

// deliverCred delivers one redeemed credential the way its request says.
// The lease's use is checked against the delivery every time: a lease
// taken for typing is never written into a variable or a file.
func (m *Manager) deliverCred(ctx context.Context, e store.Env, r *runtimeEnv, held HeldCredential, req envspec.CredRequest) error {
	use, err := deliveryUse(req)
	if err != nil {
		return err
	}
	if held.Use != use {
		return Invalidf("lease %s is for the use %s; the delivery %q needs %s", held.ID, held.Use, req.Deliver, use)
	}
	secret := m.learn(held)
	kind, target, _ := strings.Cut(req.Deliver, ":")
	var value redact.Secret
	if kind == "env" || kind == "file" {
		field, err := fieldOf(held, req.Field)
		if err != nil {
			return err
		}
		value = secret[field]
	}
	switch kind {
	case "env":
		if target == "" {
			return Invalidf("cred delivery %q: want env:NAME", req.Deliver)
		}
		if !utf8.ValidString(value.Reveal()) || strings.ContainsRune(value.Reveal(), 0) {
			return Invalidf("credential %s: a file that isn't text can't be a variable; deliver it as file:/path", held.Cred)
		}
		r.mu.Lock()
		r.credEnv[target] = value.Reveal()
		r.mu.Unlock()
		if ci, ok := r.inst.(adapter.CredInjector); ok {
			ci.InjectEnv(target, value.Reveal())
		}
		return nil
	case "file":
		if target == "" {
			return Invalidf("cred delivery %q: want file:/path", req.Deliver)
		}
		if ci, ok := r.inst.(adapter.CredInjector); ok {
			return ci.InjectFile(ctx, target, []byte(value.Reveal()))
		}
		// Generic: one-file push at mode 0400.
		dir, name := splitPath(target)
		_, err := m.push(ctx, e, r, xfer.SingleFileSource(name, []byte(value.Reveal())), dir, xfer.Options{})
		return err
	case "account":
		inj, ok := m.cfg.Injectors[held.Kind]
		if !ok {
			return WithHint("unavailable", fmt.Errorf("no account injector registered for kind %q", held.Kind),
				"use the type delivery and env.android.type-secret")
		}
		holder, err := m.store.GetLease(e.ID)
		if err != nil {
			return err
		}
		return inj(ctx, Handle{m: m, EnvID: e.ID, Generation: e.Generation, LeaseID: holder.LeaseID, Epoch: holder.Epoch},
			held.CredentialLease, secret)
	}
	return nil // "type": typed later, through LoginSecret
}

// LoginSecret redeems a lease taken for envID so one of its secret fields
// can be typed into the environment. The lease must be held by the
// environment, and one taken to inject a variable or a file is refused:
// what a person granted for that use is not typed.
//
// once takes one value of a once-only field, a backup code, which the
// store then forgets: field must name it.
func (m *Manager) LoginSecret(ctx context.Context, envID, leaseID, field string, once bool) (redact.Secret, error) {
	if m.cfg.Credentials == nil {
		return redact.Secret{}, noCredentials("typing a secret needs a credential lease")
	}
	var held HeldCredential
	var err error
	switch {
	case once && field == "":
		return redact.Secret{}, Invalidf("a once-only value is taken by its field's name, such as backup_codes")
	case once:
		held, err = m.cfg.Credentials.RedeemOnce(ctx, leaseID, credHolder(envID), field)
	default:
		held, err = m.cfg.Credentials.Redeem(ctx, leaseID, credHolder(envID))
	}
	if err != nil {
		return redact.Secret{}, err
	}
	if held.Use != envspec.UseLogin && held.Use != envspec.UseAndroidAccount {
		return redact.Secret{}, Invalidf("lease %s is for the use %s, which is not typed: lease the credential for %s",
			leaseID, held.Use, envspec.UseLogin)
	}
	if field, err = fieldOf(held, field); err != nil {
		return redact.Secret{}, err
	}
	value := m.learn(held)[field]
	if value.Reveal() == "" || slices.Contains(held.Base64, field) {
		return redact.Secret{}, Invalidf("credential %s: %q is not something to type", held.Cred, field)
	}
	return value, nil
}

// endCreds ends the credential leases an environment holds: now when ctx
// belongs to a call, and from the next SyncCredentials otherwise. The
// holder is queued in the state database first, so a lease is not left
// out because the manager stopped in between. It reports whether the
// leases were ended now.
func (m *Manager) endCreds(ctx context.Context, envID string) (bool, error) {
	holder := credHolder(envID)
	if m.cfg.Redactor != nil {
		m.cfg.Redactor.Forget(holder)
	}
	if m.cfg.Credentials == nil {
		return true, nil
	}
	if err := m.store.QueueCredDrop(holder); err != nil {
		return false, err
	}
	err := m.flushCredDrops(ctx)
	return err == nil, err
}

// flushCredDrops ends the leases of every queued holder. A holder leaves
// the queue only once the port has ended its leases.
func (m *Manager) flushCredDrops(ctx context.Context) error {
	if m.cfg.Credentials == nil {
		return nil
	}
	holders, err := m.store.CredDrops()
	if err != nil || len(holders) == 0 {
		return err
	}
	if err := m.cfg.Credentials.Drop(ctx, holders); err != nil {
		return err
	}
	return m.store.ClearCredDrops(holders)
}

// SyncCredentials does the credential work that waits for a call: it ends
// the leases of environments released while none was being served, and
// gives environments reopened after a restart their variables and files
// back and the redactor their secrets. The embedding application calls it
// with the context of each call it serves, before the call's own work. It
// is cheap when nothing waits. An error leaves the work for the next call.
func (m *Manager) SyncCredentials(ctx context.Context) error {
	if m.cfg.Credentials == nil {
		return nil
	}
	m.credSync.Lock()
	defer m.credSync.Unlock()
	err := m.flushCredDrops(ctx)
	if errors.Is(err, ErrNoCall) {
		return nil
	}
	m.mu.Lock()
	var waiting []*runtimeEnv
	for _, r := range m.runtime {
		r.mu.Lock()
		if r.credsWaiting {
			waiting = append(waiting, r)
		}
		r.mu.Unlock()
	}
	m.mu.Unlock()
	for _, r := range waiting {
		err = errors.Join(err, m.rehydrateCreds(ctx, r))
	}
	return err
}

// rehydrateCreds redeems what a reopened environment holds: every secret
// goes to the redactor, and the leases its spec delivered as a variable
// or a file are delivered again. Nothing is leased.
func (m *Manager) rehydrateCreds(ctx context.Context, r *runtimeEnv) error {
	e, err := m.freshEnv(r.id)
	if err != nil || e.Generation != r.generation || e.Phase.Terminal() {
		return nil // gone or replaced: its runtime goes with it
	}
	held, err := m.cfg.Credentials.RedeemHolder(ctx, credHolder(e.ID))
	if err != nil {
		return fmt.Errorf("credentials of %s: %w", e.ID, err)
	}
	for _, h := range held {
		m.learn(h)
	}
	var errs error
	used := make([]bool, len(held))
	for _, req := range e.Spec.Creds {
		if use, _ := deliveryUse(req); use != envspec.UseInject {
			continue
		}
		for i, h := range held {
			if used[i] || h.Use != envspec.UseInject || (req.Cred != "" && req.Cred != h.Cred) || (req.Kind != "" && req.Kind != h.Kind) {
				continue
			}
			used[i] = true
			errs = errors.Join(errs, m.deliverCred(ctx, e, r, h, req))
			break
		}
	}
	if errs != nil {
		return fmt.Errorf("credentials of %s: %w", e.ID, errs)
	}
	r.mu.Lock()
	r.credsWaiting = false
	r.mu.Unlock()
	return nil
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
