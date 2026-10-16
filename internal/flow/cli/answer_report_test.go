package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1574: the parent answers a child's project_decision_request with a decision record whose source is that report
// (`--source report=<report id>`); `crw flow answer` applies it to the run's question the report lifts.

// incidentQuestion is the question of the incident (1209 trial): a design ask of the milestone (node ""), revision 1, raised by the
// script (no call, attempt 0).
const incidentQuestion = "esc_2560f6e889d72f86bc90d1f0"

// reportAsking is a run that waits on design questions, in a world whose store takes the child's report-up and the parent's record.
type reportAsking struct {
	w   *world
	run string
}

// designAsk is the escalation row of a design question of node.
func designAsk(id, node string) map[string]any {
	return map[string]any{"escalation_id": id, "node": node, "reason": "design_question", "class": "decision", "ask": "design"}
}

// newReportAsking writes run (owned by thread-owner, plan-1 revision 3) with the rows of asks.
func newReportAsking(t *testing.T, w *world, run string, asks ...map[string]any) *reportAsking {
	t.Helper()
	r := w.newRun(run, "thread-owner")
	r.phase("design")
	for _, a := range asks {
		r.flow(engine.EvEscalation, a)
	}
	r.finish(engine.RunCompleted, "")
	r.close()
	return &reportAsking{w: w, run: run}
}

// reask asks the same question of node again (a later revision of the ask).
func (ra *reportAsking) reask(t *testing.T, id, node string) {
	t.Helper()
	r, err := ra.w.reopen(ra.run)
	if err != nil {
		t.Fatal(err)
	}
	r.flow(engine.EvEscalation, designAsk(id, node))
	r.close()
}

// reportUp is the child's `crw flow report-up` about run (kind and the extra flags given), and returns the report id.
func (ra *reportAsking) reportUp(t *testing.T, run, kind string, extra ...string) string {
	t.Helper()
	evidence := filepath.Join(t.TempDir(), "request.md")
	if err := os.WriteFile(evidence, []byte("escalation "+incidentQuestion+" r1 (design_question, ask design): which schema?"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	env := Environment{Stdout: &out, Stderr: &errOut, Sender: &fakeSender{reply: report.SendResult{Class: "accepted", RequestID: "req-1"}},
		Resolver: fixedResolver{report.Recipient{State: report.RecipientInitiative, Thread: "lead-1", InitiativeKey: "INI-1", ProjectKey: "PRJ-1"}},
		Open: func(context.Context, string, string) (*store.Store, string, error) {
			st, err := store.Open(ctx, ra.w.db, "")
			return st, ra.w.state, err
		}}
	args := append([]string{"report-up", "--kind", kind, "--run", run, "--evidence", evidence, "--subject", "which schema"}, extra...)
	if code := RunEnv(ctx, args, env); code != ExitOK {
		t.Fatalf("report-up %v: exit %d %s", args, code, errOut.String())
	}
	var up report.UpResult
	if err := json.Unmarshal(out.Bytes(), &up); err != nil || up.ReportID == "" {
		t.Fatalf("report-up printed %q: %v", out.String(), err)
	}
	return up.ReportID
}

// recorded stores the parent's decision record whose source is the report, answered, as the parent raises it in the incident (no
// blocking entry, the manager's class), and returns its id.
func (ra *reportAsking) recorded(t *testing.T, id, reportID string, mutate func(*decisions.Record, *decisions.Answer)) string {
	t.Helper()
	rec := decisions.Record{
		Schema: decisions.Schema, DecisionID: id, Kind: decisions.KindPolicy, Context: "the child asks which schema: " + id,
		Options:   []decisions.Option{{ID: "a", Label: "schema a", Effect: "the lane builds a"}, {ID: "b", Label: "schema b", Effect: "the lane builds b"}},
		Origin:    decisions.Origin{Project: "P"},
		Source:    decisions.Source{Kind: "report", Ref: reportID},
		Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}, State: decisions.StateRaised, RaisedAt: "2026-10-16T04:05:00Z", Seen: []decisions.Seen{{At: "2026-10-16T04:05:00Z", Source: "test"}},
	}
	ans := decisions.Answer{Option: "a", By: "parent-task", Via: decisions.ViaManagementMessage, Authority: decisions.Authority{Kind: decisions.AuthorityDelegatedManagement}}
	if mutate != nil {
		mutate(&rec, &ans)
	}
	rec.Fingerprint = decisions.Fingerprint(rec.Context, rec.Blocking, rec.Options)
	if _, _, err := ra.w.st.Raise(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := ra.w.st.Update(ctx, id, func(_ context.Context, r decisions.Record) (decisions.Record, error) {
		return decisions.ValidateAnswer(r, ans)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// answer is `crw flow answer --decision --run --question --revision` with no --node (a question of the milestone), and the extra flags.
func (ra *reportAsking) answer(t *testing.T, td *testDeps, decision, question, revision string, extra ...string) (int, AnswerReply, string) {
	t.Helper()
	args := append([]string{"answer", "--state", ra.w.state, "--decision", decision, "--run", ra.run, "--question", question, "--revision", revision, "--thread", "thread-owner", "--no-resume", "--json"}, extra...)
	code, out, errOut := td.do(args...)
	var reply AnswerReply
	if err := json.Unmarshal([]byte(out), &reply); err != nil && out != "" {
		t.Fatalf("%v: %s", err, out)
	}
	return code, reply, errOut
}

func (ra *reportAsking) applied(t *testing.T) []engine.Event {
	t.Helper()
	rows, _, err := loadRun(ra.w, ra.run)
	if err != nil {
		t.Fatal(err)
	}
	var out []engine.Event
	for _, e := range rows {
		if e.Kind == engine.KindFlow && e.Event == ops.EvDecisionApplied {
			out = append(out, e)
		}
	}
	return out
}

// refused says whether an answer was refused with code and a detail holding want, and nothing was appended.
func (ra *reportAsking) refused(t *testing.T, name string, code int, reply AnswerReply, errOut, wantCode, want string) {
	t.Helper()
	if code != 1 || reply.Refusal == nil || reply.Refusal.Code != wantCode || !strings.Contains(reply.Refusal.Detail, want) || reply.Applied {
		t.Errorf("%s: exit %d %+v %s, want %s naming %q", name, code, reply, errOut, wantCode, want)
	}
	if n := len(ra.applied(t)); n != 0 {
		t.Errorf("%s: %d decision_applied rows after a refused answer", name, n)
	}
}

// TestAnswerOfAReportSourcedRecordApplies (CRW-1574, the incident): a run waits on one design question of the milestone; the child lifts
// it with report-up project_decision_request (without --question, as in the incident); the parent records the answer with the report as
// its source and the manager's class; `crw flow answer` names the run, the question and revision 1 (no --node) and applies it: one
// decision_applied row for the question, exit 0. The same answer again appends nothing.
func TestAnswerOfAReportSourcedRecordApplies(t *testing.T) {
	w := newWorld(t)
	ra := newReportAsking(t, w, "run-d", designAsk(incidentQuestion, ""))
	reportID := ra.reportUp(t, "run-d", report.KindProjectDecisionRequest)
	id := ra.recorded(t, "ud-report", reportID, nil)
	td := w.deps()
	code, reply, errOut := ra.answer(t, td, id, incidentQuestion, "1")
	if code != 0 || !reply.Applied || reply.Refusal != nil {
		t.Fatalf("the answer of the report-sourced record: exit %d %+v %s", code, reply, errOut)
	}
	rows := ra.applied(t)
	if len(rows) != 1 || rows[0].Seq != reply.Seq {
		t.Fatalf("decision_applied rows %+v", rows)
	}
	d := rows[0].Data
	dec, _ := d["decision"].(map[string]any)
	if d["ask"] != incidentQuestion || d["node"] != "" || d["run"] != "run-d" || d["revision"] != float64(1) || d["attempt"] != float64(0) ||
		d["plan_basis"] != PlanBasis("plan-1", 3) || d["action"] != "a" || dec["id"] != id || dec["authority"] != decisions.AuthorityDelegatedManagement {
		t.Errorf("the row: %+v", d)
	}
	code, again, _ := ra.answer(t, td, id, incidentQuestion, "1")
	if code != 0 || !again.Replayed || again.Applied || again.Seq != reply.Seq || len(ra.applied(t)) != 1 {
		t.Errorf("the same answer again: exit %d %+v, %d rows", code, again, len(ra.applied(t)))
	}
}

// TestReportSourceBindsTheNamedQuestion (CRW-1574): a report made with --question names the question it lifts, and the record whose
// source it is binds that question even while the run waits on others: it applies to it; a claim of another question is refused as for
// a flow_question record (stale_revision), and after a re-ask of the same node and ask the record answers a replaced question
// (stale_revision).
func TestReportSourceBindsTheNamedQuestion(t *testing.T) {
	w := newWorld(t)
	ra := newReportAsking(t, w, "run-d", designAsk("esc-ms", ""), designAsk("esc-b", "B"))
	reportID := ra.reportUp(t, "run-d", report.KindProjectDecisionRequest, "--question", "esc-b")
	id := ra.recorded(t, "ud-named", reportID, nil)
	td := w.deps()
	code, reply, errOut := ra.answer(t, td, id, "esc-ms", "1", "--node", "B")
	ra.refused(t, "a claim of another question than the report names", code, reply, errOut, RefusedStaleRevison, "the record answers question esc-b revision 1")
	code, reply, errOut = ra.answer(t, td, id, "esc-b", "1", "--node", "A")
	ra.refused(t, "a claim of another node than the question's", code, reply, errOut, RefusedOtherNode, `the record binds node "B"`)
	code, reply, errOut = ra.answer(t, td, id, "esc-b", "1", "--node", "B")
	if code != 0 || !reply.Applied {
		t.Fatalf("the answer of the question the report names: exit %d %+v %s", code, reply, errOut)
	}
	if rows := ra.applied(t); len(rows) != 1 || rows[0].Data["ask"] != "esc-b" || rows[0].Data["node"] != "B" {
		t.Fatalf("decision_applied rows %+v", rows)
	}

	// the report named revision 1 of node A's ask; the lane asked it again (revision 2) before the answer
	w2 := newWorld(t)
	ra2 := newReportAsking(t, w2, "run-e", designAsk("esc-a1", "A"))
	reportID2 := ra2.reportUp(t, "run-e", report.KindProjectDecisionRequest, "--question", "esc-a1")
	ra2.reask(t, "esc-a2", "A")
	id2 := ra2.recorded(t, "ud-reasked", reportID2, nil)
	td2 := w2.deps()
	code, reply, errOut = ra2.answer(t, td2, id2, "esc-a1", "1", "--node", "A")
	ra2.refused(t, "a report of a question asked again", code, reply, errOut, RefusedStaleRevison, "replaced by a later question")
	code, reply, errOut = ra2.answer(t, td2, id2, "esc-a2", "2", "--node", "A")
	ra2.refused(t, "a claim of the later revision", code, reply, errOut, RefusedStaleRevison, "the record answers question esc-a1 revision 1")
}

// TestReportSourceWithoutQuestionNeedsOneWaiting (CRW-1574): a report made without --question binds the claim's question only when it is
// the one question the run waits on; with two waiting the record is a bad binding that names report-up --question, and with none (the
// only question answered by another decision) too.
func TestReportSourceWithoutQuestionNeedsOneWaiting(t *testing.T) {
	w := newWorld(t)
	ra := newReportAsking(t, w, "run-d", designAsk("esc-ms", ""), designAsk("esc-b", "B"))
	id := ra.recorded(t, "ud-two", ra.reportUp(t, "run-d", report.KindProjectDecisionRequest), nil)
	td := w.deps()
	code, reply, errOut := ra.answer(t, td, id, "esc-ms", "1")
	ra.refused(t, "two questions waiting", code, reply, errOut, RefusedBadBinding, "waits on 2 questions: report-up --question names it")

	// one waiting, and the claim names another (a replaced revision of it): the one waiting is the binding, the claim differs
	w2 := newWorld(t)
	ra2 := newReportAsking(t, w2, "run-e", designAsk("esc-1", ""), designAsk("esc-2", ""))
	id2 := ra2.recorded(t, "ud-one", ra2.reportUp(t, "run-e", report.KindProjectDecisionRequest), nil)
	td2 := w2.deps()
	code, reply, errOut = ra2.answer(t, td2, id2, "esc-1", "1")
	ra2.refused(t, "a claim of a replaced question", code, reply, errOut, RefusedStaleRevison, "the record answers question esc-2 revision 2")
	if code, reply, errOut = ra2.answer(t, td2, id2, "esc-2", "2"); code != 0 || !reply.Applied {
		t.Fatalf("the claim of the one waiting question: exit %d %+v %s", code, reply, errOut)
	}
	// that question is answered now: another record of the same report finds no question waiting
	other := ra2.recorded(t, "ud-after", ra2.reportUp(t, "run-e", report.KindProjectDecisionRequest, "--subject", "again"), nil)
	code, reply, errOut = ra2.answer(t, td2, other, "esc-2", "2")
	if code != 1 || reply.Refusal == nil || reply.Refusal.Code != RefusedBadBinding || !strings.Contains(reply.Refusal.Detail, "waits on 0 questions") {
		t.Errorf("a report-sourced record with no question waiting: exit %d %+v %s", code, reply, errOut)
	}
}

// TestReportSourceOfAnotherKindOrRunIsBadBinding (CRW-1574): the record's report must be a project_decision_request of the run: a
// milestone_report, a report of another run and an id no report has are bad bindings, as is a plan_node blocking entry of another node,
// and nothing is appended.
func TestReportSourceOfAnotherKindOrRunIsBadBinding(t *testing.T) {
	w := newWorld(t)
	ra := newReportAsking(t, w, "run-d", designAsk(incidentQuestion, ""))
	newReportAsking(t, w, "run-x", designAsk("esc-x", ""))
	td := w.deps()
	blocked := ra.reportUp(t, "run-d", report.KindProjectBlocked)
	foreign := ra.reportUp(t, "run-x", report.KindProjectDecisionRequest)
	request := ra.reportUp(t, "run-d", report.KindProjectDecisionRequest)
	cases := []struct {
		name, report, want string
		mutate             func(*decisions.Record, *decisions.Answer)
	}{
		{"a project_blocked report", blocked, "is a project_blocked, not a project_decision_request", nil},
		{"a report of another run", foreign, "is not a report of run run-d", nil},
		{"an id no report has", "rep_ghost", "the record's report rep_ghost is not a report of run run-d", nil},
		{"a plan node of another node", request, `plan node "run-d/n9"`, func(r *decisions.Record, _ *decisions.Answer) {
			r.Blocking = []decisions.Blocking{{Kind: decisions.BlockingPlanNode, Ref: "run-d/n9"}}
		}},
	}
	for i, c := range cases {
		id := ra.recorded(t, "ud-bad-"+itoa(i), c.report, c.mutate)
		code, reply, errOut := ra.answer(t, td, id, incidentQuestion, "1")
		ra.refused(t, c.name, code, reply, errOut, RefusedBadBinding, c.want)
	}
	// the run's own plan node entry is the binding's: it applies
	id := ra.recorded(t, "ud-node", request, func(r *decisions.Record, _ *decisions.Answer) {
		r.Blocking = []decisions.Blocking{{Kind: decisions.BlockingPlanNode, Ref: "run-d/"}}
	})
	if code, reply, errOut := ra.answer(t, td, id, incidentQuestion, "1"); code != 0 || !reply.Applied {
		t.Errorf("a report-sourced record with the question's plan node: exit %d %+v %s", code, reply, errOut)
	}
}

// TestReportSourceAuthority (CRW-1574): a report-sourced record is judged as a flow_question record: a parent-class answer of another
// orchestrator than the one that holds the run is out of scope; the orchestrator that holds the run applies.
func TestReportSourceAuthority(t *testing.T) {
	parent := func(ref string) func(*decisions.Record, *decisions.Answer) {
		return func(r *decisions.Record, ans *decisions.Answer) {
			r.Authority = decisions.Authority{Kind: decisions.AuthorityParent, Ref: ref}
			ans.Authority, ans.Via = r.Authority, decisions.ViaDirectAsk
		}
	}
	w := newWorld(t)
	ra := newReportAsking(t, w, "run-d", designAsk(incidentQuestion, ""))
	reportID := ra.reportUp(t, "run-d", report.KindProjectDecisionRequest)
	td := w.deps()
	code, reply, errOut := ra.answer(t, td, ra.recorded(t, "ud-intruder", reportID, parent("thread-intruder")), incidentQuestion, "1")
	ra.refused(t, "a parent-class answer of another orchestrator", code, reply, errOut, RefusedAuthority, "orchestrator that holds the run")
	if code, reply, errOut := ra.answer(t, td, ra.recorded(t, "ud-owner", reportID, parent("thread-owner")), incidentQuestion, "1"); code != 0 || !reply.Applied {
		t.Errorf("a parent-class answer of the orchestrator that holds the run: exit %d %+v %s", code, reply, errOut)
	}
}
