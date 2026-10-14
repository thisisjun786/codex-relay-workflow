package report

import "testing"

// CRW-1390 (Argon V5 P3 of bd2d68b6, hook.go): the escalation_notice row names the thread the notice was addressed to. Through the
// supervisor channel that is the level above the project (here the management seat's owner), not the orchestrator thread the run works
// for; a notice that goes through the fallback (crw manage send-parent) is addressed to the orchestrator thread, which the row names.
func TestEscalationNoticeNamesTheThreadItWasStagedFor(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	h := w.hook()
	h.Sender = RelaySender{Store: w.st, Fallback: w.sender}
	w.escalate("esc_1", "tiers_exhausted", "A", "the executor-t1 tier ended with the same finding twice")
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	rows := w.rows(KindEscalationNotice)
	if len(rows) != 1 || rows[0].Str("result") != "queued" {
		t.Fatalf("escalation rows %+v", rows)
	}
	if got := rows[0].Str("thread"); got != testManagement {
		t.Fatalf("the escalation_notice row names thread %q, want the management seat's owner %q the notice was staged for", got, testManagement)
	}
	messages := w.messages()
	if len(messages) != 1 || messages[0].RecipientTaskID != rows[0].Str("thread") {
		t.Fatalf("the row names %q and the message goes to %+v", rows[0].Str("thread"), messages)
	}
}

func TestEscalationNoticeThroughTheFallbackNamesTheOrchestratorThread(t *testing.T) {
	w := newWorld(t)
	h := w.hook()
	h.Sender = w.sender
	w.escalate("esc_1", "tiers_exhausted", "A", "the executor-t1 tier ended with the same finding twice")
	if err := h.BatchCommitted(ctx, testRun); err != nil {
		t.Fatal(err)
	}
	rows := w.rows(KindEscalationNotice)
	if len(rows) != 1 || rows[0].Str("thread") != testOrch {
		t.Fatalf("escalation rows %+v, want one naming the orchestrator thread %q", rows, testOrch)
	}
}
