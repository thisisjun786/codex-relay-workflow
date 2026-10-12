package manage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// flowTestStore makes a relay store in a temporary state directory and returns the directory (what cfg.Relay.State names) and the open
// store. The flow sources read it read-only through relayReadOpenStore.
func flowTestStore(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, relayReadStoreFile), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return dir, st
}

// flowTestRun writes the run record of a run (its started row, and a finished row when finished) through the run's own journal, with
// every row stamped at.
func flowTestRun(t *testing.T, st *store.Store, run, actor string, at time.Time, finished bool) {
	t.Helper()
	j, err := journal.Open(context.Background(), st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: actor, EpochNonce: "n"}, journal.Options{Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
	if finished {
		j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": "completed"}})
	}
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
}

// flowTestRow writes a flow_events row of a command into a run, stamped at.
func flowTestRow(t *testing.T, st *store.Store, run, kind string, payload map[string]any, at time.Time) {
	t.Helper()
	app := report.ForeignAppender{Store: st, Run: run, Now: func() time.Time { return at }}
	if _, err := app.Append(context.Background(), report.NewRow{Kind: kind, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

// flowTestCRW is a fake crw executable that records its arguments, and the file it records them in.
func flowTestCRW(t *testing.T) (exe, record string) {
	t.Helper()
	return coreFakeCRW(t, "", 0)
}

// flowTestCalls reads the argument lists the fake crw recorded.
func flowTestCalls(t *testing.T, record string) [][]string {
	t.Helper()
	if _, err := os.Stat(record); err != nil {
		return nil
	}
	return coreFakeCalls(t, record)
}
