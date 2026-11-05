package vault_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/internal/vault"
)

func openVault(t *testing.T, opts ...vault.VaultOption) *vault.Vault {
	t.Helper()
	v, err := vault.Open(filepath.Join(t.TempDir(), "vault.db"), opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = v.Close() })
	return v
}

func TestSecretNeverPrints(t *testing.T) {
	s := vault.NewSecret("hunter2-password")
	forms := []string{s.String(), s.GoString(), fmt.Sprintf("%v", s), fmt.Sprintf("%s", s), fmt.Sprintf("%+v", s)}
	b, _ := json.Marshal(map[string]vault.Secret{"password": s})
	forms = append(forms, string(b))
	for _, f := range forms {
		if strings.Contains(f, "hunter2") {
			t.Fatalf("secret leaked in %q", f)
		}
	}
	if !strings.Contains(string(b), "[redacted]") {
		t.Fatalf("json = %s", b)
	}
}

func TestExclusiveRaceOneWinner(t *testing.T) {
	v := openVault(t)
	c, err := v.Put(vault.Credential{Kind: "google.account", Name: "pool-1",
		Public: map[string]string{"email": "a@example.test"},
		Secret: map[string]vault.Secret{"password": vault.NewSecret("s3cret-long")},
		Policy: vault.Policy{MaxConcurrent: 1}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var wins atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: fmt.Sprintf("env:e%d", i), Owner: "agent:x"})
			if err == nil {
				wins.Add(1)
				_ = l.Release
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("exclusive lease winners = %d, want exactly 1", wins.Load())
	}
	// Denied error names the current holder.
	first, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "env:first", Owner: "agent:x"})
	if err != nil {
		// The race winner may still hold it; release all then retry.
		_ = v.ReleaseHolder(ctx, "")
		leases, _ := v.Leases(ctx, "")
		for _, l := range leases {
			_ = l.Release(ctx)
		}
		first, err = v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "env:first", Owner: "agent:x"})
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "env:second", Owner: "agent:x"})
	var pe *vault.PolicyError
	if err == nil || !strings.Contains(err.Error(), "max-concurrent") {
		t.Fatalf("second lease err = %v", err)
	}
	_ = pe
	_ = first.Release(ctx)
	second, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "env:second", Owner: "agent:x"})
	if err != nil {
		t.Fatal(err)
	}
	// Use is the only value path, and Public carries no secrets.
	if pub := second.Public(); pub["email"] != "a@example.test" {
		t.Fatalf("public = %v", pub)
	}
	got := ""
	err = second.Use("password", func(s vault.Secret) error { got = s.Reveal(); return nil })
	if err != nil || got != "s3cret-long" {
		t.Fatalf("use: %q %v", got, err)
	}
	if err := second.Use("nope", func(vault.Secret) error { return nil }); err == nil {
		t.Fatal("missing field must error")
	}
}

func TestPolicyCooldownDailyOwnersSelectors(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	v := openVault(t, vault.WithClock(clock))
	ctx := context.Background()
	for _, name := range []string{"g1", "g2"} {
		_, err := v.Put(vault.Credential{Kind: "google.account", Name: name,
			Labels: map[string]string{"pool": "test"},
			Secret: map[string]vault.Secret{"password": vault.NewSecret("pw-" + name + "-long")},
			Policy: vault.Policy{MaxConcurrent: 1, Owners: []string{"agent:*"}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Owner glob denies modules.
	if _, err := v.Lease(ctx, vault.LeaseRequest{Select: vault.Selector{Kind: "google.account"}, Holder: "h", Owner: "module:apk"}); err == nil {
		t.Fatal("module owner should be denied by owner glob")
	}
	// Selector leases one, second selector picks the OTHER (skip denied).
	l1, err := v.Lease(ctx, vault.LeaseRequest{Select: vault.Selector{Kind: "google.account", Labels: map[string]string{"pool": "test"}}, Holder: "env:1", Owner: "agent:a"})
	if err != nil {
		t.Fatal(err)
	}
	l2, err := v.Lease(ctx, vault.LeaseRequest{Select: vault.Selector{Kind: "google.account"}, Holder: "env:2", Owner: "agent:b"})
	if err != nil {
		t.Fatal(err)
	}
	if l1.Cred == l2.Cred {
		t.Fatal("selector must skip the exclusively-held credential")
	}
	if _, err := v.Lease(ctx, vault.LeaseRequest{Select: vault.Selector{Kind: "google.account"}, Holder: "env:3", Owner: "agent:c"}); err == nil {
		t.Fatal("pool exhausted must error")
	}
	_ = l1.Release(ctx)
	_ = l2.Release(ctx)

	// Cooldown + MaxPerDay.
	c, _ := v.Put(vault.Credential{Kind: "api-key", Name: "k1",
		Secret: map[string]vault.Secret{"api_key": vault.NewSecret("key-long-value")},
		Policy: vault.Policy{Cooldown: time.Hour, MaxPerDay: 2}})
	l, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "h1", Owner: "agent:a"})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Release(ctx)
	if _, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "h2", Owner: "agent:a"}); err == nil || !strings.Contains(err.Error(), "cooldown") {
		t.Fatalf("cooldown err = %v", err)
	}
	now = now.Add(2 * time.Hour)
	l, err = v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "h2", Owner: "agent:a"})
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Release(ctx)
	now = now.Add(2 * time.Hour)
	if _, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "h3", Owner: "agent:a"}); err == nil || !strings.Contains(err.Error(), "max-per-day") {
		t.Fatalf("daily cap err = %v", err)
	}
	// 25h later the daily window frees it.
	now = now.Add(25 * time.Hour)
	if _, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "h4", Owner: "agent:a"}); err != nil {
		t.Fatal(err)
	}
}

func TestOnePerHolderStickyDeletePersist(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	v1, err := vault.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_, _ = v1.Put(vault.Credential{Kind: "google.account", Name: fmt.Sprintf("g%d", i),
			Secret: map[string]vault.Secret{"password": vault.NewSecret("pw-long-value")},
			Policy: vault.Policy{OnePerHolder: "google.account", Sticky: true}})
	}
	l, err := v1.Lease(ctx, vault.LeaseRequest{Select: vault.Selector{Kind: "google.account"}, Holder: "env:x", Owner: "agent:a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v1.Lease(ctx, vault.LeaseRequest{Select: vault.Selector{Kind: "google.account"}, Holder: "env:x", Owner: "agent:a"}); err == nil {
		t.Fatal("one-per-holder violated")
	}
	if err := v1.SetSticky(vault.StickyBinding{CredID: l.Cred, AVD: "avd1", AndroidID: "aid", Snapshot: "clean"}); err != nil {
		t.Fatal(err)
	}
	credID := l.Cred
	_ = v1.Close()
	// Persistence: exclusivity and sticky survive a reopen.
	v2, err := vault.Open(filepath.Join(dir, "v.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	b, ok, err := v2.Sticky(credID)
	if err != nil || !ok || b.AVD != "avd1" {
		t.Fatalf("sticky = %+v ok=%v err=%v", b, ok, err)
	}
	if _, err := v2.Lease(ctx, vault.LeaseRequest{Select: vault.Selector{Kind: "google.account"}, Holder: "env:x", Owner: "agent:a"}); err == nil {
		t.Fatal("persisted lease must still block one-per-holder after restart")
	}
	// Real delete.
	if err := v2.Delete(credID); err != nil {
		t.Fatal(err)
	}
	if _, err := v2.Get(credID); err == nil {
		t.Fatal("deleted credential must be gone")
	}
}

func TestRedactorFormsAndSignalShapes(t *testing.T) {
	v := openVault(t)
	ctx := context.Background()
	c, _ := v.Put(vault.Credential{Kind: "password", Name: "p",
		Secret: map[string]vault.Secret{"password": vault.NewSecret("Correct Horse 42!")}})
	if _, err := v.Lease(ctx, vault.LeaseRequest{Cred: c.ID, Holder: "env:x", Owner: "agent:a"}); err != nil {
		t.Fatal(err)
	}
	r := v.Redactor()
	cases := []string{
		"pw is Correct Horse 42! ok",
		"b64 " + base64.StdEncoding.EncodeToString([]byte("Correct Horse 42!")),
		"short tp", // no leak, unchanged
	}
	for _, in := range cases[:2] {
		out := r.RedactString(in)
		if strings.Contains(out, "Correct Horse") || !strings.Contains(out, "[secret:") {
			t.Fatalf("redact(%q) = %q", in, out)
		}
	}
	if !r.ContainsSecret([]byte("xx Correct Horse 42! yy")) {
		t.Fatal("ContainsSecret missed pcap-style bytes")
	}
	// Signal.Redactor shape: maps scrubbed, bytes passed through.
	m := r.Redact("s@1", map[string]any{"text": "Correct Horse 42!"})
	if strings.Contains(fmt.Sprintf("%v", m), "Correct Horse") {
		t.Fatalf("map redact failed: %v", m)
	}
	raw := []byte("binary Correct Horse 42! bytes")
	got, _ := r.Redact("net.pcap", raw).([]byte)
	if string(got) != string(raw) {
		t.Fatal("byte payloads must pass through untouched")
	}
	// After release + refresh the matcher forgets it.
	_ = v.ReleaseHolder(ctx, "env:x")
	_ = r.Refresh(v)
	if r.ContainsSecret(raw) {
		t.Fatal("redactor must forget released secrets")
	}
}
