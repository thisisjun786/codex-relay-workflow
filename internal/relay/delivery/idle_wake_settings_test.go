package delivery

import "testing"

// CRW-1007 (decision 4): a woken attempt that the settings check withholds ends its wake. withholdSettings
// calls abandonWake before it records the refusal, so the wake the head held does not outlive the withhold.
func TestIdleWake_a_woken_attempt_withheld_for_settings_ends_its_wake(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	busyFor(t, f, event, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// The recipient has no authorized settings when the woken attempt runs, so withholdSettings withholds it
	// before the send and the wake is ended with the wait.
	w.busy(false)
	if _, err := execSQL(f.ctx, f.store, "DELETE FROM authorized_settings WHERE task_id = ?", scaleParent); err != nil {
		t.Fatal(err)
	}
	w.pass()
	if row := f.row(event); row.S("state") != WithheldPreSend {
		t.Fatalf("the delivery is %s, want %s after the settings withhold", row.S("state"), WithheldPreSend)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM delivery_wakes"); n != 0 {
		t.Fatalf("the settings withhold left %d wakes behind", n)
	}
}

// CRW-1007 (failure class 1, replay): a busy answer that settled an attempt keeps the deadline it settled with
// when the same attempt is reconciled again after its wake was spent. The replay must not promote the attempt
// a second time with a later deadline.
func TestIdleWake_a_replayed_busy_settlement_keeps_its_deadline(t *testing.T) {
	t.Parallel()
	w := newScaleWorld(t, 1)
	f := w.f
	w.busy(true)
	event := w.emit(0)
	deadline := busyFor(t, f, event, 1, 10)
	if !w.wake(scaleParent) {
		t.Fatal("the idle report woke no head")
	}
	// The woken attempt is claimed and the transport answers busy: the lifecycle read saw the recipient idle.
	w.busy(false)
	f.host.script = []string{"busy"}
	w.pass()
	row := f.row(event)
	if row.S("state") != DeferredBusy || row.F("next_eligible_at") != deadline {
		t.Fatalf("the busy answer left the delivery %s/%.0f, want %s/%.0f", row.S("state"), row.F("next_eligible_at"), DeferredBusy, deadline)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM delivery_wakes"); n != 0 {
		t.Fatalf("the settled attempt left %d wakes behind", n)
	}
	request := f.one("SELECT request_id FROM attempts WHERE event_id = ?", event).S("request_id")
	f.host.ledger[request] = Obj{{Key: "status", Value: FailedStatus}, {Key: "rpcError", Value: Obj{{Key: "code", Value: "thread_busy"}, {Key: "message", Value: "Thread is active"}}}, {Key: "retrySafe", Value: true}}
	f.clock.T = deadline + 1
	if _, err := NewReconciler(f.delivery).ReconcileAttempt(f.ctx, request, f.host, at(f.clock.Now())); err != nil {
		t.Fatal(err)
	}
	if row = f.row(event); row.F("next_eligible_at") != deadline {
		t.Fatalf("the replayed busy answer moved the deadline to %.0f, want the settled %.0f", row.F("next_eligible_at"), deadline)
	}
}
