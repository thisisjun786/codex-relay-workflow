package delivery

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// CRW-904: a delivery that waits out its recipient's busy backoff is released by that recipient's
// own idle edge rather than only by the timer. The world is the one busy_order_test.go builds: one
// parent, one relationship per index, the daemon's delivery pass over a fake host, and a busy parent
// is the fake host's status "active".
//
// The wake is the relay's own record (delivery_wakes), written by WakeBusyHead from the report the
// App Server pushed. These tests drive the delivery side: the daemon's reading of the notification
// channel is daemon/idle_wake_test.go and the subscription itself is bridge/appserver's.

// wake is the relay applying one idle report for a recipient thread at the clock's instant.
func (w *scaleWorld) wake(thread string) bool {
	w.t.Helper()
	woke, err := w.f.delivery.WakeBusyHead(w.f.ctx, thread, w.f.clock.Now())
	mustDo(w.t, err)
	return woke
}

// woken is whether the delivery still carries an unspent wake.
func (f *fixture) woken(event string) bool {
	f.t.Helper()
	woken, err := f.delivery.Woken(f.ctx, event)
	mustDo(f.t, err)
	return woken
}

// busyFor has the busy parent answer n attempts and leaves the clock inside the backoff the last
// answer set, n*BusyBase... seconds from the deadline: the state the idle edge is for.
func busyFor(t *testing.T, f *fixture, event string, answers int64, remaining float64) float64 {
	t.Helper()
	for answer := int64(1); answer <= answers; answer++ {
		answerBusy(t, f, event, answer)
	}
	deadline := f.row(event).F("next_eligible_at")
	f.clock.T = deadline - remaining
	return deadline
}

func TestIdleWake_a_head_300_seconds_from_its_deadline_is_due_when_its_recipient_reports_idle(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	// Six busy answers reach BusyMax, so the head is exactly the ceiling from its deadline.
	busyFor(t, f, event, 6, f.delivery.Policy.BusyMax)
	if got := f.row(event).F("next_eligible_at") - f.clock.Now(); got != f.delivery.Policy.BusyMax {
		t.Fatalf("the head waits %.0f s, want the ceiling %.0f s", got, f.delivery.Policy.BusyMax)
	}
	if slices.Contains(f.eligible(), event) {
		t.Fatal("the head is due before the idle report")
	}
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	if !slices.Contains(f.eligible(), event) {
		t.Fatal("the woken head is still not due")
	}
	// The scheduler attempts it at this parent's next turn, not at the deadline 300 s away.
	w.busy(false)
	if left := f.row(event).F("next_eligible_at") - f.clock.Now(); left != f.delivery.Policy.BusyMax {
		t.Fatalf("the head is %.0f s from its deadline at the attempt, want the ceiling %.0f s", left, f.delivery.Policy.BusyMax)
	}
	w.pass()
	if got := w.dispatched(); len(got) != 1 || got[0] != event {
		t.Fatalf("the next turn delivered %v, want the woken head %s", got, event)
	}
}

func TestIdleWake_a_report_for_another_recipient_wakes_nothing(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 2)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	busyFor(t, f, event, 2, 10)
	if w.wake(w.rels[1].child) {
		t.Fatal("a report for another thread woke this recipient's head")
	}
	if w.wake("01a-thread-this-relay-never-saw") {
		t.Fatal("a report for an unknown thread woke a head")
	}
	if slices.Contains(f.eligible(), event) {
		t.Fatal("the head is due without a report for its own recipient")
	}
}

func TestIdleWake_a_report_the_relay_never_saw_leaves_the_head_at_its_deadline(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	deadline := busyFor(t, f, event, 1, 10)
	w.busy(false)
	w.pass()
	if sent := len(f.host.sends); sent != 0 {
		t.Fatalf("%d messages went out before the deadline", sent)
	}
	f.clock.T = deadline
	w.pass()
	if got := w.dispatched(); len(got) != 1 || got[0] != event {
		t.Fatalf("the head was not attempted at its own deadline: %v", got)
	}
}

func TestIdleWake_a_woken_head_holds_the_line_until_it_is_claimed(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	head := w.emit(0)
	w.f.clock.Advance(1)
	younger := w.emit(0)
	busyFor(t, f, head, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// The wake releases the head, not the line: the younger delivery is still not attempted.
	if got := f.eligible(); len(got) != 1 || got[0] != head {
		t.Fatalf("eligible rows are %v, want only the woken head %s", got, head)
	}
	w.busy(false)
	w.pass()
	if got := w.dispatched(); len(got) != 1 || got[0] != head {
		t.Fatalf("the turn delivered %v, want only the head %s", got, head)
	}
	if row := f.row(younger); row.S("state") == Dispatched {
		t.Fatal("the younger delivery was delivered in the same turn as the woken head")
	}
}

func TestIdleWake_a_woken_attempt_that_meets_busy_again_keeps_the_earlier_deadline(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	deadline := busyFor(t, f, event, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// The recipient is busy again when the attempt lands.
	w.busy(true)
	w.pass()
	row := f.row(event)
	if row.F("next_eligible_at") != deadline {
		t.Fatalf("the busy answer moved the deadline to %.0f, want the original %.0f", row.F("next_eligible_at"), deadline)
	}
	if row.S("state") != DeferredBusy {
		t.Fatalf("the delivery is %s, want %s", row.S("state"), DeferredBusy)
	}
	if f.woken(event) {
		t.Fatal("the busy answer did not spend the wake")
	}
}

func TestIdleWake_a_busy_answer_after_the_claim_keeps_the_earlier_deadline(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	deadline := busyFor(t, f, event, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// The race the issue names: the lifecycle read says idle, and the transport refuses the send
	// as busy after the claim.
	w.busy(false)
	f.host.script = []string{"busy"}
	w.pass()
	row := f.row(event)
	if row.S("state") != DeferredBusy {
		t.Fatalf("the delivery is %s, want %s", row.S("state"), DeferredBusy)
	}
	if row.F("next_eligible_at") > deadline {
		t.Fatalf("the busy answer pushed the deadline to %.0f, later than the original %.0f", row.F("next_eligible_at"), deadline)
	}
}

func TestIdleWake_a_woken_head_never_interrupts_a_busy_recipient(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	busyFor(t, f, event, 2, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	w.pass()
	if sent := len(f.host.sends); sent != 0 {
		t.Fatalf("%d messages were sent to a busy recipient", sent)
	}
	if row := f.row(event); row.I("attempt_count") != 0 {
		t.Fatalf("a busy recipient was claimed against: attempt_count %d", row.I("attempt_count"))
	}
}

func TestIdleWake_the_claim_spends_the_wake(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	busyFor(t, f, event, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	if !f.woken(event) {
		t.Fatal("the wake was not recorded")
	}
	w.busy(false)
	w.pass()
	if f.woken(event) {
		t.Fatal("the claim left the wake unspent")
	}
	// A second report for the same recipient wakes nothing: the delivery is no longer waiting.
	if w.wake(scaleParent) {
		t.Fatal("a report after the delivery woke a head")
	}
}

func TestIdleWake_only_a_waiting_head_is_woken(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	event := w.emit(0)
	// A queued delivery is not waiting out a busy backoff, so an idle report changes nothing.
	if w.wake(scaleParent) {
		t.Fatal("a report woke a delivery that was never deferred as busy")
	}
	if !slices.Contains(f.eligible(), event) {
		t.Fatal("a queued delivery should already be due")
	}
}

// CRW-904 (review): a woken head holds the line only while it is inside its backoff, which is the rule
// for every waiting head. Once its own deadline has run out it is due like every other row, so a head
// that keeps failing before its claim cannot keep its recipient's younger deliveries waiting past that
// deadline: a line is held for one backoff at a time (I-478).
func TestIdleWake_a_woken_head_holds_the_line_only_inside_its_backoff(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	head := w.emit(0)
	f.clock.Advance(1)
	younger := w.emit(0)
	deadline := busyFor(t, f, head, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	if slices.Contains(f.eligible(), younger) {
		t.Fatal("the younger delivery is due while the woken head waits out its backoff")
	}
	// The head is never claimed: its attempt keeps failing before the claim, so the wake row stays.
	f.clock.T = deadline
	if !slices.Contains(f.eligible(), younger) {
		t.Fatal("a woken head that was never claimed still holds the line past its own deadline")
	}
}

// CRW-904 (review): the reconciler's busy arm takes the same minimum, and it passes a numeric instant
// to the CASE. next_eligible_at is REAL and SQLite orders every number before every text, so the ISO
// string the reconciler also carries would never be exceeded and the earlier deadline would be lost.
func TestIdleWake_a_reconciled_busy_answer_keeps_the_earlier_deadline(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	f.host.script = []string{"in_progress"}
	first := f.mustAttempt(event, nil)
	request := first.Get("requestId").(string)
	// The woken delivery carried this deadline before the wake and the claim left it alone; it is
	// still ahead, and earlier than the backoff this busy answer recomputes.
	original := f.clock.Now() + 5
	mustDo(t, f.store.Transaction(f.ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, f.store, "UPDATE deliveries SET next_eligible_at = ? WHERE event_id = ?", original, event)
		return err
	}))
	attempt, err := one(f.ctx, f.store, "SELECT * FROM attempts WHERE request_id = ?", request)
	mustDo(t, err)
	receipt := Obj{{Key: "status", Value: FailedStatus}, {Key: "rpcError", Value: Obj{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "Thread is active"}}}, {Key: "retrySafe", Value: true}}
	if facts := Classify(receipt); facts.DeliveryState != DeferredBusy {
		t.Fatalf("the fixture receipt is not a busy answer: %+v", facts)
	}
	rc := NewReconciler(f.delivery)
	if _, err := rc.settleFromReceipt(f.ctx, attempt, f.row(event), Classify(receipt), ConfirmedPreSendRejection, "fixture", f.clock.Now(), false, nil, nil); err != nil {
		t.Fatal(err)
	}
	row := f.row(event)
	if row.S("state") != DeferredBusy {
		t.Fatalf("the delivery is %s, want %s", row.S("state"), DeferredBusy)
	}
	if row.F("next_eligible_at") != original {
		t.Fatalf("the reconciled busy answer wrote %.0f, want the earlier deadline %.0f", row.F("next_eligible_at"), original)
	}
}
