package ops

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The record of the QA evidence of a final gate (crw.qaReceipt) decides nothing a model says by itself: what it takes for a
// receipt to be a PASS, and each way a receipt is INCONCLUSIVE.

func qaCapture(n int) json.RawMessage {
	caps := []map[string]any{}
	for i := 0; i < n; i++ {
		caps = append(caps, map[string]any{"path": "/qa/shot.png", "kind": "screenshot", "shows": "the screen"})
	}
	b, _ := json.Marshal(map[string]any{"captures": caps, "matrix": []string{"home"}, "teardown": []string{"no resources spawned"}, "summary": "drove it"})
	return b
}

func qaPassOut(pass, verdict string, findings ...map[string]any) json.RawMessage {
	if findings == nil {
		findings = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"pass": pass, "verdict": verdict, "findings": findings, "summary": "read"})
	return b
}

func qaReceiptOut(verdict string, findings ...map[string]any) json.RawMessage {
	if findings == nil {
		findings = []map[string]any{}
	}
	b, _ := json.Marshal(map[string]any{"verdict": verdict, "findings": findings, "summary": "receipt"})
	return b
}

func qaCall(id, status, role, thread string, out json.RawMessage) CallRef {
	c := call(id, status, "claude-fake-haiku", nil)
	c.Output = out
	c.Meta.Role, c.Meta.ThreadID = role, thread
	return c
}

func qaFinding(sev string) map[string]any {
	return map[string]any{"ref": "q1", "severity": sev, "finding": "something", "evidence": []string{"shot.png"}}
}

func qaOp(t *testing.T, gate GateRef, capture CallRef, passes []CallRef, receipt *CallRef) QAResult {
	t.Helper()
	return qaOpAfter(t, gate, capture, passes, receipt, capture.CallID)
}

// qaOpAfter is qaOp with a journal in which the receipt call continues the call resumeOf.
func qaOpAfter(t *testing.T, gate GateRef, capture CallRef, passes []CallRef, receipt *CallRef, resumeOf string) QAResult {
	t.Helper()
	w := newWorld(t)
	var rec any
	if receipt != nil {
		rec = *receipt
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: receipt.CallID, Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "resume_of": resumeOf}})
	}
	if passes == nil {
		passes = []CallRef{}
	}
	var out QAResult
	_ = json.Unmarshal(w.must("qaReceipt", "M", gate, capture, passes, rec), &out)
	return out
}

// A C2 receipt is the self-review on the thread of the QA call, over captures that exist.
func TestQAReceiptC2SelfReview(t *testing.T) {
	gate := GateRef{Head: headOne, Grade: "C2"}
	capture := qaCall("c_cap", "completed", "executor", "thr-qa", qaCapture(2))
	review := qaCall("c_rev", "completed", "executor", "thr-qa", qaReceiptOut("PASS"))
	if got := qaOp(t, gate, capture, nil, &review); got.State != QAPass || got.Captures != 2 || got.Head != headOne || got.Synthesis.Call != "c_rev" || got.Capture.Thread != "thr-qa" {
		t.Errorf("%+v", got)
	}
	for _, c := range []struct {
		name    string
		capture CallRef
		passes  []CallRef
		receipt *CallRef
		reason  string
	}{
		{"the QA call did not complete", qaCall("c_cap", "held", "executor", "thr-qa", nil), nil, nil, "capture_held"},
		{"an answer that is no QA_CAPTURE", qaCall("c_cap", "completed", "executor", "thr-qa", json.RawMessage(`{"captures":"none"}`)), nil, &review, "capture_invalid"},
		{"no capture is listed", qaCall("c_cap", "completed", "executor", "thr-qa", qaCapture(0)), nil, &review, "no_captures"},
		{"no self-review", capture, nil, nil, "synthesis_missing"},
		{"the self-review did not complete", capture, nil, &CallRef{CallID: "c_rev", Status: "null_model", Meta: review.Meta}, "synthesis_null_model"},
		{"the self-review is no QA_RECEIPT", capture, nil, ptr(qaCall("c_rev", "completed", "executor", "thr-qa", json.RawMessage(`{"verdict":"MAYBE"}`))), "synthesis_invalid"},
		{"a PASS over a P1 finding", capture, nil, ptr(qaCall("c_rev", "completed", "executor", "thr-qa", qaReceiptOut("PASS", qaFinding("P1")))), "synthesis_invalid"},
		{"passes at C2", capture, []CallRef{qaCall("c_a", "completed", "explorer", "thr-a", qaPassOut("A", "PASS"))}, &review, "passes_not_asked"},
	} {
		got := qaOp(t, gate, c.capture, c.passes, c.receipt)
		if got.State != QAInconclusive || got.Reason != c.reason {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
	// the receipt is the next turn of the QA call's thread: a call that continues another call is not (after a resume the
	// continuation runs on a fork of the thread, so the journal's resume_of is the test, not the thread id)
	other := qaCall("c_rev", "completed", "executor", "thr-qa", qaReceiptOut("PASS"))
	if got := qaOpAfter(t, gate, capture, nil, &other, "c_somebody_else"); got.State != QAInconclusive || got.Reason != "synthesis_not_continuation" {
		t.Errorf("a self-review that continues another call: %+v", got)
	}
	forked := qaCall("c_rev", "completed", "executor", "thr-fork-of-qa", qaReceiptOut("PASS"))
	if got := qaOp(t, gate, capture, nil, &forked); got.State != QAPass {
		t.Errorf("a self-review on a fork of the QA thread: %+v", got)
	}
	// a P2 finding does not turn a PASS into a failure, and REVISE and FAIL are what they say
	if got := qaOp(t, gate, capture, nil, ptr(qaCall("c_rev", "completed", "executor", "thr-qa", qaReceiptOut("PASS", qaFinding("P2"))))); got.State != QAPass || len(got.Findings) != 1 {
		t.Errorf("%+v", got)
	}
	for _, v := range []string{QARevise, QAFail} {
		if got := qaOp(t, gate, capture, nil, ptr(qaCall("c_rev", "completed", "executor", "thr-qa", qaReceiptOut(v)))); got.State != v {
			t.Errorf("%s: %+v", v, got)
		}
	}
}

func ptr[T any](v T) *T { return &v }

// At C3 and above the receipt rests on two explorer passes A and B, completed, and the synthesis on the QA call's thread; the
// receipt is as bad as the worst of the three verdicts, so a synthesis cannot lift a pass that failed.
func TestQAReceiptC3PassesAndWorstVerdict(t *testing.T) {
	gate := GateRef{Head: headOne, Grade: "C3", Passes: true}
	capture := qaCall("c_cap", "completed", "executor", "thr-qa", qaCapture(1))
	passA := qaCall("c_a", "completed", "explorer", "thr-a", qaPassOut("A", "PASS"))
	passB := qaCall("c_b", "completed", "explorer", "thr-b", qaPassOut("B", "PASS"))
	syn := qaCall("c_syn", "completed", "executor", "thr-qa", qaReceiptOut("PASS"))
	got := qaOp(t, gate, capture, []CallRef{passA, passB}, &syn)
	if got.State != QAPass || len(got.Passes) != 2 || got.Passes[1].Call.Thread != "thr-b" || got.Synthesis.Thread != "thr-qa" {
		t.Fatalf("%+v", got)
	}
	failB := qaCall("c_b", "completed", "explorer", "thr-b", qaPassOut("B", "FAIL", qaFinding("P0")))
	if got := qaOp(t, gate, capture, []CallRef{passA, failB}, &syn); got.State != QAFail {
		t.Errorf("a failed pass under a PASS synthesis: %+v", got)
	}
	reviseA := qaCall("c_a", "completed", "explorer", "thr-a", qaPassOut("A", "REVISE", qaFinding("P2")))
	if got := qaOp(t, gate, capture, []CallRef{reviseA, passB}, ptr(qaCall("c_syn", "completed", "executor", "thr-qa", qaReceiptOut("PASS")))); got.State != QARevise {
		t.Errorf("a revise pass under a PASS synthesis: %+v", got)
	}
	for _, c := range []struct {
		name    string
		passes  []CallRef
		receipt *CallRef
		reason  string
	}{
		{"only one pass", []CallRef{passA}, &syn, "passes_missing"},
		{"pass B is held", []CallRef{passA, qaCall("c_b", "held", "explorer", "thr-b", nil)}, nil, "pass_B_held"},
		{"pass A is refused", []CallRef{qaCall("c_a", "refused", "explorer", "", nil), passB}, nil, "pass_A_refused"},
		{"a pass that is not an explorer call", []CallRef{passA, qaCall("c_b", "completed", "reviewer", "thr-b", qaPassOut("B", "PASS"))}, &syn, "pass_B_not_explorer"},
		{"a pass that answers for the other", []CallRef{qaCall("c_a", "completed", "explorer", "thr-a", qaPassOut("B", "PASS")), passB}, &syn, "pass_A_invalid"},
		{"no synthesis", []CallRef{passA, passB}, nil, "synthesis_missing"},
		{"the synthesis is held", []CallRef{passA, passB}, ptr(qaCall("c_syn", "held", "executor", "thr-qa", nil)), "synthesis_held"},
	} {
		got := qaOp(t, gate, capture, c.passes, c.receipt)
		if got.State != QAInconclusive || got.Reason != c.reason {
			t.Errorf("%s: %+v", c.name, got)
		}
	}
}

// The grade decides whether the passes are asked for; a grade nobody gave is the stricter reading.
func TestGradeAtLeast(t *testing.T) {
	for grade, want := range map[string]bool{"C2": false, "c2": false, "C3": true, "C4": true, "": true, "high": true, "C1": false} {
		if got := GradeAtLeast(grade, 3); got != want {
			t.Errorf("GradeAtLeast(%q, 3) = %v", grade, got)
		}
	}
}

// The final gate asks the verifier for the commands of the plan's required set, one by one, in a worktree of the repository (an
// integrated head is no node's worktree), over the base of the range; the row it leaves has no node and names the milestone.
func TestMilestoneVerifyRecordsTheGateEvidence(t *testing.T) {
	w := newCloseWorld(t, Milestone{ID: "M1", Plan: "plan-1", Title: "m", CriteriaDigest: "crit", Criteria: []string{"c"}, RequiredCommands: []string{"make lint", CILocalCommand}})
	got := w.run(`
const g = await crw.finalGate('M1')
const out = []
for (const command of g.need_tests) out.push(await crw.verify('', g.head, { milestone: 'M1', command, base: 'base-tip' }))
const again = await crw.finalGate('M1')
return { need: g.need_tests, ran: out.map((v) => v.command), state: again.state, evidence: again.evidence.map((e) => e.command + ':' + e.state) }
`)
	if need, _ := got["need"].([]any); len(need) != 2 || need[0] != CILocalCommand || need[1] != "make lint" {
		t.Fatalf("the required set, sorted: %v", got["need"])
	}
	reqs := w.verifier.requests()
	if len(reqs) != 2 {
		t.Fatalf("verifications %+v", reqs)
	}
	for _, r := range reqs {
		if r.Node != "" || r.Worktree != "/repo" || r.Base != "base-tip" || r.Head != headOne {
			t.Errorf("request %+v", r)
		}
	}
	if got["state"] != "pass" {
		t.Errorf("after both commands: %v", got)
	}
	// a verification of a milestone that names no milestone is no evidence of any
	w2 := newWorld(t)
	if _, err := w2.op("verify", "", headOne); err == nil || !strings.Contains(err.Error(), "milestone") {
		t.Errorf("a verification with no node and no milestone: %v", err)
	}
}

// reviewer-final is a review of another family than every author of the milestone, as a node's reviewer is: a model of the
// family of an executor or of the architect is refused, a model of no known author is independence_unproven.
func TestReviewerFinalIndependenceAndStates(t *testing.T) {
	w := newWorld(t)
	w.must("milestone", "plan-1", "M")
	gate := map[string]any{"head": headOne, "range": headOne}
	final := func(c CallRef) FinalResult {
		var r FinalResult
		_ = json.Unmarshal(w.must("reviewerFinal", "M", gate, c, map[string]any{}), &r)
		return r
	}
	finalOut, _ := json.Marshal(map[string]any{"verdict": "verified", "criteria": []any{}, "defects": []any{}, "alignment": map[string]any{"verdict": "ALIGNED", "drift": []any{}}, "summary": "ok"})
	if r := final(call("c_f", "completed", "gpt-fake-sol", nil)); r.State != StateNullModel || r.Reason != "verdict_invalid" {
		t.Errorf("an answer that is no VERDICT_FINAL: %+v", r)
	}
	ok := call("c_f", "completed", "gpt-fake-sol", nil)
	ok.Output = finalOut
	if r := final(ok); r.State != StateRefused || r.Reason != "independence_unproven" {
		t.Errorf("no author is known: %+v", r)
	}
	for _, model := range []string{"claude-fake-opus", "claude-fake-haiku"} {
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: "c_" + model, Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": map[string]string{"claude-fake-opus": "architect", "claude-fake-haiku": "executor"}[model], "node": "A", "model": model}})
	}
	if r := final(ok); r.State != StateVerdict || r.Alignment != "ALIGNED" {
		t.Errorf("a reviewer of another family: %+v", r)
	}
	sameFamily := call("c_f", "completed", "claude-fake-sonnet", nil)
	sameFamily.Output = finalOut
	if r := final(sameFamily); r.State != StateRefused || r.Reason != engine.ReasonIndependenceRefused {
		t.Errorf("a reviewer of the family of the authors: %+v", r)
	}
	for status, state := range map[string]string{"held": StateHeld, "unknown": StateUnknown, "refused": StateRefused, "null_model": StateNullModel, "superseded": StateSuperseded} {
		if r := final(call("c_f", status, "gpt-fake-sol", nil)); r.State != state {
			t.Errorf("%s: %+v", status, r)
		}
	}
	misaligned, _ := json.Marshal(map[string]any{"verdict": "needs_changes", "criteria": []any{}, "defects": []any{}, "alignment": map[string]any{"verdict": "MISALIGNED", "drift": []any{}}, "summary": "no"})
	bad := call("c_f", "completed", "gpt-fake-sol", nil)
	bad.Output = misaligned
	if r := final(bad); r.State != StateNullModel {
		t.Errorf("MISALIGNED with no drift named: %+v", r)
	}
}
