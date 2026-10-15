package cli

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// endPaused appends what the engine writes when the operator paused the run (CRW-1487): the run_paused row of the ask, then the
// run/paused row that ends the process instead of a finished row.
func (r *run) endPaused() {
	r.flow(engine.EvRunPaused, map[string]any{"by": "operator", "reason": "crw flow pause"})
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "paused", Data: map[string]any{"status": engine.RunPaused, "by": "operator", "reason": "crw flow pause"}})
}

// endRowsAfterLastStart are the events of the run rows (KindRun) after the last started row, in order.
func endRowsAfterLastStart(events []engine.Event) []string {
	last := -1
	for i, e := range events {
		if e.Kind == engine.KindRun && e.Event == "started" {
			last = i
		}
	}
	var out []string
	for _, e := range events[last+1:] {
		if e.Kind == engine.KindRun {
			out = append(out, e.Event)
		}
	}
	return out
}

// pausedEvents are the rows of a run: KindRun rows by name ("started", "paused", "finished:<status>:<error kind>") and the flow
// run_paused row of the operator ("run_paused").
func pausedEvents(rows ...string) []engine.Event {
	var out []engine.Event
	for i, r := range rows {
		e := engine.Event{RunID: "run-p", Seq: i + 1, Kind: engine.KindRun}
		switch {
		case r == "started":
			e.Event, e.Data = "started", map[string]any{"workflow": "two", "script_digest": "d", "args_digest": "a"}
		case r == "paused":
			e.Event, e.Data = "paused", map[string]any{"status": engine.RunPaused, "by": "operator", "reason": "crw flow pause"}
		case r == "run_paused":
			e.Kind, e.Event, e.Data = engine.KindFlow, engine.EvRunPaused, map[string]any{"by": "operator", "reason": "crw flow pause"}
		case r == "run_paused_deadline":
			e.Kind, e.Event, e.Data = engine.KindFlow, engine.EvRunPaused, map[string]any{"by": "deadline", "reason": "the run's deadline T passed"}
		case strings.HasPrefix(r, "finished:"):
			parts := strings.Split(r, ":")
			e.Event, e.Data = "finished", map[string]any{"status": parts[1], "error_kind": parts[2]}
		}
		out = append(out, e)
	}
	return out
}

// TestSummarizeReadsAPausedEnd (CRW-1487): a run whose last run row after its last start is the run/paused row an operator pause
// ends with reads paused when no process holds it, with status paused and no error kind of an older end; a paused end of an
// older process does not make a later start paused (no process: dead), and a cancel of a paused end (cancelDead's finished row) is
// cancelled.
func TestSummarizeReadsAPausedEnd(t *testing.T) {
	rec := RunRecord{RunID: "run-p", Workflow: "two"}
	cases := []struct {
		name   string
		rows   []string
		live   bool
		state  string
		status string
		kind   string
		paused string
	}{
		{"a paused end", []string{"started", "run_paused", "paused"}, false, StatePaused, engine.RunPaused, "", "operator: crw flow pause"},
		{"a paused end with its process still there", []string{"started", "run_paused", "paused"}, true, StatePaused, engine.RunPaused, "", "operator: crw flow pause"},
		{"a paused end after an older completed end", []string{"started", "finished:completed:", "started", "run_paused", "paused"}, false, StatePaused, engine.RunPaused, "", "operator: crw flow pause"},
		{"a paused end after an older failed end", []string{"started", "finished:script_error:engine", "started", "run_paused", "paused"}, false, StatePaused, engine.RunPaused, "", "operator: crw flow pause"},
		{"a start after a paused end", []string{"started", "run_paused", "paused", "started"}, false, StateDead, engine.RunPaused, "", ""},
		{"a cancel of a paused end", []string{"started", "run_paused", "paused", "finished:script_error:cancelled"}, false, StateCancelled, engine.RunScriptError, engine.ErrKindCancelled, "operator: crw flow pause"},
		{"a pause ask with no end row", []string{"started", "run_paused"}, false, StateDead, "", "", "operator: crw flow pause"},
		{"an operator pause then a deadline ask", []string{"started", "run_paused", "run_paused_deadline", "paused"}, false, StatePaused, engine.RunPaused, "", "operator: crw flow pause"},
		{"a cancel of a paused end after an operator pause then a deadline ask", []string{"started", "run_paused", "run_paused_deadline", "paused", "finished:script_error:cancelled"}, false, StateCancelled, engine.RunScriptError, engine.ErrKindCancelled, "operator: crw flow pause"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			info := Summarize(rec, pausedEvents(c.rows...), c.live, 0)
			if info.State != c.state || info.Paused != c.paused || info.ErrorKind != c.kind {
				t.Errorf("%v: state %q paused %q error kind %q, want %q %q %q (%+v)", c.rows, info.State, info.Paused, info.ErrorKind, c.state, c.paused, c.kind, info)
			}
			if c.state == StatePaused && info.Status != c.status {
				t.Errorf("%v: status %q, want %q", c.rows, info.Status, c.status)
			}
			if c.state == StateCancelled && info.Status != c.status {
				t.Errorf("%v: status %q, want %q", c.rows, info.Status, c.status)
			}
		})
	}
}

// TestDigestSaysAPausedRunWasPaused (CRW-1487): the digest of a paused end no process holds says the run was paused by the
// operator, not that it is still running, and a night-batch selection mismatch does not overwrite that; a paused run whose process
// is still there is still running.
func TestDigestSaysAPausedRunWasPaused(t *testing.T) {
	rec := RunRecord{RunID: "run-p", Workflow: "night-batch"}
	events := pausedEvents("started", "run_paused", "paused")
	if got := BuildDigest(DigestInput{Run: rec, Events: events}).Stopped; got != "paused by operator: crw flow pause" {
		t.Errorf("the digest of a paused end says %q", got)
	}
	if got := BuildDigest(DigestInput{Run: rec, Events: events, Live: true}).Stopped; got != "still running" {
		t.Errorf("the digest of a paused run its process still holds says %q", got)
	}
	mismatch := append(pausedEvents("started"), engine.Event{RunID: "run-p", Seq: 2, Kind: engine.KindFlow, Event: engine.EvEscalation, Data: map[string]any{"reason": "night_batch_selection", "detail": "M2 is not M1"}})
	for i, e := range pausedEvents("run_paused", "paused") {
		e.Seq = 3 + i
		mismatch = append(mismatch, e)
	}
	if got := BuildDigest(DigestInput{Run: rec, Events: mismatch}).Stopped; got != "paused by operator: crw flow pause" {
		t.Errorf("the digest of a paused end with a selection mismatch says %q", got)
	}
}

// TestShowNotesAPausedEnd (CRW-1487): `crw flow show` of a paused end no process holds says it is paused and that a resume goes
// on with it, not that its process ended without finishing it.
func TestShowNotesAPausedEnd(t *testing.T) {
	w := newWorld(t)
	r := w.newRun("run-shown", "thread-owner")
	r.endPaused()
	r.close()
	td := w.deps()
	code, out, errOut := td.do("show", "run-shown", "--state", w.state)
	if code != 0 {
		t.Fatalf("show: exit %d %s", code, errOut)
	}
	if !strings.HasPrefix(out, "run run-shown  project-lane  paused\n") {
		t.Errorf("the state line:\n%s", out)
	}
	if !strings.Contains(out, "  paused by operator: crw flow pause; no process holds the run: crw flow resume run-shown goes on\n") {
		t.Errorf("no paused note:\n%s", out)
	}
	if strings.Contains(out, "  no process holds the run: crw flow resume run-shown\n") || strings.Count(out, "paused by") != 1 {
		t.Errorf("a paused end is noted as a dead run, or twice:\n%s", out)
	}
}

// TestAuditCountsAPausedEndAsEnded (CRW-1487): a run that ended paused owns its journal no more than one that finished, so an
// audit records its result; a run with only a start has not ended. An end counts only after the last started row: an end of an
// earlier process followed by a start (a resume) leaves the journal to the resumed process, which numbers its rows on.
func TestAuditCountsAPausedEndAsEnded(t *testing.T) {
	cases := []struct {
		rows []string
		want bool
	}{
		{[]string{"started", "run_paused", "paused"}, true},
		{[]string{"started", "finished:completed:"}, true},
		{[]string{"started"}, false},
		{[]string{"started", "run_paused"}, false},
		{[]string{"started", "run_paused", "paused", "started"}, false},
		{[]string{"started", "finished:completed:", "started"}, false},
		{[]string{"started", "run_paused", "paused", "started", "finished:completed:"}, true},
		{[]string{"started", "finished:completed:", "started", "run_paused", "paused"}, true},
	}
	for _, c := range cases {
		if got := runEnded(pausedEvents(c.rows...)); got != c.want {
			t.Errorf("runEnded(%v) = %t, want %t", c.rows, got, c.want)
		}
	}
}

// TestPausedEndReadersNameTheEndRowsAsker (CRW-1487): a run the operator paused and whose deadline then passed ends paused by the
// operator (the deadline ask does not end a run paused). `crw flow show`, the digest and `crw flow next` say who paused it from the
// run/paused end row, not from the last run_paused ask.
func TestPausedEndReadersNameTheEndRowsAsker(t *testing.T) {
	rec := RunRecord{RunID: "run-p", Workflow: "two"}
	events := pausedEvents("started", "run_paused", "run_paused_deadline", "paused")
	info := Summarize(rec, events, false, 0)
	if info.State != StatePaused || info.Paused != "operator: crw flow pause" {
		t.Errorf("summary: state %q paused %q", info.State, info.Paused)
	}
	if got := BuildDigest(DigestInput{Run: rec, Events: events}).Stopped; got != "paused by operator: crw flow pause" {
		t.Errorf("the digest says %q", got)
	}
	if got := nextHint(NextAnswer{Run: "run-p", State: NextPaused}, info, nil); !strings.Contains(got, "run run-p is paused (operator: crw flow pause)") {
		t.Errorf("the next hint says %q", got)
	}

	w := newWorld(t)
	r := w.newRun("run-shown", "thread-owner")
	r.flow(engine.EvRunPaused, map[string]any{"by": "operator", "reason": "crw flow pause"})
	r.flow(engine.EvRunPaused, map[string]any{"by": "deadline", "reason": "the run's deadline T passed"})
	r.j.Append(engine.Event{RunID: r.id, Kind: engine.KindRun, Event: "paused", Data: map[string]any{"status": engine.RunPaused, "by": "operator", "reason": "crw flow pause"}})
	r.close()
	code, out, errOut := w.deps().do("show", "run-shown", "--state", w.state)
	if code != 0 {
		t.Fatalf("show: exit %d %s", code, errOut)
	}
	if !strings.Contains(out, "  paused by operator: crw flow pause; no process holds the run: crw flow resume run-shown goes on\n") || strings.Contains(out, "deadline") {
		t.Errorf("show names the deadline, or not the operator:\n%s", out)
	}
}
