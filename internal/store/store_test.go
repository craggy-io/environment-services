package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/craggy-io/environment-services/envspec"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func provisionFixture(id string) (Env, Operation, Lease) {
	now := time.Unix(1000, 0)
	e := Env{ID: id, Kind: envspec.Container, Profile: "p", Owner: envspec.Owner{ID: "agent:a"},
		Generation: 1, Revision: 1, Desired: DesiredPresent, Phase: envspec.StateAcquiring,
		Presence: PresenceUnknown, CostPool: envspec.PoolDocker, CostMemMB: 512, Created: now}
	op := Operation{ID: "op_" + id, Env: id, Generation: 1, Kind: OpProvision,
		OwnerScope: "agent:a", RequestKey: "k1", RequestDigest: "d1",
		Outcome: OutcomePending, Created: now, Updated: now}
	lease := Lease{Env: id, LeaseID: "lease_" + id, Epoch: 1, Holder: "agent:a", State: LeaseHeld}
	return e, op, lease
}

func TestInsertProvisionIdempotentAndAtomic(t *testing.T) {
	st := testStore(t)
	now := time.Unix(1000, 0)
	e, op, lease := provisionFixture("env_a")
	if _, _, existed, err := st.InsertProvision(e, op, lease, now); err != nil || existed {
		t.Fatalf("insert: existed=%v err=%v", existed, err)
	}
	// Same key+digest: returns the prior env/op (the join point).
	e2, op2, lease2 := provisionFixture("env_b")
	gotE, gotOp, existed, err := st.InsertProvision(e2, op2, lease2, now)
	if err != nil || !existed || gotE.ID != "env_a" || gotOp.ID != op.ID {
		t.Fatalf("join: env=%s op=%s existed=%v err=%v", gotE.ID, gotOp.ID, existed, err)
	}
	// Same key, different digest: conflict.
	op2.RequestDigest = "other"
	if _, _, _, err := st.InsertProvision(e2, op2, lease2, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("digest conflict: err=%v", err)
	}
	// Event landed in the same tx.
	evs, err := st.EventsSince(context.Background(), 0, 10)
	if err != nil || len(evs) < 2 || evs[0].Kind != "env.acquiring" {
		t.Fatalf("events: %v %v", evs, err)
	}
	// Admission SUM counts acquiring envs.
	if _, mem, err := st.ReservedCost(envspec.PoolDocker); err != nil || mem != 512 {
		t.Fatalf("reserved: mem=%d err=%v", mem, err)
	}
}

func TestRevisionConditionalTransitions(t *testing.T) {
	st := testStore(t)
	now := time.Unix(1000, 0)
	e, op, lease := provisionFixture("env_a")
	e, _, _, _ = st.InsertProvision(e, op, lease, now)
	stale := e
	e, err := st.RecordInstance(e, `{"env":"env_a"}`, "img", PresencePresent, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordInstance(stale, `{}`, "", PresencePresent, now); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale revision should conflict, got %v", err)
	}
	if !st.Healthy() {
		t.Fatal("conflict must not flip store unhealthy (it is a lost race, not a failed write)") // may be false if impl marks; check below
	}
}

func TestClaimWarmOneWinner(t *testing.T) {
	st := testStore(t)
	now := time.Unix(1000, 0)
	e, op, lease := provisionFixture("env_w")
	lease.LeaseID, lease.State = "", LeaseReleased
	e.Warm = true
	e, _, _, _ = st.InsertProvision(e, op, lease, now)
	e, _ = st.RecordInstance(e, `{}`, "", PresencePresent, now)
	if _, err := st.MarkPresent(e, op.ID, now); err != nil {
		t.Fatal(err)
	}
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			claimOp := Operation{ID: envspec.NewID("op_"), Kind: OpProvision, Created: now, Updated: now}
			_, _, won, err := st.ClaimWarm("p", envspec.Owner{ID: "agent:x"}, envspec.NewID("lease_"), time.Time{}, claimOp, now)
			if err != nil {
				t.Error(err)
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("warm claim winners = %d, want 1", wins)
	}
	l, err := st.GetLease("env_w")
	if err != nil || l.State != LeaseHeld || l.Epoch != 2 {
		t.Fatalf("lease after claim: %+v %v", l, err)
	}
}

func TestRenewLeaseFencedByEpoch(t *testing.T) {
	st := testStore(t)
	now := time.Unix(1000, 0)
	e, op, lease := provisionFixture("env_a")
	_, _, _, _ = st.InsertProvision(e, op, lease, now)
	if _, err := st.RenewLease("env_a", lease.LeaseID, 99, now.Add(time.Hour), now); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong epoch should conflict, got %v", err)
	}
	if _, err := st.RenewLease("env_a", lease.LeaseID, 1, now.Add(time.Hour), now); err != nil {
		t.Fatalf("right epoch: %v", err)
	}
}
