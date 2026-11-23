package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/envtestfake"
	"github.com/craggy-io/environment-services/internal/redact"
)

// fakeCreds is a credential store behind the port. A context carries a
// call only when it has callKey: anything else is the manager's own
// background work, which the store refuses with ErrNoCall.
type fakeCreds struct {
	mu      sync.Mutex
	secrets map[string]HeldCredential // by credential id: kind and secret
	leases  []CredentialLease
	seq     int
	refuse  error // what Hold answers, when set
	dropped [][]string
}

type callKey struct{}

func inCall() context.Context { return context.WithValue(context.Background(), callKey{}, true) }

func (f *fakeCreds) called(ctx context.Context) error {
	if ctx.Value(callKey{}) == nil {
		return ErrNoCall
	}
	return nil
}

func (f *fakeCreds) Hold(ctx context.Context, req CredentialRequest) (CredentialLease, error) {
	if err := f.called(ctx); err != nil {
		return CredentialLease{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return CredentialLease{}, f.refuse
	}
	cred, ok := f.secrets[req.Cred]
	if !ok {
		return CredentialLease{}, fmt.Errorf("%w: no credential %s", envspec.ErrNotFound, req.Cred)
	}
	for _, l := range f.leases {
		if l.Cred == req.Cred && l.Use == req.Use && l.Holder == req.Holder {
			return l, nil
		}
	}
	f.seq++
	l := CredentialLease{ID: fmt.Sprintf("lease_%d", f.seq), Cred: req.Cred, Kind: cred.Kind, Use: req.Use, Holder: req.Holder}
	f.leases = append(f.leases, l)
	return l, nil
}

func (f *fakeCreds) held(l CredentialLease) HeldCredential {
	return HeldCredential{CredentialLease: l, Secret: f.secrets[l.Cred].Secret}
}

func (f *fakeCreds) Redeem(ctx context.Context, lease, holder string) (HeldCredential, error) {
	if err := f.called(ctx); err != nil {
		return HeldCredential{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, l := range f.leases {
		if l.ID == lease && l.Holder == holder {
			return f.held(l), nil
		}
	}
	return HeldCredential{}, fmt.Errorf("%w: no lease %s", envspec.ErrNotFound, lease)
}

func (f *fakeCreds) RedeemHolder(ctx context.Context, holder string) ([]HeldCredential, error) {
	if err := f.called(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []HeldCredential
	for _, l := range f.leases {
		if l.Holder == holder {
			out = append(out, f.held(l))
		}
	}
	return out, nil
}

func (f *fakeCreds) Drop(ctx context.Context, holders []string) error {
	if err := f.called(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropped = append(f.dropped, slices.Clone(holders))
	f.leases = slices.DeleteFunc(f.leases, func(l CredentialLease) bool { return slices.Contains(holders, l.Holder) })
	return nil
}

func (f *fakeCreds) leasesOf(holder string) []CredentialLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []CredentialLease
	for _, l := range f.leases {
		if l.Holder == holder {
			out = append(out, l)
		}
	}
	return out
}

const testSecret = "Correct Horse 42!"

func newFakeCreds() *fakeCreds {
	return &fakeCreds{secrets: map[string]HeldCredential{
		"cred_key":  {CredentialLease: CredentialLease{Kind: "api-key"}, Secret: map[string]string{"api_key": testSecret}},
		"cred_acct": {CredentialLease: CredentialLease{Kind: "google.account"}, Secret: map[string]string{"password": "Battery Staple 7?"}},
	}}
}

func credsConfig(dir string, creds *fakeCreds, adapters ...adapter.Adapter) Config {
	cfg := testConfig(dir, adapters...)
	cfg.Credentials, cfg.Redactor = creds, redact.New()
	return cfg
}

// A spec's credentials are leased for the environment under the use the
// delivery decides, delivered, redacted, and ended with the environment.
func TestCredsLeasedDeliveredAndDroppedUnderACall(t *testing.T) {
	world := envtestfake.NewWorld()
	creds := newFakeCreds()
	m := openMgr(t, credsConfig(t.TempDir(), creds, envtestfake.NewCreated(world, envspec.Container)))
	h, err := m.Provision(inCall(), ownerA, envspec.Spec{Profile: "fake", Creds: []envspec.CredRequest{
		{Cred: "cred_key", Deliver: "env:API_TOKEN"},
		{Cred: "cred_key", Deliver: "file:/run/token"},
		{Cred: "cred_acct", Deliver: "type"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	holder := "env:" + h.EnvID
	uses := map[string]string{}
	for _, l := range creds.leasesOf(holder) {
		uses[l.Cred] = l.Use
	}
	if len(uses) != 2 || uses["cred_key"] != envspec.UseInject || uses["cred_acct"] != envspec.UseLogin {
		t.Fatalf("leases by credential = %v, want one %s and one %s", uses, envspec.UseInject, envspec.UseLogin)
	}
	res := world.Get(adapter.NameFor(h.EnvID))
	if got := res.InjectedEnv()["API_TOKEN"]; got != testSecret {
		t.Fatalf("API_TOKEN = %q", got)
	}
	if got := m.redact("the key is " + testSecret); strings.Contains(got, testSecret) {
		t.Fatalf("a delivered secret is not redacted: %q", got)
	}
	if got := m.redact("Battery Staple 7?"); got != "Battery Staple 7?" {
		t.Fatalf("a secret leased for typing was redeemed at provision: %q", got)
	}

	if err := m.Release(inCall(), h.EnvID); err != nil {
		t.Fatal(err)
	}
	if left := creds.leasesOf(holder); len(left) != 0 {
		t.Fatalf("leases after release = %v", left)
	}
	if queued, _ := m.store.CredDrops(); len(queued) != 0 {
		t.Fatalf("drops still queued after a release under a call: %v", queued)
	}
	if got := m.redact(testSecret); got != testSecret {
		t.Fatalf("the redactor kept a released holder's secret: %q", got)
	}
}

// A release with no call being served queues the holder durably, and the
// next call ends its leases.
func TestCredDropWaitsForACall(t *testing.T) {
	dir := t.TempDir()
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	creds := newFakeCreds()
	m := openMgr(t, credsConfig(dir, creds, created))
	h, err := m.Provision(inCall(), ownerA, envspec.Spec{Profile: "fake", Creds: []envspec.CredRequest{
		{Cred: "cred_key", Deliver: "env:API_TOKEN"}}})
	if err != nil {
		t.Fatal(err)
	}
	holder := "env:" + h.EnvID
	// The reaper's context: no call.
	if err := m.Release(context.Background(), h.EnvID); err != nil {
		t.Fatal(err)
	}
	if left := creds.leasesOf(holder); len(left) != 1 {
		t.Fatalf("leases after a release outside a call = %v, want the lease still out", left)
	}
	if queued, _ := m.store.CredDrops(); !slices.Equal(queued, []string{holder}) {
		t.Fatalf("queued drops = %v, want [%s]", queued, holder)
	}
	if err := m.SyncCredentials(context.Background()); err != nil {
		t.Fatalf("SyncCredentials outside a call: %v", err)
	}

	// The queue outlives the manager.
	_ = m.Close(context.Background(), ShutdownDisconnect)
	m2 := openMgr(t, credsConfig(dir, creds, created))
	if err := m2.SyncCredentials(inCall()); err != nil {
		t.Fatal(err)
	}
	if left := creds.leasesOf(holder); len(left) != 0 {
		t.Fatalf("leases after the next call = %v", left)
	}
	if queued, _ := m2.store.CredDrops(); len(queued) != 0 {
		t.Fatalf("queued drops after the next call = %v", queued)
	}
}

// After a restart nothing is redeemed at Open: the first call gives a
// reopened environment its variable back and the redactor its secret.
func TestCredsRedeliveredByFirstCallAfterRestart(t *testing.T) {
	dir := t.TempDir()
	world := envtestfake.NewWorld()
	created := envtestfake.NewCreated(world, envspec.Container)
	creds := newFakeCreds()
	m1 := openMgr(t, credsConfig(dir, creds, created))
	h, err := m1.Provision(inCall(), ownerA, envspec.Spec{Profile: "fake", Creds: []envspec.CredRequest{
		{Cred: "cred_key", Deliver: "env:API_TOKEN"}}})
	if err != nil {
		t.Fatal(err)
	}
	m1.simulateCrash()

	m2 := openMgr(t, credsConfig(dir, creds, created))
	runtime := func() *runtimeEnv {
		m2.mu.Lock()
		defer m2.mu.Unlock()
		return m2.runtime[h.EnvID]
	}
	eventually(t, "runtime reopened", func() bool { return runtime() != nil })
	r := runtime()
	r.mu.Lock()
	early := len(r.credEnv)
	r.mu.Unlock()
	if early != 0 || m2.redact(testSecret) != testSecret {
		t.Fatal("a secret was redeemed at Open, with no call being served")
	}
	if err := m2.SyncCredentials(inCall()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	got, waiting := r.credEnv["API_TOKEN"], r.credsWaiting
	r.mu.Unlock()
	if got != testSecret || waiting {
		t.Fatalf("after the first call: API_TOKEN = %q, still waiting = %t", got, waiting)
	}
	if strings.Contains(m2.redact(testSecret), testSecret) {
		t.Fatal("the redactor was not taught the redelivered secret")
	}
	if n := len(creds.leasesOf("env:" + h.EnvID)); n != 1 {
		t.Fatalf("leases after redelivery = %d, want the one it had", n)
	}
}

// A refused lease fails the provision with the port's error intact, and a
// provision the manager drives with no call can't lease at all.
func TestCredRefusalsFailProvision(t *testing.T) {
	world := envtestfake.NewWorld()
	creds := newFakeCreds()
	m := openMgr(t, credsConfig(t.TempDir(), creds, envtestfake.NewCreated(world, envspec.Container)))
	spec := envspec.Spec{Profile: "fake", Creds: []envspec.CredRequest{{Cred: "cred_key", Deliver: "env:API_TOKEN"}}}

	creds.refuse = fmt.Errorf("%w: cred_key is held by env:other", ErrCredentialUnavailable)
	if _, err := m.Provision(inCall(), ownerA, spec); !errors.Is(err, ErrCredentialUnavailable) || !errors.Is(err, ErrCredentials) {
		t.Fatalf("provision with a held credential: %v", err)
	}
	creds.refuse = nil
	if _, err := m.Provision(context.Background(), ownerA, spec); !errors.Is(err, ErrNoCall) {
		t.Fatalf("provision outside a call: %v", err)
	}
	for name, req := range map[string]envspec.CredRequest{
		"a variable is never a login":     {Cred: "cred_key", Deliver: "env:API_TOKEN", Use: envspec.UseLogin},
		"an account is never injected":    {Cred: "cred_acct", Deliver: "account", Use: envspec.UseInject},
		"typing is never an injected use": {Cred: "cred_acct", Deliver: "type", Use: envspec.UseInject},
		"unknown delivery":                {Cred: "cred_key", Deliver: "clipboard"},
	} {
		_, err := m.Provision(inCall(), ownerA, envspec.Spec{Profile: "fake", Creds: []envspec.CredRequest{req}})
		if !errors.Is(err, envspec.ErrInvalid) {
			t.Errorf("%s: %v, want invalid", name, err)
		}
	}
	eventually(t, "failed provisions leave no lease", func() bool {
		_ = m.SyncCredentials(inCall())
		creds.mu.Lock()
		defer creds.mu.Unlock()
		return len(creds.leases) == 0
	})
}

// A lease is typed only when the environment holds it and it was taken
// for typing.
func TestLoginSecretChecksHolderAndUse(t *testing.T) {
	world := envtestfake.NewWorld()
	creds := newFakeCreds()
	m := openMgr(t, credsConfig(t.TempDir(), creds, envtestfake.NewCreated(world, envspec.Container)))
	ctx := inCall()
	hold := func(cred, use, holder string) string {
		t.Helper()
		l, err := creds.Hold(ctx, CredentialRequest{Cred: cred, Use: use, Holder: holder})
		if err != nil {
			t.Fatal(err)
		}
		return l.ID
	}
	login := hold("cred_acct", envspec.UseLogin, "env:env_a")
	secret, err := m.LoginSecret(ctx, "env_a", login, "")
	if err != nil || secret.Reveal() != "Battery Staple 7?" {
		t.Fatalf("LoginSecret = %v, %v", secret, err)
	}
	if strings.Contains(m.redact("typed Battery Staple 7?"), "Battery") {
		t.Fatal("a typed secret is not redacted")
	}
	if _, err := m.LoginSecret(ctx, "env_b", login, ""); !errors.Is(err, envspec.ErrNotFound) {
		t.Fatalf("a lease another environment holds: %v", err)
	}
	if _, err := m.LoginSecret(ctx, "env_a", login, "pin"); !errors.Is(err, envspec.ErrInvalid) {
		t.Fatalf("a field the credential lacks: %v", err)
	}
	inject := hold("cred_key", envspec.UseInject, "env:env_a")
	if _, err := m.LoginSecret(ctx, "env_a", inject, ""); !errors.Is(err, envspec.ErrInvalid) || !strings.Contains(err.Error(), envspec.UseInject) {
		t.Fatalf("a lease taken for %s: %v", envspec.UseInject, err)
	}
	account := hold("cred_acct", envspec.UseAndroidAccount, "env:env_a")
	if _, err := m.LoginSecret(ctx, "env_a", account, "password"); err != nil {
		t.Fatalf("a lease taken for %s: %v", envspec.UseAndroidAccount, err)
	}
}
