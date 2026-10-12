package report

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var ctx = context.Background()

var fixedNow = func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }

// newStore opens a relay store in a temporary directory.
func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// seedRun writes the run record of a run (its started row) through the run's own journal and returns the journal.
func seedRun(t *testing.T, st *store.Store, run, plan, actor string) *journal.Journal {
	t.Helper()
	j, err := journal.Open(ctx, st, run, journal.Identity{PlanID: plan, PlanRevision: 1, ActorTaskID: actor, EpochNonce: "nonce"}, journal.Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	return j
}

// seedRunOpen opens the journal of an existing run again (a resume).
func seedRunOpen(t *testing.T, st *store.Store, run string) *journal.Journal {
	t.Helper()
	j, err := journal.Open(ctx, st, run, journal.Identity{}, journal.Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
