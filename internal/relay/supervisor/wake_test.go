package supervisor

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1334 (CRW-1314 design section 3): a supervisor message that found its recipient busy waited
// out its doubling backoff (15 s up to 300 s) with nothing to end the wait early, and after forty
// busy answers it was held at busy_cap with nothing at all to release it. The recipient's idle or
// notLoaded edge now makes such a message due at once, and an idle edge or a verified readback of the
// recipient releases a busy_cap or attempt_cap hold, each with a journal row.

const wakeNow = 1_700_000_000.0

// busyMessage stages the fixture's report and attempts it once against a busy recipient, so the
// message carries the real busy deferral: deferred_busy, BusyBase from now.
func busyMessage(t *testing.T) (*stageFixture, string) {
	t.Helper()
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	if _, err := f.c.Attempt(f.ctx, id, &sendHost{status: "active"}, wakeNow); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "deferred_busy" || !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 != wakeNow+15 || row.HoldReason.Valid {
		t.Fatalf("the busy attempt left %+v, want deferred_busy due at now+15 with no hold", row)
	}
	return f, id
}

// journalOf is every journal row of kind for subject, oldest first, with its detail decoded.
func journalOf(t *testing.T, f *stageFixture, kind, subject string) []map[string]any {
	t.Helper()
	rows, err := f.s.All(f.ctx, "SELECT detail FROM journal WHERE kind=? AND subject=? ORDER BY seq", kind, subject)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, row := range rows {
		var detail map[string]any
		if err := json.Unmarshal([]byte(row.Text("detail")), &detail); err != nil {
			t.Fatalf("%s detail %q: %v", kind, row.Text("detail"), err)
		}
		out = append(out, detail)
	}
	return out
}

func TestSupervisorWake_an_idle_edge_makes_a_busy_deferred_message_due(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"idle", "notLoaded"} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()
			f, id := busyMessage(t)
			// A report about another thread wakes nothing.
			if got, err := f.c.WakeBusyRecipient(f.ctx, "someone-else", status, wakeNow+1); err != nil || got.Woken != 0 || got.Released != 0 {
				t.Fatalf("a report for another thread: %+v %v, want nothing", got, err)
			}
			got, err := f.c.WakeBusyRecipient(f.ctx, "supervisor", status, wakeNow+1)
			if err != nil || got.Woken != 1 || got.Released != 0 {
				t.Fatalf("the recipient's %s edge: %+v %v, want one message woken", status, got, err)
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != "deferred_busy" || row.HoldReason.Valid || !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 != wakeNow+1 {
				t.Fatalf("the woken message is %+v, want deferred_busy due at the edge", row)
			}
			woken := journalOf(t, f, "supervisor_message_woken", id)
			if len(woken) != 1 || woken[0]["status"] != status || woken[0]["thread"] != "supervisor" || woken[0]["previousEligibleAt"] != wakeNow+15 || woken[0]["eligibleAt"] != wakeNow+1 {
				t.Fatalf("supervisor_message_woken journal %v, want one row naming the edge and both deadlines", woken)
			}
			// The message is already due: a second edge writes nothing.
			if got, err := f.c.WakeBusyRecipient(f.ctx, "supervisor", status, wakeNow+2); err != nil || got.Woken != 0 {
				t.Fatalf("a second edge: %+v %v, want nothing", got, err)
			}
			if n := len(journalOf(t, f, "supervisor_message_woken", id)); n != 1 {
				t.Fatalf("%d woken rows after the second edge, want 1", n)
			}
			// And the woken message is attempted before its own deadline: an idle recipient takes it.
			host := &sendHost{status: "idle"}
			result, err := f.c.Attempt(f.ctx, id, host, wakeNow+2)
			if err != nil || result == nil || result["deliveryState"] != "dispatched" || len(host.sends) != 1 {
				t.Fatalf("the woken attempt: %v %v, want it dispatched", result, err)
			}
		})
	}
}

func TestSupervisorWake_an_idle_edge_does_not_touch_other_states_or_holds(t *testing.T) {
	t.Parallel()
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	// A queued message is not waiting on a busy recipient: nothing moves.
	if got, err := f.c.WakeBusyRecipient(f.ctx, "supervisor", "idle", wakeNow); err != nil || got.Woken != 0 || got.Released != 0 {
		t.Fatalf("a queued message: %+v %v, want nothing", got, err)
	}
	// The holds this lane does not own stay exactly where they are.
	for _, hold := range []string{store.SupervisorHoldSuperseded, store.SupervisorHoldUnaddressed} {
		if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET state='deferred_busy',hold_reason=?,next_eligible_at=? WHERE message_id=?", hold, wakeNow+300, id); err != nil {
			t.Fatal(err)
		}
		if got, err := f.c.WakeBusyRecipient(f.ctx, "supervisor", "idle", wakeNow); err != nil || got.Woken != 0 || got.Released != 0 {
			t.Fatalf("a message held %s: %+v %v, want nothing", hold, got, err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil || row.HoldReason.String != hold || row.NextEligibleAt.Float64 != wakeNow+300 {
			t.Fatalf("the %s hold moved: %+v %v", hold, row, err)
		}
	}
}

func TestSupervisorWake_an_idle_edge_releases_busy_cap_and_attempt_cap_holds(t *testing.T) {
	t.Parallel()
	for _, hold := range []string{delivery.BusyCap, delivery.AttemptCap} {
		t.Run(hold, func(t *testing.T) {
			t.Parallel()
			f, id := busyMessage(t)
			if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET hold_reason=?,next_eligible_at=? WHERE message_id=?", hold, wakeNow+300, id); err != nil {
				t.Fatal(err)
			}
			// Held, the message is claimable at no instant: its timer never releases it.
			row, err := f.c.Get(f.ctx, id)
			if err != nil || row.ClaimableAt(wakeNow+100000) {
				t.Fatalf("a %s message is claimable on its timer: %+v %v", hold, row, err)
			}
			got, err := f.c.WakeBusyRecipient(f.ctx, "supervisor", "idle", wakeNow+5)
			if err != nil || got.Released != 1 || got.Woken != 0 {
				t.Fatalf("the idle edge over a %s hold: %+v %v, want one release", hold, got, err)
			}
			row, err = f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.HoldReason.Valid || !row.NextEligibleAt.Valid || row.NextEligibleAt.Float64 != wakeNow+5 || !row.ClaimableAt(wakeNow+5) {
				t.Fatalf("the released message is %+v, want no hold and due at the edge", row)
			}
			released := journalOf(t, f, "supervisor_message_released", id)
			if len(released) != 1 || released[0]["holdReason"] != hold || released[0]["by"] != "idle_edge" || released[0]["status"] != "idle" || released[0]["thread"] != "supervisor" {
				t.Fatalf("supervisor_message_released journal %v, want one row naming the hold and the edge", released)
			}
			// The hold rules are unchanged: the busy count is not reset, so one more busy answer after a
			// release of busy_cap holds the message again, and the next edge releases it again.
			if hold == delivery.BusyCap {
				for range 39 {
					if _, err := f.s.DB.ExecContext(f.ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES('t','supervisor_message_deferred',?,'{}')", id); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.c.Attempt(f.ctx, id, &sendHost{status: "active"}, wakeNow+6); err != nil {
					t.Fatal(err)
				}
				row, err = f.c.Get(f.ctx, id)
				if err != nil || row.HoldReason.String != delivery.BusyCap {
					t.Fatalf("a busy answer past the cap after the release: %+v %v, want busy_cap again", row, err)
				}
			}
		})
	}
}

// cloneHeld adds a second unsent message to the fixture's recipient, staged after id, held at hold:
// the message a readback of another message to the same recipient releases.
func cloneHeld(t *testing.T, f *stageFixture, id, clone, hold string) {
	t.Helper()
	if _, err := f.s.DB.ExecContext(f.ctx, "INSERT INTO supervisor_messages (message_id,obligation_id,obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,attempt_count,next_eligible_at,hold_reason,staged_at,updated_at) SELECT ?,obligation_id||'-clone',obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,'withheld_pre_send',6,?,?,'2023-11-14T22:13:21.000000+00:00',updated_at FROM supervisor_messages WHERE message_id=?", clone, wakeNow+900, hold, id); err != nil {
		t.Fatal(err)
	}
}

func TestSupervisorWake_a_verified_readback_releases_capped_holds_to_that_recipient(t *testing.T) {
	t.Parallel()
	f, h, id, _ := delivered24(t)
	cloneHeld(t, f, id, "held-attempt-cap", delivery.AttemptCap)
	cloneHeld(t, f, id, "held-busy-cap", delivery.BusyCap)
	cloneHeld(t, f, id, "held-unaddressed", store.SupervisorHoldUnaddressed)
	// A readback that does not verify proves nothing about the recipient: nothing is released.
	if read, err := f.c.ReadBack(f.ctx, id, "missing", Proof(id, "missing"), "", h, wakeNow+2); err != nil || read["verified"] != "turn_not_found" {
		t.Fatalf("unverified readback %v %v", read, err)
	}
	for _, clone := range []string{"held-attempt-cap", "held-busy-cap"} {
		if row, err := f.c.Get(f.ctx, clone); err != nil || !row.HoldReason.Valid {
			t.Fatalf("an unverified readback released %s: %+v %v", clone, row, err)
		}
	}
	read, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, wakeNow+3)
	if err != nil || read["verified"] != "host_read" {
		t.Fatalf("readback %v %v", read, err)
	}
	for _, c := range []struct{ clone, hold string }{{"held-attempt-cap", delivery.AttemptCap}, {"held-busy-cap", delivery.BusyCap}} {
		row, err := f.c.Get(f.ctx, c.clone)
		if err != nil {
			t.Fatal(err)
		}
		if row.HoldReason.Valid || row.NextEligibleAt != (sql.NullFloat64{Float64: wakeNow + 3, Valid: true}) || row.State != "withheld_pre_send" {
			t.Fatalf("after the recipient's readback %s is %+v, want released and due at the readback", c.clone, row)
		}
		released := journalOf(t, f, "supervisor_message_released", c.clone)
		if len(released) != 1 || released[0]["holdReason"] != c.hold || released[0]["by"] != "readback" || released[0]["readMessageId"] != id || released[0]["thread"] != "supervisor" {
			t.Fatalf("supervisor_message_released journal of %s: %v, want one row naming the readback", c.clone, released)
		}
	}
	row, err := f.c.Get(f.ctx, "held-unaddressed")
	if err != nil || row.HoldReason.String != store.SupervisorHoldUnaddressed {
		t.Fatalf("the readback released a hold it does not own: %+v %v", row, err)
	}
}

func TestSupervisorWake_idle_wake_recipients_names_busy_and_capped_recipients(t *testing.T) {
	t.Parallel()
	f, id := busyMessage(t)
	holds, err := f.c.IdleWakeRecipients(f.ctx, wakeNow+1)
	if err != nil || len(holds) != 1 || holds[0].RecipientTaskID != "supervisor" || holds[0].RecipientThreadID != "supervisor" || holds[0].Deadline != wakeNow+15 {
		t.Fatalf("a busy-deferred message: %+v %v, want its recipient with its deadline", holds, err)
	}
	// Past its deadline and not yet attempted, it is still waiting.
	if holds, err := f.c.IdleWakeRecipients(f.ctx, wakeNow+100); err != nil || len(holds) != 1 {
		t.Fatalf("a due busy-deferred message: %+v %v, want it still held", holds, err)
	}
	for _, hold := range []string{delivery.BusyCap, delivery.AttemptCap} {
		if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET hold_reason=? WHERE message_id=?", hold, id); err != nil {
			t.Fatal(err)
		}
		if holds, err := f.c.IdleWakeRecipients(f.ctx, wakeNow+1); err != nil || len(holds) != 1 || holds[0].RecipientTaskID != "supervisor" {
			t.Fatalf("a %s message: %+v %v, want its recipient held for the edge that releases it", hold, holds, err)
		}
	}
	for _, update := range []string{"UPDATE supervisor_messages SET hold_reason='" + store.SupervisorHoldUnaddressed + "' WHERE message_id=?", "UPDATE supervisor_messages SET hold_reason=NULL,state='queued',next_eligible_at=NULL WHERE message_id=?", "UPDATE supervisor_messages SET state='dispatched' WHERE message_id=?"} {
		if _, err := f.s.DB.ExecContext(f.ctx, update, id); err != nil {
			t.Fatal(err)
		}
		if holds, err := f.c.IdleWakeRecipients(f.ctx, wakeNow+1); err != nil || len(holds) != 0 {
			t.Fatalf("after %q: %+v %v, want no recipient to hold", update, holds, err)
		}
	}
}
