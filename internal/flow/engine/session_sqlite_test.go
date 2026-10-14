package engine_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1367: the session crash tests of session_test.go run over the SQLite journal too. That journal commits an intent row
// (prepared, step_published) in a transaction of its own and batches the others, so a process that dies loses the rows of its
// open batch - the thread_started and turn_started a bind writes together among them - which the memory journal never does. The
// resumed process opens the journal again over what the store holds.
func init() {
	engine.SQLiteDeathJournal = func(t *testing.T, runID string) (engine.Journal, func() engine.Journal) {
		t.Helper()
		ctx := context.Background()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		open := func() engine.Journal {
			t.Helper()
			j, err := journal.Open(ctx, st, runID, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "task-owner"}, journal.Options{})
			if err != nil {
				t.Fatal(err)
			}
			return j
		}
		return open(), open
	}
}
