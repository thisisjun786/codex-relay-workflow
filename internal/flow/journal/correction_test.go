package journal

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1269 d5: a journal reopened for a resume keeps the run's plan identity and the execution of the node a call belongs to:
// the rows of a call the resumed process reaches for the first time carry the plan_id and the execution_id the relay reads
// them by (dag-restart finds a call of an execution by these columns), as the earlier calls of the node do.
func TestResumedJournalKeepsPlanAndExecutionOfTheNode(t *testing.T) {
	st, path := newStore(t)
	j := newJournal(t, st)
	j.Append(engine.Event{RunID: runID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "t", "script_digest": "d", "args_digest": "a"}})
	row := func(j *Journal, call, event string, data map[string]any) {
		j.Append(engine.Event{RunID: runID, Kind: engine.KindCall, CallID: call, Path: "root", Position: 1, Op: "agent", Label: call, Event: event, Data: data})
	}
	row(j, "c_old", engine.EvPrepared, map[string]any{"role": "executor", "family": "executor", "node": "n1", "execution_id": "exe-1"})
	row(j, "c_old", engine.EvCompleted, map[string]any{"result": `"ok"`})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	// what the shipped resume does: it opens the journal with no identity of its own
	again := openStore(t, path)
	j2, err := Open(ctx, again, runID, Identity{}, Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	row(j2, "c_new", engine.EvPrepared, map[string]any{"role": "executor", "family": "executor", "node": "n1"})
	row(j2, "c_new", engine.EvThreadRequested, map[string]any{"cwd": "/w", "send": 1})
	row(j2, "c_other", engine.EvPrepared, map[string]any{"role": "executor", "family": "executor", "node": "n2"})
	if err := j2.Close(); err != nil {
		t.Fatal(err)
	}
	columns := func(call string) (events []string, plans, executions map[string]bool) {
		rows, err := again.DB.QueryContext(ctx, `SELECT event, COALESCE(plan_id, ''), COALESCE(execution_id, '') FROM flow_calls WHERE call_id = ? ORDER BY event_seq`, call)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		plans, executions = map[string]bool{}, map[string]bool{}
		for rows.Next() {
			var ev, plan, exe string
			if err := rows.Scan(&ev, &plan, &exe); err != nil {
				t.Fatal(err)
			}
			events = append(events, ev)
			plans[plan], executions[exe] = true, true
		}
		return events, plans, executions
	}
	for _, call := range []string{"c_old", "c_new"} {
		events, plans, executions := columns(call)
		if len(plans) != 1 || !plans["plan-1"] || len(executions) != 1 || !executions["exe-1"] {
			t.Errorf("call %s (%v): plan ids %v, execution ids %v, want plan-1 and exe-1 on every row", call, events, plans, executions)
		}
	}
	// a node the run had no execution for has none to inherit, and still has the run's plan
	if _, plans, executions := columns("c_other"); !plans["plan-1"] || !executions[""] || len(executions) != 1 {
		t.Errorf("call c_other: plan ids %v, execution ids %v", plans, executions)
	}
}
