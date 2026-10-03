package faults

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A sweeper that is given a store selection but no managed observer cannot read the settled managed turns it
// is meant to read. It used to fall back to a copy of the omission reader; it now refuses, so that a daemon
// built without the observer fails its sweep loudly (the tick notes "fault sweep failed") and does not go on
// without those readings. A sweeper with no selection reads no managed turns, as before.
func TestSweepWithASelectionNeedsAManagedObserver(t *testing.T) {
	ctx, gd := f1ReplayStores(t)
	s := fcOpen(t, ctx, gd)
	clock := &testClock{now: 100000}
	sweeper := func(selection any, observer ManagedReadingObserver) *Sweeper {
		return &Sweeper{Store: s, HostRecordPath: testHostRecordPath(), Now: clock.ISO, MaxAttempts: 6, Installation: Installation{Package: "codex-session-relay", Version: "test", Location: "test"}, Selection: selection, ManagedObserver: observer}
	}
	_, err := sweeper(store.StateSelection{Path: gd}, nil).Sweep(ctx, "crw")
	if err == nil || !strings.HasPrefix(err.Error(), "fault_sweep_misconfigured:") {
		t.Fatalf("a selection without an observer sweeps with %v, want fault_sweep_misconfigured", err)
	}
	for name, sw := range map[string]*Sweeper{"no selection": sweeper(nil, nil), "selection and observer": sweeper(store.StateSelection{Path: gd}, realObserver(t))} {
		if _, err = sw.Sweep(context.Background(), "crw"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}
