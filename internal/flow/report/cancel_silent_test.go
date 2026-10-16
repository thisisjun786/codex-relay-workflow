package report

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"testing"
)

func TestCancelledRunSendsNoCompletionOrPendingEscalation(t *testing.T) {
	w := newWorld(t)
	w.supervise(testLead)
	w.escalate("esc-cancel", "dispatch_unknown", "A", "stopped by the operator")
	w.journal.Append(engine.Event{RunID: testRun, Kind: engine.KindRun, Event: "finished", Data: map[string]any{"status": engine.RunScriptError, "error_kind": engine.ErrKindCancelled}})
	if err := w.journal.Flush(); err != nil {
		t.Fatal(err)
	}
	h := w.hook()
	for i := 0; i < 2; i++ {
		if err := h.RunFinished(ctx, testRun, engine.RunScriptError); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(w.sender.parentSent()) + len(w.sender.supervisorSent()); n != 0 {
		t.Errorf("cancel woke upper layers %d times; want zero", n)
	}
	if n := len(w.rows(KindCompletionNotice)) + len(w.rows(KindEscalationNotice)); n != 0 {
		t.Errorf("cancel queued %d notices; want zero", n)
	}
}
