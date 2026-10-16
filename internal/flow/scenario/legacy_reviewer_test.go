package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	flowcli "github.com/thisisjun786/codex-relay-workflow/internal/flow/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// The node reviewer path and the reviewer-final path are retired (CRW-1429), and the records an old run left stay read only. The current
// project lane runs the unit check where the old one called the node reviewer, and the crosscheck where the old close called
// reviewer-final, so a resume of an old run under the current script would not replay those positions: it would run them live, and every
// later position of the path after them. The resume is refused before anything is written, with a reason that names the retired row and
// CRW-1429; Recover and the recording audit write nothing to it; the readers still read its rows.

// laneProgram is the project lane the lab runs: its Source and Lib when set, else the embedded one.
func laneProgram(t *testing.T, l *Lab) *script.Program {
	t.Helper()
	if l.Lib == nil && l.Source == nil {
		prog, err := workflows.Load("project-lane")
		if err != nil {
			t.Fatal(err)
		}
		return prog
	}
	text := l.Source
	if text == nil {
		var err error
		if text, err = workflows.Source("project-lane"); err != nil {
			t.Fatal(err)
		}
	}
	prog, err := script.Load(script.Source{Name: "project-lane.js", Text: text, Dir: workflows.FS, Lib: l.Lib})
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

// runOver runs prog over the lab's journal (a new run, or the resume of the run the journal holds) and returns what engine.Run does,
// error included; lane, when set, is the project lane prog nests with workflow().
func runOver(l *Lab, prog, lane *script.Program, resume bool) (*engine.Result, error) {
	l.t.Helper()
	cfg := l.Config(resume)
	cfg.Args = l.Args()
	if lane != nil {
		// the lane is what the wrapper nests, by its name or by a script path
		cfg.Workflows = func(name, scriptPath string) (*script.Program, error) {
			if name == "project-lane" || scriptPath != "" {
				return lane, nil
			}
			return workflows.Load(name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	l.cancel = cancel
	defer cancel()
	res, err := engine.Run(ctx, prog, cfg)
	l.journal.flush()
	return res, err
}

// refusedUnchanged resumes the lab's run under the current project lane (nested in wrapper when set) and asserts the resume is refused
// with a reason naming every word of want, and that no row is written.
func refusedUnchanged(t *testing.T, l *Lab, wrapper *script.Program, want ...string) {
	t.Helper()
	l.Source, l.Lib, l.OldOps = nil, nil, nil
	current := laneProgram(t, l)
	prog, lane := current, (*script.Program)(nil)
	if wrapper != nil {
		prog, lane = wrapper, current
	}
	before := len(l.Events())
	res, err := runOver(l, prog, lane, true)
	if err == nil {
		status := ""
		if res != nil {
			status = string(res.Status)
		}
		t.Fatalf("the old run was resumed under the current lane (%s): rows %d -> %d", status, before, len(l.Events()))
	}
	for _, w := range append(want, "CRW-1429", "which the current project lane does not replay", "nothing is written to it") {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal %q does not name %q", err, w)
		}
	}
	if got := len(l.Events()); got != before {
		t.Errorf("the refused resume wrote rows: %d -> %d", before, got)
	}
}

// reviewRow is the first completed crw.review row of node.
func reviewRow(t *testing.T, l *Lab, node string) engine.Event {
	t.Helper()
	for _, ev := range l.Rows(ops.OpReview) {
		if ev.Event == engine.EvCompleted && str(ev.Data, "node") == node {
			return ev
		}
	}
	t.Fatalf("no completed crw.review row of node %s", node)
	return engine.Event{}
}

// TestOldJournalWithReviewerRowsStillReplaysOrIsRefusedWithAReason (CRW-1429): an old project-lane run whose node lane called the node
// reviewer (a reviewer agent call and its crw.review row) is refused for a resume under the current lane, naming crw.review, its call id,
// the node and CRW-1429, before any row is written; `crw flow show`'s run reader still lists the review and ReviewProof still reads it.
// A custom run that nested the old lane with workflow('project-lane') and ended between the reviewer call and its crw.review row is
// refused as well, naming the nested label, its workflow_opened call id and the reviewer call.
func TestOldJournalWithReviewerRowsStillReplaysOrIsRefusedWithAReason(t *testing.T) {
	t.Run("an old project lane", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		reviews(l, "A", Verdict(true))
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("the old run: %q; calls %v", got.State, l.Labels())
		}
		rev := reviewRow(t, l, "A")
		l.NewProcess("proc-2")
		refusedUnchanged(t, l, nil, ops.OpReview, rev.CallID, "node A")

		// the records stay readable
		detail := flowcli.Detail(flowcli.RunRecord{RunID: l.RunID, Workflow: "project-lane"}, l.Events(), false, 0)
		listed := false
		for _, n := range detail.Nodes {
			if n.Node == "A" && len(n.Reviews) > 0 && n.Reviews[0].Verdict == "verified" {
				listed = true
			}
		}
		if !listed {
			t.Errorf("the run reader lists no review of node A: %+v", detail.Nodes)
		}
		if v := ops.ReviewProof(l.Events(), "A", str(rev.Data, "head"), str(rev.Data, "criteria_digest")); !v.Proven || v.CallID != rev.CallID {
			t.Errorf("ReviewProof of the old review: %+v", v)
		}
	})

	t.Run("an old lane nested in a custom run, ended before its review row", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		reviewerLane(t, l)
		old := laneProgram(t, l)
		wrapper, err := script.Load(script.Source{Name: "custom.js", Text: []byte("export const meta = { name: 'custom' }\nreturn await workflow('project-lane', args)\n")})
		if err != nil {
			t.Fatal(err)
		}
		reviewer := ""
		l.OnRow(func(ev engine.Event) {
			if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Label, "review:A:") && reviewer == "" {
				reviewer = ev.CallID
				l.Kill()
			}
		})
		_, _ = runOver(l, wrapper, old, false)
		l.OnRow(nil)
		l.NewProcess("proc-2")
		var opened engine.Event
		decided := false
		for _, ev := range l.Events() {
			if ev.Event == "workflow_opened" {
				opened = ev
			}
			if ev.Op == ops.OpSpawnDecision && ev.Event == engine.EvCompleted && str(ev.Data, "rule") == ops.RuleRev1 {
				decided = true
			}
		}
		if reviewer == "" || opened.Label != "workflow:project-lane" || !decided || len(l.Rows(ops.OpReview)) != 0 {
			t.Fatalf("the old run: reviewer call %q, opened %q, SP-REV-1 decided %v, crw.review rows %d", reviewer, opened.Label, decided, len(l.Rows(ops.OpReview)))
		}
		refusedUnchanged(t, l, wrapper, "workflow:project-lane", str(opened.Data, "call_id"), reviewer, "reviewer agent call")
	})
}

// TestOldJournalWithAReviewerFinalRowIsRefusedWithAReason (CRW-1429): an old project-lane run whose close called reviewer-final (a
// reviewer-final agent call and its crw.reviewerFinal row) is refused for a resume under the current lane, naming crw.reviewerFinal, its
// call id, the milestone and CRW-1429, before any row is written; ReviewerFinalProof still reads the row.
func TestOldJournalWithAReviewerFinalRowIsRefusedWithAReason(t *testing.T) {
	l := NewLab(t, oneNode()...)
	// the close before CRW-1505 called reviewer-final over the passed gate; the crosscheck that follows it here leaves the close as it is
	l.Lib = libWith(t, "close.js", "    const x = await crosscheckRound(ms, g, opts)\n", "    await finalReview(ms, g)\n    const x = await crosscheckRound(ms, g, opts)\n")
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("the old run: %q; calls %v", got.State, l.Labels())
	}
	rows := l.Rows(ops.OpReviewerFinal)
	if len(rows) == 0 || len(l.CallsOf("reviewer-final")) == 0 {
		t.Fatalf("the old run wrote %d crw.reviewerFinal rows and %d reviewer-final calls", len(rows), len(l.CallsOf("reviewer-final")))
	}
	first := rows[0]
	l.NewProcess("proc-2")
	refusedUnchanged(t, l, nil, ops.OpReviewerFinal, first.CallID, "milestone M1", "the retired reviewer-final path")
	var last engine.Event
	for _, ev := range l.Rows(ops.OpReviewerFinal) {
		if ev.Event == engine.EvCompleted {
			last = ev
		}
	}
	if v := ops.ReviewerFinalProof(l.Events(), str(last.Data, "range"), str(last.Data, "criteria_digest")); !v.Proven {
		t.Errorf("ReviewerFinalProof of the old final review: %+v", v)
	}
}

// TestRecoverAndTheRecordingAuditWriteNothingToALegacyReviewerRun (CRW-1429): Recover of a call of an old reviewer run is refused with the
// reason and appends no recovery_cleared row; `crw flow audit` of the finished run prints its results, records nothing and says why;
// `--no-record` says what it said before.
func TestRecoverAndTheRecordingAuditWriteNothingToALegacyReviewerRun(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviewerLane(t, l)
	reviews(l, "A", Verdict(true))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("the old run: %q; calls %v", got.State, l.Labels())
	}
	rev := reviewRow(t, l, "A")
	recoverAndAuditRefused(t, l, str(rev.Data, "call"), true, ops.OpReview, rev.CallID)
}

// recoverAndAuditRefused asserts that Recover of a call of the lab's legacy run is refused with a reason naming every word of want and
// appends nothing, and that the recording `crw flow audit` of the run records nothing: for a run with acceptances (results) it prints
// their results and says why, while `--no-record` says what it said before; a run with none has nothing for the command to audit.
func recoverAndAuditRefused(t *testing.T, l *Lab, callID string, results bool, want ...string) {
	t.Helper()
	want = append(want, "CRW-1429")
	before := len(l.Events())
	_, err := engine.Recover(context.Background(), l.journal, engine.ReconcileConfig{RunID: l.RunID, Host: l.backend}, engine.RecoverRequest{CallID: callID, Absent: true})
	for _, w := range want {
		if err == nil || !strings.Contains(err.Error(), w) {
			t.Errorf("Recover of the legacy run: %v, want a refusal naming %q", err, w)
		}
	}
	for _, ev := range l.Events() {
		if ev.Event == "recovery_cleared" {
			t.Errorf("a recovery_cleared row: %+v", ev)
		}
	}
	if got := len(l.Events()); got != before {
		t.Errorf("the refused recovery wrote rows: %d -> %d", before, got)
	}

	prev := flowcli.AuditEnv
	flowcli.AuditEnv = flowcli.OpenAuditStore
	t.Cleanup(func() { flowcli.AuditEnv = prev })
	auditRun := func(extra ...string) (int, map[string]any, string) {
		_ = l.Store.Close()
		defer func() { l.Store = l.openStore() }()
		var out, errOut bytes.Buffer
		code := flowcli.RunAudit(context.Background(), append([]string{"audit", "--state", l.Root, "--json", "--run", l.RunID, "--socket", l.Host.SocketPath}, extra...), &out, &errOut)
		var rep map[string]any
		_ = json.Unmarshal(out.Bytes(), &rep)
		return code, rep, errOut.String()
	}
	auditRows := func() int {
		var n int
		if err := l.Store.DB.QueryRow(`SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND kind = 'crw.audit'`, l.RunID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	rowsBefore := auditRows()
	code, rep, errOut := auditRun()
	if !results {
		if got := auditRows(); got != rowsBefore || code == 0 {
			t.Errorf("the audit of a legacy run with no acceptance: exit %d, %d crw.audit rows written, %s", code, got-rowsBefore, errOut)
		}
		return
	}
	why, _ := rep["not_recorded_because"].(string)
	printed, _ := rep["results"].([]any)
	if rep["recorded"] != false || len(printed) == 0 || !strings.Contains(why, flowcli.RefusedLegacyRun) {
		t.Errorf("the recording audit of the legacy run: exit %d, %v, %s", code, rep, errOut)
	}
	for _, w := range want {
		if !strings.Contains(why, w) {
			t.Errorf("the recording audit of the legacy run says %q, want it to name %q", why, w)
		}
	}
	code, rep, errOut = auditRun("--no-record")
	if why, _ := rep["not_recorded_because"].(string); rep["recorded"] != false || why != "--no-record" {
		t.Errorf("the audit --no-record of the legacy run: exit %d, %v, %s", code, rep, errOut)
	}
	if got := auditRows(); got != rowsBefore {
		t.Errorf("the audits of the legacy run wrote %d crw.audit rows", got-rowsBefore)
	}
}

// oldSettleLib is crw:lib as the close of the build before CRW-1447 ran it: the audit of the acceptances, its audit_mismatch escalation,
// then crw.archive of the reviewer threads whose audit passed, before the report.
func oldSettleLib(t *testing.T) fs.FS {
	t.Helper()
	return libWith(t, "close.js", "  return { audit: audited }\n", "  await crw.archive(ms.id)\n  return { audit: audited }\n")
}

// archiveRow is the completed crw.archive row of the lab's run.
func archiveRow(t *testing.T, l *Lab) engine.Event {
	t.Helper()
	for _, ev := range l.Rows(ops.OpArchive) {
		if ev.Event == engine.EvCompleted {
			return ev
		}
	}
	t.Fatalf("no completed crw.archive row; calls %v", l.Labels())
	return engine.Event{}
}

// lastAgentCall is the id of the last agent call of the lab's run.
func lastAgentCall(t *testing.T, l *Lab) string {
	t.Helper()
	id := ""
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == "agent" {
			id = ev.CallID
		}
	}
	if id == "" {
		t.Fatal("no agent call")
	}
	return id
}

// TestOldSettleRunThatArchivedIsRefusedBeforeAnyWrite (CRW-1447): an old project-lane run whose close archived the reviewer threads after
// its audit (the build of the unit check and the crosscheck: no reviewer row, completed crw.audit, crw.archive and crw.reportUp rows) is
// refused for a resume under the current lane, which audits and reports and archives nothing, naming crw.archive, its call id and
// CRW-1429; Recover and the recording audit write nothing to it. The row count is unchanged.
func TestOldSettleRunThatArchivedIsRefusedBeforeAnyWrite(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Lib = oldSettleLib(t)
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("the old run: %q; calls %v", got.State, l.Labels())
	}
	if len(l.Rows(ops.OpReview)) != 0 || len(l.CallsOf("reviewer")) != 0 || len(l.Rows(audit.Op)) == 0 || len(l.Rows(ops.OpReportUp)) == 0 {
		t.Fatalf("the old run: %d crw.review rows, %d reviewer calls, %d crw.audit rows, %d crw.reportUp rows", len(l.Rows(ops.OpReview)), len(l.CallsOf("reviewer")), len(l.Rows(audit.Op)), len(l.Rows(ops.OpReportUp)))
	}
	arc := archiveRow(t, l)
	l.NewProcess("proc-2")
	refusedUnchanged(t, l, nil, ops.OpArchive, arc.CallID, "milestone M1", "the retired review-thread archive of the close")
	recoverAndAuditRefused(t, l, lastAgentCall(t, l), true, ops.OpArchive, arc.CallID)
}

// TestOldLaneNestedInACustomWrapperIsRefusedBeforeAnyWrite (CRW-1447): a custom run that nested the old project lane - by its name, or by
// a script path of another name, whose archive the lane scope does not reach and the settle shape does - or that called the old close's
// settle step itself (crw.audit, its audit_mismatch escalation, crw.archive on its own path) is refused for a resume, for Recover and for
// the recording audit before any write, naming crw.archive, its call id and CRW-1429 (and the nested label when the lane was nested by
// name). The row count is unchanged.
func TestOldLaneNestedInACustomWrapperIsRefusedBeforeAnyWrite(t *testing.T) {
	nested := func(t *testing.T, wrapperText string, want ...string) {
		t.Helper()
		l := NewLab(t, oneNode()...)
		l.Lib = oldSettleLib(t)
		old := laneProgram(t, l)
		wrapper, err := script.Load(script.Source{Name: "custom.js", Text: []byte(wrapperText)})
		if err != nil {
			t.Fatal(err)
		}
		res, err := runOver(l, wrapper, old, false)
		if err != nil || res.Status != engine.RunCompleted {
			t.Fatalf("the old run: %v %+v; calls %v", err, res, l.Labels())
		}
		arc := archiveRow(t, l)
		var opened engine.Event
		for _, ev := range l.Events() {
			if ev.Event == "workflow_opened" {
				opened = ev
			}
		}
		if opened.Event == "" || !strings.HasPrefix(arc.Path, engine.RootPath+"/") {
			t.Fatalf("the old run: opened %+v, the archive on %q", opened, arc.Path)
		}
		l.NewProcess("proc-2")
		want = append(want, ops.OpArchive, arc.CallID)
		if strings.Contains(wrapperText, "workflow('project-lane'") {
			want = append(want, str(opened.Data, "call_id"))
		}
		refusedUnchanged(t, l, wrapper, want...)
		recoverAndAuditRefused(t, l, lastAgentCall(t, l), true, ops.OpArchive, arc.CallID)
	}
	t.Run("nested by its name", func(t *testing.T) {
		nested(t, "export const meta = { name: 'custom' }\nreturn await workflow('project-lane', args)\n", "workflow:project-lane", "the retired review-thread archive of the close")
	})
	t.Run("nested by a script path of another name", func(t *testing.T) {
		nested(t, "export const meta = { name: 'custom' }\nreturn await workflow({ scriptPath: 'lanes/my-lane.js' }, args)\n", "after its crw.audit")
	})
	t.Run("the settle step on the custom's own path", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		custom, err := script.Load(script.Source{Name: "custom.js", Text: []byte("export const meta = { name: 'custom' }\n" +
			"const audited = await crw.audit('M1')\n" +
			"await crw.escalate(null, { reason: 'audit_mismatch', detail: 'n: ' + audited.mismatch })\n" +
			"await crw.archive('M1')\n" +
			"return { audited: audited.ok }\n")})
		if err != nil {
			t.Fatal(err)
		}
		res, err := runOver(l, custom, nil, false)
		if err != nil || res.Status != engine.RunCompleted {
			t.Fatalf("the custom run: %v %+v", err, res)
		}
		arc := archiveRow(t, l)
		l.NewProcess("proc-2")
		refusedUnchanged(t, l, custom, ops.OpArchive, arc.CallID, "after its crw.audit")
		recoverAndAuditRefused(t, l, arc.CallID, false, ops.OpArchive, arc.CallID)
	})
}
