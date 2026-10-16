package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
)

// questionRun writes run with a started row and then the rows, in the cliWorld's store.
func (w *cliWorld) questionRun(run string, rows ...engine.Event) {
	w.t.Helper()
	j, err := journal.Open(ctx, w.st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 1, ActorTaskID: "orch-task", EpochNonce: "n"}, journal.Options{Immediate: true})
	if err != nil {
		w.t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "project-lane", "script_digest": "d", "args_digest": "a"}})
	for _, e := range rows {
		e.RunID = run
		j.Append(e)
	}
	if err := j.Close(); err != nil {
		w.t.Fatal(err)
	}
}

func askRow(id, node, ask string) engine.Event {
	return engine.Event{Kind: engine.KindFlow, Event: engine.EvEscalation,
		Data: map[string]any{"escalation_id": id, "node": node, "reason": "design_question", "class": "decision", "ask": ask}}
}

func (w *cliWorld) evidenceFile(text string) string {
	w.t.Helper()
	path := filepath.Join(w.t.TempDir(), "evidence.md")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		w.t.Fatal(err)
	}
	return path
}

func (w *cliWorld) reportRows(run string) []report.Row {
	w.t.Helper()
	rows, err := report.ReadRows(ctx, w.st.DB, run, report.KindReport)
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

// TestReportUpRecordsTheQuestion (CRW-1574): `crw flow report-up --kind project_decision_request --question ESC` about a run that waits
// on that question writes the report row with the question, its revision and its node (node "" for a milestone's design ask); without
// --question the row has none of the three members, as before.
func TestReportUpRecordsTheQuestion(t *testing.T) {
	w := newCLIWorld(t)
	w.questionRun("run-q", askRow("esc-ms", "", "design"), askRow("esc-node", "B", "design"))
	for _, c := range []struct{ id, node, subject string }{{"esc-ms", "", "milestone ask"}, {"esc-node", "B", "node ask"}} {
		ev := w.evidenceFile("which schema for " + c.subject)
		if code := w.run("report-up", "--kind", "project_decision_request", "--run", "run-q", "--evidence", ev, "--subject", c.subject, "--question", c.id); code != ExitOK {
			t.Fatalf("report-up --question %s: exit %d %s", c.id, code, w.errOut.String())
		}
		var up report.UpResult
		w.json(&up)
		var row *report.Row
		for _, r := range w.reportRows("run-q") {
			if r.Str("report_id") == up.ReportID {
				row = &r
			}
		}
		if row == nil {
			t.Fatalf("no report row %s", up.ReportID)
		}
		node, hasNode := row.Payload["node"]
		if row.Str("question") != c.id || num(row.Payload, "revision") != 1 || !hasNode || node != c.node {
			t.Errorf("the report row of %s: %+v, want question %s, revision 1, node %q", c.id, row.Payload, c.id, c.node)
		}
	}
	ev := w.evidenceFile("which schema, no question")
	if code := w.run("report-up", "--kind", "project_decision_request", "--run", "run-q", "--evidence", ev, "--subject", "plain"); code != ExitOK {
		t.Fatalf("report-up without --question: exit %d %s", code, w.errOut.String())
	}
	var up report.UpResult
	w.json(&up)
	for _, r := range w.reportRows("run-q") {
		if r.Str("report_id") != up.ReportID {
			continue
		}
		for _, k := range []string{"question", "revision", "node"} {
			if _, ok := r.Payload[k]; ok {
				t.Errorf("the report row without --question has %s: %+v", k, r.Payload)
			}
		}
	}
}

// TestReportUpRefusesAQuestionNotWaiting (CRW-1574): a --question the run does not wait on (an unknown id, a revision a later ask of the
// same node and ask replaced, a question a decision answered, a request to send up such as bundle_split_request) is refused with exit 1
// and no report row is written, no notice sent.
func TestReportUpRefusesAQuestionNotWaiting(t *testing.T) {
	w := newCLIWorld(t)
	w.questionRun("run-q",
		askRow("esc-old", "A", "design"), askRow("esc-new", "A", "design"),
		askRow("esc-done", "B", "design"),
		engine.Event{Kind: engine.KindFlow, Event: ops.EvDecisionApplied, Data: map[string]any{"ask": "esc-done", "node": "B", "decision": map[string]any{"id": "dec-1"}}},
		askRow("esc-split", "C", "bundle_split_request"))
	ev := w.evidenceFile("which schema")
	for _, c := range []struct{ id, want string }{
		{"esc-missing", "no question esc-missing"},
		{"esc-old", "replaced"},
		{"esc-done", "answered"},
		{"esc-split", "bundle_split_request"},
	} {
		code := w.run("report-up", "--kind", "project_decision_request", "--run", "run-q", "--evidence", ev, "--subject", "s", "--question", c.id)
		if code != ExitRefused || !strings.Contains(w.errOut.String(), c.want) {
			t.Errorf("report-up --question %s: exit %d %q, want %d naming %q", c.id, code, w.errOut.String(), ExitRefused, c.want)
		}
	}
	if rows := w.reportRows("run-q"); len(rows) != 0 || len(w.sender.supervisor) != 0 {
		t.Errorf("the refused report-ups wrote %d report rows and sent %d notices", len(rows), len(w.sender.supervisor))
	}
}

// TestReportUpQuestionOnlyForADecisionRequest (CRW-1574): --question names the question a project_decision_request lifts; with another
// kind it is a usage error and nothing is written.
func TestReportUpQuestionOnlyForADecisionRequest(t *testing.T) {
	w := newCLIWorld(t)
	w.questionRun("run-q", askRow("esc-ms", "", "design"))
	ev := w.evidenceFile("the milestone is closed")
	for _, kind := range []string{report.KindMilestoneReport, report.KindProjectBlocked, report.KindJunDecisionRequest} {
		if code := w.run("report-up", "--kind", kind, "--run", "run-q", "--evidence", ev, "--question", "esc-ms"); code != ExitUsage || !strings.Contains(w.errOut.String(), "--question") {
			t.Errorf("report-up --kind %s --question: exit %d %q, want a usage error", kind, code, w.errOut.String())
		}
	}
	if rows := w.reportRows("run-q"); len(rows) != 0 || len(w.sender.supervisor) != 0 {
		t.Errorf("the refused report-ups wrote %d report rows and sent %d notices", len(rows), len(w.sender.supervisor))
	}
	if !strings.Contains(Usage, "--question ESC") {
		t.Error("the usage of report-up does not name --question")
	}
}
