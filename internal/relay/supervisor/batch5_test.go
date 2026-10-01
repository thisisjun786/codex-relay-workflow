package supervisor

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func Test24_SCH_42_UncertainSendSettledOnlyByToken(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	h := &sendHost{status: "idle", outcome: "unknown"}
	result, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || result["deliveryState"] != "held_uncertain" {
		t.Fatalf("send %v %v", result, err)
	}
	h.items = map[string]string{}
	first, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil || first["verified"] != "transcript_unconfirmed" || first["reconciled"] != nil {
		t.Fatalf("no token %v %v", first, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "held_uncertain" {
		t.Fatalf("held %v %v", row, err)
	}
	h.items["turn-supervisor-1"] = h.sends[0]
	second, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_003)
	if err != nil || second["verified"] != "host_read" || second["reconciled"].(map[string]any)["requestId"] != result["requestId"] {
		t.Fatalf("reconcile %v %v", second, err)
	}
	row, err = f.c.Get(f.ctx, id)
	if err != nil || row.State != "read" {
		t.Fatalf("settled %v %v", row, err)
	}
	journal, err := f.s.Journal(f.ctx, "supervisor_message_reconciled", id)
	if err != nil || len(journal) != 1 {
		t.Fatalf("journal %+v %v", journal, err)
	}
}
func Test24_SCH_43_LateReceiptNeverDemotesRead(t *testing.T) {
	f, h, id, _ := delivered24(t)
	if _, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "read" {
		t.Fatalf("read %v %v", row, err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts %v %v", attempts, err)
	}
	if err = f.s.SettleSupervisorAttempt(f.ctx, attempts[0].RequestID, "dispatched", "yes", 0, attempts[0].TurnID, "{}", f.at); err != nil {
		t.Fatal(err)
	}
	row, err = f.c.Get(f.ctx, id)
	if err != nil || row.State != "read" {
		t.Fatalf("late receipt %v %v", row, err)
	}
}
func Test24_SCH_44_StageChecksPriorReportUnderWrite(t *testing.T) {
	f := fixture24(t)
	o := f.obligation(t)
	_, err := f.s.DB.ExecContext(f.ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_report',?,'{}')", f.at, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.c.Stage(f.ctx, o, "", f.at)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "not_claimable" || !strings.Contains(refusal.Detail, "decided under the staging write lock") {
		t.Fatalf("refusal %v", err)
	}
}
func Test24_SCH_45_ReaddressConverges(t *testing.T) {
	f := fixture24(t)
	o, stage := f.staged(t)
	id := stage["messageId"].(string)
	if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "b-successor", f.at); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.DB.ExecContext(f.ctx, "INSERT INTO scope_bindings(binding_id,role,scope_kind,scope_key,task_id,host_id,status,revision,created_at,updated_at) VALUES ('b-successor','supervisor','initiative','INI-1','successor','host','active',2,'t','t')")
	if err != nil {
		t.Fatal(err)
	}
	if err := storeseed.RepointScopeLink(f.ctx, f.s, "lnk-project", "active", "parent", "successor", f.at); err != nil {
		t.Fatal(err)
	}
	moved, err := f.c.Stage(f.ctx, o, "", f.at)
	if err != nil || moved["readdressed"] != true {
		t.Fatalf("moved %v %v", moved, err)
	}
	again, err := f.c.Stage(f.ctx, o, "", f.at)
	if err != nil || again["staged"] != false {
		t.Fatalf("again %v %v", again, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.RecipientTaskID != "successor" {
		t.Fatalf("recipient %v %v", row, err)
	}
}
func Test24_SCH_46_StaleRescheduleKeepsNewHold(t *testing.T) {
	f := fixture24(t)
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	stale, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.DB.ExecContext(f.ctx, "UPDATE supervisor_messages SET state='deferred_busy',hold_reason='busy_cap' WHERE message_id=?", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.c.deferMessage(f.ctx, stale, "queued", "", f.at, 1_700_000_005); err != nil {
		t.Fatal(err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "deferred_busy" || row.HoldReason.String != "busy_cap" {
		t.Fatalf("stale update %v %v", row, err)
	}
}
func Test24_SCH_47_FutureRecipientRateDoesNotPaceNow(t *testing.T) {
	f := fixture24(t)
	_, err := f.s.DB.ExecContext(f.ctx, "INSERT INTO recipient_rate(recipient_task_id,window_start,sends,last_send_at) VALUES ('supervisor',?,1,?)", float64(1_700_086_400), float64(1_700_086_400))
	if err != nil {
		t.Fatal(err)
	}
	service := delivery.NewService(f.s, delivery.SystemClock{})
	reason, err := service.SendRefusal(f.ctx, "other-relationship", "supervisor", 1_700_000_000)
	if err != nil || reason != "" {
		t.Fatalf("future pacing %q %v", reason, err)
	}
}
func Test24_SCH_48_SendDatedAtTransportStart(t *testing.T) {
	f, h, id, _ := delivered24(t)
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || !attempts[0].TransportStartedAt.Valid {
		t.Fatalf("start %+v %v", attempts, err)
	}
	h.turns = map[string]float64{"turn-supervisor-1": 1_699_999_998}
	answer, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil || answer["verified"] != "turn_predates_send" {
		t.Fatalf("chronology %v %v", answer, err)
	}
}
func Test24_SCH_49_TokenInUnnamedTurnUnconfirmed(t *testing.T) {
	f, h, id, _ := delivered24(t)
	h.items = map[string]string{"": h.sends[0]}
	answer, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil || answer["verified"] == "host_read" {
		t.Fatalf("unnamed token %v %v", answer, err)
	}
}
func Test24_SCH_50_SettledAnswersHaveSameShape(t *testing.T) {
	f, h, id, _ := delivered24(t)
	first, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", Proof(id, "turn-supervisor-1"), "", h, 1_700_000_002)
	if err != nil {
		t.Fatal(err)
	}
	later, err := f.c.ReadBack(f.ctx, id, "turn-supervisor-1", "invalid", "", h, 1_700_000_003)
	if err != nil {
		t.Fatal(err)
	}
	first["recorded"] = false
	if !reflect.DeepEqual(first, later) {
		t.Fatalf("answer changed: first=%v later=%v", first, later)
	}
}
