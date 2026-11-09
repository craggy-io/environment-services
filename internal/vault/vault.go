// Package vault holds environment test credentials behind leases and
// declarative policy. It is mutable, plaintext (test accounts
// only) and independent of env. Agents use credentials by reference and never
// see values: Secret redacts itself in every formatting path.
package vault

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/foundation-kit/adapters/sqlite"
)

// Secret wraps a credential value. All formatting prints "[redacted]". It
// is not encryption: it keeps values out of logs, tool results and agent
// context. The value is reachable only inside Lease.Use callbacks.
type Secret struct{ v string }

// NewSecret wraps a value.
func NewSecret(v string) Secret { return Secret{v: v} }

func (s Secret) String() string               { return "[redacted]" }
func (s Secret) GoString() string             { return "[redacted]" }
func (s Secret) Format(f fmt.State, _ rune)   { fmt.Fprint(f, "[redacted]") }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }
func (s Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Reveal returns the value. It exists for injection code inside this
// library (Lease.Use callbacks); agent-facing code must never call it.
func (s Secret) Reveal() string { return s.v }

// Policy is declarative and checked in the lease transaction.
type Policy struct {
	MaxConcurrent int           `json:"max_concurrent,omitempty"` // 1 = exclusive; 0 = unlimited
	Cooldown      time.Duration `json:"cooldown,omitempty"`
	MaxPerDay     int           `json:"max_per_day,omitempty"`
	HolderKinds   []string      `json:"holder_kinds,omitempty"` // holder globs, e.g. "env:android"
	Owners        []string      `json:"owners,omitempty"`       // owner-ID globs
	Sticky        bool          `json:"sticky,omitempty"`
	OnePerHolder  string        `json:"one_per_holder,omitempty"` // cred kind limited to one per holder
}

// Credential is one vault record.
type Credential struct {
	ID     string            `json:"id"`
	Kind   string            `json:"kind"` // "google.account", "password", "api-key", …
	Name   string            `json:"name"`
	Public map[string]string `json:"public,omitempty"` // shown to agents
	Secret map[string]Secret `json:"secret,omitempty"` // never shown; marshals redacted
	Labels map[string]string `json:"labels,omitempty"`
	Policy Policy            `json:"policy,omitempty"`
}

// PublicView is what agents may see: public fields, labels, availability.
type PublicView struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Public    map[string]string `json:"public,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Available bool              `json:"available"`
	Reason    string            `json:"reason,omitempty"` // why not, when unavailable
}

// Selector picks a credential by kind + labels (least-recently-used wins).
type Selector struct {
	Kind   string            `json:"kind"`
	Labels map[string]string `json:"labels,omitempty"`
}

// LeaseRequest asks for a credential lease.
type LeaseRequest struct {
	Cred       string // a credential ID, or empty to Select
	Select     Selector
	Holder     string // "env:env_01J…" or an owner ID; "env:android" style kinds also match HolderKinds via HolderKind
	HolderKind string // e.g. "env:android" for HolderKinds checks
	Owner      string // who asked; checked against Policy.Owners
	Purpose    string
	TTL        time.Duration
}

// StickyBinding records the device identity a sticky credential last used.
type StickyBinding struct {
	CredID    string `json:"cred_id"`
	AVD       string `json:"avd"`
	AndroidID string `json:"android_id"`
	Snapshot  string `json:"snapshot"`
}

var (
	// ErrPolicy names the rule that denied a lease.
	ErrPolicy = errors.New("vault: policy denied")
	// ErrNotFound — no credential or lease by that ID.
	ErrNotFound = errors.New("vault: not found")
	// ErrReleased — the lease is no longer active.
	ErrReleased = errors.New("vault: lease released")
)

// PolicyError carries the denying rule and context.
type PolicyError struct {
	Rule   string `json:"rule"`
	Detail string `json:"detail"`
	Holder string `json:"holder,omitempty"` // current holder, when relevant
}

func (e *PolicyError) Error() string        { return fmt.Sprintf("vault: policy %s: %s", e.Rule, e.Detail) }
func (e *PolicyError) Is(target error) bool { return target == ErrPolicy }

// Vault is the credential store. Safe for concurrent use. Callers that
// need lifecycle events write them around their own Lease/Release calls;
// the vault reports outcomes only through its return values.
type Vault struct {
	db  *sql.DB
	mu  sync.Mutex // serialises lease transactions
	now func() time.Time
}

// VaultOption configures a Vault.
type VaultOption func(*Vault)

func WithClock(now func() time.Time) VaultOption { return func(v *Vault) { v.now = now } }

const schema = `
CREATE TABLE IF NOT EXISTS creds (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
  public TEXT NOT NULL DEFAULT '{}', secret TEXT NOT NULL DEFAULT '{}',
  labels TEXT NOT NULL DEFAULT '{}', policy TEXT NOT NULL DEFAULT '{}',
  last_used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS leases (
  id TEXT PRIMARY KEY, cred TEXT NOT NULL, holder TEXT NOT NULL,
  holder_kind TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL DEFAULT '',
  purpose TEXT NOT NULL DEFAULT '', started INTEGER NOT NULL,
  expires INTEGER NOT NULL DEFAULT 0, released INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS lease_starts (
  cred TEXT NOT NULL, started INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS sticky (
  cred TEXT PRIMARY KEY, avd TEXT NOT NULL DEFAULT '',
  android_id TEXT NOT NULL DEFAULT '', snapshot TEXT NOT NULL DEFAULT ''
);`

// Open opens (or creates) vault.db at path.
func Open(path string, opts ...VaultOption) (*Vault, error) {
	db, err := sqlite.Open(context.Background(), path, sqlite.Options{})
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	v := &Vault{db: db, now: time.Now}
	for _, o := range opts {
		o(v)
	}
	// Expire stale leases left by a previous process.
	_, _ = db.Exec(`UPDATE leases SET released=? WHERE released=0 AND expires>0 AND expires<?`,
		v.now().Unix(), v.now().Unix())
	return v, nil
}

// Close closes the store.
func (v *Vault) Close() error { return v.db.Close() }

// Put creates or replaces a credential (admin path — the only way secrets
// enter). An ID is assigned when empty.
func (v *Vault) Put(c Credential) (Credential, error) {
	if c.ID == "" {
		c.ID = envspec.NewID("cred_")
	}
	pub, _ := json.Marshal(c.Public)
	sec := make(map[string]string, len(c.Secret))
	for k, s := range c.Secret {
		sec[k] = s.v
	}
	secJSON, _ := json.Marshal(sec)
	labels, _ := json.Marshal(c.Labels)
	policy, _ := json.Marshal(c.Policy)
	_, err := v.db.Exec(`INSERT INTO creds(id,kind,name,public,secret,labels,policy)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET
		kind=excluded.kind, name=excluded.name, public=excluded.public,
		secret=excluded.secret, labels=excluded.labels, policy=excluded.policy`,
		c.ID, c.Kind, c.Name, pub, secJSON, labels, policy)
	if err != nil {
		return c, err
	}
	return c, nil
}

// Delete really deletes a credential (the vault is mutable).
func (v *Vault) Delete(id string) error {
	tx, err := v.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM leases WHERE cred=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM lease_starts WHERE cred=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sticky WHERE cred=?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM creds WHERE id=?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// SetPolicy replaces a credential's policy.
func (v *Vault) SetPolicy(id string, p Policy) error {
	b, _ := json.Marshal(p)
	res, err := v.db.Exec(`UPDATE creds SET policy=? WHERE id=?`, b, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

type credRow struct {
	c        Credential
	lastUsed int64
}

func scanCred(row interface{ Scan(...any) error }) (credRow, error) {
	var r credRow
	var pub, sec, labels, policy string
	if err := row.Scan(&r.c.ID, &r.c.Kind, &r.c.Name, &pub, &sec, &labels, &policy, &r.lastUsed); err != nil {
		return r, err
	}
	_ = json.Unmarshal([]byte(pub), &r.c.Public)
	rawSec := map[string]string{}
	_ = json.Unmarshal([]byte(sec), &rawSec)
	r.c.Secret = make(map[string]Secret, len(rawSec))
	for k, s := range rawSec {
		r.c.Secret[k] = Secret{v: s}
	}
	_ = json.Unmarshal([]byte(labels), &r.c.Labels)
	_ = json.Unmarshal([]byte(policy), &r.c.Policy)
	return r, nil
}

// Get returns a credential by ID (internal use; secrets stay wrapped).
func (v *Vault) Get(id string) (Credential, error) {
	row := v.db.QueryRow(`SELECT id,kind,name,public,secret,labels,policy,last_used FROM creds WHERE id=?`, id)
	r, err := scanCred(row)
	if err == sql.ErrNoRows {
		return Credential{}, ErrNotFound
	}
	return r.c, err
}

// List returns agent-visible views with live availability.
func (v *Vault) List(ctx context.Context) ([]PublicView, error) {
	rows, err := v.db.QueryContext(ctx, `SELECT id,kind,name,public,secret,labels,policy,last_used FROM creds ORDER BY last_used ASC`)
	if err != nil {
		return nil, err
	}
	// Collect first: availability() queries again and the pool has one
	// connection — calling it with rows open deadlocks.
	var creds []credRow
	for rows.Next() {
		r, err := scanCred(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		creds = append(creds, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []PublicView
	for _, r := range creds {
		avail, reason := v.availability(r.c)
		out = append(out, PublicView{ID: r.c.ID, Kind: r.c.Kind, Name: r.c.Name,
			Public: r.c.Public, Labels: r.c.Labels, Available: avail, Reason: reason})
	}
	return out, nil
}

func (v *Vault) availability(c Credential) (bool, string) {
	now := v.now().Unix()
	active := 0
	_ = v.db.QueryRow(`SELECT COUNT(*) FROM leases WHERE cred=? AND released=0 AND (expires=0 OR expires>?)`, c.ID, now).Scan(&active)
	if c.Policy.MaxConcurrent > 0 && active >= c.Policy.MaxConcurrent {
		return false, fmt.Sprintf("leased (%d active)", active)
	}
	if c.Policy.MaxPerDay > 0 {
		starts := 0
		_ = v.db.QueryRow(`SELECT COUNT(*) FROM lease_starts WHERE cred=? AND started>?`, c.ID, now-86400).Scan(&starts)
		if starts >= c.Policy.MaxPerDay {
			return false, "daily lease cap reached"
		}
	}
	if c.Policy.Cooldown > 0 {
		var lastEnd sql.NullInt64
		_ = v.db.QueryRow(`SELECT MAX(released) FROM leases WHERE cred=? AND released>0`, c.ID).Scan(&lastEnd)
		if lastEnd.Valid && now-lastEnd.Int64 < int64(c.Policy.Cooldown/time.Second) {
			return false, "cooldown after previous lease"
		}
	}
	return true, ""
}
