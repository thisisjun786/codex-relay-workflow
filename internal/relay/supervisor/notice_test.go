package supervisor

import (
	"testing"
)

func Test29NoticeProjectUsesLiveHierarchy(t *testing.T) {
	f := fixture24(t)
	r, err := f.c.Resolve(f.ctx, "project:PRJ-1")
	if err != nil || r.Sender != "parent" || r.Recipient != "supervisor" || r.ProjectKey != "PRJ-1" {
		t.Fatal(r, err)
	}
	if _, err = f.s.DB.Exec("UPDATE scope_bindings SET status='released' WHERE scope_kind='project'"); err != nil {
		t.Fatal(err)
	}
	_, err = f.c.Resolve(f.ctx, "project:PRJ-1")
	if refusal, ok := err.(Refusal); !ok || refusal.Reason != "unregistered_scope" {
		t.Fatal(err)
	}
}

func Test29NoticeWithoutReservationCannotSend(t *testing.T) {
	f := fixture24(t)
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	if _, err := f.s.DB.Exec("UPDATE supervisor_messages SET obligation_kind='fault_notification',obligation_id='missing-notification',event_id=NULL WHERE message_id=?", id); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	live, err := f.c.Resolve(f.ctx, row.RelationshipID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.c.refreshProposal(f.ctx, row, live, f.at, 0)
	if refusal, ok := err.(Refusal); !ok || refusal.Reason != "superseded_revision" {
		t.Fatalf("unreserved notice was claimable: %v", err)
	}
	var attempts int
	if err = f.s.DB.QueryRow("SELECT count(*) FROM supervisor_attempts").Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal(attempts, err)
	}
}
