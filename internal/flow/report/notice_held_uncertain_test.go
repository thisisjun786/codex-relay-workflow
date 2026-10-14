package report

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

// CRW-1406 (Argon V5 P3 of 2741763e, relay.go stage): a run's notice (an escalation, the completion digest) has no delivery row the
// writer reads; its escalation_notice or completion_notice row is what the run records of it. A run process that staged the notice and
// died before it wrote that row, while the daemon's send of the message went held_uncertain, left a message the channel does not stage
// again: the next reconcile was answered no class (as a report's replay is), so it wrote no notice row, and every reconcile after it
// staged the notice again. A notice found held_uncertain is answered as the staged message it is, so the next reconcile writes its notice
// row once and none after it stages it again.

// diedBeforeTheNoticeRow is the run process that dies after the staging of a notice and before the row of it: the row is not written.
type diedBeforeTheNoticeRow struct{ Appender }

func (a diedBeforeTheNoticeRow) AppendOnce(ctx context.Context, row NewRow, key string) (bool, int64, error) {
	if row.Kind == KindEscalationNotice || row.Kind == KindCompletionNotice {
		return false, 0, errors.New("the run process died before the notice row")
	}
	return a.Appender.AppendOnce(ctx, row, key)
}

// stagingSender counts the stagings the relay sender is asked for, by logical id.
type stagingSender struct {
	RelaySender
	mu     sync.Mutex
	staged map[string]int
}

func (s *stagingSender) SendParent(ctx context.Context, p ParentSend) (SendResult, error) {
	s.mu.Lock()
	s.staged[p.LogicalID]++
	s.mu.Unlock()
	return s.RelaySender.SendParent(ctx, p)
}

func (s *stagingSender) count(logicalID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.staged[logicalID]
}

// heldUncertain moves the one supervisor message of the world to held_uncertain: the daemon sent it and nobody heard back.
func (w *world) heldUncertain() string {
	w.t.Helper()
	msgs := w.messages()
	if len(msgs) != 1 {
		w.t.Fatalf("%d messages, want 1", len(msgs))
	}
	if _, err := w.st.DB.ExecContext(ctx, "UPDATE supervisor_messages SET state=? WHERE message_id=?", delivery.HeldUncertain, msgs[0].MessageID); err != nil {
		w.t.Fatal(err)
	}
	return msgs[0].MessageID
}

func TestAnEscalationNoticeWhoseSendIsHeldUncertainIsRecordedOnceAndNotStagedAgain(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	w.escalate("esc_1", "tiers_exhausted", "A", "the executor-t1 tier ended with the same finding twice")
	died := w.hook()
	died.Sender = RelaySender{Store: w.st, Fallback: w.sender}
	died.Rows = diedBeforeTheNoticeRow{died.Rows}
	if err := died.BatchCommitted(ctx, testRun); err == nil {
		t.Fatal("the process that died before the notice row reconciled without an error")
	}
	if rows := w.rows(KindEscalationNotice); len(rows) != 0 {
		t.Fatalf("escalation rows %+v before the resume, want none", rows)
	}
	id := w.heldUncertain()

	// the resumed run (a new process) reconciles twice
	s := &stagingSender{RelaySender: RelaySender{Store: w.st, Fallback: w.sender}, staged: map[string]int{}}
	h := w.hook()
	h.Sender = s
	for i := 0; i < 2; i++ {
		if err := h.BatchCommitted(ctx, testRun); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.count("esc-esc_1"); n != 1 {
		t.Fatalf("the escalation was staged %d times by the resumed run, want once (the reconcile that writes its notice row)", n)
	}
	rows := w.rows(KindEscalationNotice)
	if len(rows) != 1 || rows[0].Str("escalation_id") != "esc_1" || rows[0].Str("result") != ResultQueued || rows[0].Str("request_id") != id ||
		rows[0].Str("thread") != testManagement {
		t.Fatalf("escalation rows %+v, want one queued under %s for %s", rows, id, testManagement)
	}
	if msgs := w.messages(); len(msgs) != 1 || msgs[0].State != delivery.HeldUncertain {
		t.Fatalf("the messages are %+v, want the one held_uncertain left as it is", msgs)
	}
	if len(w.sender.parentSent()) != 0 {
		t.Fatalf("the notice went through send-parent: %+v", w.sender.parentSent())
	}
}

func TestACompletionNoticeWhoseSendIsHeldUncertainIsRecordedOnceAndNotStagedAgain(t *testing.T) {
	w := newWorld(t)
	w.manage(testManagement)
	died := w.hook()
	died.Sender = RelaySender{Store: w.st, Fallback: w.sender}
	died.Rows = diedBeforeTheNoticeRow{died.Rows}
	if err := died.RunFinished(ctx, testRun, engine.RunCompleted); err == nil {
		t.Fatal("the process that died before the notice row finished without an error")
	}
	if rows := w.rows(KindCompletionNotice); len(rows) != 0 {
		t.Fatalf("completion rows %+v before the resume, want none", rows)
	}
	id := w.heldUncertain()
	msg, err := w.st.SupervisorMessage(ctx, id)
	if err != nil || msg.ObligationKind != supervisor.KindFlowCompletion {
		t.Fatalf("the message %+v (%v), want the completion", msg, err)
	}

	// the resumed run (a new process) finishes the run twice in the same state
	s := &stagingSender{RelaySender: RelaySender{Store: w.st, Fallback: w.sender}, staged: map[string]int{}}
	h := w.hook()
	h.Sender = s
	for i := 0; i < 2; i++ {
		if err := h.RunFinished(ctx, testRun, engine.RunCompleted); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for _, n := range s.staged {
		total += n
	}
	if total != 1 {
		t.Fatalf("the completion was staged %d times by the resumed run (%v), want once (the finish that writes its notice row)", total, s.staged)
	}
	rows := w.rows(KindCompletionNotice)
	if len(rows) != 1 || rows[0].Str("status") != engine.RunCompleted || rows[0].Str("result") != ResultQueued || rows[0].Str("request_id") != id {
		t.Fatalf("completion rows %+v, want one queued under %s", rows, id)
	}
	if msgs := w.messages(); len(msgs) != 1 || msgs[0].State != delivery.HeldUncertain {
		t.Fatalf("the messages are %+v, want the one held_uncertain left as it is", msgs)
	}
}
