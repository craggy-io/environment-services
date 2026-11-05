// Package store is the durable state of the lifecycle core: one SQLite
// file holding envs, operations, leases (+borrows), conditions,
// activities and the lifecycle event log. Every mutation is a typed,
// revision-conditional transition that writes its event in the same
// transaction; a failed write flips the store unhealthy and the core
// closes admission rather than acting on unrecorded intent.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/foundation-kit/adapters/sqlite"
)

// ErrConflict is a stale-revision or lost-race transition.
var ErrConflict = envspec.ErrConflict

// ErrNotFound wraps envspec.ErrNotFound for missing rows.
var ErrNotFound = envspec.ErrNotFound

type Desired string

const (
	DesiredPresent  Desired = "present"
	DesiredReleased Desired = "released"
)

type Presence string

const (
	PresencePresent Presence = "present"
	PresenceAbsent  Presence = "absent"
	PresenceUnknown Presence = "unknown"
)

// Env is one envs row.
type Env struct {
	ID         string
	Kind       envspec.Kind
	Profile    string
	Spec       envspec.Spec
	Owner      envspec.Owner
	Generation int64
	Revision   int64
	Desired    Desired
	Phase      envspec.State
	Presence   Presence
	Instance   string // adapter.Ref JSON
	Image      string
	Warm       bool
	CostPool   envspec.Pool
	CostCPUs   float64
	CostMemMB  int64
	CostDiskMB int64
	Created    time.Time
}

type OpKind string

const (
	OpProvision OpKind = "provision"
	OpRelease   OpKind = "release"
	OpReset     OpKind = "reset"
	OpAttach    OpKind = "attach"
)

type Outcome string

const (
	OutcomePending Outcome = "pending"
	OutcomeOK      Outcome = "ok"
	OutcomeFailed  Outcome = "failed"
	OutcomeUnknown Outcome = "unknown"
)

// Operation is one durable operation row.
type Operation struct {
	ID            string
	Env           string
	Generation    int64
	Kind          OpKind
	OwnerScope    string
	RequestKey    string
	RequestDigest string
	Phase         string
	Attempt       int
	NextAttempt   time.Time
	Deadline      time.Time
	Outcome       Outcome
	ExternalIDs   string // JSON map of discovered external identities
	Disposition   string
	Error         string
	Created       time.Time
	Updated       time.Time
}

type LeaseState string

const (
	LeaseHeld     LeaseState = "held"
	LeaseReleased LeaseState = "released"
	LeaseExpired  LeaseState = "expired"
)

// Lease is an env's holder lease (one row per env).
type Lease struct {
	Env     string
	LeaseID string
	Epoch   int64
	Holder  string // owner ID
	Expires time.Time
	State   LeaseState
}

type Borrow struct {
	Env     string
	Job     string
	Expires time.Time
}

type ActivityKind string

const (
	ActivityProbe     ActivityKind = "probe"
	ActivityCollector ActivityKind = "collector"
	ActivityComponent ActivityKind = "component"
	ActivityService   ActivityKind = "service"
)

type ActivityState string

const (
	ActivityPending  ActivityState = "pending"
	ActivityStarting ActivityState = "starting"
	ActivityRunning  ActivityState = "running"
	ActivityBackoff  ActivityState = "backing_off"
	ActivityHeld     ActivityState = "held"
	ActivityStopping ActivityState = "stopping"
	ActivityStopped  ActivityState = "stopped"
	ActivityFailed   ActivityState = "failed"
)

// Activity is one desired-activity row.
type Activity struct {
	Env         string
	Generation  int64
	Name        string
	Kind        ActivityKind
	Desired     string // "running" | "stopped"
	Required    bool
	State       ActivityState
	Attempts    int
	NextAttempt time.Time
	LastOK      time.Time
	LastError   string
	Params      string // JSON, kind-specific
}

const schema = `
CREATE TABLE IF NOT EXISTS envs (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, profile TEXT NOT NULL DEFAULT '',
  spec TEXT NOT NULL DEFAULT '{}', owner TEXT NOT NULL DEFAULT '{}',
  generation INTEGER NOT NULL DEFAULT 1, revision INTEGER NOT NULL DEFAULT 1,
  desired TEXT NOT NULL DEFAULT 'present', phase TEXT NOT NULL,
  presence TEXT NOT NULL DEFAULT 'unknown', instance TEXT NOT NULL DEFAULT '',
  image TEXT NOT NULL DEFAULT '', warm INTEGER NOT NULL DEFAULT 0,
  cost_pool TEXT NOT NULL DEFAULT '', cost_cpus REAL NOT NULL DEFAULT 0,
  cost_mem_mb INTEGER NOT NULL DEFAULT 0, cost_disk_mb INTEGER NOT NULL DEFAULT 0,
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY, env TEXT NOT NULL, generation INTEGER NOT NULL DEFAULT 1,
  kind TEXT NOT NULL, owner_scope TEXT NOT NULL DEFAULT '',
  request_key TEXT NOT NULL DEFAULT '', request_digest TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 0,
  next_attempt INTEGER NOT NULL DEFAULT 0, deadline INTEGER NOT NULL DEFAULT 0,
  outcome TEXT NOT NULL DEFAULT 'pending', external_ids TEXT NOT NULL DEFAULT '',
  disposition TEXT NOT NULL DEFAULT '', error TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL, updated INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS operations_request_key
  ON operations(owner_scope, request_key) WHERE request_key <> '';
CREATE INDEX IF NOT EXISTS operations_env ON operations(env);
CREATE TABLE IF NOT EXISTS leases (
  env TEXT PRIMARY KEY, lease_id TEXT NOT NULL DEFAULT '',
  epoch INTEGER NOT NULL DEFAULT 0, holder TEXT NOT NULL DEFAULT '',
  expires INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'released'
);
CREATE TABLE IF NOT EXISTS borrows (
  env TEXT NOT NULL, job TEXT NOT NULL, expires INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (env, job)
);
CREATE TABLE IF NOT EXISTS conditions (
  env TEXT NOT NULL, generation INTEGER NOT NULL, name TEXT NOT NULL,
  status TEXT NOT NULL, reason TEXT NOT NULL DEFAULT '',
  observed_at INTEGER NOT NULL, detail TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (env, generation, name)
);
CREATE TABLE IF NOT EXISTS activities (
  env TEXT NOT NULL, generation INTEGER NOT NULL, name TEXT NOT NULL,
  kind TEXT NOT NULL, desired TEXT NOT NULL DEFAULT 'running',
  required INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL DEFAULT 'pending',
  attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0,
  last_ok INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
  params TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (env, generation, name)
);
CREATE TABLE IF NOT EXISTS closed_scopes (
  kind TEXT NOT NULL, id TEXT NOT NULL, PRIMARY KEY (kind, id)
);
CREATE TABLE IF NOT EXISTS events (
  seq INTEGER PRIMARY KEY AUTOINCREMENT, kind TEXT NOT NULL,
  env TEXT NOT NULL DEFAULT '', job TEXT NOT NULL DEFAULT '',
  session TEXT NOT NULL DEFAULT '', owner TEXT NOT NULL DEFAULT '',
  ts INTEGER NOT NULL, data TEXT NOT NULL DEFAULT '{}'
);`

// Store wraps the state database. MaxOpenConns(1) serialises writers.
type Store struct {
	db      *sql.DB
	healthy atomic.Bool
}

// Open opens (creating) the state database at path.
func Open(path string) (*Store, error) {
	db, err := sqlite.Open(context.Background(), path, sqlite.Options{ForeignKeys: true})
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	s.healthy.Store(true)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Healthy reports whether the last durable writes succeeded. A failed
// write flips it false permanently for this Open; the core refuses new
// state-changing work while unhealthy.
func (s *Store) Healthy() bool { return s.healthy.Load() }

func (s *Store) fail(err error) error {
	if err != nil {
		s.healthy.Store(false)
	}
	return err
}

func ts(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func unts(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Emit appends a lifecycle event outside a state transition.
func (s *Store) Emit(ev envspec.LifecycleEvent) error {
	return s.fail(emitExec(s.db, ev))
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func emitExec(x execer, ev envspec.LifecycleEvent) error {
	data, _ := json.Marshal(ev.Data)
	if ev.Data == nil {
		data = []byte("{}")
	}
	_, err := x.Exec(`INSERT INTO events(kind,env,job,session,owner,ts,data) VALUES(?,?,?,?,?,?,?)`,
		ev.Kind, ev.Env, ev.Job, ev.Session, ev.Owner, ts(ev.Time), string(data))
	return err
}

// EventFor builds an event attributed to an env's owner.
func EventFor(kind string, e Env, now time.Time, data map[string]any) envspec.LifecycleEvent {
	return envspec.LifecycleEvent{Kind: kind, Env: e.ID, Job: e.Owner.Job,
		Session: e.Owner.Session, Owner: e.Owner.ID, Time: now, Data: data}
}

// requireOne turns a conditional write's result into ErrConflict unless
// exactly one row changed.
func requireOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: stale revision", ErrConflict)
	}
	return nil
}

const envCols = `id,kind,profile,spec,owner,generation,revision,desired,phase,presence,instance,image,warm,cost_pool,cost_cpus,cost_mem_mb,cost_disk_mb,created`

func scanEnv(row interface{ Scan(...any) error }) (Env, error) {
	var e Env
	var specJSON, ownerJSON, kind, desired, phase, presence, pool string
	var warm int
	var created int64
	err := row.Scan(&e.ID, &kind, &e.Profile, &specJSON, &ownerJSON, &e.Generation, &e.Revision,
		&desired, &phase, &presence, &e.Instance, &e.Image, &warm, &pool,
		&e.CostCPUs, &e.CostMemMB, &e.CostDiskMB, &created)
	if err != nil {
		return e, err
	}
	e.Kind = envspec.Kind(kind)
	e.Desired = Desired(desired)
	e.Phase = envspec.State(phase)
	e.Presence = Presence(presence)
	e.CostPool = envspec.Pool(pool)
	e.Warm = warm != 0
	e.Created = unts(created)
	_ = json.Unmarshal([]byte(specJSON), &e.Spec)
	_ = json.Unmarshal([]byte(ownerJSON), &e.Owner)
	return e, nil
}

// GetEnv reads one env row.
func (s *Store) GetEnv(id string) (Env, error) {
	e, err := scanEnv(s.db.QueryRow(`SELECT `+envCols+` FROM envs WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("%w: env %s", ErrNotFound, id)
	}
	return e, err
}

// ListEnvs reads all env rows, oldest first.
func (s *Store) ListEnvs() ([]Env, error) {
	rows, err := s.db.Query(`SELECT ` + envCols + ` FROM envs ORDER BY created ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Env
	for rows.Next() {
		e, err := scanEnv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReservedCost sums admission cost over envs in acquiring|present for a
// pool: admission is a query, not a ledger, so it cannot leak.
func (s *Store) ReservedCost(pool envspec.Pool) (cpus float64, memMB int64, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(cost_cpus),0), COALESCE(SUM(cost_mem_mb),0)
		FROM envs WHERE cost_pool=? AND phase IN ('acquiring','present')`, string(pool)).Scan(&cpus, &memMB)
	return cpus, memMB, err
}

// --- provision / attach insert ---

// InsertProvision writes env(acquiring) + operation + lease(epoch 1) in
// one transaction, before any external dispatch. With a request key it
// is the idempotency point: an existing key returns the prior env/op
// (same digest) or ErrConflict (different digest).
func (s *Store) InsertProvision(e Env, op Operation, lease Lease, now time.Time) (Env, Operation, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return e, op, false, s.fail(err)
	}
	defer tx.Rollback()
	if op.RequestKey != "" {
		existing, err := scanOp(tx.QueryRow(opSelect+` WHERE owner_scope=? AND request_key=?`, op.OwnerScope, op.RequestKey))
		if err == nil {
			if existing.RequestDigest != op.RequestDigest {
				return e, op, false, fmt.Errorf("%w: request key %q reused with a different request", ErrConflict, op.RequestKey)
			}
			envRow, err := scanEnv(tx.QueryRow(`SELECT `+envCols+` FROM envs WHERE id=?`, existing.Env))
			if err != nil {
				return e, op, false, s.fail(err)
			}
			return envRow, existing, true, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return e, op, false, s.fail(err)
		}
	}
	specJSON, _ := json.Marshal(e.Spec)
	ownerJSON, _ := json.Marshal(e.Owner)
	if _, err := tx.Exec(`INSERT INTO envs(`+envCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.ID, string(e.Kind), e.Profile, specJSON, ownerJSON, e.Generation, e.Revision,
		string(e.Desired), string(e.Phase), string(e.Presence), e.Instance, e.Image, b2i(e.Warm),
		string(e.CostPool), e.CostCPUs, e.CostMemMB, e.CostDiskMB, ts(e.Created)); err != nil {
		return e, op, false, s.fail(err)
	}
	if err := insertOp(tx, op); err != nil {
		return e, op, false, s.fail(err)
	}
	if _, err := tx.Exec(`INSERT INTO leases(env,lease_id,epoch,holder,expires,state) VALUES(?,?,?,?,?,?)`,
		lease.Env, lease.LeaseID, lease.Epoch, lease.Holder, ts(lease.Expires), string(lease.State)); err != nil {
		return e, op, false, s.fail(err)
	}
	kind := "env.acquiring"
	if op.Kind == OpAttach {
		kind = "env.attaching"
	}
	if err := emitExec(tx, EventFor(kind, e, now, map[string]any{"profile": e.Profile, "op": op.ID, "generation": e.Generation, "warm": e.Warm})); err != nil {
		return e, op, false, s.fail(err)
	}
	if lease.State == LeaseHeld {
		if err := emitExec(tx, EventFor("lease.granted", e, now, map[string]any{"lease": lease.LeaseID, "epoch": lease.Epoch, "kind": "env"})); err != nil {
			return e, op, false, s.fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return e, op, false, s.fail(err)
	}
	return e, op, false, nil
}

const opSelect = `SELECT id,env,generation,kind,owner_scope,request_key,request_digest,phase,attempt,next_attempt,deadline,outcome,external_ids,disposition,error,created,updated FROM operations`

func scanOp(row interface{ Scan(...any) error }) (Operation, error) {
	var o Operation
	var kind, outcome string
	var next, deadline, created, updated int64
	err := row.Scan(&o.ID, &o.Env, &o.Generation, &kind, &o.OwnerScope, &o.RequestKey, &o.RequestDigest,
		&o.Phase, &o.Attempt, &next, &deadline, &outcome, &o.ExternalIDs, &o.Disposition, &o.Error, &created, &updated)
	if err != nil {
		return o, err
	}
	o.Kind = OpKind(kind)
	o.Outcome = Outcome(outcome)
	o.NextAttempt, o.Deadline, o.Created, o.Updated = unts(next), unts(deadline), unts(created), unts(updated)
	return o, nil
}

func insertOp(x execer, o Operation) error {
	_, err := x.Exec(`INSERT INTO operations(id,env,generation,kind,owner_scope,request_key,request_digest,phase,attempt,next_attempt,deadline,outcome,external_ids,disposition,error,created,updated)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.Env, o.Generation, string(o.Kind), o.OwnerScope, o.RequestKey, o.RequestDigest,
		o.Phase, o.Attempt, ts(o.NextAttempt), ts(o.Deadline), string(o.Outcome), o.ExternalIDs,
		o.Disposition, o.Error, ts(o.Created), ts(o.Updated))
	return err
}

// GetOperation reads one operation.
func (s *Store) GetOperation(id string) (Operation, error) {
	o, err := scanOp(s.db.QueryRow(opSelect+` WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return o, fmt.Errorf("%w: operation %s", ErrNotFound, id)
	}
	return o, err
}

// OperationByKey finds the operation for an idempotency key.
func (s *Store) OperationByKey(ownerScope, key string) (Operation, error) {
	o, err := scanOp(s.db.QueryRow(opSelect+` WHERE owner_scope=? AND request_key=?`, ownerScope, key))
	if errors.Is(err, sql.ErrNoRows) {
		return o, fmt.Errorf("%w: request key", ErrNotFound)
	}
	return o, err
}

// OpenOperation returns the env's pending/unknown operation of a kind.
func (s *Store) OpenOperation(envID string, kind OpKind) (Operation, error) {
	o, err := scanOp(s.db.QueryRow(opSelect+` WHERE env=? AND kind=? AND outcome IN ('pending','unknown') ORDER BY created DESC LIMIT 1`, envID, string(kind)))
	if errors.Is(err, sql.ErrNoRows) {
		return o, fmt.Errorf("%w: open %s operation", ErrNotFound, kind)
	}
	return o, err
}

// ListOperations reads an env's operations, oldest first.
func (s *Store) ListOperations(envID string) ([]Operation, error) {
	rows, err := s.db.Query(opSelect+` WHERE env=? ORDER BY created ASC, id ASC`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// PendingOperations lists every open operation (recovery work queue).
func (s *Store) PendingOperations() ([]Operation, error) {
	rows, err := s.db.Query(opSelect + ` WHERE outcome IN ('pending','unknown') ORDER BY created ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operation
	for rows.Next() {
		o, err := scanOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpdateOperation records attempt/outcome progress for an operation.
func (s *Store) UpdateOperation(o Operation, now time.Time) error {
	res, err := s.db.Exec(`UPDATE operations SET phase=?,attempt=?,next_attempt=?,deadline=?,outcome=?,external_ids=?,disposition=?,error=?,updated=? WHERE id=?`,
		o.Phase, o.Attempt, ts(o.NextAttempt), ts(o.Deadline), string(o.Outcome), o.ExternalIDs, o.Disposition, o.Error, ts(now), o.ID)
	return s.fail(requireOne(res, err))
}

// --- typed env transitions (all revision-conditional, event in same tx) ---

func (s *Store) transition(e Env, now time.Time, evKind string, data map[string]any, mutate func(*sql.Tx, Env) (Env, error)) (Env, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return e, s.fail(err)
	}
	defer tx.Rollback()
	next, err := mutate(tx, e)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			return e, err
		}
		return e, s.fail(err)
	}
	if evKind != "" {
		if err := emitExec(tx, EventFor(evKind, next, now, data)); err != nil {
			return e, s.fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return e, s.fail(err)
	}
	return next, nil
}

// RecordInstance stores the external identity the moment Create/Attach
// returns (tx2 of provision), so a crash after this point can re-open it.
func (s *Store) RecordInstance(e Env, instanceJSON, image string, presence Presence, now time.Time) (Env, error) {
	return s.transition(e, now, "env.instance", map[string]any{"image": image}, func(tx *sql.Tx, cur Env) (Env, error) {
		res, err := tx.Exec(`UPDATE envs SET instance=?,image=?,presence=?,revision=revision+1 WHERE id=? AND revision=?`,
			instanceJSON, image, string(presence), cur.ID, cur.Revision)
		if err := requireOne(res, err); err != nil {
			return cur, err
		}
		cur.Instance, cur.Image, cur.Presence, cur.Revision = instanceJSON, image, presence, cur.Revision+1
		return cur, nil
	})
}

// MarkPresent completes the readiness barriers: phase present, provision
// op ok, in one tx. Only valid while desired is still present.
func (s *Store) MarkPresent(e Env, opID string, now time.Time) (Env, error) {
	return s.transition(e, now, "env.ready", map[string]any{"profile": e.Profile, "image": e.Image, "generation": e.Generation}, func(tx *sql.Tx, cur Env) (Env, error) {
		res, err := tx.Exec(`UPDATE envs SET phase='present',presence='present',revision=revision+1 WHERE id=? AND revision=? AND desired='present' AND phase='acquiring'`,
			cur.ID, cur.Revision)
		if err := requireOne(res, err); err != nil {
			return cur, err
		}
		if _, err := tx.Exec(`UPDATE operations SET outcome='ok',phase='present',error='',updated=? WHERE id=?`, ts(now), opID); err != nil {
			return cur, err
		}
		cur.Phase, cur.Presence, cur.Revision = envspec.StatePresent, PresencePresent, cur.Revision+1
		return cur, nil
	})
}

// MarkFailed ends an env that never became present and holds nothing:
// op failed, lease released, phase failed.
func (s *Store) MarkFailed(e Env, opID, errText string, now time.Time) (Env, error) {
	return s.transition(e, now, "env.failed", map[string]any{"error": errText}, func(tx *sql.Tx, cur Env) (Env, error) {
		res, err := tx.Exec(`UPDATE envs SET phase='failed',revision=revision+1 WHERE id=? AND revision=?`, cur.ID, cur.Revision)
		if err := requireOne(res, err); err != nil {
			return cur, err
		}
		if opID != "" {
			if _, err := tx.Exec(`UPDATE operations SET outcome='failed',error=?,updated=? WHERE id=?`, errText, ts(now), opID); err != nil {
				return cur, err
			}
		}
		if _, err := tx.Exec(`UPDATE leases SET state='released' WHERE env=?`, cur.ID); err != nil {
			return cur, err
		}
		cur.Phase, cur.Revision = envspec.StateFailed, cur.Revision+1
		return cur, nil
	})
}

// BeginRelease flips desired to released and records the release op —
// that pair is the cleanup obligation. Releasing an already-releasing
// env returns its open op; terminal envs are a no-op (op.ID "").
func (s *Store) BeginRelease(e Env, op Operation, now time.Time) (Env, Operation, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return e, op, s.fail(err)
	}
	defer tx.Rollback()
	cur, err := scanEnv(tx.QueryRow(`SELECT `+envCols+` FROM envs WHERE id=?`, e.ID))
	if err != nil {
		return e, op, s.fail(err)
	}
	if cur.Phase.Terminal() {
		return cur, Operation{}, nil
	}
	if cur.Desired == DesiredReleased {
		existing, err := scanOp(tx.QueryRow(opSelect+` WHERE env=? AND kind='release' AND outcome IN ('pending','unknown') ORDER BY created DESC LIMIT 1`, e.ID))
		if err == nil {
			return cur, existing, nil
		}
	}
	res, err := tx.Exec(`UPDATE envs SET desired='released',phase='releasing',revision=revision+1 WHERE id=? AND revision=?`, cur.ID, cur.Revision)
	if err := requireOne(res, err); err != nil {
		return cur, op, err
	}
	op.Env, op.Generation = cur.ID, cur.Generation
	if err := insertOp(tx, op); err != nil {
		return cur, op, s.fail(err)
	}
	cur.Desired, cur.Phase, cur.Revision = DesiredReleased, envspec.StateReleasing, cur.Revision+1
	if err := emitExec(tx, EventFor("env.releasing", cur, now, map[string]any{"op": op.ID})); err != nil {
		return cur, op, s.fail(err)
	}
	if err := tx.Commit(); err != nil {
		return cur, op, s.fail(err)
	}
	return cur, op, nil
}

// CompleteRelease records verified cleanup: phase released, presence as
// observed, lease released, release op ok.
func (s *Store) CompleteRelease(e Env, opID, disposition string, presence Presence, now time.Time) (Env, error) {
	return s.transition(e, now, "env.released", map[string]any{"disposition": disposition}, func(tx *sql.Tx, cur Env) (Env, error) {
		res, err := tx.Exec(`UPDATE envs SET phase='released',presence=?,revision=revision+1 WHERE id=? AND revision=?`,
			string(presence), cur.ID, cur.Revision)
		if err := requireOne(res, err); err != nil {
			return cur, err
		}
		if _, err := tx.Exec(`UPDATE leases SET state='released' WHERE env=?`, cur.ID); err != nil {
			return cur, err
		}
		if _, err := tx.Exec(`UPDATE operations SET outcome='ok',phase='released',disposition=?,error='',updated=? WHERE id=?`,
			disposition, ts(now), opID); err != nil {
			return cur, err
		}
		if err := emitExec(tx, EventFor("lease.released", cur, now, map[string]any{"kind": "env", "disposition": disposition})); err != nil {
			return cur, err
		}
		cur.Phase, cur.Presence, cur.Revision = envspec.StateReleased, presence, cur.Revision+1
		return cur, nil
	})
}

// ReturnToPool resets a released env back to a warm, unowned present env
// under a new generation; the old lease is released and its epoch is
// retired, so prior handles can never validate again.
func (s *Store) ReturnToPool(e Env, opID string, warmOwner envspec.Owner, now time.Time) (Env, error) {
	return s.transition(e, now, "env.ready", map[string]any{"warm": true, "reset": true}, func(tx *sql.Tx, cur Env) (Env, error) {
		ownerJSON, _ := json.Marshal(warmOwner)
		res, err := tx.Exec(`UPDATE envs SET desired='present',phase='present',presence='present',warm=1,owner=?,generation=generation+1,revision=revision+1 WHERE id=? AND revision=?`,
			ownerJSON, cur.ID, cur.Revision)
		if err := requireOne(res, err); err != nil {
			return cur, err
		}
		if _, err := tx.Exec(`UPDATE leases SET lease_id='',holder=?,expires=0,state='released',epoch=epoch+1 WHERE env=?`, warmOwner.ID, cur.ID); err != nil {
			return cur, err
		}
		if _, err := tx.Exec(`UPDATE operations SET outcome='ok',phase='present',disposition='reset_to_pool',error='',updated=? WHERE id=?`, ts(now), opID); err != nil {
			return cur, err
		}
		if err := emitExec(tx, EventFor("lease.released", cur, now, map[string]any{"kind": "env", "returned_to_warm_pool": true})); err != nil {
			return cur, err
		}
		cur.Desired, cur.Phase, cur.Presence, cur.Warm = DesiredPresent, envspec.StatePresent, PresencePresent, true
		cur.Owner, cur.Generation, cur.Revision = warmOwner, cur.Generation+1, cur.Revision+1
		return cur, nil
	})
}

// SetPresence records an observation outcome (no event unless asked).
func (s *Store) SetPresence(e Env, p Presence, now time.Time) (Env, error) {
	return s.transition(e, now, "", nil, func(tx *sql.Tx, cur Env) (Env, error) {
		res, err := tx.Exec(`UPDATE envs SET presence=?,revision=revision+1 WHERE id=? AND revision=?`, string(p), cur.ID, cur.Revision)
		if err := requireOne(res, err); err != nil {
			return cur, err
		}
		cur.Presence, cur.Revision = p, cur.Revision+1
		return cur, nil
	})
}

// ClaimWarm atomically claims one warm present env of a profile: the
// conditional update bumps generation+revision, installs the new owner
// and a fresh lease at epoch+1, and records the idempotency op. Exactly
// one concurrent caller wins; losers get found=false.
func (s *Store) ClaimWarm(profile string, owner envspec.Owner, leaseID string, expires time.Time, op Operation, now time.Time) (Env, Lease, bool, error) {
	rows, err := s.ListEnvs()
	if err != nil {
		return Env{}, Lease{}, false, err
	}
	for _, cand := range rows {
		if !cand.Warm || cand.Phase != envspec.StatePresent || cand.Desired != DesiredPresent || cand.Profile != profile {
			continue
		}
		envRow, lease, won, err := s.claimOne(cand, owner, leaseID, expires, op, now)
		if err != nil {
			return Env{}, Lease{}, false, err
		}
		if won {
			return envRow, lease, true, nil
		}
	}
	return Env{}, Lease{}, false, nil
}

func (s *Store) claimOne(cand Env, owner envspec.Owner, leaseID string, expires time.Time, op Operation, now time.Time) (Env, Lease, bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return cand, Lease{}, false, s.fail(err)
	}
	defer tx.Rollback()
	ownerJSON, _ := json.Marshal(owner)
	res, err := tx.Exec(`UPDATE envs SET warm=0,owner=?,generation=generation+1,revision=revision+1
		WHERE id=? AND revision=? AND warm=1 AND phase='present' AND desired='present'`, ownerJSON, cand.ID, cand.Revision)
	if err != nil {
		return cand, Lease{}, false, s.fail(err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return cand, Lease{}, false, nil // another caller won this one
	}
	var epoch int64
	if err := tx.QueryRow(`SELECT epoch FROM leases WHERE env=?`, cand.ID).Scan(&epoch); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return cand, Lease{}, false, s.fail(err)
	}
	lease := Lease{Env: cand.ID, LeaseID: leaseID, Epoch: epoch + 1, Holder: owner.ID, Expires: expires, State: LeaseHeld}
	if _, err := tx.Exec(`INSERT INTO leases(env,lease_id,epoch,holder,expires,state) VALUES(?,?,?,?,?,?)
		ON CONFLICT(env) DO UPDATE SET lease_id=excluded.lease_id,epoch=excluded.epoch,holder=excluded.holder,expires=excluded.expires,state=excluded.state`,
		lease.Env, lease.LeaseID, lease.Epoch, lease.Holder, ts(lease.Expires), string(lease.State)); err != nil {
		return cand, Lease{}, false, s.fail(err)
	}
	cand.Warm, cand.Owner, cand.Generation, cand.Revision = false, owner, cand.Generation+1, cand.Revision+1
	op.Env, op.Generation, op.Outcome, op.Phase = cand.ID, cand.Generation, OutcomeOK, "present"
	if err := insertOp(tx, op); err != nil {
		return cand, Lease{}, false, s.fail(err)
	}
	if err := emitExec(tx, EventFor("lease.granted", cand, now, map[string]any{"lease": leaseID, "epoch": lease.Epoch, "kind": "env", "warm_claim": true})); err != nil {
		return cand, Lease{}, false, s.fail(err)
	}
	if err := tx.Commit(); err != nil {
		return cand, Lease{}, false, s.fail(err)
	}
	return cand, lease, true, nil
}

// --- leases & borrows ---

const leaseCols = `env,lease_id,epoch,holder,expires,state`

func scanLease(row interface{ Scan(...any) error }) (Lease, error) {
	var l Lease
	var exp int64
	var state string
	err := row.Scan(&l.Env, &l.LeaseID, &l.Epoch, &l.Holder, &exp, &state)
	l.Expires, l.State = unts(exp), LeaseState(state)
	return l, err
}

// GetLease reads an env's holder lease.
func (s *Store) GetLease(envID string) (Lease, error) {
	l, err := scanLease(s.db.QueryRow(`SELECT `+leaseCols+` FROM leases WHERE env=?`, envID))
	if errors.Is(err, sql.ErrNoRows) {
		return l, fmt.Errorf("%w: lease for %s", ErrNotFound, envID)
	}
	return l, err
}

// RenewLease extends a held lease only if the caller's lease ID and
// epoch still match — the sole holder-renewal path.
func (s *Store) RenewLease(envID, leaseID string, epoch int64, expires time.Time, now time.Time) (Lease, error) {
	res, err := s.db.Exec(`UPDATE leases SET expires=? WHERE env=? AND lease_id=? AND epoch=? AND state='held'`,
		ts(expires), envID, leaseID, epoch)
	if err := requireOne(res, err); err != nil {
		return Lease{}, s.failOnWrite(err)
	}
	return s.GetLease(envID)
}

func (s *Store) failOnWrite(err error) error {
	if errors.Is(err, ErrConflict) {
		return err
	}
	return s.fail(err)
}

// ExpireLease marks a held lease expired (reaper).
func (s *Store) ExpireLease(envID string, e Env, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return s.fail(err)
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE leases SET state='expired' WHERE env=? AND state='held'`, envID)
	if err := requireOne(res, err); err != nil {
		return err
	}
	if err := emitExec(tx, EventFor("lease.expired", e, now, map[string]any{"kind": "env"})); err != nil {
		return s.fail(err)
	}
	return s.fail(tx.Commit())
}

// TransferLease hands an env to a new owner under a fresh lease ID and
// epoch+1 (old handles die). Blocked by the caller when borrows exist.
func (s *Store) TransferLease(e Env, to envspec.Owner, leaseID string, expires time.Time, now time.Time) (Env, Lease, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return e, Lease{}, s.fail(err)
	}
	defer tx.Rollback()
	ownerJSON, _ := json.Marshal(to)
	res, err := tx.Exec(`UPDATE envs SET owner=?,revision=revision+1 WHERE id=? AND revision=? AND warm=0`, ownerJSON, e.ID, e.Revision)
	if err := requireOne(res, err); err != nil {
		return e, Lease{}, err
	}
	var epoch int64
	_ = tx.QueryRow(`SELECT epoch FROM leases WHERE env=?`, e.ID).Scan(&epoch)
	lease := Lease{Env: e.ID, LeaseID: leaseID, Epoch: epoch + 1, Holder: to.ID, Expires: expires, State: LeaseHeld}
	if _, err := tx.Exec(`INSERT INTO leases(env,lease_id,epoch,holder,expires,state) VALUES(?,?,?,?,?,?)
		ON CONFLICT(env) DO UPDATE SET lease_id=excluded.lease_id,epoch=excluded.epoch,holder=excluded.holder,expires=excluded.expires,state=excluded.state`,
		lease.Env, lease.LeaseID, lease.Epoch, lease.Holder, ts(lease.Expires), string(lease.State)); err != nil {
		return e, Lease{}, s.fail(err)
	}
	e.Owner, e.Revision = to, e.Revision+1
	if err := emitExec(tx, EventFor("lease.granted", e, now, map[string]any{"lease": leaseID, "epoch": lease.Epoch, "kind": "env", "transfer": true})); err != nil {
		return e, Lease{}, s.fail(err)
	}
	if err := tx.Commit(); err != nil {
		return e, Lease{}, s.fail(err)
	}
	return e, lease, nil
}

func (s *Store) SetBorrow(envID, job string, exp time.Time) error {
	_, err := s.db.Exec(`INSERT INTO borrows(env,job,expires) VALUES(?,?,?)
		ON CONFLICT(env,job) DO UPDATE SET expires=excluded.expires`, envID, job, ts(exp))
	return s.fail(err)
}

func (s *Store) DropBorrow(envID, job string) error {
	_, err := s.db.Exec(`DELETE FROM borrows WHERE env=? AND job=?`, envID, job)
	return s.fail(err)
}

func (s *Store) Borrows(envID string) ([]Borrow, error) {
	rows, err := s.db.Query(`SELECT job,expires FROM borrows WHERE env=? ORDER BY job`, envID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Borrow
	for rows.Next() {
		var b Borrow
		var exp int64
		if err := rows.Scan(&b.Job, &exp); err != nil {
			return nil, err
		}
		b.Env, b.Expires = envID, unts(exp)
		out = append(out, b)
	}
	return out, rows.Err()
}

// --- conditions ---

// SetCondition upserts one observed condition for a generation.
func (s *Store) SetCondition(envID string, generation int64, c envspec.Condition) error {
	_, err := s.db.Exec(`INSERT INTO conditions(env,generation,name,status,reason,observed_at,detail) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(env,generation,name) DO UPDATE SET status=excluded.status,reason=excluded.reason,observed_at=excluded.observed_at,detail=excluded.detail`,
		envID, generation, c.Name, string(c.Status), c.Reason, ts(c.ObservedAt), c.Detail)
	return s.fail(err)
}

// Conditions reads a generation's conditions.
func (s *Store) Conditions(envID string, generation int64) ([]envspec.Condition, error) {
	rows, err := s.db.Query(`SELECT name,status,reason,observed_at,detail FROM conditions WHERE env=? AND generation=? ORDER BY name`, envID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envspec.Condition
	for rows.Next() {
		var c envspec.Condition
		var status string
		var at int64
		if err := rows.Scan(&c.Name, &status, &c.Reason, &at, &c.Detail); err != nil {
			return nil, err
		}
		c.Status, c.ObservedAt = envspec.ConditionStatus(status), unts(at)
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- activities ---

const actCols = `env,generation,name,kind,desired,required,state,attempts,next_attempt,last_ok,last_error,params`

func scanActivity(row interface{ Scan(...any) error }) (Activity, error) {
	var a Activity
	var kind, state string
	var required int
	var next, lastOK int64
	err := row.Scan(&a.Env, &a.Generation, &a.Name, &kind, &a.Desired, &required, &state, &a.Attempts, &next, &lastOK, &a.LastError, &a.Params)
	a.Kind, a.State, a.Required = ActivityKind(kind), ActivityState(state), required != 0
	a.NextAttempt, a.LastOK = unts(next), unts(lastOK)
	return a, err
}

// UpsertActivity declares (or re-declares) a desired activity.
func (s *Store) UpsertActivity(a Activity) error {
	_, err := s.db.Exec(`INSERT INTO activities(`+actCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(env,generation,name) DO UPDATE SET kind=excluded.kind,desired=excluded.desired,required=excluded.required,params=excluded.params`,
		a.Env, a.Generation, a.Name, string(a.Kind), a.Desired, b2i(a.Required), string(a.State),
		a.Attempts, ts(a.NextAttempt), ts(a.LastOK), a.LastError, a.Params)
	return s.fail(err)
}

// UpdateActivity records reconciler progress for one activity.
func (s *Store) UpdateActivity(a Activity) error {
	res, err := s.db.Exec(`UPDATE activities SET desired=?,state=?,attempts=?,next_attempt=?,last_ok=?,last_error=?,params=? WHERE env=? AND generation=? AND name=?`,
		a.Desired, string(a.State), a.Attempts, ts(a.NextAttempt), ts(a.LastOK), a.LastError, a.Params, a.Env, a.Generation, a.Name)
	return s.failOnWrite(requireOne(res, err))
}

// Activities lists a generation's activities.
func (s *Store) Activities(envID string, generation int64) ([]Activity, error) {
	rows, err := s.db.Query(`SELECT `+actCols+` FROM activities WHERE env=? AND generation=? ORDER BY name`, envID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Activity
	for rows.Next() {
		a, err := scanActivity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetActivity reads one activity.
func (s *Store) GetActivity(envID string, generation int64, name string) (Activity, error) {
	a, err := scanActivity(s.db.QueryRow(`SELECT `+actCols+` FROM activities WHERE env=? AND generation=? AND name=?`, envID, generation, name))
	if errors.Is(err, sql.ErrNoRows) {
		return a, fmt.Errorf("%w: activity %s", ErrNotFound, name)
	}
	return a, err
}

// --- events & scopes ---

// EventsSince pages lifecycle events after seq.
func (s *Store) EventsSince(ctx context.Context, since uint64, limit int) ([]envspec.LifecycleEvent, error) {
	return s.queryEvents(ctx, `SELECT seq,kind,env,job,session,owner,ts,data FROM events WHERE seq>? ORDER BY seq ASC LIMIT ?`, since, limit)
}

// RecentEvents reads the newest limit events, newest first.
func (s *Store) RecentEvents(ctx context.Context, limit int) ([]envspec.LifecycleEvent, error) {
	return s.queryEvents(ctx, `SELECT seq,kind,env,job,session,owner,ts,data FROM events ORDER BY seq DESC LIMIT ?`, limit)
}

func (s *Store) queryEvents(ctx context.Context, q string, args ...any) ([]envspec.LifecycleEvent, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envspec.LifecycleEvent
	for rows.Next() {
		var ev envspec.LifecycleEvent
		var t int64
		var data string
		if err := rows.Scan(&ev.Seq, &ev.Kind, &ev.Env, &ev.Job, &ev.Session, &ev.Owner, &t, &data); err != nil {
			return nil, err
		}
		ev.Time = unts(t)
		_ = json.Unmarshal([]byte(data), &ev.Data)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// PruneEvents deletes events older than retain. Consumers keep their
// own cursors; there is no cursor table to couple retention to.
func (s *Store) PruneEvents(retain time.Duration, now time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM events WHERE ts<?`, ts(now.Add(-retain)))
	if err != nil {
		return 0, s.fail(err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (s *Store) CloseScope(kind, id string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO closed_scopes(kind,id) VALUES(?,?)`, kind, id)
	return s.fail(err)
}

func (s *Store) ScopeClosed(kind, id string) bool {
	if id == "" {
		return false
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM closed_scopes WHERE kind=? AND id=?`, kind, id).Scan(&n)
	return n > 0
}
