package daemon

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1349 (CRW-1334 follow-up 1): an idle or notLoaded report is taken off the host's channel before
// it is applied, and a busy_cap or attempt_cap hold has no timer, so a report the idle pass drained and
// then dropped left the hold standing until the recipient's next edge or a readback. A store failure
// that is not corruption still ends the tick on the failed write (I-564: nothing after it writes), but
// the reports of that batch the pass did not apply - the failing one and every one after it - are
// applied at the next tick instead of being lost.
func TestIdleWake_reports_a_failed_wake_did_not_apply_are_applied_on_the_next_tick(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, trigger string }{
		// The delivery wake is the first write of the supervisor's report: it fails, and the supervisor wake
		// and the later report are never reached.
		{"the delivery wake fails", "BEFORE INSERT ON delivery_wakes"},
		// The delivery wake of the supervisor's report is written, the supervisor wake fails, and the later
		// report is never reached.
		{"the supervisor wake fails", "BEFORE UPDATE ON supervisor_messages"},
	} {
		for _, hold := range []string{delivery.BusyCap, delivery.AttemptCap} {
			t.Run(c.name+", "+hold, func(t *testing.T) {
				t.Parallel()
				host := &idleHost{}
				d, s, id := supervisorWakeDaemon(t, host)
				// The staged message meets its recipient busy and is then held at the cap: only an edge
				// releases it. The same recipient has a delivery head waiting, and another recipient has one
				// whose report comes later in the batch.
				tickAt(t, d, idleNow)
				exec(t, s, "UPDATE supervisor_messages SET hold_reason = ? WHERE message_id = ?", hold, id)
				seedSharedHead(t, s, idleNow+300)
				seedHeadFor(t, s, "later", "parent-later", idleNow+300)
				tickAt(t, d, idleNow+1)
				tries := host.holdTries
				host.reports = []delivery.IdleReport{{ThreadID: supWakeRecipient, Status: "idle"}, {ThreadID: "parent-later", Status: "notLoaded"}}
				// A failure that is not corruption (a constraint abort, SQLITE_CONSTRAINT_TRIGGER) of the
				// idle pass's own write.
				d.idle.beforeIdle = func() {
					exec(t, s, "CREATE TRIGGER crw1349_injected "+c.trigger+" BEGIN SELECT RAISE(ABORT, 'injected store failure'); END")
				}
				d.Clock.(*delivery.FakeClock).T = idleNow + 2
				if _, err := d.Tick(context.Background()); err == nil {
					t.Fatal("the failed wake write did not end the tick with its error")
				}
				d.idle.beforeIdle = nil
				if state := store.HaltStateAt(s.Path); state.Present || d.haltedStore {
					t.Fatalf("a failure that is not corruption halted the store: %+v", state.Marker)
				}
				// The tick ended on the failed write: no later report was applied and the hold pass did not run.
				if host.holdTries != tries {
					t.Fatalf("the hold pass ran after the failed wake: %d tries, want %d", host.holdTries, tries)
				}
				if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'supervisor_message_released' AND subject = ?", id); n != 0 {
					t.Fatalf("%d supervisor_message_released rows in the failed tick, want none", n)
				}
				if n := count(t, s, "SELECT COUNT(*) FROM delivery_wakes WHERE event_id = 'ev-later'"); n != 0 {
					t.Fatal("the report after the failed wake was applied in the failed tick")
				}
				if len(host.reports) != 0 {
					t.Fatalf("the failed tick did not drain the host's reports: %v", host.reports)
				}
				exec(t, s, "DROP TRIGGER crw1349_injected")
				// The host has nothing new to report: the next tick applies what the failed one drained.
				tickAt(t, d, idleNow+3)
				if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'supervisor_message_released' AND subject = ?", id); n != 1 {
					t.Fatalf("%d supervisor_message_released rows after the next tick, want the %s hold released by the drained report", n, hold)
				}
				// Both heads were woken and attempted in that tick, before their +300 deadlines: each met its
				// recipient busy once.
				for _, event := range []string{"ev-shared", "ev-later"} {
					if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'delivery_deferred_busy' AND subject = ?", event); n != 1 {
						t.Fatalf("%s has %d busy answers after the next tick, want the drained report to have woken it", event, n)
					}
				}
				// A report is applied once: a later tick with nothing new wakes nothing again.
				tickAt(t, d, idleNow+4)
				for _, event := range []string{"ev-shared", "ev-later"} {
					if n := count(t, s, "SELECT COUNT(*) FROM journal WHERE kind = 'delivery_deferred_busy' AND subject = ?", event); n != 1 {
						t.Fatalf("%s has %d busy answers after a tick with no report, want the carried report applied once", event, n)
					}
				}
			})
		}
	}
}
