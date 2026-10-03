package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func reportNumber24(t *testing.T, f *stageFixture, submission, number int) {
	t.Helper()
	_, err := f.s.Q(f.ctx).ExecContext(f.ctx, "INSERT OR REPLACE INTO work_reports(event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,pr_number,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES ('event-1',?,'rel-1',1,'abc123456789abcdef','thisisjun786/codex-relay-workflow',?,'a1b2c3','DONE','proved','v1','the work is done','merge',?)", submission, number, f.at)
	if err != nil {
		t.Fatal(err)
	}
}
func packetNumber24(t *testing.T, f *stageFixture, id string) int {
	t.Helper()
	row, err := f.c.Get(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var p Packet
	if err = json.Unmarshal([]byte(row.Packet), &p); err != nil {
		t.Fatal(err)
	}
	return int(p["artifact"].(map[string]any)["number"].(float64))
}
func Test24_SCH_53_CorrectedProposalBeforeAndAfterTransport(t *testing.T) {
	for _, submission := range []int{1, 2} {
		t.Run(string(rune('0'+submission)), func(t *testing.T) {
			f := fixture24(t)
			f.c.Settings = &delivery.TaskSettings{}
			reportNumber24(t, f, 1, 10)
			_, stage := f.staged(t)
			id := stage["messageId"].(string)
			reportNumber24(t, f, submission, 11)
			h := &sendHost{status: "idle"}
			answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
			if err != nil || answer["deliveryState"] != "dispatched" || packetNumber24(t, f, id) != 11 || len(h.sends) != 1 || !strings.Contains(h.sends[0], "codex-relay-workflow #11 at") {
				t.Fatalf("answer %v err %v sends %+v", answer, err, h.sends)
			}
		})
	}
}
func Test24_SCH_54_CorrectionBeforeStageLock(t *testing.T) {
	for _, submission := range []int{1, 2} {
		t.Run(string(rune('0'+submission)), func(t *testing.T) {
			f := fixture24(t)
			reportNumber24(t, f, 1, 10)
			o := f.obligation(t)
			f.c.beforeStageLock = func() { reportNumber24(t, f, submission, 11) }
			_, err := f.c.Stage(f.ctx, o, "", f.at)
			var refusal Refusal
			if !errors.As(err, &refusal) || refusal.Reason != "superseded_revision" || !strings.Contains(refusal.Detail, "changed while this was being staged") {
				t.Fatalf("refusal %v", err)
			}
			f.c.beforeStageLock = nil
			result, err := f.c.Stage(f.ctx, f.obligation(t), "", f.at)
			if err != nil || packetNumber24(t, f, result["messageId"].(string)) != 11 {
				t.Fatalf("restage %v %v", result, err)
			}
		})
	}
}
func Test24_SCH_55_OmissionContradictoryReading(t *testing.T) {
	f := fixture24(t)
	base := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": "rel-1", "executionGeneration": float64(1), "reason": "missing", "selectors": map[string]any{"state": f.c.StoreDirectory(), "markerRoot": "/tmp/markers", "workspace": "/tmp/work", "assignment": "a", "session": "s", "turn": "turn-1"}}
	o := ObservationObligation(base)
	if o == nil {
		t.Fatal("no obligation")
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{{"schema", func(m map[string]any) { m["schema"] = "reporting-observation/0" }}, {"state", func(m map[string]any) { m["reportingState"] = "reported" }}, {"relationship", func(m map[string]any) { m["relationshipId"] = "other" }}, {"turn", func(m map[string]any) { m["selectors"].(map[string]any)["turn"] = "other" }}} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var wrong map[string]any
			_ = json.Unmarshal(raw, &wrong)
			tc.change(wrong)
			_, err := f.c.StageWithReading(f.ctx, *o, wrong, "", f.at)
			var refusal Refusal
			if !errors.As(err, &refusal) || refusal.Reason != "contradictory_observation" || !strings.Contains(refusal.Detail, "contradicts it") {
				t.Fatalf("refusal %v", err)
			}
		})
	}
	var count int
	if err := f.s.Q(f.ctx).QueryRowContext(f.ctx, "SELECT count(*) FROM supervisor_messages").Scan(&count); err != nil || count != 0 {
		t.Fatalf("messages %d %v", count, err)
	}
}
func omissionReading24(f *stageFixture) map[string]any {
	return map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": "rel-1", "executionGeneration": float64(1), "reason": "missing", "selectors": map[string]any{"state": f.c.StoreDirectory(), "markerRoot": "/tmp/markers", "workspace": "/tmp/My Project", "assignment": "it's here", "session": "\"quoted\"", "turn": "turn-1"}}
}
func Test24_SCH_60_OmissionObservationsConvergeAndContradict(t *testing.T) {
	f := fixture24(t)
	base := omissionReading24(f)
	raw, _ := json.Marshal(base)
	var changed map[string]any
	_ = json.Unmarshal(raw, &changed)
	changed["executionGeneration"] = float64(2)
	answer, err := f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{base, changed}, f.at)
	if err != nil || len(answer["refused"].([]any)) != 1 || answer["refused"].([]any)[0].(map[string]any)["reason"] != "contradictory_observation" {
		t.Fatalf("contradiction %v %v", answer, err)
	}
	answer, err = f.c.StageStandingWithObservations(f.ctx, "PRJ-1", []map[string]any{base, base}, f.at)
	if err != nil || len(answer["refused"].([]any)) != 0 {
		t.Fatalf("same reading %v %v", answer, err)
	}
	o := ObservationObligation(base)
	result, err := f.c.StageWithReading(f.ctx, *o, base, "", f.at)
	if err != nil || result["staged"] != false {
		t.Fatalf("converged %v %v", result, err)
	}
	changed["executionGeneration"] = float64(1)
	changed["selectors"].(map[string]any)["session"] = "another"
	_, err = f.c.StageWithReading(f.ctx, *o, changed, "", f.at)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "contradictory_observation" || !strings.Contains(refusal.Detail, "keeps the reading it froze") {
		t.Fatalf("frozen %v", err)
	}
}
func Test24_SCH_61_UnstagedCompletionStillOwed(t *testing.T) {
	f := fixture24(t)
	o := f.obligation(t)
	if o.Kind != "completion" {
		t.Fatalf("kind %q", o.Kind)
	}
	can, reason, err := f.c.Reportable(f.ctx, o)
	if err != nil || !can || reason != "deliverability_was_not_asked_about" {
		t.Fatalf("owed %t %s %v", can, reason, err)
	}
	prior, err := f.c.PriorReport(f.ctx, o.ID)
	if err != nil || prior != nil {
		t.Fatalf("prior %v %v", prior, err)
	}
	var count int
	for _, table := range []string{"supervisor_messages", "journal"} {
		if err = f.s.Q(f.ctx).QueryRowContext(f.ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s %d %v", table, count, err)
		}
	}
}
func Test24_SCH_62_OmissionEvidenceSelectsItsStore(t *testing.T) {
	f := fixture24(t)
	reading := omissionReading24(f)
	o := ObservationObligation(reading)
	result, err := f.c.StageWithReading(f.ctx, *o, reading, "", f.at)
	if err != nil {
		t.Fatal(err)
	}
	shown, err := f.c.Show(f.ctx, result["messageId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	from := shown["stagedFrom"].(map[string]any)
	if pyjson.Dumps(from["reading"], pyjson.Options{SortKeys: true, Unicode: true}) != pyjson.Dumps(reading, pyjson.Options{SortKeys: true, Unicode: true}) {
		t.Fatalf("reading %v", from)
	}
	line := from["recheck"].(string)
	for _, part := range []string{"--state", f.c.StoreDirectory(), "reporting-show", "--marker-root", "--workspace", "--assignment", "--session", "--turn"} {
		if !strings.Contains(line, part) {
			t.Fatalf("recheck %q lacks %q", line, part)
		}
	}
}
func Test24_SCH_63_ReadbackEstablishesArrivalOnly(t *testing.T) {
	f, h, id, record := delivered24(t)
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts %v %v", attempts, err)
	}
	if strings.Contains(strings.ToLower(attempts[0].Message), "confirm you read") || !strings.Contains(attempts[0].Message, "It never records that you read") {
		t.Fatalf("message %s", attempts[0].Message)
	}
	answer, err := f.c.ReadBack(f.ctx, id, record["turnId"].(string), Proof(id, record["turnId"].(string)), "", h, 1_700_000_002)
	if err != nil || answer["verified"] != "host_read" || !strings.HasPrefix(answer["establishes"].(string), "arrival only") {
		t.Fatalf("readback %v %v", answer, err)
	}
	shown, err := f.c.Show(f.ctx, id)
	if err != nil || shown["state"] != "read" || shown["turnOrigin"] != "relay_opened" {
		t.Fatalf("show %v %v", shown, err)
	}
}

// policyRefusalHost is a host whose recipient refuses the first send for its approval policy
// while refused is set, without starting a turn, and which then sends as the host it wraps.
type policyRefusalHost struct {
	SendAdapter
	refused bool
}

func (h *policyRefusalHost) SendMessage(_ context.Context, id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	if h.refused {
		h.refused = false
		return delivery.Obj{{Key: "status", Value: "failed"}, {Key: "resumed", Value: delivery.Obj{{Key: "approvalPolicy", Value: "untrusted"}}}, {Key: "rpcError", Value: delivery.Obj{{Key: "code", Value: "unsupported_approval_policy"}}}}, nil
	}
	return h.SendAdapter.SendMessage(context.Background(), id, thread, message, settings)
}
func Test24_SCH_58_ClaimOwnerSettlesAfterExpiredLease(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	h := &sendHost{status: "idle"}
	h.beforeSend = func() {
		if _, err := f.s.Q(f.ctx).ExecContext(f.ctx, "UPDATE supervisor_messages SET lease_until=? WHERE message_id=?", float64(1_700_000_000), id); err != nil {
			t.Fatal(err)
		}
	}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer["messageState"] != "dispatched" {
		t.Fatalf("owner %v %v", answer, err)
	}
}
func Test24_SCH_58_LateRefusalAfterRecoveryKeepsUncertain(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	h := &presendRecoveryHost24{sendHost: &sendHost{status: "idle"}}
	h.sendHost.beforeSend = func() {
		if _, err := f.s.Q(f.ctx).ExecContext(f.ctx, "UPDATE supervisor_messages SET lease_until=? WHERE message_id=?", float64(1_700_000_000), id); err != nil {
			t.Fatal(err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := f.c.recoverStranded(f.ctx, row, 1_700_000_001)
		if err != nil || recovered.State != "held_uncertain" {
			t.Fatalf("recovery %+v %v", recovered, err)
		}
	}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer["deliveryState"] != "withheld_pre_send" || answer["messageState"] != "held_uncertain" {
		t.Fatalf("late %v %v", answer, err)
	}
}

type presendRecoveryHost24 struct{ *sendHost }

func (h *presendRecoveryHost24) SendMessage(_ context.Context, id, thread, message string, settings *delivery.TaskSettings) (delivery.Obj, error) {
	if h.beforeSend != nil {
		h.beforeSend()
	}
	return delivery.Obj{{Key: "status", Value: "failed"}, {Key: "error", Value: "thread/read: unavailable"}}, nil
}
func Test24_SCH_65_ExpiredUnstartedClaimReleasesForNextSend(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	f.c.beforeTransport = func() {
		if _, err := f.s.Q(f.ctx).ExecContext(f.ctx, "UPDATE supervisor_messages SET lease_until=? WHERE message_id=?", float64(1_700_000_000), id); err != nil {
			t.Fatal(err)
		}
		row, err := f.c.Get(f.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := f.c.recoverStranded(f.ctx, row, 1_700_000_001)
		if err != nil || recovered.State != "queued" {
			t.Fatalf("recovery %+v %v", recovered, err)
		}
	}
	h := &sendHost{status: "idle"}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer != nil {
		t.Fatalf("owner %v %v", answer, err)
	}
	f.c.beforeTransport = nil
	answer, err = f.c.Attempt(f.ctx, id, h, 1_700_000_002)
	if err != nil || answer["deliveryState"] != "dispatched" || len(h.sends) != 1 {
		t.Fatalf("retry %v %v %+v", answer, err, h.sends)
	}
}
func Test24_SCH_66_CancelledClaimDoesNotSpendBudget(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	f.c.beforeTransport = func() {
		if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "successor", f.at); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "unregistered_scope" && refusal.Reason != "relation_owner_drift" {
		t.Fatalf("cancel %v", err)
	}
	var sends int
	err = f.s.Q(f.ctx).QueryRowContext(f.ctx, "SELECT sends FROM recipient_rate WHERE recipient_task_id='supervisor'").Scan(&sends)
	if err == nil && sends != 0 {
		t.Fatalf("unsent claim spent %d", sends)
	}
}
func Test24_SCH_68_NewerBlockStatementRestatesSameMessage(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, err := f.s.Q(f.ctx).ExecContext(f.ctx, "UPDATE events SET outcome='blocked_needs_input' WHERE event_id='event-1'")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Q(f.ctx).ExecContext(f.ctx, "UPDATE work_reports SET cxc_status='BLOCKED' WHERE event_id='event-1'")
	if err != nil {
		t.Fatal(err)
	}
	o := f.obligation(t)
	staged, err := f.c.Stage(f.ctx, o, "", f.at)
	if err != nil {
		t.Fatal(err)
	}
	id := staged["messageId"].(string)
	_, err = f.s.Q(f.ctx).ExecContext(f.ctx, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) SELECT 'event-2',relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,'turn-2',turn_status,receipt,'2023-11-14T22:13:25.000000+00:00','2023-11-14T22:13:25.000000+00:00' FROM events WHERE event_id='event-1'")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Q(f.ctx).ExecContext(f.ctx, "INSERT INTO work_reports(event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) SELECT 'event-2',submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at FROM work_reports WHERE event_id='event-1'")
	if err != nil {
		t.Fatal(err)
	}
	newest, err := f.c.FromEvent(f.ctx, "event-2")
	if err != nil || newest.ID != o.ID {
		t.Fatalf("same obligation %v %v", newest, err)
	}
	result, err := f.c.Stage(f.ctx, o, "", f.at)
	if err != nil || result["restated"] != true || result["messageId"] != id {
		t.Fatalf("restage %v %v", result, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.EventID.String != "event-2" || !strings.Contains(row.Packet, "show --event event-2") {
		t.Fatalf("row %+v %v", row, err)
	}
	h := &sendHost{status: "idle"}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer["deliveryState"] != "dispatched" || len(h.sends) != 1 || !strings.Contains(h.sends[0], "show --event event-2") {
		t.Fatalf("send %v %v %+v", answer, err, h.sends)
	}
}
func Test24_SCH_69_DeliveryTokenIsPerAttempt(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	_, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || !attempts[0].DeliveryToken.Valid || !strings.Contains(attempts[0].Message, "deliveryToken: "+attempts[0].DeliveryToken.String) {
		t.Fatalf("token %+v %v", attempts, err)
	}
}
func Test24_SCH_70_ReportAfterStagingIsRestatedAtClaim(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	reportNumber24(t, f, 2, 10)
	h := &sendHost{status: "idle"}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer["deliveryState"] != "dispatched" || packetNumber24(t, f, id) != 10 || len(h.sends) != 1 {
		t.Fatalf("restated %v %v %+v", answer, err, h.sends)
	}
}
func Test24_SCH_72_SettingsChangedBeforeTransportCancelsClaim(t *testing.T) {
	f := fixture24(t)
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	current := &delivery.TaskSettings{Data: delivery.Obj{{Key: "cwd", Value: "/before"}}}
	f.c.SettingsLoader = func(context.Context, string) (*delivery.TaskSettings, error) { return current, nil }
	f.c.beforeTransport = func() {
		current = &delivery.TaskSettings{Data: delivery.Obj{{Key: "cwd", Value: "/changed-after-gate"}}}
	}
	h := &sendHost{status: "idle"}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer != nil || len(h.sends) != 0 {
		t.Fatalf("cancel %v %v %+v", answer, err, h.sends)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "queued" || row.HoldReason.Valid {
		t.Fatalf("row %+v %v", row, err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || attempts[0].SendAttempted != "no" || attempts[0].RetrySafe != 1 || attempts[0].TransportStartedAt.Valid {
		t.Fatalf("attempts %+v %v", attempts, err)
	}
	f.c.beforeTransport = nil
	answer, err = f.c.Attempt(f.ctx, id, h, 1_700_000_001)
	if err != nil || answer["deliveryState"] != "dispatched" || h.settings.Data[0].Value != "/changed-after-gate" {
		t.Fatalf("retry %v %v settings %+v", answer, err, h.settings)
	}
}
func Test24_SCH_73_DischargedCompletionHeldAndReopened(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	_, err := f.s.Q(f.ctx).ExecContext(f.ctx, "INSERT INTO sync_targets(relationship_id,target,target_ref,recorded_at) VALUES ('rel-1','coordination_document','document-a',?)", f.at)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Q(f.ctx).ExecContext(f.ctx, "INSERT INTO sync_outbox(sync_id,relationship_id,issue_key,target,target_ref,subject_kind,event_id,identity_digest,summary,state,created_at,updated_at) VALUES ('sync-1','rel-1','REL-1','coordination_document','document-a','verdict','event-1','digest','ruled','confirmed',?,?)", f.at, f.at)
	if err != nil {
		t.Fatal(err)
	}
	h := &sendHost{status: "idle"}
	_, err = f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "superseded_revision" || !strings.Contains(refusal.Detail, "Linear record") {
		t.Fatalf("discharged %v", err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.HoldReason.String != "superseded_by_report" || len(h.sends) != 0 {
		t.Fatalf("held %+v %v", row, err)
	}
	_, err = f.s.Q(f.ctx).ExecContext(f.ctx, "UPDATE sync_targets SET target_ref='document-b' WHERE relationship_id='rel-1'")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_001)
	if err != nil || answer["deliveryState"] != "dispatched" || len(h.sends) != 1 {
		t.Fatalf("reopened %v %v %+v", answer, err, h.sends)
	}
}
func Test24_SCH_74_ApprovalPolicyPushRefusalCanRetry(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, staged := f.staged(t)
	id := staged["messageId"].(string)
	h := &policyRefusalHost{SendAdapter: &sendHost{status: "idle"}, refused: true}
	answer, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	if err != nil || answer["deliveryState"] != "withheld_pre_send" || answer["transportDeliveryState"] != "inbox_only" || answer["sendAttempted"] != "no" || answer["turnId"] != nil {
		t.Fatalf("refusal %v %v", answer, err)
	}
	row, err := f.c.Get(f.ctx, id)
	if err != nil || row.State != "withheld_pre_send" || row.HoldReason.Valid || !row.NextEligibleAt.Valid {
		t.Fatalf("row %+v %v", row, err)
	}
	ladder, err := f.c.Reach(f.ctx, id)
	if err != nil || ladder["transport_accepted"].(map[string]any)["state"] == "yes" {
		t.Fatalf("ladder %v %v", ladder, err)
	}
	answer, err = f.c.Attempt(f.ctx, id, h, row.NextEligibleAt.Float64)
	if err != nil || answer["deliveryState"] != "dispatched" {
		t.Fatalf("retry %v %v", answer, err)
	}
}
func Test24_SCH_56_ArchivedAtTransportDoesNotSend(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	f.c.beforeTransport = func() {
		if err := storeseed.ArchiveScopeBinding(f.ctx, f.s, "b-supervisor", "archived", "none", f.at); err != nil {
			t.Fatal(err)
		}
	}
	h := &sendHost{status: "idle"}
	_, err := f.c.Attempt(f.ctx, id, h, 1_700_000_000)
	var refusal Refusal
	if !errors.As(err, &refusal) || len(h.sends) != 0 {
		t.Fatalf("refusal %v sends %+v", err, h.sends)
	}
}
func Test24_SCH_57_TransportStartRecorded(t *testing.T) {
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	_, stage := f.staged(t)
	id := stage["messageId"].(string)
	_, err := f.c.Attempt(f.ctx, id, &sendHost{status: "idle"}, 1_700_000_000)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := f.s.SupervisorAttempts(f.ctx, id)
	if err != nil || len(attempts) != 1 || !attempts[0].TransportStartedAt.Valid {
		t.Fatalf("attempts %+v %v", attempts, err)
	}
}
