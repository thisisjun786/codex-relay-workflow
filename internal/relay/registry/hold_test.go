package registry

import (
	"testing"
)

// test_registration_hold.py and test_registration_contention.py, the relay half: a marker
// registration decides its dispatch's generation under the store's write lock.

func registeredForHold(t *testing.T) (*Registry, string) {
	t.Helper()
	r := newRegistry(t)
	x, err := r.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	return r, x.ID
}

// RCT-3: after an advance, dispatch_generation_state says stale, not absent; an unknown
// dispatch is absent; an unopenable store is (absent, unreadable) and is not created.
func Test25_RCT3_a_dispatch_after_an_advance_reads_stale_not_absent(t *testing.T) {
	r, rid := registeredForHold(t)
	if state, readable := DispatchGenerationState(ctx(), r.Store.Path, rid, "dispatch-1"); state != DispatchCurrent || !readable {
		t.Fatal(state)
	}
	if _, err := r.OpenGeneration(ctx(), rid, "revision-after", "needs_changes_revision", ns("turn-after")); err != nil {
		t.Fatal(err)
	}
	if state, readable := DispatchGenerationState(ctx(), r.Store.Path, rid, "dispatch-1"); state != DispatchStale || !readable {
		t.Fatal(state, readable)
	}
	if state, _ := DispatchGenerationState(ctx(), r.Store.Path, rid, "never"); state != DispatchAbsent {
		t.Fatal(state)
	}
	missing := t.TempDir() + "/absent/relay.sqlite3"
	if state, readable := DispatchGenerationState(ctx(), missing, rid, "dispatch-1"); state != DispatchAbsent || readable {
		t.Fatal(state, readable)
	}
}
