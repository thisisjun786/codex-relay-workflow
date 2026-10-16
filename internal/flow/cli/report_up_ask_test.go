package cli

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// withEscalation is the rows with an escalation row of node A asking ask after the first row (the run's start).
func withEscalation(rows []engine.Event, id, ask string) []engine.Event {
	esc := engine.Event{RunID: "run-p", Seq: 100, Kind: engine.KindFlow, Event: engine.EvEscalation,
		Data: map[string]any{"escalation_id": id, "node": "A", "reason": "size_split", "ask": ask, "class": "size"}}
	out := append([]engine.Event{rows[0], esc}, rows[1:]...)
	for i := range out {
		out[i].Seq = i + 1
	}
	return out
}

// TestABundleSplitRequestIsNoQuestionTheRunWaitsFor (CRW-1437): a node sized for a bundle split escalates with ask bundle_split_request
// and the lane goes on without waiting for an answer: the 자식 sends the request up with `crw flow report-up`. The CLI reads that ask
// for what it is: the question is listed with report_up, it is no question the run waits for, and a run that goes on or finishes with it
// reads running or completed (ps, show, next: running, not waiting_decision or held). A question the run waits for (ask land) still
// makes the finished run held and the live one waiting_decision.
func TestABundleSplitRequestIsNoQuestionTheRunWaitsFor(t *testing.T) {
	rec := RunRecord{RunID: "run-p", Workflow: "project-lane"}
	finished := withEscalation(pausedEvents("started", "finished:completed:"), "esc-b", "bundle_split_request")
	live := withEscalation(pausedEvents("started"), "esc-b", "bundle_split_request")

	qs := Questions(finished)
	if len(qs) != 1 || !qs[0].ReportUp || !qs[0].Current || qs[0].Applied != "" {
		t.Errorf("the questions %+v, want one current report_up question", qs)
	}
	if info := Summarize(rec, finished, false, 0); info.State != StateCompleted || info.Waiting != 0 {
		t.Errorf("a finished run with a bundle split request: state %s, waiting %d, want completed and none", info.State, info.Waiting)
	}
	info := Summarize(rec, live, true, 1)
	if info.State != StateRunning || info.Waiting != 0 {
		t.Errorf("a live run with a bundle split request: state %s, waiting %d, want running and none", info.State, info.Waiting)
	}
	if got := nextState(info, true, nil); got != NextRunning {
		t.Errorf("next of a live run with a bundle split request: %s, want %s", got, NextRunning)
	}

	waits := withEscalation(pausedEvents("started", "finished:completed:"), "esc-l", "land")
	if info := Summarize(rec, waits, false, 0); info.State != StateHeld || info.Waiting != 1 {
		t.Errorf("a finished run with a land question: state %s, waiting %d, want held and 1", info.State, info.Waiting)
	}
	liveWaits := Summarize(rec, withEscalation(pausedEvents("started"), "esc-l", "land"), true, 1)
	if got := nextState(liveWaits, true, nil); got != NextWaitingDecision {
		t.Errorf("next of a live run with a land question: %s, want %s", got, NextWaitingDecision)
	}
}

// `crw flow show` prints a bundle split request as a request to send up (the escalation does not say it was sent), and `crw flow ps` reads the finished run completed (CRW-1437).
func TestShowPrintsABundleSplitRequestAsARequestToSendUp(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-up", "thread-owner")
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindFlow, Event: engine.EvEscalation,
		Data: map[string]any{"escalation_id": "esc-b", "node": "A", "reason": "size_split", "ask": "bundle_split_request", "class": "size"}})
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunCompleted, "error_kind": ""}})
	r.close()
	td := w.deps()
	code, out, errOut := td.do("show", "run-up", "--state", w.state)
	if code != 0 {
		t.Fatalf("show: exit %d %s", code, errOut)
	}
	if !strings.Contains(out, "question esc-b r1  node A  size_split  a request to send up with crw flow report-up (bundle_split_request); the run waits for no answer") {
		t.Errorf("show:\n%s", out)
	}
	code, out, errOut = td.do("ps", "--all", "--state", w.state)
	if code != 0 || !strings.Contains(out, "run-up") || strings.Contains(out, StateHeld) {
		t.Errorf("ps: exit %d %s\n%s", code, errOut, out)
	}
}
