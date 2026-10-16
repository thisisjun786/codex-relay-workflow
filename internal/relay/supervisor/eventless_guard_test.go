package supervisor

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// TestEventlessRestatementGuard (CRW-1546 item 2): the restatement guard of an eventless omission
// (refreshEventlessProposal's UPDATE: the row is in a restatable state and no other attempt of it
// may have gone) refuses to rewrite the packet of an omission that went. The fixture keeps
// recipient.scopeKind in the stored packet and moves the seat the same recipient task answers from,
// so the refresh reaches the guard with a rewrite to make. Each clause must hold alone, which the
// cases pin: a row that went, refreshed as the transport start of its own attempt (which the attempt
// clause leaves out), meets only the state clause; a row that reads restatable while another
// attempt that may have gone stands meets only the attempt clause. Modelled on
// TestOldWordingSentRowStaysAsItIs.
func TestEventlessRestatementGuard(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// state is what the row reads when it is refreshed ("" the state the send left); attempt is the
		// attempt the refresh is made for (0 none).
		state   string
		attempt int64
	}{
		{"dispatched", "", 0},
		{"dispatched, refreshed for its own attempt", "", 1},
		{"read", "read", 0},
		{"read, refreshed for its own attempt", "read", 1},
		{"unsent again behind an attempt that may have gone", "queued", 0},
		{"sending for another attempt behind one that may have gone", "sending", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := fixture24(t)
			f.c.Socket = "/tmp/host.sock"
			f.c.Settings = &delivery.TaskSettings{}
			// A turn the fixture holds no final event for, so this omission is not superseded by one.
			reading := omissionReading24(f)
			reading["selectors"].(map[string]any)["turn"] = "turn-9"
			o := ObservationObligation(reading)
			if o == nil {
				t.Fatal("the omission reading raised no obligation")
			}
			result, err := f.c.StageWithReading(f.ctx, *o, reading, "", f.at)
			if err != nil {
				t.Fatal(err)
			}
			id := result["messageId"].(string)
			host := &sendHost{status: "idle"}
			if _, err := f.c.Attempt(f.ctx, id, host, 1_700_000_000); err != nil {
				t.Fatal(err)
			}
			sent, err := f.c.Get(f.ctx, id)
			if err != nil || sent.State != "dispatched" || len(host.sends) != 1 {
				t.Fatalf("the send: state %q, %d sends, %v", sent.State, len(host.sends), err)
			}
			if sent.EventID.Valid || !sent.Reading.Valid {
				t.Fatalf("the fixture is not an eventless omission: event %v reading %v", sent.EventID, sent.Reading)
			}
			if recipient := recipientOfPacket(t, sent.Packet); recipient["scopeKind"] != "initiative" {
				t.Fatalf("the omission went without the initiative kind: recipient %v", recipient)
			}
			var mayHaveGone int
			if err := f.s.DB.QueryRowContext(f.ctx, "SELECT COUNT(*) FROM supervisor_attempts WHERE message_id=? AND attempt_no=1 AND "+store.SupervisorAttemptMayHaveGoneSQL(""), id).Scan(&mayHaveGone); err != nil || mayHaveGone != 1 {
				t.Fatalf("the send left no attempt 1 that may have gone: %d, %v", mayHaveGone, err)
			}
			// The same task takes the store seat and the initiative level goes, so the recipient task
			// does not move while the seat it answers from does.
			if err := storeseed.InsertScopeBinding(f.ctx, f.s, store.ScopeBindingsRow{BindingID: "bnd-eventless-store",
				Role: "supervisor", ScopeKind: "store", ScopeKey: "store", TaskID: "supervisor", HostID: "host",
				Status: "active", Revision: 1, CreatedAt: "t", UpdatedAt: "t"}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE scope_bindings SET status='released' WHERE scope_kind='initiative'"); err != nil {
				t.Fatal(err)
			}
			if c.state != "" {
				if _, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET state=? WHERE message_id=?", c.state, id); err != nil {
					t.Fatal(err)
				}
			}
			row, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			r, err := f.c.Resolve(f.ctx, row.RelationshipID)
			if err != nil {
				t.Fatal(err)
			}
			if r.Recipient != row.RecipientTaskID {
				t.Fatalf("the fixture moved the recipient task: %q -> %q", row.RecipientTaskID, r.Recipient)
			}
			if r.RecipientScopeKind != "store" {
				t.Fatalf("the fixture did not move the seat: %q", r.RecipientScopeKind)
			}
			before := restatedRows(t, f.s)
			changed, err := f.c.refreshProposal(f.ctx, row, r, f.at, c.attempt)
			if changed {
				t.Fatal("the packet of an omission that went was restated")
			}
			if detail := refusalDetail(t, err); detail != "the report was sent; the staged packet cannot be changed" {
				t.Fatalf("the refresh refused with %q", detail)
			}
			after, err := f.c.Get(f.ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if after.Packet != row.Packet || after.UpdatedAt != row.UpdatedAt || after.State != row.State {
				t.Fatalf("the omission that went moved: state %q -> %q, packet %s", row.State, after.State, after.Packet)
			}
			if recipient := recipientOfPacket(t, after.Packet); recipient["scopeKind"] != "initiative" {
				t.Fatalf("the omission that went was rewritten to the new seat: recipient %v", recipient)
			}
			if n := restatedRows(t, f.s); n != before {
				t.Fatalf("restatement rows: %d, want %d", n, before)
			}
		})
	}
}
