package ops

import (
	"encoding/json"
	"fmt"
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
