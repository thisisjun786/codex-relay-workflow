package cli

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// outcomeEvents are pausedEvents where "finished:<status>:<error kind>:<state>" is a finished row whose outcome has that state (and the
// reason r-<state>).
func outcomeEvents(rows ...string) []engine.Event {
	out := pausedEvents(rows...)
	for i, r := range rows {
		if parts := strings.Split(r, ":"); len(parts) == 4 && parts[0] == "finished" {
			out[i].Data["outcome"] = map[string]any{"state": parts[3], "reason": "r-" + parts[3]}
		}
	}
	return out
}

// TestSummarizeShowsOnlyTheCurrentOutcome (CRW-1487): the outcome of a run is the one its current end carries: the last finished row
// when it is the end of the last process. A process started since (running, or dead with no process), a paused end since, and a finish
// with no outcome since show none, so an outcome of an older process is never shown for the run as it is now.
func TestSummarizeShowsOnlyTheCurrentOutcome(t *testing.T) {
	rec := RunRecord{RunID: "run-p", Workflow: "two"}
	cases := []struct {
		name  string
		live  bool
		rows  []string
		state string // "" when there is no outcome
	}{
		{"finished with an outcome", false, []string{"started", "finished:completed::escalated"}, "escalated"},
		{"started since, no process", false, []string{"started", "finished:completed::escalated", "started"}, ""},
		{"started since, running", true, []string{"started", "finished:completed::escalated", "started"}, ""},
		{"paused since", false, []string{"started", "finished:completed::escalated", "started", "run_paused", "paused"}, ""},
		{"a later finish with no outcome", false, []string{"started", "finished:completed::escalated", "started", "finished:completed:"}, ""},
		{"a later finish with another outcome", false, []string{"started", "finished:completed::escalated", "started", "finished:completed::incomplete"}, "incomplete"},
		{"no outcome", false, []string{"started", "finished:completed:"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := Summarize(rec, outcomeEvents(c.rows...), c.live, 0)
			switch {
			case c.state == "" && info.Outcome != nil:
				t.Fatalf("state %s: an outcome %+v", info.State, info.Outcome)
			case c.state != "" && (info.Outcome == nil || info.Outcome.State != c.state || info.Outcome.Reason != "r-"+c.state):
				t.Fatalf("state %s: outcome %+v, want %s", info.State, info.Outcome, c.state)
			}
		})
	}
	// the JSON of ps and show carries it only when there is one
	with := Summarize(rec, outcomeEvents("started", "finished:completed::escalated"), false, 0)
	if j := strings.Join(strings.Fields(string(JSON(with))), ""); !strings.Contains(j, `"outcome":{"state":"escalated","reason":"r-escalated"}`) {
		t.Errorf("json %s", j)
	}
	without := Summarize(rec, outcomeEvents("started", "finished:completed:"), false, 0)
	if j := string(JSON(without)); strings.Contains(j, `"outcome"`) {
		t.Errorf("json %s", j)
	}
}

// `crw flow show` and `ps` print the outcome of the run's current end (CRW-1487): state/reason, the escalation, and each lane that did
// not land.
func TestShowAndPsPrintTheOutcome(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-out", "thread-owner")
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunCompleted, "error_kind": "",
		"outcome": map[string]any{"state": "incomplete", "milestone": "M1", "lanes": []any{map[string]any{"node": "n1", "state": "escalated", "reason": "blocked_scope"}}}}})
	r.close()
	td := w.deps()
	code, out, errOut := td.do("show", "run-out", "--state", w.state)
	if code != 0 {
		t.Fatalf("show: exit %d %s", code, errOut)
	}
	if !strings.Contains(out, "\noutcome incomplete  milestone M1\n  lane n1 escalated/blocked_scope\n") {
		t.Errorf("show:\n%s", out)
	}
	code, out, errOut = td.do("ps", "--all", "--state", w.state)
	if code != 0 {
		t.Fatalf("ps: exit %d %s", code, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "OUTCOME") || !strings.HasPrefix(lines[1], "run-out") || !strings.HasSuffix(lines[1], " incomplete") {
		t.Errorf("ps:\n%s", out)
	}
}

// ---- CRW-1595 (finding 3) ----

func crw1595EscFlow(seq int, run, call, id, node, ask, reason string) engine.Event {
	return engine.Event{RunID: run, Seq: seq, Kind: engine.KindFlow, CallID: call, Op: ops.OpEscalate, Event: engine.EvEscalation,
		Data: map[string]any{"call_id": "c_exec", "escalation_id": id, "node": node, "ask": ask, "reason": reason}}
}

func crw1595EscDone(seq int, run, call, id string) engine.Event {
	return engine.Event{RunID: run, Seq: seq, Kind: engine.KindCall, CallID: call, Op: ops.OpEscalate, Event: engine.EvCompleted,
		Data: map[string]any{"escalation_id": id}}
}

func crw1595Superseded(seq int, run, call string) engine.Event {
	return engine.Event{RunID: run, Seq: seq, Kind: engine.KindCall, CallID: call, Event: engine.EvSuperseded, Data: map[string]any{"reason": "superseded_by_resume"}}
}

// A question whose escalate call a resume superseded is kept as history — marked superseded, never current, never
// waiting — and a completed crw.escalate of the same escalation id after the supersession raises it again (the
// replayed op writes no second flow row). Superseding the causal agent call the row names (data.call_id) retires
// nothing.
func TestCRW1595SupersededQuestionIsHistory(t *testing.T) {
	rec := RunRecord{RunID: "run-s", Workflow: "two"}
	started := engine.Event{RunID: "run-s", Seq: 1, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "two"}}
	finished := func(seq int, outcome map[string]any) engine.Event {
		return engine.Event{RunID: "run-s", Seq: seq, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunCompleted, "outcome": outcome}}
	}
	// (a) the escalation's own call was superseded after it completed; the run then finished closed
	events := []engine.Event{started,
		crw1595EscFlow(2, "run-s", "c_esc", "esc_old", "n1", "land", "verification_failed"), crw1595EscDone(3, "run-s", "c_esc", "esc_old"),
		crw1595Superseded(4, "run-s", "c_esc"),
		{RunID: "run-s", Seq: 5, Kind: engine.KindCall, CallID: "c_int", Op: ops.OpIntegrate, Event: engine.EvCompleted, Data: map[string]any{}},
		finished(6, map[string]any{"state": "closed"}),
	}
	qs := Questions(events)
	if len(qs) != 1 || qs[0].ID != "esc_old" || qs[0].Current || !qs[0].Superseded || qs[0].Waits() {
		t.Fatalf("the superseded question: %+v", qs)
	}
	if info := Summarize(rec, events, false, 0); info.State != StateCompleted || info.Waiting != 0 {
		t.Fatalf("a closed run whose only question was superseded: state %s waiting %d", info.State, info.Waiting)
	}
	// the same rows while the process lives read running, not waiting_decision
	live := Summarize(rec, events[:5], true, 42)
	if live.State != StateRunning || nextState(live, true, nil) != NextRunning {
		t.Fatalf("the live prefix reads %s/%s, want running", live.State, nextState(live, true, nil))
	}
	// superseding the causal agent call named in data.call_id retires nothing: the question stands
	causal := append([]engine.Event{}, events...)
	causal[3] = crw1595Superseded(4, "run-s", "c_exec")
	if qs := Questions(causal); len(qs) != 1 || !qs[0].Current || qs[0].Superseded || !qs[0].Waits() {
		t.Fatalf("superseding the causal call retired the question: %+v", qs)
	}
	// a flow row written before CallID existed keeps the historical reading: never superseded
	noCall := append([]engine.Event{}, events...)
	noCall[1].CallID = ""
	if qs := Questions(noCall); len(qs) != 1 || !qs[0].Current || qs[0].Superseded {
		t.Fatalf("a call-less escalation row read superseded: %+v", qs)
	}
	// (b) a completed crw.escalate of the same id after the supersession re-raises the question
	raised := []engine.Event{started,
		crw1595EscFlow(2, "run-s", "c_esc", "esc_old", "n1", "land", "verification_failed"), crw1595EscDone(3, "run-s", "c_esc", "esc_old"),
		crw1595Superseded(4, "run-s", "c_esc"),
		crw1595EscDone(5, "run-s", "c_esc2", "esc_old"),
		finished(6, map[string]any{"state": "closed"}),
	}
	if qs := Questions(raised); len(qs) != 1 || !qs[0].Current || qs[0].Superseded || !qs[0].Waits() {
		t.Fatalf("a replayed escalate did not re-raise the question: %+v", qs)
	}
	// a completed escalate of another escalation id reactivates nothing
	other := []engine.Event{started,
		crw1595EscFlow(2, "run-s", "c_esc", "esc_old", "n1", "land", "verification_failed"), crw1595EscDone(3, "run-s", "c_esc", "esc_old"),
		crw1595Superseded(4, "run-s", "c_esc"),
		crw1595EscDone(5, "run-s", "c_esc2", "esc_other"),
	}
	if qs := Questions(other); len(qs) != 1 || qs[0].Current || qs[0].Waits() {
		t.Fatalf("a different escalation id reactivated the question: %+v", qs)
	}
	// (c) two revisions of one question, the latest superseded: the older is not revived
	revisions := []engine.Event{started,
		crw1595EscFlow(2, "run-s", "c1", "esc_r1", "n1", "land", "verification_failed"), crw1595EscDone(3, "run-s", "c1", "esc_r1"),
		crw1595EscFlow(4, "run-s", "c2", "esc_r2", "n1", "land", "verification_failed"), crw1595EscDone(5, "run-s", "c2", "esc_r2"),
		crw1595Superseded(6, "run-s", "c2"),
	}
	if qs := Questions(revisions); len(qs) != 2 || qs[0].Current || qs[1].Current || !qs[1].Superseded || qs[0].Revision != 1 || qs[1].Revision != 2 {
		t.Fatalf("the revisions of a superseded question: %+v", qs)
	}
	// an ask-less escalation on a closed outcome is a notice: the run stays completed and waits on nothing
	noted := []engine.Event{started,
		crw1595EscFlow(2, "run-s", "c_note", "esc_note", "", "", "design_loop_tokens"), crw1595EscDone(3, "run-s", "c_note", "esc_note"),
		finished(4, map[string]any{"state": "closed"}),
	}
	if info := Summarize(rec, noted, false, 0); info.State != StateCompleted || info.Waiting != 0 {
		t.Fatalf("a closed run with an ask-less notice: %+v", info)
	}
	if live := Summarize(rec, noted[:3], true, 42); nextState(live, true, nil) != NextRunning {
		t.Fatalf("a live run with an ask-less notice reads %s", nextState(live, true, nil))
	}
}

// A finished run whose outcome is escalated is held — its final escalation asks nothing, so no waiting question is
// invented — and a live prefix before the end still reads running.
func TestCRW1595EscalatedOutcomeHoldsTheRun(t *testing.T) {
	rec := RunRecord{RunID: "run-s", Workflow: "two"}
	events := []engine.Event{
		{RunID: "run-s", Seq: 1, Kind: engine.KindRun, Event: "started", Data: map[string]any{"workflow": "two"}},
		crw1595EscFlow(2, "run-s", "c_final", "esc_final", "n1", "", "test_failed"), crw1595EscDone(3, "run-s", "c_final", "esc_final"),
		{RunID: "run-s", Seq: 4, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunCompleted,
			"outcome": map[string]any{"state": "escalated", "reason": "test_failed", "escalation": "esc_final"}}},
	}
	if qs := Questions(events); len(qs) != 0 {
		t.Fatalf("an ask-less escalation became a question: %+v", qs)
	}
	info := Summarize(rec, events, false, 0)
	if info.State != StateHeld || info.Waiting != 0 {
		t.Fatalf("an escalated end: state %s waiting %d, want held/0", info.State, info.Waiting)
	}
	if d := Detail(rec, events, false, 0); len(d.Escalations) != 1 || d.Escalations[0].ID != "esc_final" || !d.Escalations[0].Current {
		t.Fatalf("the escalations of the run: %+v", d.Escalations)
	}
}
