package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
		cfg.Workflows = func(name, _ string) (*script.Program, error) {
			if name == "project-lane" {
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
	l.Source, l.Lib = nil, nil
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
	before := len(l.Events())
	_, err := engine.Recover(context.Background(), l.journal, engine.ReconcileConfig{RunID: l.RunID, Host: l.backend}, engine.RecoverRequest{CallID: str(rev.Data, "call"), Absent: true})
	if err == nil || !strings.Contains(err.Error(), ops.OpReview) || !strings.Contains(err.Error(), rev.CallID) || !strings.Contains(err.Error(), "CRW-1429") {
		t.Errorf("Recover of the old reviewer run: %v", err)
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
	why, _ := rep["not_recorded_because"].(string)
	results, _ := rep["results"].([]any)
	if rep["recorded"] != false || len(results) == 0 || !strings.Contains(why, flowcli.RefusedLegacyRun) || !strings.Contains(why, ops.OpReview) || !strings.Contains(why, "CRW-1429") {
		t.Errorf("the recording audit of the old reviewer run: exit %d, %v, %s", code, rep, errOut)
	}
	code, rep, errOut = auditRun("--no-record")
	if why, _ := rep["not_recorded_because"].(string); rep["recorded"] != false || why != "--no-record" {
		t.Errorf("the audit --no-record of the old reviewer run: exit %d, %v, %s", code, rep, errOut)
	}
	if got := auditRows(); got != rowsBefore {
		t.Errorf("the audits of the old reviewer run wrote %d crw.audit rows", got-rowsBefore)
	}
}
