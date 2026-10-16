package ops

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
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
	return qaOpSeeded(t, gate, capture, passes, receipt, resumeOf, qaSeed{})
}

// qaSeed is what the journal holds of the capture call besides its answer: the directory it ran in and the source identity it ended
// in. The zero value is a call that ran in a clean checkout of the gate's head.
type qaSeed struct {
	// Root is the scratch worktree of the capture call; "" is the default one, "none" a call with no allocation.
	Root string
	// Source replaces the identity the capture ended in; NoSource records none, SourceError records the error of reading it.
	Source      *source.Identity
	NoSource    bool
	SourceError string
	// SynthesisSource is the identity the receipt call ended in; nil records none.
	SynthesisSource *source.Identity
}

const qaRoot = "/state/flow/run-1/calls/c_cap/wt"

func qaSourceMap(id source.Identity) map[string]any {
	b, _ := json.Marshal(id)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func qaOpSeeded(t *testing.T, gate GateRef, capture CallRef, passes []CallRef, receipt *CallRef, resumeOf string, seed qaSeed) QAResult {
	t.Helper()
	w := newWorld(t)
	seedCapture(w, gate, capture, seed)
	var rec any
	if receipt != nil {
		rec = *receipt
		w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: receipt.CallID, Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "resume_of": resumeOf}})
		if seed.SynthesisSource != nil {
			w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: receipt.CallID, Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"qa_source": qaSourceMap(*seed.SynthesisSource)}})
		}
	}
	if passes == nil {
		passes = []CallRef{}
	}
	var out QAResult
	_ = json.Unmarshal(w.must("qaReceipt", "M", gate, capture, passes, rec), &out)
	return out
}

// seedCapture writes the prepared and completed rows of the capture call the way the engine does for a QA call.
func seedCapture(w *world, gate GateRef, capture CallRef, seed qaSeed) {
	prep := map[string]any{"role": "executor", "qa": true}
	if seed.Root != "none" {
		root := seed.Root
		if root == "" {
			root = qaRoot
		}
		prep["allocation"] = map[string]any{"kind": "call", "path": root, "start_commit": gate.Head}
	}
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: capture.CallID, Op: "agent", Event: engine.EvPrepared, Data: prep})
	done := map[string]any{}
	switch {
	case seed.SourceError != "":
		done["qa_source_error"] = seed.SourceError
	case seed.NoSource:
	case seed.Source != nil:
		done["qa_source"] = qaSourceMap(*seed.Source)
	default:
		done["qa_source"] = qaSourceMap(source.CommitIdentity(gate.Head, nil))
	}
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: capture.CallID, Op: "agent", Event: engine.EvCompleted, Data: done})
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

// The receipt stands on the tree the capture call exercised, not on the head it was asked to exercise (CRW-1206 correction, d2): a
// capture whose scratch worktree ended on another commit, with uncommitted work, with a source nobody could read or with none
// recorded is INCONCLUSIVE - never a PASS over code the gate did not pin - and the receipt row keeps the identity it rests on.
func TestQAReceiptRestsOnTheSourceTheCaptureExercised(t *testing.T) {
	gate := GateRef{Head: headOne, Grade: "C2"}
	capture := qaCall("c_cap", "completed", "executor", "thr-qa", qaCapture(1))
	review := qaCall("c_rev", "completed", "executor", "thr-qa", qaReceiptOut("PASS"))
	clean := source.CommitIdentity(headOne, nil)
	got := qaOpSeeded(t, gate, capture, nil, &review, capture.CallID, qaSeed{Source: &clean})
	if got.State != QAPass || got.Source == nil || got.Source.CommitSha != headOne || got.Source.Dirty {
		t.Fatalf("a capture over the clean head: %+v", got)
	}
	dirty := clean
	dirty.Dirty, dirty.TreeHash = true, "hash-of-the-edit"
	elsewhere := source.CommitIdentity(headTwo, nil)
	unavailable := source.Identity{Kind: source.KindUnavailable}
	for name, c := range map[string]struct {
		seed   qaSeed
		reason string
	}{
		"a tracked file was edited":      {qaSeed{Source: &dirty}, "capture_source_modified"},
		"another commit":                 {qaSeed{Source: &elsewhere}, "capture_source_modified"},
		"git could not answer":           {qaSeed{Source: &unavailable}, "capture_source_unknown"},
		"the engine could not read it":   {qaSeed{SourceError: "git status failed"}, "capture_source_unknown"},
		"the backend records no source":  {qaSeed{NoSource: true}, "capture_source_unknown"},
		"the synthesis turn edited code": {qaSeed{SynthesisSource: &dirty}, "synthesis_source_modified"},
	} {
		got := qaOpSeeded(t, gate, capture, nil, &review, capture.CallID, c.seed)
		if got.State != QAInconclusive || got.Reason != c.reason {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// the judgement of the gate: a receipt whose state is not PASS is not evidence, and the PASS row carries its identity
	w := newCloseWorld(t, web("C2"))
	w.fake.SourceHook = func(string, string) (source.Identity, error) { return dirty, nil }
	closed := w.close()
	if closed["state"] != "stop" || closed["source"] != "qa" || closed["reason"] != GateQAInconclusive {
		t.Fatalf("a QA over an edited tree closes the milestone: %v", closed)
	}
	if n := w.count("final"); n != 0 {
		t.Errorf("reviewer-final ran over a QA that exercised another tree")
	}
	if rows := w.opRows(OpQAReceipt); len(rows) != 1 || dataStr(rows[0], "reason") != "capture_source_modified" || dataStr(rows[0], "state") != QAInconclusive {
		t.Errorf("receipt rows %+v", rows)
	}
}

// The artifacts of a capture are files of the capture call's own scratch worktree, named relative to it (the crw-qa skill puts them
// under .crw/evidence/<session>/qa/...), and every other call reads a checkout of its own: the run hands the passes and the
// self-review the paths resolved against the directory the capture ran in (CRW-1206 correction, d3).
func TestQACaptureResolvesRelativeArtifactPaths(t *testing.T) {
	gate := GateRef{Head: headOne, Grade: "C3", Passes: true}
	relative := json.RawMessage(`{"captures":[{"path":".crw/evidence/s1/qa/home/shot.png","kind":"screenshot","shows":"home"},{"path":"/var/qa/abs.png","kind":"screenshot","shows":"abs"}],"matrix":["home"],"teardown":["none"],"summary":"ok"}`)
	view := func(out json.RawMessage, seed qaSeed) QACaptureView {
		t.Helper()
		w := newWorld(t)
		capture := qaCall("c_cap", "completed", "executor", "thr-qa", out)
		seedCapture(w, gate, capture, seed)
		var v QACaptureView
		if err := json.Unmarshal(w.must("qaCapture", "M", gate, capture), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	v := view(relative, qaSeed{})
	if !v.OK || v.Root != qaRoot || len(v.Captures) != 2 {
		t.Fatalf("%+v", v)
	}
	if v.Captures[0].Abs != qaRoot+"/.crw/evidence/s1/qa/home/shot.png" || v.Captures[0].Path != ".crw/evidence/s1/qa/home/shot.png" || v.Captures[1].Abs != "/var/qa/abs.png" {
		t.Errorf("captures %+v", v.Captures)
	}
	if v.Source == nil || v.Source.CommitSha != headOne || v.Source.Dirty {
		t.Errorf("source %+v", v.Source)
	}
	escaping := json.RawMessage(`{"captures":[{"path":"../../other/call/shot.png","kind":"screenshot","shows":"x"}],"matrix":[],"teardown":[],"summary":"ok"}`)
	for name, c := range map[string]struct {
		out    json.RawMessage
		seed   qaSeed
		reason string
	}{
		"a path out of the capture's worktree": {escaping, qaSeed{}, "capture_path_outside"},
		"a relative path and no worktree":      {relative, qaSeed{Root: "none"}, "capture_root_unknown"},
		"a capture over an edited tree":        {relative, qaSeed{Source: &source.Identity{Kind: source.KindResolved, CommitSha: headOne, Dirty: true, TreeHash: "h"}}, "capture_source_modified"},
		"no capture is listed":                 {qaCapture(0), qaSeed{}, "no_captures"},
		"an answer that is no QA_CAPTURE":      {json.RawMessage(`{"captures":"none"}`), qaSeed{}, "capture_invalid"},
	} {
		if v := view(c.out, c.seed); v.OK || v.Reason != c.reason {
			t.Errorf("%s: %+v", name, v)
		}
	}
	// absolute paths need no worktree
	abs := json.RawMessage(`{"captures":[{"path":"/var/qa/abs.png","kind":"screenshot","shows":"abs"}],"matrix":[],"teardown":[],"summary":"ok"}`)
	if v := view(abs, qaSeed{Root: "none"}); !v.OK || v.Captures[0].Abs != "/var/qa/abs.png" {
		t.Errorf("an absolute path: %+v", v)
	}
	// the receipt reads the same rules: a relative path nobody can resolve is not evidence
	capture := qaCall("c_cap", "completed", "executor", "thr-qa", relative)
	syn := qaCall("c_syn", "completed", "executor", "thr-qa", qaReceiptOut("PASS"))
	passA := qaCall("c_a", "completed", "explorer", "thr-a", qaPassOut("A", "PASS"))
	passB := qaCall("c_b", "completed", "explorer", "thr-b", qaPassOut("B", "PASS"))
	if got := qaOpSeeded(t, gate, capture, []CallRef{passA, passB}, &syn, "c_cap", qaSeed{Root: "none"}); got.State != QAInconclusive || got.Reason != "capture_root_unknown" {
		t.Errorf("%+v", got)
	}
}
