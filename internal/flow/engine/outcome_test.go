package engine

import (
	"reflect"
	"strings"
	"testing"
)

// finishedRow is the KindRun finished row of the run (the last one).
func (h *harness) finishedRow() Event {
	h.t.Helper()
	var out Event
	found := false
	for _, e := range h.journal.Events() {
		if e.Kind == KindRun && e.Event == "finished" {
			out, found = e, true
		}
	}
	if !found {
		h.t.Fatalf("no finished row in %+v", h.journal.Events())
	}
	return out
}

// The finished row carries the outcome of the script (CRW-1487): the state, reason, source, escalation and milestone a project lane or
// a milestone halt returns, and the lanes that did not land. The bodies are literal copies of the returns they stand for (the escalation
// id and the milestone are constants here, since crw.escalate and the plan are not in the engine's tests).
func TestFinishedRowCarriesTheOutcome(t *testing.T) {
	cases := []struct {
		name string
		body string
		want *Outcome
	}{
		{
			// project-lane.js:85-86 (auditedDesign, the AUDIT_ROUNDS cap), returned by the lane at :153
			name: "design_audit_rounds",
			body: "const esc = { id: 'esc-1' }\n" +
				"const r = { ok: false, result: { state: 'escalated', source: 'design_audit', reason: 'design_audit_rounds', escalation: esc.id } }\n" +
				"return r.result\n",
			want: &Outcome{State: "escalated", Source: "design_audit", Reason: "design_audit_rounds", Escalation: "esc-1"},
		},
		{
			// project-lane.js:309, three lanes of crw.dag, one landed
			name: "incomplete",
			body: "const ms = { id: 'M1' }\n" +
				"const lanes = [{ node: 'n1', state: 'escalated', reason: 'blocked_scope', head: '' }, { node: 'n2', state: 'escalated', reason: 'land_review_needs_changes', head: '' }, { node: 'n3', state: 'landed', reason: '', head: 'abc' }]\n" +
				"if (!lanes.every((l) => l && l.state === 'landed')) return { state: 'incomplete', milestone: ms.id, lanes }\n" +
				"return { state: 'closed' }\n",
			want: &Outcome{State: "incomplete", Milestone: "M1", Lanes: []OutcomeLane{{Node: "n1", State: "escalated", Reason: "blocked_scope"}, {Node: "n2", State: "escalated", Reason: "land_review_needs_changes"}}},
		},
		{
			// lib/lane.js:567-568 (milestoneHalt, a design call that was refused)
			name: "milestoneHalt",
			body: "const source = 'architect', status = 'refused', reason = '', callId = 'c-1'\n" +
				"const esc = { id: 'esc-2' }\n" +
				"return { state: 'escalated', source, reason: reason || status, call: callId, escalation: esc.id }\n",
			want: &Outcome{State: "escalated", Source: "architect", Reason: "refused", Escalation: "esc-2"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			res := h.run(c.body)
			if res.Status != RunCompleted {
				t.Fatalf("run %s: %s", res.Status, res.Error)
			}
			got := FinishedOutcome(h.finishedRow().Data)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("outcome %+v, want %+v (row %+v)", got, c.want, h.finishedRow().Data)
			}
		})
	}
	// a value that is not a milestone outcome gives none, and so does a script error
	for name, body := range map[string]string{
		"number":       "return 3\n",
		"no state":     "return { ok: true }\n",
		"state number": "return { state: 3 }\n",
		"array":        "return [{ state: 'escalated' }]\n",
		"script error": "throw new Error('boom')\n",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			res := h.run(body)
			row := h.finishedRow()
			if _, ok := row.Data["outcome"]; ok {
				t.Fatalf("%s (%s): the finished row has an outcome: %+v", name, res.Status, row.Data)
			}
			if name == "script error" && res.Status != RunScriptError {
				t.Fatalf("status %s", res.Status)
			}
		})
	}
}

// outcomeOf keeps the fields to one line of at most 120 runes, keeps at most 16 lanes that did not land and counts the rest, and reads
// an escalation only when it is a string.
func TestOutcomeOfCapsWhatItKeeps(t *testing.T) {
	long := strings.Repeat("é", 200)
	lanes := `[`
	for i := 0; i < 20; i++ {
		if i > 0 {
			lanes += `,`
		}
		lanes += `{"node":"n` + string(rune('a'+i)) + `","state":"escalated","reason":"r"}`
	}
	lanes += `,{"node":"z","state":"landed"},null,7]`
	o := outcomeOf([]byte(`{"state":"incomplete\nnow","reason":"` + long + `","escalation":{"id":"x"},"lanes":` + lanes + `}`))
	if o == nil {
		t.Fatal("no outcome")
	}
	if o.State != "incomplete now" || len([]rune(o.Reason)) != 120 || o.Escalation != "" {
		t.Fatalf("outcome %+v", o)
	}
	if len(o.Lanes) != 16 || o.LanesMore != 4 || o.Lanes[0].Node != "na" {
		t.Fatalf("lanes %d more %d: %+v", len(o.Lanes), o.LanesMore, o.Lanes)
	}
	if outcomeOf([]byte(`null`)) != nil || outcomeOf([]byte(`"escalated"`)) != nil || outcomeOf(nil) != nil {
		t.Fatal("a value that is not an object gave an outcome")
	}
}
