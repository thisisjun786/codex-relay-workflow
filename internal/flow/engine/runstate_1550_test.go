package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// TestCRW1550StartNotificationFollowsDurableStart (CRW-1550 item 2): the Started callback fires exactly once, after the started
// row is committed, and never when the start cannot be recorded (a failing journal or a failed flush).
func TestCRW1550StartNotificationFollowsDurableStart(t *testing.T) {
	h := newHarness(t)
	calls, sawStarted := 0, false
	h.cfg.Started = func() {
		calls++
		for _, ev := range h.journal.Events() {
			if ev.Kind == KindRun && ev.Event == "started" {
				sawStarted = true
			}
		}
	}
	h.run(`return 1`)
	if calls != 1 || !sawStarted {
		t.Fatalf("the start notification: %d calls, started row visible %v", calls, sawStarted)
	}

	t.Run("a failing journal never notifies", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Journal = &errJournal{Journal: h.journal, err: errors.New("no store")}
		notified := 0
		h.cfg.Started = func() { notified++ }
		if _, err := Run(context.Background(), h.load(`return 1`), h.cfg); err == nil || notified != 0 {
			t.Fatalf("a run whose start cannot be recorded: err %v, %d notifications", err, notified)
		}
	})
	t.Run("a failed flush never notifies", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Journal = &failFlush{MemoryJournal: h.journal, err: errors.New("no sync")}
		notified := 0
		h.cfg.Started = func() { notified++ }
		if _, err := Run(context.Background(), h.load(`return 1`), h.cfg); err == nil || notified != 0 {
			t.Fatalf("a run whose start cannot be made durable: err %v, %d notifications", err, notified)
		}
	})
}

// errJournal reports its failure through FailingJournal (Err).
type errJournal struct {
	Journal
	err error
}

func (j *errJournal) Err() error { return j.err }

// failFlush fails its Flush only: the started row is appended but cannot be made durable.
type failFlush struct {
	*MemoryJournal
	err error
}

func (j *failFlush) Flush() error { return j.err }

// TestCRW1550DrainAfterDeadlineKeepsItsEnd (CRW-1550 item 5): a drain asked after a deadline pause is a no-op by design: no second
// run_paused ask and no answer-paused end is added.
func TestCRW1550DrainAfterDeadlineKeepsItsEnd(t *testing.T) {
	res, rows := drainRun(t, nil, func(ctl *Control) {
		ctl.Pause("deadline", "the run's deadline passed")
		ctl.Drain("answer", drainReason)
	})
	if got := fmt.Sprint(pausedAsks(rows)); got != "[deadline]" {
		t.Fatalf("the pause asks: %s, want only the deadline's", got)
	}
	if res.Status != RunCompleted {
		t.Fatalf("a deadline-paused run ends completed: %s", res.Status)
	}
	for _, ev := range rows {
		if ev.Kind == KindRun && ev.Event == "paused" && fmt.Sprint(ev.Data["by"]) == "answer" {
			t.Fatalf("a no-op drain added an answer-paused end: %+v", ev)
		}
	}
}
