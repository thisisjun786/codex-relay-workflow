package supervisor

import (
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TestSendByIDOfAnUnheldMessageWhoseEndpointsMovedHoldsItUnaddressed (CRW-1558 U3) pins the send by
// id of a message that carries no hold, is unsent and is due, after the hierarchy moved its recipient
// or its sender. The hold rule is asked only of a superseded_by_report hold (read), so this send is
// not answered by the rule's send_by_id cell: the attempt's resolve holds the message
// hierarchy_unresolved, journals one supervisor_message_unaddressed row and answers
// relation_owner_drift, and nothing is sent. docs/relay/supervisor-channel.md and the supervisor-send
// --message help describe this path (TestSendByIDDocsAndHelpNameTheUnheldPath).
func TestSendByIDOfAnUnheldMessageWhoseEndpointsMovedHoldsItUnaddressed(t *testing.T) {
	for _, change := range []string{"other_recipient", "other_sender"} {
		t.Run(change, func(t *testing.T) {
			w := newHoldWorld(t, "event")
			before, err := w.c.Get(w.ctx, w.id)
			if err != nil || before.HoldReason.Valid || !store.SupervisorUnsent(before.State) {
				t.Fatalf("the staged message is %s with hold %v (%v), want unsent with no hold", before.State, before.HoldReason, err)
			}
			w.apply(t, change)
			w.now += 60
			if row, err := w.c.Get(w.ctx, w.id); err != nil || !row.ClaimableAt(w.now) {
				t.Fatalf("the message is not claimable before the send (%v): %+v", err, row)
			}
			var refusal Refusal
			if record, err := w.c.Attempt(w.ctx, w.id, w.host, w.now); record != nil || !errors.As(err, &refusal) || refusal.Reason != "relation_owner_drift" {
				t.Fatalf("a send by id of an unheld message after the hierarchy moved answered %v %v, want relation_owner_drift", record, err)
			}
			row, err := w.c.Get(w.ctx, w.id)
			if err != nil {
				t.Fatal(err)
			}
			if row.HoldReason.String != store.SupervisorHoldUnaddressed || row.State != before.State || row.AttemptCount != 0 {
				t.Fatalf("the message is %s with hold %v after %d attempts, want %s held %s and never attempted", row.State, row.HoldReason, row.AttemptCount, before.State, store.SupervisorHoldUnaddressed)
			}
			if row.RecipientTaskID != before.RecipientTaskID || row.SenderTaskID != before.SenderTaskID {
				t.Fatalf("the send by id re-addressed the message: %s -> %s, want %s -> %s", row.SenderTaskID, row.RecipientTaskID, before.SenderTaskID, before.RecipientTaskID)
			}
			if n := w.journalRows(t, "supervisor_message_unaddressed"); n != 1 {
				t.Fatalf("the send by id journaled %d supervisor_message_unaddressed rows, want 1", n)
			}
			if n := w.journalRows(t, "supervisor_message_attempted"); n != 0 || w.host.sends != 0 {
				t.Fatalf("the send by id journaled %d attempts and sent %d times, want neither", n, w.host.sends)
			}
		})
	}
}
