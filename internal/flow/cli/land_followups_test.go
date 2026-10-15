package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// CRW-1397: the follow-ups of the gate-only land (CRW-1354) over the lane lab.

// endRun appends the finished row of a run whose process died, as the run's end: the run is over (failed) with what it holds.
func (l *laneLab) endRun(run string) {
	l.t.Helper()
	j, err := openJournal(l.w, run)
	if err != nil {
		l.t.Fatal(err)
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunScriptError, "error_kind": "script_error"}})
	if err := j.Close(); err != nil {
		l.t.Fatal(err)
	}
}

// releaseRowsOf are the completed crw.release rows of the current process's journal for a node.
func (l *laneLab) releaseRowsOf(node string) []engine.Event {
	var out []engine.Event
	for _, e := range l.events() {
		if e.Kind == engine.KindCall && e.Op == ops.OpRelease && e.Event == engine.EvCompleted && str(e.Data, "node") == node {
			out = append(out, e)
		}
	}
	return out
}

// TestLandHeadOfANodeAnEndedRunHolds (item 1): a node whose current execution belongs to a run that has ended (a land whose execution
// was not given back) is answered land_node_held as before, and the answer names the command that gives that execution up - crw flow
// cancel of the ended run, not a resume of a run that is over - with the run's state on the row; nothing is refused that was not. The
// cancel of the ended run gives the execution up, and the next land lands.
func TestLandHeadOfANodeAnEndedRunHolds(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	// the first land's process dies right after the relay released the node, and the run ends with the execution it holds
	l.killAt = l.writes + 1
	run1 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	if _, killed := l.process(run1.RunID, false); !killed {
		t.Fatal("the first land was not killed")
	}
	l.killAt = 0
	l.endRun(run1.RunID)

	run2 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	v := landValueOf(t, l.runToEnd(run2.RunID))
	if v.State != "unreleased" || v.Reason != "land_node_held" {
		t.Fatalf("a land of a node an ended run holds: %+v", v)
	}
	res, _ := ReadResult(l.w.state, run2.RunID)
	if !strings.Contains(string(res.Value), "crw flow cancel "+run1.RunID) || strings.Contains(string(res.Value), "crw flow resume "+run1.RunID) {
		t.Errorf("the land does not name the command that gives the ended run's execution up: %s", res.Value)
	}
	rows := l.releaseRowsOf(l.nodeOf["A-1"])
	if len(rows) != 1 || str(rows[0].Data, "held_run") != run1.RunID || str(rows[0].Data, "held_run_state") != StateFailed {
		t.Fatalf("the release row of the held land: %+v", rows)
	}

	code, out, errOut := l.td.do("cancel", run1.RunID, "--state", l.w.state, "--socket", l.host.SocketPath)
	if !strings.Contains(out, "1 execution(s) of run "+run1.RunID+" given up") {
		t.Fatalf("the cancel of the ended run: exit %d\n%s%s", code, out, errOut)
	}
	run3 := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	if v := landValueOf(t, l.runToEnd(run3.RunID)); v.State != "landed" {
		t.Fatalf("the land after the cancel: %+v", v)
	}
}

// TestLandHeadStartWarnsOfALiveRunOfThePlan (item 2): a land-head start while another run of the same plan has not ended warns and
// records it: the preflight fact plan_live_run names the run, printed and kept in the run's spec, and the start goes on (a land-head
// release claims the plan's coordinator epoch, so that run would stop epoch_lost at its next fenced write). A plan with no live run has
// no such fact.
func TestLandHeadStartWarnsOfALiveRunOfThePlan(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	author := l.authorThread("claude-fake-sonnet", "high")
	quiet := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", author), "--handoff", l.file)
	for _, f := range quiet.Preflight {
		if f.Name == "plan_live_run" {
			t.Fatalf("a start with no run of the plan warns: %+v", f)
		}
	}
	l.killAt = l.writes + 1
	if _, killed := l.process(quiet.RunID, false); !killed {
		t.Fatal("the first land was not killed")
	}
	l.killAt = 0
	second := l.start("start", "land-head", "--args", landArgs(l, "A-2", "outside-a1", author), "--handoff", l.file)
	var fact *Fact
	for i, f := range second.Preflight {
		if f.Name == "plan_live_run" {
			fact = &second.Preflight[i]
		}
	}
	if fact == nil || fact.Level != LevelWarn || !strings.Contains(fact.Reading, quiet.RunID) {
		t.Fatalf("the start of a land while run %s of the plan has not ended: %+v", quiet.RunID, second.Preflight)
	}
}

// TestLandHeadLeavesTheAuthorPairOfAnUnprovenSetEmpty (item 7): a land one of whose named authors was not read back records no author
// pair on the node's execution (the observed pair of the others would stand for authors the run does not know), with the warning
// land_author_pair_unrecorded on the release row; the review is independence_unproven as before.
func TestLandHeadLeavesTheAuthorPairOfAnUnprovenSetEmpty(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	l.outsideHead("A-1", "outside-a1")
	authors := []string{l.authorThread("claude-fake-sonnet", "high"), "0190a000-0000-7000-8000-ffffffffffff"}
	spec := l.start("start", "land-head", "--args", landArgs(l, "A-1", "outside-a1", authors...), "--handoff", l.file)
	if v := landValueOf(t, l.runToEnd(spec.RunID)); v.State != "escalated" || v.Reason != "independence_unproven" {
		t.Fatalf("the land of an unproven author set: %+v", v)
	}
	var model, effort string
	if err := l.w.st.DB.QueryRow("SELECT COALESCE(author_model, ''), COALESCE(author_effort, '') FROM dag_agent_executions WHERE node_id = ?", l.nodeOf["A-1"]).Scan(&model, &effort); err != nil || model != "" || effort != "" {
		t.Fatalf("the execution of an unproven author set records the pair %q %q (%v), want none", model, effort, err)
	}
	rows := l.releaseRowsOf(l.nodeOf["A-1"])
	if len(rows) != 1 || !strings.Contains(strings.Join(rowWarningsOf(rows[0].Data), " | "), "land_author_pair_unrecorded") {
		t.Fatalf("the release row: %+v", rows)
	}
}

// TestAcceptOfANodeAnotherRunHolds (item 6, CRW-1468): the acceptance a run asks for a node whose current execution another run holds is
// refused by the relay - the review it rests on has to be a call of the run that holds the execution (reviewer_missing) - and writes
// nothing: no acceptance, and the execution stays the other run's. A verified unit check of the head that the asking run recorded for
// that execution (a crw.unitCheck row of role unit-check in its journal) is no such call either: the refusal stays reviewer_missing.
func TestAcceptOfANodeAnotherRunHolds(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	node := l.nodeOf["A-1"]
	r1 := l.buildLane("run-holder001", l.handoffBytes(nil), LaneOptions{})
	rel, err := r1.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r1.RunID, RequestID: "req-held"})
	if err != nil || !rel.Released {
		t.Fatalf("the release to the first run: %+v %v", rel, err)
	}
	// the work of the node, in the worktree the first run's execution names
	base := l.git(l.repo, "rev-parse", "integration")
	dir := filepath.Join(t.TempDir(), "held")
	l.git(l.repo, "worktree", "add", "-q", "-b", "held-a1", dir, "integration")
	if err := os.WriteFile(filepath.Join(dir, "node-A-1.txt"), []byte("the work of A-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.git(dir, "add", "-A")
	l.git(dir, "commit", "-q", "-m", "A-1")
	head := l.git(dir, "rev-parse", "HEAD")
	handle := r1.worktreeHandle(node)
	if err := os.MkdirAll(filepath.Dir(handle), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, handle); err != nil {
		t.Fatal(err)
	}
	record, err := l.sealed(ctx, l.repo, head, base)
	if err != nil {
		t.Fatal(err)
	}

	r2 := l.buildLane("run-other0002", l.handoffBytes(nil), LaneOptions{})
	// the asking run's own unit check of the head, recorded on the execution the first run holds
	check, _ := json.Marshal(reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictVerified, Findings: []reviewadapter.Finding{}, EngineFamily: "grok",
		Model: labCheckModel, Effort: "medium", GuideRev: ops.UnitCheckGuideRev})
	r2.Journal.Append(engine.Event{RunID: r2.RunID, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "x", "script_digest": "sha256:x", "args_digest": "sha256:x"}})
	r2.Journal.Append(engine.Event{RunID: r2.RunID, Kind: engine.KindCall, Op: ops.OpUnitCheck, Event: engine.EvCompleted, CallID: "uc-other", Data: map[string]any{
		"role": "unit-check", "node": node, "mode": ops.UnitCheckCommitted, "execution_id": rel.ExecutionID, "commit": head, "base": base, "state": "ok", "verdict": "verified",
		"receipt": map[string]any{"state": ops.ReceiptNoExecutor}, "result": string(check)}})
	if n := l.count("SELECT COUNT(*) FROM flow_calls WHERE run_id = ? AND kind = ? AND role = 'unit-check' AND execution_id = ? AND commit_sha = ?", r2.RunID, ops.OpUnitCheck, rel.ExecutionID, head); n != 1 {
		t.Fatalf("the asking run's unit check of the head is not in flow_calls: %d rows", n)
	}
	got, err := r2.Accept(ctx, ops.AcceptRequest{Plan: l.plan, Node: node, Head: head, Base: base, Verification: record, Premerge: json.RawMessage(`{}`), RequestID: "req-other"})
	if err != nil || got.Accepted || got.Reason != dagsched.ReviewerMissing {
		t.Fatalf("the acceptance another run asks for the first run's execution: %+v %v", got, err)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_acceptances"); n != 0 {
		t.Errorf("%d acceptances after the refusal", n)
	}
	cur, found, err := dagsched.CurrentExecution(ctx, l.w.st.Q(ctx), l.plan, node)
	if err != nil || !found || cur.Kind != dagsched.ExecutionAgent || cur.Agent.RunID != r1.RunID || cur.Agent.ExecutionID != rel.ExecutionID {
		t.Errorf("the node's current execution after the refusal: %+v %v %v", cur, found, err)
	}
}

// TestAnswerAgainWithOtherPartsIsAlreadyApplied (item 8): a decision applied once is replayed only by the same application. Applying it
// again with other parts for the questions of its ask (--answer-for), or with none after parts, is the input refusal already_applied,
// naming the parts the row holds and the parts given; the same parts again replay, and nothing is appended.
func TestAnswerAgainWithOtherPartsIsAlreadyApplied(t *testing.T) {
	const d1, d2 = "1111111111111111111111111111111111111111111111111111111111111111", "2222222222222222222222222222222222222222222222222222222222222222"
	setup := func(t *testing.T) (*testDeps, *asking) {
		w := newWorld(t)
		r := w.newRun("run-q", "thread-owner")
		r.phase("design")
		r.flow(engine.EvEscalation, map[string]any{"reason": "design_question", "class": "decision", "node": "n1", "escalation_id": "esc_one", "ask": "design",
			"detail": "[authority:policy] q one | [authority:environment] q two", "evidence": map[string]any{"questions": []any{d1, d2}}})
		r.finish(engine.RunCompleted, "")
		r.close()
		return w.deps(), &asking{run: r, q: "esc_one", rec: RunRecord{RunID: "run-q", Owner: "thread-owner", PlanID: "plan-1", PlanRevision: 3}}
	}
	attempt0 := func(b *Binding, _ *decisions.Record, ans *decisions.Answer) {
		b.Attempt = 0
		ans.Option, ans.Text = "", "No flag in 1.0. The shared runner is fine."
	}
	parts := []string{"--answer-for", d1 + "=No flag in 1.0.", "--answer-for", d2 + "=The shared runner is fine."}

	// first without parts, then with them
	td, a := setup(t)
	id := a.answered(t, "ud-whole", attempt0)
	if code, reply, errOut := a.answer(td, id, "--no-resume"); code != 0 || !reply.Applied {
		t.Fatalf("the first application: exit %d %+v %s", code, reply, errOut)
	}
	code, reply, errOut := a.answer(td, id, append([]string{"--no-resume"}, parts...)...)
	if code == 0 || reply.Applied || reply.Refusal == nil || reply.Refusal.Code != RefusedApplied || !strings.Contains(reply.Refusal.Detail, "No flag in 1.0.") {
		t.Errorf("the same decision again with parts: exit %d %+v %s", code, reply, errOut)
	}
	if n := len(a.appliedRows(t)); n != 1 {
		t.Errorf("%d decision_applied rows, want 1", n)
	}

	// first with parts, then without, then the same parts again
	td2, a2 := setup(t)
	id2 := a2.answered(t, "ud-parts", attempt0)
	if code, reply, errOut := a2.answer(td2, id2, append([]string{"--no-resume"}, parts...)...); code != 0 || !reply.Applied {
		t.Fatalf("the application with parts: exit %d %+v %s", code, reply, errOut)
	}
	if code, reply, errOut := a2.answer(td2, id2, "--no-resume"); code == 0 || reply.Refusal == nil || reply.Refusal.Code != RefusedApplied || !strings.Contains(reply.Refusal.Detail, d1) {
		t.Errorf("the same decision again without parts: exit %d %+v %s", code, reply, errOut)
	}
	if code, reply, errOut := a2.answer(td2, id2, append([]string{"--no-resume"}, parts...)...); code != 0 || !reply.Replayed {
		t.Errorf("the same parts again: exit %d %+v %s", code, reply, errOut)
	}
	if n := len(a2.appliedRows(t)); n != 1 {
		t.Errorf("%d decision_applied rows, want 1", n)
	}
}
