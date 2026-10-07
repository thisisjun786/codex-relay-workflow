package delivery

import (
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
	busyFor(t, f, head, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// CRW-904 (correction, d4): the younger delivery arrives between the wake and the claim, which is
	// the interleaving the head priority exists for. The wake releases the head, not the line, so the
	// younger delivery is still not attempted.
	f.clock.Advance(1)
	younger := w.emit(0)
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

// CRW-904 (correction, d1): a woken head keeps its recipient's line until that wake is spent, whatever
// its own deadline. The wake exists to hand this delivery the turn the recipient's idle edge opened; a
// head whose deadline ran out before the claim would otherwise hand that turn to a younger delivery of
// the same recipient. The head is spent when it is claimed, so the line is held no longer than the
// attempt the wake released.
func TestIdleWake_a_woken_head_holds_the_line_past_its_own_deadline(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	head := w.emit(0)
	deadline := busyFor(t, f, head, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// CRW-904 (correction, d4): the younger delivery arrives between the wake and the claim, so the
	// test exercises the interleaving and not a row that was already waiting when the wake landed.
	f.clock.Advance(1)
	younger := w.emit(0)
	// The attempt is never made inside the backoff: the head's own deadline passes with the wake still
	// unspent, which is the state a scheduler budget or an error before the claim leaves behind.
	f.clock.T = deadline + 5
	if !slices.Contains(f.eligible(), head) {
		t.Fatal("a woken head past its own deadline is not due")
	}
	if slices.Contains(f.eligible(), younger) {
		t.Fatal("a younger delivery overtook a woken head whose deadline passed unclaimed")
	}
	// The recipient is idle when the attempt lands, and the head takes the turn, not the younger row.
	w.busy(false)
	w.pass()
	if got := w.dispatched(); len(got) != 1 || got[0] != head {
		t.Fatalf("the turn delivered %v, want only the woken head %s", got, head)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM delivery_wakes"); n != 0 {
		t.Fatal("the claimed attempt left its wake behind")
	}
	// The wake is spent, so the line it held is gone: the younger delivery is due on its own turn.
	if !slices.Contains(f.eligible(), younger) {
		t.Fatal("the younger delivery is not due after the wake was spent")
	}
}

// CRW-904 (correction, d3): a wake the delivery never spent does not outlive the wait it was written
// for. A woken attempt that is withheld before its send leaves deferred_busy without ever being
// claimed, so no attempt of it is outstanding and the deadline the wake carries is not one any later
// busy answer owes. If the row were kept, a later ordinary busy deferral would read it and take
// due = min(expired original, its own recomputed backoff), which is already in the past, and the
// delivery would retry without waiting the backoff it just computed.
func TestIdleWake_an_unspent_wake_does_not_survive_a_withhold(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	deadline := busyFor(t, f, event, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// The woken attempt finds the recipient archived, so the delivery is withheld before its send: the
	// state leaves deferred_busy and no attempt of this delivery is ever claimed.
	yes := true
	w.f.host.threads[scaleParent].archived = &yes
	w.pass()
	row := f.row(event)
	if row.S("state") != WithheldPreSend {
		t.Fatalf("the delivery is %s, want %s after the withhold", row.S("state"), WithheldPreSend)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM delivery_wakes"); n != 0 {
		t.Fatalf("the withheld delivery left %d wakes behind, want the wake ended with its wait", n)
	}
	// The recipient is active again and the delivery's recheck comes round after the original deadline,
	// where it finds the recipient busy once more. The backoff it computes now is the one it must wait.
	no := false
	w.f.host.threads[scaleParent].archived = &no
	w.busy(true)
	f.clock.T = row.F("next_eligible_at") + 1
	if f.clock.Now() <= deadline {
		t.Fatalf("the recheck at %.0f is not past the original deadline %.0f", f.clock.Now(), deadline)
	}
	w.pass()
	row = f.row(event)
	if row.S("state") != DeferredBusy {
		t.Fatalf("the delivery is %s, want %s after the busy answer", row.S("state"), DeferredBusy)
	}
	want := f.delivery.Policy.DelayFor(2, "busy")
	if got := row.F("next_eligible_at") - f.clock.Now(); got != want {
		t.Fatalf("the busy answer left the delivery due in %.0f s, want the busy curve's %.0f s: an expired wake was inherited", got, want)
	}
	if slices.Contains(f.eligible(), event) {
		t.Fatal("the delivery is due at once on an expired wake's deadline")
	}
}

// CRW-904 (correction, d2): the deadline a woken attempt carried survives an uncertain outcome and the
// busy answer that reconciles it, and it is taken through the real path rather than by writing the row:
// the wake is recorded, the attempt is claimed, the transport leaves the outcome unknown (the delivery
// is held with no deadline of its own), and a busy operation receipt is reconciled afterwards. The wake
// row the claim keeps carries the original deadline until that answer arrives, so due = min(original,
// recomputed) holds even though the attempt outlived it.
func TestIdleWake_a_reconciled_busy_answer_keeps_the_earlier_deadline(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	deadline := busyFor(t, f, event, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// The attempt is claimed and the transport leaves its outcome unknown: the delivery is held for the
	// parent with no deadline of its own, and the wake is kept for the answer that will follow.
	w.busy(false)
	f.host.script = []string{"transport_unknown"}
	w.pass()
	row := f.row(event)
	if row.S("state") != HeldUncertain || !row.N("next_eligible_at") {
		t.Fatalf("the uncertain attempt left the delivery %s/%.0f, want %s with no deadline", row.S("state"), row.F("next_eligible_at"), HeldUncertain)
	}
	wake := f.one("SELECT original_deadline, spent_at FROM delivery_wakes WHERE event_id = ?", event)
	if wake == nil || wake.F("original_deadline") != deadline {
		t.Fatalf("the wake does not carry the original deadline %.0f: %v", deadline, wake)
	}
	request := f.one("SELECT request_id FROM attempts WHERE event_id = ?", event).S("request_id")
	// The recipient answered busy after all, and that answer is reconciled after the original deadline
	// has passed: the earlier deadline is still the one the answer owes.
	f.clock.T = deadline + 120
	f.host.ledger[request] = Obj{{Key: "status", Value: FailedStatus}, {Key: "rpcError", Value: Obj{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "Thread is active"}}}, {Key: "retrySafe", Value: true}}
	rc := NewReconciler(f.delivery)
	if _, err := rc.ReconcileAttempt(f.ctx, request, f.host, at(f.clock.Now())); err != nil {
		t.Fatal(err)
	}
	row = f.row(event)
	if row.S("state") != DeferredBusy {
		t.Fatalf("the delivery is %s, want %s", row.S("state"), DeferredBusy)
	}
	if row.F("next_eligible_at") != deadline {
		t.Fatalf("the reconciled busy answer wrote %.0f, want the earlier deadline %.0f", row.F("next_eligible_at"), deadline)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM delivery_wakes"); n != 0 {
		t.Fatal("the answered attempt left its wake behind")
	}
}
