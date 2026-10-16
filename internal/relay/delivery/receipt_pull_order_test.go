package delivery

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"testing"
)

func TestReceiptPullSkipsUnpullablePredecessors(t *testing.T) {
	for _, kind := range []string{"busy_cap", "stale_generation", "superseded_revision", "withheld"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, "")
			if kind == "busy_cap" {
				f.delivery.Policy.BusyMaxAttempts = 1
			}
			older := f.busyCompletion(t, pullTurn)
			f.clock.Advance(5)
			if kind == "stale_generation" {
				f.advanceTo(2)
			}
			if kind == "withheld" {
				_, err := execSQL(f.ctx, f.store, "UPDATE deliveries SET state=? WHERE event_id=?", WithheldPreSend, older)
				mustDo(t, err)
			}
			var younger string
			if kind == "superseded_revision" {
				n := f.readyPayload(f.rid, 1, []string{f.artifact("newer.txt", "corrected deliverable")}, 2, assigned("completed"))
				_, err := f.accept(n, store.AcceptOptions{})
				mustDo(t, err)
				younger = pyjson.Text(n.Get("eventId"))
				f.declare(younger, older)
				_, err = f.delivery.Enqueue(f.ctx, younger, "", "")
				mustDo(t, err)
			} else {
				younger = f.queuedEvent(regOpts{issue: "REL-2", dispatchRequest: "dispatch-2"})
			}
			_, err := f.pull(older, pullTurn, parent, parent)
			requireReason(t, err, PullNotPullable)
			_, err = f.pull(younger, pullTurn, parent, parent)
			mustDo(t, err)
		})
	}
}
