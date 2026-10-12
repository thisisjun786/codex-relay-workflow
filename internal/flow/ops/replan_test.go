package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The share of the plan's slices that changed is a reason to re-plan above a quarter of the nodes; exactly a quarter is not. A node
// the plan added or dropped since the revision is a change.
func TestSliceChangeArithmetic(t *testing.T) {
	for _, c := range []struct {
		changed, total int
		due            bool
	}{{0, 4, false}, {1, 4, false}, {2, 4, true}, {1, 3, true}, {1, 5, false}, {2, 5, true}, {1, 1, true}, {0, 0, false}, {25, 100, false}, {26, 100, true}} {
		sc := SliceChange{Changed: make([]string, c.changed), Total: c.total}
		if sc.Due() != c.due {
			t.Errorf("%d of %d changed: due %v, want %v", c.changed, c.total, sc.Due(), c.due)
		}
	}
	then := map[string]string{"A": "a1", "B": "b1", "C": "c1", "D": "d1"}
	now := map[string]string{"A": "a1", "B": "b2", "C": "c1", "E": "e1"}
	sc := sliceChange(then, now)
	if fmt.Sprint(sc.Changed) != "[B D E]" || sc.Total != 5 || sc.Percent != 60 || !sc.Due() {
		t.Errorf("%+v", sc)
	}
	same := sliceChange(then, then)
	if len(same.Changed) != 0 || same.Total != 4 || same.Due() {
		t.Errorf("%+v", same)
	}
}

// A revision is named once: the revisions a resume superseded keep their ids, the ledger that stands is the newest row that was not
// superseded, and the next revision is named after every row there is.
func TestRevisionNamesAreUsedOnce(t *testing.T) {
	w := newWorld(t)
	w.ledgered(true, designOf("A"))
	approve := func(d Design, round int) json.RawMessage {
		design, _ := json.Marshal(d)
		cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
		w.must("designAudit", "M", round, cand, call("c_arch2", "completed", "claude-x", nil),
			call(fmt.Sprintf("c_aud%d", round), "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
		return design
	}
	ledger := func(design json.RawMessage) (Ledger, error) {
		out, err := w.op("designLedger", "M", map[string]any{"design": design, "answers": json.RawMessage(`[]`), "architect": call("c_arch2", "completed", "claude-x", nil), "signal": 2})
		var led Ledger
		_ = json.Unmarshal(out, &led)
		return led, err
	}
	r := rows(w.j.Events())
	if cur, ok := currentLedger(r, "M"); !ok || dataStr(cur, "revision") != "r0" || nextRevision(r, "M") != "r1" {
		t.Fatalf("the ledger: %v %v, next %s", cur.Data, ok, nextRevision(r, "M"))
	}
	led, err := ledger(approve(designOf("A", "B"), 2))
	if err != nil || led.Revision != "r1" {
		t.Fatalf("revision after r0: %+v %v", led, err)
	}
	r = rows(w.j.Events())
	if cur, _ := currentLedger(r, "M"); dataStr(cur, "revision") != "r1" || dataInt(cur, "signal") != 2 {
		t.Errorf("the ledger that stands: %v", cur.Data)
	}
	// a resume ends the position of r1 superseded: r0 stands, r1 is a revision that was superseded, and the next one is r2
	r1, _ := ledgerOf(r, "M", "r1")
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: r1.CallID, Op: OpDesignLedger, Event: engine.EvSuperseded})
	r = rows(w.j.Events())
	if cur, _ := currentLedger(r, "M"); dataStr(cur, "revision") != "r0" || !revisionSuperseded(r, "M", "r1") || revisionSuperseded(r, "M", "r0") || nextRevision(r, "M") != "r2" {
		t.Errorf("after the supersede: stands %v, r1 superseded %v, next %s", cur.Data, revisionSuperseded(r, "M", "r1"), nextRevision(r, "M"))
	}
	// the audit that approved r1's DESIGN stands in for nothing: the revision that follows r0 now needs an audit made after r0, and the
	// audit of the DESIGN r1 had is one (it was approved after r0) - a different DESIGN has none until it is audited
	if _, err := ledger(json.RawMessage(mustJSON(designOf("A", "B", "C")))); err == nil {
		t.Errorf("a DESIGN nobody audited became a revision")
	}
	led, err = ledger(approve(designOf("A", "B", "C"), 3))
	if err != nil || led.Revision != "r2" {
		t.Fatalf("the revision after a superseded one: %+v %v", led, err)
	}
	if r = rows(w.j.Events()); len(ledgerRows(r, "M")) != 2 {
		t.Errorf("revisions that stand: %d", len(ledgerRows(r, "M")))
	}
}

func dataInt(ev engine.Event, key string) int { return intOf(ev.Data[key]) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// ---- the numbers of SP-ARCH-2 on a journal ----

// planOf is a milestone of nodes whose slice digests are the ones given.
func planOfNodes(slices map[string]string, order ...string) *Milestone {
	m := &Milestone{ID: "M", Plan: "plan-1", Criteria: []string{"c"}, CriteriaDigest: "cd"}
	for _, id := range order {
		m.Nodes = append(m.Nodes, PlanNode{ID: id, Issue: "CRW-" + id, SliceDigest: slices[id], CriteriaDigest: "cd-" + id, Criteria: []string{"c"}})
	}
	return m
}

// receipt writes the rows of an executor call that completed with a RECEIPT.
func (w *world) receipt(call, node, mismatch string) {
	w.t.Helper()
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: call, Op: "agent", Event: engine.EvPrepared, Data: map[string]any{"role": "executor", "node": node}})
	var pm any
	if mismatch != "" {
		pm = mismatch
	}
	result, _ := json.Marshal(map[string]any{"outcome": "ready_for_review", "files": []string{}, "verification": []any{}, "summary": "s", "gap": nil, "research_question": nil, "plan_mismatch": pm})
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: call, Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"result": string(result)}})
}

// signal binds the next signal of the milestone over the RECEIPT calls.
func (w *world) signal(k int, calls ...string) SignalBind {
	w.t.Helper()
	var members []Trigger
	for _, c := range calls {
		node := ""
		for _, ev := range rows(w.j.Events()).prepared() {
			if ev.CallID == c {
				node = str(ev.Data, "node")
			}
		}
		members = append(members, Trigger{Kind: "receipt", Call: c, Node: node, Digest: "digest-" + c})
	}
	var out SignalBind
	_ = json.Unmarshal(w.must("signalBind", "M", k, "", members), &out)
	return out
}

func (w *world) numbers(opts ReplanOpts) ReplanState {
	w.t.Helper()
	var st ReplanState
	_ = json.Unmarshal(w.must("replanSignals", "M", opts), &st)
	return st
}

func (w *world) release(node string) ReleaseOutcome {
	w.t.Helper()
	var rel ReleaseOutcome
	_ = json.Unmarshal(w.must("release", "plan-1", node, "M"), &rel)
	if !rel.Released {
		w.t.Fatalf("release of %s: %+v", node, rel)
	}
	return rel
}

func TestPlanMismatchOnceNoReplan(t *testing.T) {
	w := newWorld(t)
	w.ledgered(true, designOf("A", "B"))
	w.release("A")
	w.receipt("c_a1", "A", "the plan assumed more")
	w.receipt("c_a2", "A", "")
	w.signal(1, "c_a1", "c_a2")
	st := w.numbers(ReplanOpts{Phase: "signal", Signal: 1})
	if st.PlanMismatch != 1 || st.Due || len(st.Reasons) != 0 || st.Revision != "r0" {
		t.Errorf("%+v", st)
	}
}

// Two RECEIPTs that report a plan mismatch, of lanes released under the revision, are a reason whatever the alignment says; the same
// call taken in twice is one RECEIPT.
func TestPlanMismatchTwiceReplansEvenIfAligned(t *testing.T) {
	w := newWorld(t)
	w.ledgered(true, designOf("A", "B"))
	w.release("A")
	w.receipt("c_a1", "A", "the plan assumed more")
	w.receipt("c_a2", "A", "the plan assumed less")
	w.signal(1, "c_a1")
	if st := w.numbers(ReplanOpts{Phase: "signal", Signal: 1}); st.PlanMismatch != 1 || st.Due {
		t.Fatalf("after the first RECEIPT: %+v", st)
	}
	w.signal(2, "c_a2", "c_a1")
	st := w.numbers(ReplanOpts{Phase: "close", Signal: 2, Final: "ALIGNED", Reflection: "ALIGNED"})
	if st.PlanMismatch != 2 || !st.Due || len(st.Reasons) != 1 || st.Reasons[0] != ReasonPlanMismatch {
		t.Errorf("%+v", st)
	}
	// the close adds the alignments: a MISALIGNED reviewer-final and a MISALIGNED reflection are reasons of their own
	w2 := newWorld(t)
	w2.ledgered(true, designOf("A"))
	w2.release("A")
	w2.receipt("c_a1", "A", "")
	w2.signal(1, "c_a1")
	if st := w2.numbers(ReplanOpts{Phase: "close", Final: "MISALIGNED", Reflection: "MISALIGNED"}); !st.Due || len(st.Reasons) != 2 || st.Reasons[0] != ReasonFinalMisaligned || st.Reasons[1] != ReasonReflectionMisaligned {
		t.Errorf("%+v", st)
	}
	if st := w2.numbers(ReplanOpts{Phase: "close", Final: "NOT_ASSESSED", Reflection: "ALIGNED"}); st.Due {
		t.Errorf("an alignment that was not assessed is a reason: %+v", st)
	}
	if st := w2.numbers(ReplanOpts{Phase: "signal", Signal: 1, Final: "MISALIGNED", Reflection: "MISALIGNED"}); st.Due {
		t.Errorf("a signal of the run adds the alignments of the close: %+v", st)
	}
}

// A RECEIPT of a lane released under an older revision does not count in the number of the revision that stands.
func TestOldRevisionReceiptNotCounted(t *testing.T) {
	w := newWorld(t)
	w.ledgered(true, designOf("A", "B"))
	w.release("A")
	w.receipt("c_a1", "A", "the plan assumed more")
	w.receipt("c_a2", "A", "the plan assumed less")
	w.signal(1, "c_a1", "c_a2")
	if st := w.numbers(ReplanOpts{Phase: "signal", Signal: 1}); st.PlanMismatch != 2 || !st.Due {
		t.Fatalf("under r0: %+v", st)
	}
	// the re-plan made r1; B is released under it and reports one mismatch
	design, _ := json.Marshal(designOf("A", "B"))
	cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
	w.must("designAudit", "M", 1, cand, call("c_arch2", "completed", "claude-x", nil), call("c_aud2", "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
	w.must("designLedger", "M", map[string]any{"design": json.RawMessage(design), "answers": json.RawMessage(`[]`), "architect": call("c_arch2", "completed", "claude-x", nil), "signal": 1})
	if rel := w.release("B"); rel.Revision != "r1" {
		t.Fatalf("B was released under %s", rel.Revision)
	}
	w.receipt("c_b1", "B", "the plan assumed another thing")
	w.signal(2, "c_b1")
	st := w.numbers(ReplanOpts{Phase: "signal", Signal: 2})
	if st.Revision != "r1" || st.PlanMismatch != 1 || st.Due || len(st.Calls) != 1 || st.Calls[0] != "c_b1" {
		t.Errorf("under r1: %+v", st)
	}
}

// More than a quarter of the nodes changed their slice since the revision is a reason; exactly a quarter is not.
func TestSliceChangeOver25Replans(t *testing.T) {
	w := newWorld(t)
	then := map[string]string{"A": "a1", "B": "b1", "C": "c1", "D": "d1"}
	w.relay.plan = planOfNodes(then, "A", "B", "C", "D")
	w.ledgered(true, designOf("A", "B", "C", "D"))
	w.release("A")
	w.receipt("c_a1", "A", "")
	w.signal(1, "c_a1")
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b2", "C": "c2", "D": "d1"}, "A", "B", "C", "D")
	st := w.numbers(ReplanOpts{Phase: "signal", Signal: 1})
	if !st.Due || len(st.Reasons) != 1 || st.Reasons[0] != ReasonSliceChange || len(st.Slices.Changed) != 2 || st.Slices.Total != 4 || st.Slices.Percent != 50 {
		t.Errorf("%+v", st)
	}
}

func TestSliceChangeExactly25NoReplan(t *testing.T) {
	w := newWorld(t)
	then := map[string]string{"A": "a1", "B": "b1", "C": "c1", "D": "d1"}
	w.relay.plan = planOfNodes(then, "A", "B", "C", "D")
	w.ledgered(true, designOf("A", "B", "C", "D"))
	w.release("A")
	w.receipt("c_a1", "A", "")
	w.signal(1, "c_a1")
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b2", "C": "c1", "D": "d1"}, "A", "B", "C", "D")
	st := w.numbers(ReplanOpts{Phase: "signal", Signal: 1})
	if st.Due || len(st.Reasons) != 0 || len(st.Slices.Changed) != 1 || st.Slices.Total != 4 || st.Slices.Percent != 25 {
		t.Errorf("%+v", st)
	}
	// the plan gains a node: five nodes, two changed (the new one and B): 40%
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b2", "C": "c1", "D": "d1", "E": "e1"}, "A", "B", "C", "D", "E")
	if st := w.numbers(ReplanOpts{Phase: "signal", Signal: 1}); !st.Due || st.Slices.Total != 5 || len(st.Slices.Changed) != 2 {
		t.Errorf("%+v", st)
	}
}

// The reflection of the architect is a verdict only when the call completed with a valid REFLECTION; the handle of the architect moves
// to a completed reflection and to nothing else, and the row names the head the reflection judged.
func TestReflectionRunsAtIntegratedHead(t *testing.T) {
	w := newWorld(t)
	w.ledgered(true, designOf("A"))
	h := "abcdef1234567890abcdef1234567890abcdef12"
	gate := map[string]any{"head": h, "range": "base.." + h}
	cur := func() CurrentDesign {
		var cd CurrentDesign
		_ = json.Unmarshal(w.must("currentDesign", "M"), &cd)
		return cd
	}
	if cd := cur(); cd.Handle != "crwh1:run-1:c_arch" || cd.Revision != "r0" {
		t.Fatalf("the architect before the reflection: %+v", cd)
	}
	held := call("c_refl_held", "held", "claude-x", nil)
	held.Reason = "host_infra_serverOverloaded"
	var res ReflectionResult
	_ = json.Unmarshal(w.must("reflection", "M", gate, held), &res)
	if res.State != StateHeld || res.Alignment != "" || res.Head != h {
		t.Errorf("held: %+v", res)
	}
	if cd := cur(); cd.Handle != "crwh1:run-1:c_arch" {
		t.Errorf("a held reflection moved the handle: %+v", cd)
	}
	empty := call("c_refl_bad", "completed", "claude-x", map[string]any{"verdict": "MISALIGNED", "misaligned": []any{}})
	_ = json.Unmarshal(w.must("reflection", "M", gate, empty), &res)
	if res.State != StateNullModel || res.Alignment != "" {
		t.Errorf("MISALIGNED over no decision: %+v", res)
	}
	if cd := cur(); cd.Handle != "crwh1:run-1:c_arch" {
		t.Errorf("an invalid reflection moved the handle: %+v", cd)
	}
	good := call("c_refl", "completed", "claude-x", map[string]any{"verdict": "MISALIGNED", "misaligned": []any{map[string]any{"decision_id": "D1", "observed": "o", "affected_nodes": []string{"A"}, "evidence": []string{"f"}}}})
	_ = json.Unmarshal(w.must("reflection", "M", gate, good), &res)
	if res.State != StateVerdict || res.Alignment != "MISALIGNED" || len(res.Misaligned) != 1 || res.Head != h {
		t.Errorf("completed: %+v", res)
	}
	if cd := cur(); cd.Handle != "crwh1:run-1:c_refl" || cd.Call != "c_refl" {
		t.Errorf("the handle after the reflection: %+v", cd)
	}
}

// A signal row records the tip of the integration target as it read it when it was decided, the head a re-plan turn of that signal judges.
func TestReplanCwdIsCurrentTip(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b1"}, "A", "B")
	w.ledgered(true, designOf("A", "B"))
	w.release("A")
	w.receipt("c_a1", "A", "")
	w.receipt("c_a2", "A", "")
	t1, t2 := "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	w.relay.tip = t1
	if s := w.signal(1, "c_a1"); s.Tip != t1 || len(s.Unstarted) != 1 || s.Unstarted[0] != "B" || len(s.Started) != 1 || s.Started[0] != "A" {
		t.Errorf("signal 1: %+v", s)
	}
	w.relay.tip = t2
	s := w.signal(2, "c_a2")
	if s.Tip != t2 {
		t.Errorf("signal 2 recorded tip %s, the target was at %s", s.Tip, t2)
	}
	var rowTips []string
	for _, ev := range rows(w.j.Events()).opRows(OpSignalBind) {
		rowTips = append(rowTips, dataStr(ev, "tip"))
	}
	if len(rowTips) != 2 || rowTips[0] != t1 || rowTips[1] != t2 {
		t.Errorf("the rows record tips %v", rowTips)
	}
}

// crw.planRead says whether the plan revision a lane reads before its release is another than the one the last signal (or the first
// revision) read; once a signal read it, the same revision is not news again. The read stands once the node was released after it.
func TestPlanReadChangedOnlyAgainstWhatTheLastSignalRead(t *testing.T) {
	w := newWorld(t)
	then := map[string]string{"A": "a1", "B": "b1"}
	w.relay.plan = planOfNodes(then, "A", "B")
	w.ledgered(true, designOf("A", "B"))
	read := func(node string) PlanSeen {
		var out PlanSeen
		_ = json.Unmarshal(w.must("planRead", "M", node), &out)
		return out
	}
	first := read("A")
	if first.Changed || first.CallID == "" || first.Digest == "" {
		t.Fatalf("the plan the first revision was made against: %+v", first)
	}
	w.release("A")
	w.receipt("c_a1", "A", "")
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b2"}, "A", "B")
	moved := read("B")
	if !moved.Changed || moved.Digest == first.Digest || moved.Base != first.Digest {
		t.Fatalf("a plan revision that came after: %+v (first %+v)", moved, first)
	}
	// the signal that read it makes it the revision to compare with
	w.signal(1, "c_a1")
	if again := read("B"); again.Changed {
		t.Errorf("the revision the last signal read is news again: %+v", again)
	}
	// the read stands only for a node that was released after it
	op := &planReadOp{w.lane}
	rows0 := w.j.Events()
	readRow := rows0[len(rows0)-1]
	for _, ev := range rows0 {
		if ev.Op == OpPlanRead && ev.Event == engine.EvCompleted {
			readRow = ev
		}
	}
	if ok, _, _ := op.Proven(context.Background(), w.j.Events(), readRow); ok {
		t.Errorf("a read the node was not released after stands")
	}
	w.release("B")
	if ok, why, _ := op.Proven(context.Background(), w.j.Events(), readRow); !ok {
		t.Errorf("a read the node was released after does not stand: %s", why)
	}
}

// A changed plan revision a lane read stands, before its node is released, while a signal row that stands took it as a trigger: read
// again it would answer the revision that signal read as its base (no change), and the signal could not replay from its rows.
func TestPlanReadStandsWhileASignalTookItsRevision(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b1"}, "A", "B")
	w.ledgered(true, designOf("A", "B"))
	w.must("planRead", "M", "A")
	w.release("A")
	w.receipt("c_a1", "A", "")
	w.signal(1, "c_a1")
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b2"}, "A", "B")
	var seen PlanSeen
	_ = json.Unmarshal(w.must("planRead", "M", "B"), &seen)
	if !seen.Changed {
		t.Fatalf("the revision after the signal: %+v", seen)
	}
	ev := w.j.Events()
	readRow := ev[len(ev)-1]
	op := &planReadOp{w.lane}
	if ok, _, _ := op.Proven(context.Background(), w.j.Events(), readRow); ok {
		t.Fatalf("a read that neither the node nor a signal went on from stands")
	}
	// a signal of another revision does not take it
	w.must("signalBind", "M", 2, "", []Trigger{{Kind: "revision", Call: "plan:other", Node: "B", Digest: "other"}})
	if ok, _, _ := op.Proven(context.Background(), w.j.Events(), readRow); ok {
		t.Errorf("a read stands on a signal that took another revision")
	}
	w.must("signalBind", "M", 3, "", []Trigger{{Kind: "revision", Call: "plan:" + seen.Digest, Node: "B", Digest: seen.Digest}})
	ev = w.j.Events()
	bind := ev[len(ev)-1]
	if ok, why, _ := op.Proven(context.Background(), w.j.Events(), readRow); !ok {
		t.Errorf("a read whose revision a signal took does not stand: %s", why)
	}
	// once a resume superseded that signal, the read is made again
	w.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindCall, CallID: bind.CallID, Op: OpSignalBind, Event: engine.EvSuperseded})
	if ok, _, _ := op.Proven(context.Background(), w.j.Events(), readRow); ok {
		t.Errorf("a read stands on a signal a resume superseded")
	}
}

// A revision after the first records the plan the architect and the auditor evaluated (the plan crw.replanSignals read for the decision),
// not the plan as it is when the ledger is written: a plan revision made while the delta was audited is the change the next signal
// counts against it.
func TestLedgerRecordsTheEvaluatedPlan(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b1"}, "A", "B")
	w.ledgered(true, designOf("A", "B"))
	w.release("A")
	w.receipt("c_a1", "A", "")
	w.signal(1, "c_a1")
	// the plan the decision evaluates: both slices changed
	w.relay.plan = planOfNodes(map[string]string{"A": "a2", "B": "b2"}, "A", "B")
	st := map[string]any{}
	_ = json.Unmarshal(w.must("replanSignals", "M", ReplanOpts{Phase: "signal", Signal: 1}), &st)
	evaluated, _ := st["plan"].(map[string]any)
	if evaluated == nil {
		t.Fatalf("crw.replanSignals names no plan it evaluated: %v", st)
	}
	// B changes once more while the delta is audited
	w.relay.plan = planOfNodes(map[string]string{"A": "a2", "B": "b3"}, "A", "B")
	d1 := designOf("A", "B")
	d1.Issues[0].Approach = "approach A after the re-plan"
	design, _ := json.Marshal(d1)
	cand := Candidate{Design: design, Answers: json.RawMessage(`[]`)}
	w.must("designAudit", "M", 1, cand, call("c_arch2", "completed", "claude-x", nil),
		call("c_aud2", "completed", "gpt-y", map[string]any{"verdict": "approved", "findings": []any{}, "summary": "ok"}))
	w.must("designLedger", "M", map[string]any{"design": json.RawMessage(design), "answers": json.RawMessage(`[]`), "architect": call("c_arch2", "completed", "claude-x", nil), "signal": 1, "head": strings.Repeat("2", 40), "plan": evaluated})
	led, ok := currentLedger(rows(w.j.Events()), "M")
	if !ok || dataStr(led, "revision") != "r1" {
		t.Fatalf("the current revision: %v %v", led.Data["revision"], ok)
	}
	if got := ledgerSlices(led); got["A"] != "a2" || got["B"] != "b2" {
		t.Errorf("the slices of r1 are %v, want the ones the architect evaluated (a2, b2)", got)
	}
	next := w.numbers(ReplanOpts{Phase: "signal", Signal: 2})
	if !slices.Equal(next.Slices.Changed, []string{"B"}) {
		t.Errorf("the change after the evaluation: %v, want [B]", next.Slices.Changed)
	}
}

// A release records the slice its node has in the plan the relay reads when the node is released; it never takes the slice of the plan the
// run read first for it: a plan that cannot be read, or that has dropped the node, releases nothing.
func TestReleaseNeverRecordsTheFrozenSliceForALiveOne(t *testing.T) {
	w := newWorld(t)
	w.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
	w.ledgered(true, designOf("A"))
	w.relay.plan = planOfNodes(map[string]string{"A": "a2"}, "A")
	w.relay.milestoneErr = errors.New("the relay is unavailable")
	if _, err := w.op("release", "plan-1", "A", "M"); err == nil {
		t.Fatalf("a release was answered although the plan could not be read")
	}
	if len(w.relay.releases) != 0 {
		t.Fatalf("the relay released with a plan nobody read: %+v", w.relay.releases)
	}
	w.relay.milestoneErr = nil
	w.relay.plan = planOfNodes(map[string]string{"B": "b1"}, "B")
	var rel ReleaseOutcome
	raw, err := w.op("release", "plan-1", "A", "M")
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(raw, &rel)
	if rel.Released || rel.Reason != "node_not_in_plan" || len(w.relay.releases) != 0 {
		t.Fatalf("a node the plan dropped: %+v, releases %+v", rel, w.relay.releases)
	}
	w.relay.plan = planOfNodes(map[string]string{"A": "a2"}, "A")
	w.release("A")
	slice := ""
	for _, ev := range w.j.Events() {
		if ev.Op == OpRelease && ev.Event == engine.EvCompleted && dataBool(ev, "released") {
			slice = dataStr(ev, "slice_digest")
		}
	}
	if slice != "a2" {
		t.Errorf("the release row records the slice %q, want the live a2", slice)
	}
}
