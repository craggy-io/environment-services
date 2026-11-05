package core

import (
	"context"
	"testing"

	"github.com/craggy-io/environment-services/envspec"
	"github.com/craggy-io/environment-services/internal/adapter"
	"github.com/craggy-io/environment-services/internal/store"
)

// discoverOnlyAdapter returns a fixed candidate list; only Discover is
// exercised by the non-adoption test below.
type discoverOnlyAdapter struct{ cands []adapter.Candidate }

func (a discoverOnlyAdapter) Describe() adapter.Description {
	return adapter.Description{Name: "discover-only", Relationship: adapter.Created}
}
func (a discoverOnlyAdapter) Validate(context.Context, adapter.OpContext, envspec.Spec) error {
	return nil
}
func (a discoverOnlyAdapter) Observe(context.Context, adapter.OpContext, adapter.Ref) (adapter.Observation, error) {
	return adapter.Observation{Presence: adapter.Unknown}, nil
}
func (a discoverOnlyAdapter) Discover(context.Context, adapter.Scope) ([]adapter.Candidate, error) {
	return a.cands, nil
}

// Foreign resources (other tooling's, other managers', leftovers) are
// never adopted: even a candidate carrying this env's own
// deterministic name is skipped when marked Foreign, so recovery
// cannot claim a resource this manager did not create.
func TestForeignCandidateNeverAdopted(t *testing.T) {
	m := &Manager{cfg: Config{ManagerID: "mgr_test"}}
	e := store.Env{ID: "env_foreign1"}
	foreign := discoverOnlyAdapter{cands: []adapter.Candidate{
		{Ref: adapter.Ref{EnvID: e.ID, Name: adapter.NameFor(e.ID), External: "ext-1"}, Foreign: true},
	}}
	if _, ok := m.discoverFor(context.Background(), foreign, e); ok {
		t.Fatal("discoverFor adopted a Foreign candidate")
	}
	// The same candidate without the Foreign mark is rediscovered.
	ours := discoverOnlyAdapter{cands: []adapter.Candidate{
		{Ref: adapter.Ref{Name: adapter.NameFor(e.ID), External: "ext-1"}},
	}}
	c, ok := m.discoverFor(context.Background(), ours, e)
	if !ok || c.Ref.EnvID != e.ID || c.Ref.External != "ext-1" {
		t.Fatalf("discoverFor ours = %+v, %v; want env-stamped candidate", c, ok)
	}
}
