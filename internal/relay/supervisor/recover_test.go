package supervisor

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func Test24_SCH_21_RecoverClaimBeforeTransport(t *testing.T) {
	f := fixture24(t)
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	request := "sup-" + id[:12] + "-a1"
	_, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET state='sending',attempt_count=1,lease_owner='worker',lease_until=? WHERE message_id=?", float64(1_700_000_001), id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.DB.ExecContext(f.ctx, "INSERT INTO supervisor_attempts(request_id,message_id,attempt_no,message,state,send_attempted,retry_safe,record,sent_at,observed_at,delivery_token) VALUES(?,?,1,'frozen','held_uncertain','unknown',0,'{}',?,?,'token')", request, id, f.at, f.at)
	if err != nil {
		t.Fatal(err)
	}
	stranded, err := f.c.Stranded(f.ctx, 1_700_000_002)
	if err != nil || len(stranded) != 1 {
		t.Fatalf("stranded %v %v", stranded, err)
	}
	row, err := f.c.recoverStranded(f.ctx, stranded[0], 1_700_000_002)
	if err != nil || row.State != "queued" {
		t.Fatalf("row %+v %v", row, err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || attempts[0].State != "withheld_pre_send" || attempts[0].SendAttempted != "no" || attempts[0].RetrySafe != 1 {
		t.Fatalf("attempt %+v %v", attempts, err)
	}
	journal, err := f.s.Journal(f.ctx, "supervisor_message_released", id)
	if err != nil || len(journal) != 1 || !strings.Contains(journal[0].Detail, "before the transport started") {
		t.Fatalf("journal %+v %v", journal, err)
	}
}
func Test24_SCH_22_OlderClaimableBlocksNamedNewer(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, first := f.staged(t)
	second := f.obligation(t)
	second.Subject = "different-event"
	second.ID = hash32(second.Kind + "|" + second.RelationID + "|" + second.Subject)
	p, err := f.c.Compose(f.ctx, second, Resolution{"parent", "supervisor", "PRJ-1", "INI-1", "linkage"}, f.at)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := canonicalPacket(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.DB.ExecContext(f.ctx, "INSERT INTO supervisor_messages(message_id,obligation_id,obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,staged_at,updated_at) VALUES(?,?,?,'rel-1','PRJ-1','completion','notification','parent','supervisor',?,?,'queued',?,?)", p.ID(), second.ID, second.Kind, second.Subject, packet, "2023-11-14T22:13:21.000000+00:00", f.at)
	if err != nil {
		t.Fatal(err)
	}
	h := &sendHost{status: "idle"}
	_, err = f.c.Attempt(f.ctx, p.ID(), h, 1_700_000_000)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "not_claimable" || !strings.Contains(refusal.Detail, first["messageId"].(string)) {
		t.Fatalf("older %v", err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, p.ID())
	if err != nil || len(attempts) != 0 {
		t.Fatalf("attempts %+v %v", attempts, err)
	}
}
func Test24_SCH_23_HeldOrStrandedOlderDoesNotBlock(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, first := f.staged(t)
	id := first["messageId"].(string)
	_, err := f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET hold_reason='attempt_cap' WHERE message_id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	eligible, err := f.c.Eligible(f.ctx, 1_700_000_000, 4)
	if err != nil || len(eligible) != 0 {
		t.Fatalf("held eligible: %v %v", eligible, err)
	}
}
