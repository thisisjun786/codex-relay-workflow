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
