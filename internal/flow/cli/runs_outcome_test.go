package cli

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
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
