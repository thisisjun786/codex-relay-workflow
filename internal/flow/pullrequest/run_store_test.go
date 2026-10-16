package pullrequest

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// stateWith makes a relay state directory whose store holds run runID with the rows of spec, and answers the directory.
func stateWith(t *testing.T, runID string, spec runSpec) string {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	j, err := journal.Open(context.Background(), st, runID, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "task-orchestrator", EpochNonce: "nonce-1"}, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range spec.events(t) {
		e.RunID = runID
		e.CallID, e.Path = "", ""
		if e.Kind == engine.KindCall {
			e.CallID, e.Path = "call-"+e.Op, "/"+e.Op
		}
		j.Append(e)
	}
	if err := j.Err(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestOpenFromARunReadsTheJournalOfTheNamedState (CRW-1451, R1-03): the run is read from the relay store `--state` names, read-only,
// and the same run id in another state is another run; a run the store does not hold is ErrNoRun.
func TestOpenFromARunReadsTheJournalOfTheNamedState(t *testing.T) {
	tip := strings.Repeat("2", 40)
	closed := newRunSpec(tip, "o:r:10")
	closed.Crosscheck = []engine.Event{crosscheckRow("M1", tip, "verdict", "verified")}
	failed := closed
	failed.Status, failed.Outcome = "failed", nil
	a := stateWith(t, "run-r1", closed)
	b := stateWith(t, "run-r1", failed)
	ctx := context.Background()

	for _, c := range []struct {
		name  string
		state string
		want  bool
	}{{"state A holds the closed run", a, true}, {"state B holds the failed run", b, false}} {
		t.Run(c.name, func(t *testing.T) {
			events, err := readJournal(ctx, Source{State: c.state, Socket: filepath.Join(t.TempDir(), "relay.sock")}, "run-r1")
			if err != nil {
				t.Fatal(err)
			}
			f, err := FactsOf(events)
			if err != nil {
				t.Fatal(err)
			}
			if f.Closed != c.want {
				t.Errorf("closed=%v (%s), want %v", f.Closed, f.Why, c.want)
			}
			if c.want && (f.Body.Tip != tip || len(f.Nodes) != 1) {
				t.Errorf("facts %+v nodes %+v", f.Body, f.Nodes)
			}
			if _, ok := crosscheckForTest(events); c.want && !ok {
				t.Errorf("the crosscheck row is not read back")
			}
		})
	}

	if _, err := readJournal(ctx, Source{State: a, Socket: "/nonexistent/relay.sock"}, "run-other"); !errors.Is(err, ErrNoRun) {
		t.Errorf("a run absent from the state: err = %v, want ErrNoRun", err)
	}
	if _, err := readJournal(ctx, Source{State: filepath.Join(t.TempDir(), "empty"), Socket: "/nonexistent/relay.sock"}, "run-r1"); err == nil || errors.Is(err, ErrNoRun) || !strings.Contains(err.Error(), "no relay store") {
		t.Errorf("a state with no store: err = %v, want the missing-store error", err)
	}

	t.Run("RunWith hands --state and --socket to the journal seam", func(t *testing.T) {
		w := newRunWorld(t)
		w.unknown = true
		code, _, errOut := w.open("--state", "/x/state", "--socket", "/x/relay.sock")
		if code != 2 || !strings.Contains(errOut, "run_unknown") {
			t.Errorf("exit %d %q", code, errOut)
		}
		if len(w.sources) != 1 || w.sources[0] != (Source{State: "/x/state", Socket: "/x/relay.sock"}) {
			t.Errorf("sources %+v", w.sources)
		}
	})
}

// crosscheckForTest reads the verified crosscheck of milestone M1 at the tip of the events' close row.
func crosscheckForTest(events []engine.Event) (Crosscheck, bool) {
	f, err := FactsOf(events)
	if err != nil {
		return Crosscheck{}, false
	}
	c, reason := crosscheckOf(events, f.Body.Milestone, f.Body.Tip)
	return c, reason == ""
}
