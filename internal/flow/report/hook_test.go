package report

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

func (w *world) escalate(id, reason, node, detail string) {
	w.t.Helper()
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindFlow, Event: engine.EvEscalation, Data: map[string]any{
		"escalation_id": id, "reason": reason, "class": "tiers", "node": node, "detail": detail}})
	if err := w.journal.Flush(); err != nil {
		w.t.Fatal(err)
	}
}

// A run's escalation goes to the orchestrator thread through send-parent, queued: the run does not look at the thread first (it cannot
// prove a send wakes it), so no probe of any kind is made. It is told once, whatever the number of reconciles; a thread whose wake-up is
// proven is sent the notice directly.
func TestEscalationViaSendParentQueuedWithoutPreflight(t *testing.T) {
	w := newWorld(t)
	h := w.hook()
	w.escalate("esc_1", "tiers_exhausted", "A", "the executor-t1 tier ended with the same finding twice")
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	sent := w.sender.parentSent()
	if len(sent) != 1 || sent[0].Thread != testOrch || sent[0].LogicalID != "esc-esc_1" || !sent[0].Queue {
		t.Fatalf("sent %+v", sent)
	}
	for _, want := range []string{"run escalation", "run=" + testRun, "node=A", "reason=tiers_exhausted", "the executor-t1 tier ended"} {
		if !strings.Contains(sent[0].Text, want) {
			t.Errorf("the notice lacks %q: %s", want, sent[0].Text)
		}
	}
	// the hook has no Wake here, so it queues; that the real send-parent --queue path makes no get_active_turn call is pinned end to end by
	// TestSendParentQueueWritesTheNoticeAndSendsNothing (internal/manage), which reads the fake bridge's log
	if len(w.sender.supervisorSent()) != 0 {
		t.Errorf("a supervisor was told (%d)", len(w.sender.supervisorSent()))
	}
	rows := w.rows(KindEscalationNotice)
	if len(rows) != 1 || rows[0].Str("escalation_id") != "esc_1" || rows[0].Str("result") != "queued" {
		t.Fatalf("notice rows %+v", rows)
	}
	if err := h.BatchCommitted(ctx, testRun); err != nil || len(w.sender.parentSent()) != 1 {
		t.Errorf("told again: %v %+v", err, w.sender.parentSent())
	}
	// a delivery that got no answer is made again by the next reconcile, under the same logical id
	w.escalate("esc_2", "dispatch_unknown", "B", "")
	w.sender.replyParent = func(ParentSend) (SendResult, error) { return SendResult{Class: "unknown"}, nil }
	if err := h.BatchCommitted(ctx, testRun); err != nil || len(w.rows(KindEscalationNotice)) != 1 {
		t.Fatalf("unknown: %v rows %d", err, len(w.rows(KindEscalationNotice)))
	}
	w.sender.replyParent = nil
	if err := h.BatchCommitted(ctx, testRun); err != nil || len(w.rows(KindEscalationNotice)) != 2 {
		t.Fatalf("the next reconcile: %v", err)
	}
	if got := w.sender.distinctParent(); len(got) != 2 || got[1] != "esc-esc_2" {
		t.Errorf("distinct %v", got)
	}
	// the wake-up proven: the notice is sent, not queued
	probes := 0
	h.Wake = func(context.Context, string) bool { probes++; return true }
	w.escalate("esc_3", "size_split", "C", "")
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	last := w.sender.parentSent()[len(w.sender.parentSent())-1]
	if last.LogicalID != "esc-esc_3" || last.Queue || probes != 1 {
		t.Errorf("last %+v probes %d", last, probes)
	}
}

// The run's completion digest goes the same way when the run finishes, once.
func TestRunFinishedTellsTheCompletionDigest(t *testing.T) {
	w := newWorld(t)
	h := w.hook()
	if err := h.RunFinished(ctx, testRun, "completed"); err != nil {
		t.Fatal(err)
	}
	sent := w.sender.parentSent()
	if len(sent) != 1 || sent[0].Thread != testOrch || !sent[0].Queue || !strings.Contains(sent[0].Text, "run finished run="+testRun) || !strings.Contains(sent[0].Text, "status=completed") || !strings.HasPrefix(sent[0].LogicalID, "done-") {
		t.Fatalf("sent %+v", sent)
	}
	if rows := w.rows(KindCompletionNotice); len(rows) != 1 || rows[0].Str("status") != "completed" {
		t.Fatalf("rows %+v", rows)
	}
	if err := h.RunFinished(ctx, testRun, "completed"); err != nil || len(w.rows(KindCompletionNotice)) != 1 {
		t.Errorf("the second finish: %v rows %d", err, len(w.rows(KindCompletionNotice)))
	}
}

// finish appends a finished row to the run's journal, as the engine writes it before RunFinished (CRW-1487: with the outcome of the
// script when it returned one).
func (w *world) finish(status string, outcome map[string]any) {
	w.t.Helper()
	data := map[string]any{"status": status, "error_kind": ""}
	if outcome != nil {
		data["outcome"] = outcome
	}
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindRun, Event: "finished", Data: data})
	if err := w.journal.Flush(); err != nil {
		w.t.Fatal(err)
	}
}

// The completion notice tells the milestone state the script came to (CRW-1487): a run whose lane escalated or did not land all its
// lanes is never told completed. The completion_notice row carries the state, the reason and the lanes; a later finish with another
// outcome is told again (a new key). A finish with no outcome keeps the old text and key (TestRunFinishedTellsTheCompletionDigest).
func TestRunFinishedTellsTheMilestoneState(t *testing.T) {
	w := newWorld(t)
	h := w.hook()
	// the old key of this journal, before any outcome
	plain := "done:" + testRun + ":" + digestOfRows(w.journal.Events(), "completed", nil)
	// with no outcome the digest is byte for byte the one before CRW-1487: the completed calls and the status
	var parts []string
	for _, ev := range w.journal.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted {
			parts = append(parts, ev.CallID)
		}
	}
	if old := "done:" + testRun + ":" + shortHash(append(parts, "status=completed")...); plain != old {
		t.Fatalf("the key with no outcome moved: %s, was %s", plain, old)
	}
	w.finish("completed", map[string]any{"state": "escalated", "source": "design_audit", "reason": "design_audit_rounds", "escalation": "esc-1"})
	if err := h.RunFinished(ctx, testRun, "completed"); err != nil {
		t.Fatal(err)
	}
	sent := w.sender.parentSent()
	if len(sent) != 1 {
		t.Fatalf("sent %+v", sent)
	}
	for _, want := range []string{"run finished run=" + testRun, "state=escalated reason=design_audit_rounds escalation=esc-1", "digest="} {
		if !strings.Contains(sent[0].Text, want) {
			t.Errorf("the notice lacks %q: %s", want, sent[0].Text)
		}
	}
	if strings.Contains(sent[0].Text, "status=completed") {
		t.Errorf("an escalated run is told completed: %s", sent[0].Text)
	}
	rows := w.rows(KindCompletionNotice)
	if len(rows) != 1 || rows[0].Str("status") != "completed" || rows[0].Str("state") != "escalated" || rows[0].Str("reason") != "design_audit_rounds" {
		t.Fatalf("rows %+v", rows)
	}
	if rows[0].Str("notice_key") == plain {
		t.Errorf("the outcome kept the old key %s", plain)
	}
	// a resume that ends with the lanes incomplete is told again, with the lanes that did not land
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindRun, Event: "started", Data: map[string]any{}})
	w.finish("completed", map[string]any{"state": "incomplete", "milestone": "M1", "lanes": []any{
		map[string]any{"node": "n1", "state": "escalated", "reason": "blocked_scope"},
		map[string]any{"node": "n2", "state": "escalated", "reason": "land_review_needs_changes"},
	}})
	if err := h.RunFinished(ctx, testRun, "completed"); err != nil {
		t.Fatal(err)
	}
	sent = w.sender.parentSent()
	if len(sent) != 2 {
		t.Fatalf("the incomplete finish was not told: %+v", sent)
	}
	if !strings.Contains(sent[1].Text, "state=incomplete lanes=n1:escalated/blocked_scope,n2:escalated/land_review_needs_changes") || strings.Contains(sent[1].Text, "status=completed") {
		t.Errorf("the incomplete notice: %s", sent[1].Text)
	}
	rows = w.rows(KindCompletionNotice)
	if len(rows) != 2 || rows[1].Str("state") != "incomplete" || rows[1].Str("notice_key") == rows[0].Str("notice_key") {
		t.Fatalf("rows %+v", rows)
	}
	if lanes, _ := rows[1].Payload["lanes"].([]any); len(lanes) != 2 {
		t.Errorf("the row's lanes %+v", rows[1].Payload["lanes"])
	}
	// told once
	if err := h.RunFinished(ctx, testRun, "completed"); err != nil || len(w.sender.parentSent()) != 2 {
		t.Errorf("told again: %v %d", err, len(w.sender.parentSent()))
	}
}

// The key of a completion notice tells apart every outcome it could tell (CRW-1487): two outcomes that differ in any member (the
// escalation, the source, the milestone, the run state, a lane, the lanes beyond the cap) have two keys, so a resume that ends with an
// outcome that differs only there is told again; no outcome keeps the key of the rows and the status alone.
func TestCompletionKeyTellsEveryOutcomeMemberApart(t *testing.T) {
	events := []engine.Event{{RunID: testRun, Kind: engine.KindCall, Event: engine.EvCompleted, CallID: "c1"}}
	base := engine.Outcome{State: "escalated", Reason: "blocked_scope", Source: "lane", Escalation: "esc-1", Milestone: "M1", RunState: "incomplete",
		Lanes: []engine.OutcomeLane{{Node: "n1", State: "escalated", Reason: "blocked_scope"}}}
	variants := map[string]func(o *engine.Outcome){
		"escalation": func(o *engine.Outcome) { o.Escalation = "esc-2" },
		"source":     func(o *engine.Outcome) { o.Source = "design_audit" },
		"milestone":  func(o *engine.Outcome) { o.Milestone = "M2" },
		"run state":  func(o *engine.Outcome) { o.RunState = "closed" },
		"lanes more": func(o *engine.Outcome) { o.LanesMore = 1 },
		"lane": func(o *engine.Outcome) {
			o.Lanes = []engine.OutcomeLane{{Node: "n1", State: "escalated", Reason: "blocked"}}
		},
		// a member's value that reads as another member's: the members are named, so they do not run together
		"reason that reads as a source": func(o *engine.Outcome) { o.Reason, o.Source = "blocked_scope\x00source=lane", "" },
	}
	key := digestOfRows(events, "completed", &base)
	for name, change := range variants {
		o := base
		o.Lanes = append([]engine.OutcomeLane(nil), base.Lanes...)
		change(&o)
		if got := digestOfRows(events, "completed", &o); got == key {
			t.Errorf("an outcome that differs in %s keeps the key %s", name, key)
		}
	}
	same := base
	if digestOfRows(events, "completed", &same) != key {
		t.Error("the same outcome has another key")
	}
	if got, old := digestOfRows(events, "completed", nil), shortHash("c1", "status=completed"); got != old {
		t.Errorf("the key with no outcome moved: %s, was %s", got, old)
	}
}

// The completion notice of a process tells only the outcome of its own finished row (CRW-1487): a finished row of an older process,
// before the last started row, is not this process's end, so a finish told with no finished row after the last start carries no
// outcome and keeps the old text.
func TestRunFinishedTellsNoOutcomeOfAnOlderProcess(t *testing.T) {
	older := []engine.Event{
		{RunID: testRun, Kind: engine.KindRun, Event: "started", Data: map[string]any{}},
		{RunID: testRun, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": "completed", "outcome": map[string]any{"state": "escalated", "reason": "design_audit_rounds"}}},
		{RunID: testRun, Kind: engine.KindRun, Event: "started", Data: map[string]any{}},
	}
	if o := finishedOutcome(older); o != nil {
		t.Fatalf("the outcome of an older process is read for this one: %+v", o)
	}
	if o := finishedOutcome(older[:2]); o == nil || o.State != "escalated" {
		t.Fatalf("the outcome of this process's finished row: %+v", o)
	}

	w := newWorld(t)
	h := w.hook()
	w.finish("completed", map[string]any{"state": "escalated", "source": "design_audit", "reason": "design_audit_rounds"})
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindRun, Event: "started", Data: map[string]any{}})
	if err := w.journal.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := h.RunFinished(ctx, testRun, "completed"); err != nil {
		t.Fatal(err)
	}
	sent := w.sender.parentSent()
	if len(sent) != 1 || strings.Contains(sent[0].Text, "state=") || !strings.Contains(sent[0].Text, "status=completed") {
		t.Fatalf("the notice tells an older process's outcome: %+v", sent)
	}
	if rows := w.rows(KindCompletionNotice); len(rows) != 1 || rows[0].Str("state") != "" {
		t.Fatalf("rows %+v", rows)
	}
}

// The engine hands the run to the hook: at its start, after the batches its journal committed, and at its end; the summary document the
// command line named is recorded in the journal first, so the hook reads it from there. A resumed run finds the stream as it left it.
func TestEngineHandsTheRunToTheHook(t *testing.T) {
	w := newWorld(t)
	j := w.journal
	h := &countingHook{RunHook: w.hook()}
	prog, err := script.Load(script.Source{Name: "t.js", Text: []byte("export const meta = { name: 't' }\nconst a = await agent.call('say hi', { agentType: 'executor', label: 'hi' })\nreturn a.status\n")})
	if err != nil {
		t.Fatal(err)
	}
	res, err := engine.Run(ctx, prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy(), Journal: j, RunID: testRun, Upward: h, SummaryDocument: testDocument, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %+v", res)
	}
	if h.started != 1 || h.batches == 0 || h.finished != 1 || h.status != engine.RunCompleted {
		t.Fatalf("hook calls: started %d batches %d finished %d (%s)", h.started, h.batches, h.finished, h.status)
	}
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	docs := w.rows(engine.EvSummaryDocument)
	if len(docs) != 1 || docs[0].Str("document") != testDocument {
		t.Fatalf("document rows %+v", docs)
	}
	if entries := w.entries(); len(entries) == 0 || entries[len(entries)-1].Document != testDocument {
		t.Fatalf("entries %+v", entries)
	}
	if rows := w.rows(KindCompletionNotice); len(rows) != 1 {
		t.Errorf("completion notices %+v", rows)
	}
	// the document is recorded once for a resumed run that names the same document
	j2, err := journal.Open(ctx, w.st, testRun, journal.Identity{}, journal.Options{Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	h2 := &countingHook{RunHook: &RunHook{Store: w.st, Sched: w.sched, Journal: j2, Rows: JournalAppender{Journal: j2, RunID: testRun, Store: w.st}, Sender: w.sender, Run: testRun}}
	if _, err := engine.Run(ctx, prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy(), Journal: j2, RunID: testRun, Upward: h2, SummaryDocument: testDocument, Resume: &engine.ResumeOptions{}, Now: fixedNow}); err != nil {
		t.Fatal(err)
	}
	if err := j2.Flush(); err != nil {
		t.Fatal(err)
	}
	if h2.started != 1 || len(w.rows(engine.EvSummaryDocument)) != 1 {
		t.Errorf("resume: started %d documents %d", h2.started, len(w.rows(engine.EvSummaryDocument)))
	}
}

// countingHook counts the engine's calls and passes them on.
type countingHook struct {
	*RunHook
	started, batches, finished int
	status                     string
}

func (c *countingHook) RunStarted(ctx context.Context, run string) error {
	c.started++
	return c.RunHook.RunStarted(ctx, run)
}

func (c *countingHook) BatchCommitted(ctx context.Context, run string) error {
	c.batches++
	return c.RunHook.BatchCommitted(ctx, run)
}

func (c *countingHook) RunFinished(ctx context.Context, run, status string) error {
	c.finished++
	c.status = status
	return c.RunHook.RunFinished(ctx, run, status)
}

// A completion notice that is on record is not sent again: the queue path of send-parent writes the notice anew once the pump flushed it,
// and a notice that came back with another one would wake the parent a second time. The check is made before the send, not after it.
// (post-evaluation d8)
func TestRunFinishedDoesNotSendARecordedCompletionAgain(t *testing.T) {
	w := newWorld(t)
	h := w.hook()
	if err := h.RunFinished(ctx, testRun, "completed"); err != nil {
		t.Fatal(err)
	}
	// an unchanged resume finishes the run again, by a hook that never saw the first send (a new process)
	h2 := w.hook()
	if err := h2.RunFinished(ctx, testRun, "completed"); err != nil {
		t.Fatal(err)
	}
	if sent := w.sender.parentSent(); len(sent) != 1 {
		t.Fatalf("the completion went out %d times: %+v", len(sent), sent)
	}
	// another status is another completion
	if err := h2.RunFinished(ctx, testRun, "script_error"); err != nil {
		t.Fatal(err)
	}
	if sent := w.sender.parentSent(); len(sent) != 2 || len(w.rows(KindCompletionNotice)) != 2 {
		t.Errorf("sent %d, rows %d", len(sent), len(w.rows(KindCompletionNotice)))
	}
}
