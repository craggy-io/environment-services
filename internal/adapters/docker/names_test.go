package docker

import (
	"strings"
	"testing"
)

// Environment ids are time-ordered UUIDv7s whose first 12 characters are
// only a 16 ms timestamp. Docker names built from that prefix collided
// for environments provisioned in the same instant ("container name
// already in use"), so the name must also carry the random tail.
func TestShortIDTellsApartEnvsCreatedTogether(t *testing.T) {
	a := "env_01a18cca-55a3-7000-a5cf-71f7040a672c"
	b := "env_01a18cca-55a9-7000-8d1e-4be20c91f3a0" // same 16 ms window as a
	if sa, sb := shortID(a), shortID(b); sa == sb {
		t.Fatalf("envs created in the same instant share the name %q", sa)
	}
	if !strings.HasPrefix(a, "env_"+shortID(a)[:8]) {
		t.Errorf("name %q does not start with the id's creation time", shortID(a))
	}
	// Names stay short enough to read and are deterministic.
	if got := shortID(a); len(got) > 17 || got != shortID(a) {
		t.Errorf("shortID = %q", got)
	}
	// Ids that are already short (tests, hand-made ids) are kept whole.
	if got := shortID("env_abc123"); got != "abc123" {
		t.Errorf("short id = %q, want it unchanged", got)
	}
}
