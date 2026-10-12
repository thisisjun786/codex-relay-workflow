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
	probes := 0
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
	if probes != 0 || len(w.sender.supervisorSent()) != 0 {
		t.Errorf("the thread was looked at (%d) or a supervisor was told (%d)", probes, len(w.sender.supervisorSent()))
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
