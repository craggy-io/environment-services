package vault

import (
	"context"
	"database/sql"
	"fmt"
	"path"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

// Lease is an active credential lease. Values are reachable only via Use.
type Lease struct {
	ID     string
	Cred   string
	Holder string
	Owner  string

	v        *Vault
	cred     Credential
	released bool
}

// Public returns the credential's agent-visible fields.
func (l *Lease) Public() map[string]string {
	out := make(map[string]string, len(l.cred.Public))
	for k, val := range l.cred.Public {
		out[k] = val
	}
	return out
}

// Kind returns the credential kind.
func (l *Lease) Kind() string { return l.cred.Kind }

// Use hands one secret field to fn, in-process only. It is the only way to
// touch a value (§6.3).
func (l *Lease) Use(field string, fn func(Secret) error) error {
	if l.released {
		return ErrReleased
	}
	s, ok := l.cred.Secret[field]
	if !ok {
		return fmt.Errorf("vault: credential %s has no secret field %q", l.Cred, field)
	}
	return fn(s)
}

// Release ends the lease. Idempotent.
func (l *Lease) Release(ctx context.Context) error {
	if l.released {
		return nil
	}
	l.released = true
	return l.v.release(l.ID, l.Cred, l.Holder)
}

// ReleaseByID releases a lease without a handle (envcap vault.release).
func (v *Vault) ReleaseByID(ctx context.Context, leaseID string) error {
	var cred, holder string
	var released int64
	err := v.db.QueryRowContext(ctx, `SELECT cred,holder,released FROM leases WHERE id=?`, leaseID).Scan(&cred, &holder, &released)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if released > 0 {
		return nil
	}
	return v.release(leaseID, cred, holder)
}

func (v *Vault) release(leaseID, cred, holder string) error {
	_, err := v.db.Exec(`UPDATE leases SET released=? WHERE id=? AND released=0`, v.now().Unix(), leaseID)
	return err
}

// ReleaseHolder releases every lease a holder owns (env released/lost).
func (v *Vault) ReleaseHolder(ctx context.Context, holder string) error {
	rows, err := v.db.QueryContext(ctx, `SELECT id,cred FROM leases WHERE holder=? AND released=0`, holder)
	if err != nil {
		return err
	}
	type item struct{ id, cred string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.cred); err != nil {
			rows.Close()
			return err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		if err := v.release(it.id, it.cred, holder); err != nil {
			return err
		}
	}
	return nil
}

// Leases returns active leases, optionally for one holder.
func (v *Vault) Leases(ctx context.Context, holder string) ([]*Lease, error) {
	now := v.now().Unix()
	q := `SELECT id,cred,holder,owner FROM leases WHERE released=0 AND (expires=0 OR expires>?)`
	args := []any{now}
	if holder != "" {
		q += ` AND holder=?`
		args = append(args, holder)
	}
	rows, err := v.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	// Collect first: the pool has one connection, so v.Get must not run
	// while rows are open (that deadlocks).
	var stubs []*Lease
	for rows.Next() {
		l := &Lease{v: v}
		if err := rows.Scan(&l.ID, &l.Cred, &l.Holder, &l.Owner); err != nil {
			rows.Close()
			return nil, err
		}
		stubs = append(stubs, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	out := stubs[:0]
	for _, l := range stubs {
		c, err := v.Get(l.Cred)
		if err != nil {
			continue
		}
		l.cred = c
		out = append(out, l)
	}
	return out, nil
}

// LeaseByID rehydrates a lease handle (TypeSecret by lease ID).
func (v *Vault) LeaseByID(ctx context.Context, leaseID string) (*Lease, error) {
	l := &Lease{v: v, ID: leaseID}
	var released, expires int64
	err := v.db.QueryRowContext(ctx, `SELECT cred,holder,owner,released,expires FROM leases WHERE id=?`, leaseID).
		Scan(&l.Cred, &l.Holder, &l.Owner, &released, &expires)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if released > 0 || (expires > 0 && expires < v.now().Unix()) {
		return nil, ErrReleased
	}
	c, err := v.Get(l.Cred)
	if err != nil {
		return nil, err
	}
	l.cred = c
	return l, nil
}

// Lease grants a credential lease. Policy is checked inside the same
// transaction that grants it, so two environments racing for the same
// account can't both win (§6.2). Selector requests skip denied
// credentials and pick the least-recently-used that passes every rule.
func (v *Vault) Lease(ctx context.Context, r LeaseRequest) (*Lease, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var candidates []credRow
	if r.Cred != "" {
		row := tx.QueryRowContext(ctx, `SELECT id,kind,name,public,secret,labels,policy,last_used FROM creds WHERE id=?`, r.Cred)
		c, err := scanCred(row)
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		candidates = []credRow{c}
	} else {
		rows, err := tx.QueryContext(ctx, `SELECT id,kind,name,public,secret,labels,policy,last_used FROM creds WHERE kind=? ORDER BY last_used ASC`, r.Select.Kind)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCred(rows)
			if err != nil {
				return nil, err
			}
			if labelsMatch(c.c.Labels, r.Select.Labels) {
				candidates = append(candidates, c)
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
	}
	if len(candidates) == 0 {
		return nil, &PolicyError{Rule: "no-candidate", Detail: fmt.Sprintf("no credential matches kind %q and labels %v", r.Select.Kind, r.Select.Labels)}
	}

	var lastErr *PolicyError
	for _, c := range candidates {
		lease, perr := v.tryLease(ctx, tx, c.c, r)
		if perr != nil {
			lastErr = perr
			if r.Cred != "" {
				break // explicit credential: deny loudly
			}
			continue // selector: skip and try the next
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		lease.v = v
		lease.cred = c.c
		return lease, nil
	}
	return nil, lastErr
}

func (v *Vault) tryLease(ctx context.Context, tx *sql.Tx, c Credential, r LeaseRequest) (*Lease, *PolicyError) {
	p := c.Policy
	now := v.now().Unix()

	if len(p.Owners) > 0 && !globAny(p.Owners, r.Owner) {
		return nil, &PolicyError{Rule: "owners", Detail: fmt.Sprintf("owner %q may not lease %s", r.Owner, c.ID)}
	}
	holderKind := r.HolderKind
	if holderKind == "" {
		holderKind = r.Holder
	}
	if len(p.HolderKinds) > 0 && !globAny(p.HolderKinds, holderKind) {
		return nil, &PolicyError{Rule: "holder-kinds", Detail: fmt.Sprintf("%s may not be held by %q", c.ID, holderKind)}
	}
	var active int
	var activeHolder string
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM leases WHERE cred=? AND released=0 AND (expires=0 OR expires>?)`, c.ID, now).Scan(&active)
	if p.MaxConcurrent > 0 && active >= p.MaxConcurrent {
		_ = tx.QueryRowContext(ctx, `SELECT holder FROM leases WHERE cred=? AND released=0 LIMIT 1`, c.ID).Scan(&activeHolder)
		return nil, &PolicyError{Rule: "max-concurrent",
			Detail: fmt.Sprintf("%s already has %d active lease(s)", c.ID, active), Holder: activeHolder}
	}
	if p.MaxPerDay > 0 {
		var starts int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM lease_starts WHERE cred=? AND started>?`, c.ID, now-86400).Scan(&starts)
		if starts >= p.MaxPerDay {
			return nil, &PolicyError{Rule: "max-per-day", Detail: fmt.Sprintf("%s reached %d lease starts in 24h", c.ID, p.MaxPerDay)}
		}
	}
	if p.Cooldown > 0 {
		var lastEnd sql.NullInt64
		_ = tx.QueryRowContext(ctx, `SELECT MAX(released) FROM leases WHERE cred=? AND released>0`, c.ID).Scan(&lastEnd)
		if lastEnd.Valid && now-lastEnd.Int64 < int64(p.Cooldown/time.Second) {
			return nil, &PolicyError{Rule: "cooldown", Detail: fmt.Sprintf("%s is cooling down until %s",
				c.ID, time.Unix(lastEnd.Int64, 0).Add(p.Cooldown).Format(time.RFC3339))}
		}
	}
	if p.OnePerHolder != "" {
		var held int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM leases l JOIN creds c2 ON c2.id=l.cred
			WHERE l.holder=? AND l.released=0 AND (l.expires=0 OR l.expires>?) AND c2.kind=?`,
			r.Holder, now, p.OnePerHolder).Scan(&held)
		if held >= 1 {
			return nil, &PolicyError{Rule: "one-per-holder", Detail: fmt.Sprintf("holder %s already leases a %s", r.Holder, p.OnePerHolder)}
		}
	}

	lease := &Lease{ID: envspec.NewID("lease_"), Cred: c.ID, Holder: r.Holder, Owner: r.Owner}
	var expires int64
	if r.TTL > 0 {
		expires = now + int64(r.TTL/time.Second)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO leases(id,cred,holder,holder_kind,owner,purpose,started,expires)
		VALUES(?,?,?,?,?,?,?,?)`, lease.ID, c.ID, r.Holder, holderKind, r.Owner, r.Purpose, now, expires); err != nil {
		return nil, &PolicyError{Rule: "store", Detail: err.Error()}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO lease_starts(cred,started) VALUES(?,?)`, c.ID, now); err != nil {
		return nil, &PolicyError{Rule: "store", Detail: err.Error()}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE creds SET last_used=? WHERE id=?`, now, c.ID); err != nil {
		return nil, &PolicyError{Rule: "store", Detail: err.Error()}
	}
	return lease, nil
}

// Sticky returns a credential's sticky device binding, if any.
func (v *Vault) Sticky(cred string) (StickyBinding, bool, error) {
	b := StickyBinding{CredID: cred}
	err := v.db.QueryRow(`SELECT avd,android_id,snapshot FROM sticky WHERE cred=?`, cred).
		Scan(&b.AVD, &b.AndroidID, &b.Snapshot)
	if err == sql.ErrNoRows {
		return b, false, nil
	}
	return b, err == nil, err
}

// SetSticky records the device identity a credential signed in from.
func (v *Vault) SetSticky(b StickyBinding) error {
	_, err := v.db.Exec(`INSERT INTO sticky(cred,avd,android_id,snapshot) VALUES(?,?,?,?)
		ON CONFLICT(cred) DO UPDATE SET avd=excluded.avd, android_id=excluded.android_id, snapshot=excluded.snapshot`,
		b.CredID, b.AVD, b.AndroidID, b.Snapshot)
	return err
}

// ExpireLeases releases TTL-expired leases and reports how many.
func (v *Vault) ExpireLeases(ctx context.Context) (int, error) {
	now := v.now().Unix()
	rows, err := v.db.QueryContext(ctx, `SELECT id,cred,holder FROM leases WHERE released=0 AND expires>0 AND expires<?`, now)
	if err != nil {
		return 0, err
	}
	type item struct{ id, cred, holder string }
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.cred, &it.holder); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	for _, it := range items {
		if _, err := v.db.ExecContext(ctx, `UPDATE leases SET released=? WHERE id=?`, now, it.id); err != nil {
			return 0, err
		}
	}
	return len(items), nil
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func globAny(patterns []string, s string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, s); ok {
			return true
		}
		if p == s {
			return true
		}
	}
	return false
}
