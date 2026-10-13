package supervisor

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-1334 (evaluation bbcacfcf d1): a release does not reset the busy count. A message that reached
// busy_cap through real busy answers, released by its recipient's edge, is held again by the very next
// busy answer, and the next edge releases it again.
func TestSupervisorWake_a_released_busy_cap_message_is_held_again_by_the_next_busy_answer(t *testing.T) {
	t.Parallel()
	f, id := busyMessage(t)
	busy := &sendHost{status: "active"}
	at := wakeNow
	answers := 1
	for {
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.HoldReason.Valid {
			if row.HoldReason.String != delivery.BusyCap {
				t.Fatalf("held %s after %d busy answers, want busy_cap", row.HoldReason.String, answers)
			}
			break
		}
		if answers > 40 {
			t.Fatalf("no busy_cap hold after %d busy answers", answers)
		}
		at = row.NextEligibleAt.Float64
		if _, err := f.c.Attempt(f.ctx, id, busy, at); err != nil {
			t.Fatal(err)
		}
		answers++
	}
	if answers != int(delivery.DefaultPolicy().BusyMaxAttempts) {
		t.Fatalf("busy_cap after %d busy answers, want %d", answers, delivery.DefaultPolicy().BusyMaxAttempts)
	}
	for round := 1; round <= 2; round++ {
		edge := at + float64(round)
		got, err := f.c.WakeBusyRecipient(f.ctx, "supervisor", "idle", edge)
		if err != nil || got.Released != 1 {
			t.Fatalf("edge %d over busy_cap: %+v %v, want one release", round, got, err)
		}
		// Released and due at the edge, the message meets the recipient busy again at that instant: the
		// count was not reset, so that one answer holds it at once, with no backoff step in between.
		if _, err := f.c.Attempt(f.ctx, id, busy, edge); err != nil {
			t.Fatal(err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil || row.HoldReason.String != delivery.BusyCap || row.ClaimableAt(edge+100000) {
			t.Fatalf("after release %d and one busy answer: %+v %v, want busy_cap again and never due on a timer", round, row, err)
		}
		if n := len(journalOf(t, f, "supervisor_message_deferred", id)); n != answers+round {
			t.Fatalf("%d busy answers after release %d, want %d", n, round, answers+round)
		}
		if n := len(journalOf(t, f, "supervisor_message_released", id)); n != round {
			t.Fatalf("%d supervisor_message_released rows after edge %d, want %d", n, round, round)
		}
		holds, err := f.c.IdleWakeRecipients(f.ctx, edge)
		if err != nil || len(holds) != 1 || holds[0].RecipientTaskID != "supervisor" || holds[0].Deadline != 0 {
			t.Fatalf("the re-held message's recipient: %+v %v, want it kept for the next edge with no deadline", holds, err)
		}
	}
}
