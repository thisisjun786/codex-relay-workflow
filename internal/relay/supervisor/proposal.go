package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// refreshProposal re-derives an unsent event's packet in the writer's transaction.
// The bytes are frozen only after the transport-start write commits.
func (c *Channel) latestStatement(ctx context.Context, o Obligation) (Obligation, error) {
	if o.Kind != "blocked" && o.Kind != "decision_request" {
		return o, nil
	}
	row := store.SupervisorMessagesRow{ObligationID: o.ID, ObligationKind: o.Kind, RelationshipID: o.RelationID, EventID: nullString(evidence.Text(o.Basis["eventId"]))}
	latest, err := c.newestRaising(ctx, row)
	if err != nil || latest == nil {
		return o, err
	}
	return *latest, nil
}

func (c *Channel) omissionWithdrawn(ctx context.Context, o Obligation, reading map[string]any, at string) string {
	derived := orderedMap(delivery.DeriveOmission(ctx, c.Store, c.StoreDirectory(), o.RelationID, o.Subject, at, 0))
	if derived["reason"] == delivery.OmittedDeclarationsMissing || derived["reason"] == "registration_unresolved" {
		if reading["source"] == delivery.OmittedStoreSource {
			return "this store no longer holds the claim record this omission was derived under, so it cannot derive it again"
		}
		return ""
	}
	if derived["reportingState"] != "unreported" || derived["owed"] != true {
		return "this store, which records this session's declarations, derives turn " + store.PyRepr(o.Subject) + " as " + store.PyRepr(evidence.Text(derived["reportingState"])) + " (" + evidence.Text(derived["reason"]) + ") and owed answers " + store.PyRepr(evidence.Text(derived["owedReason"])) + ", so nothing is owed through this message"
	}
	if reading["source"] == delivery.OmittedStoreSource && observationReadingKey(o, derived) != observationReadingKey(o, reading) {
		return "this store derives turn " + store.PyRepr(o.Subject) + "'s omission differently from the reading frozen on this message"
	}
	return ""
}

func (c *Channel) refreshProposal(ctx context.Context, row store.SupervisorMessagesRow, r Resolution, at string, currentAttempt int64) (changed bool, err error) {
	defer evidence.RecoverPython(&err)
	if !row.EventID.Valid {
		if row.Reading.Valid && row.ObligationKind == "unreported" {
			reading := map[string]any{}
			decoder := json.NewDecoder(strings.NewReader(row.Reading.String))
			decoder.UseNumber()
			if err := decoder.Decode(&reading); err != nil {
				return false, err
			}
			o := ObservationObligation(reading)
			if o != nil {
				if withdrawn := c.omissionWithdrawn(ctx, *o, reading, at); withdrawn != "" {
					return false, Refusal{"superseded_revision", withdrawn + ". Nothing is sent through this message; it is held as 'superseded_by_report', and staging the project stages what is owed now"}
				}
			}
			var event string
			err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT event_id FROM events WHERE relationship_id=? AND turn_id=? AND stage='final' ORDER BY rowid DESC LIMIT 1", row.RelationshipID, row.Subject).Scan(&event)
			if err == nil {
				return false, Refusal{"superseded_revision", "turn " + store.PyRepr(row.Subject) + " has a final receipt, event " + store.PyRepr(event) + ", accepted after this omission was staged; the turn reported, and what that event raises goes up as its own fact. Nothing is sent through this message; it is held as 'superseded_by_report', and staging the project stages what is owed now"}
			}
			if err != sql.ErrNoRows {
				return false, err
			}
		}
		return false, nil
	}
	o, err := c.newestRaising(ctx, row)
	if err != nil {
		return false, err
	}
	if o == nil || o.ID != row.ObligationID {
		raised, err := c.FromEvent(ctx, row.EventID.String)
		if err != nil {
			return false, err
		}
		description := "nothing"
		if raised != nil {
			description = store.PyRepr(raised.Kind) + " obligation " + store.PyRepr(raised.ID)
		}
		return false, Refusal{"superseded_revision", "no event raises obligation " + store.PyRepr(row.ObligationID) + " any more; event " + store.PyRepr(row.EventID.String) + " now raises " + description + ". Nothing is sent through this message; it is held as 'superseded_by_report', and staging the project stages what is owed now"}
	}
	owed, _, err := c.ReportableCurrent(ctx, *o)
	if err != nil {
		return false, err
	}
	if !owed {
		return false, Refusal{"superseded_revision", "the Linear record now confirms this obligation, so the level above already has it. Nothing is sent through this message; it is held as 'superseded_by_report', and staging the project stages what is owed now"}
	}
	eventID, _ := o.Basis["eventId"].(string)
	report, err := currentReport(ctx, c.Store, eventID)
	if err != nil {
		return false, err
	}
	old := evidence.Decode(row.Packet)
	observed := evidence.Dict(evidence.Item(old, "envelope"), false)["observedAt"]
	current, err := c.Compose(ctx, *o, r, at)
	if err != nil {
		return false, err
	}
	if observed != nil {
		current["envelope"].(map[string]any)["observedAt"] = observed
	}
	comparison, err := canonicalPacket(current)
	if err != nil {
		return false, err
	}
	if comparison == row.Packet && eventID == row.EventID.String && (!row.SubmissionNo.Valid && report.submission == 0 || row.SubmissionNo.Valid && row.SubmissionNo.Int64 == report.submission) {
		return false, nil
	}
	// A restatement at claim or transport start is observed at that instant; stage-time
	// restatement is handled in StageWithReading and retains its staging instant.
	p, err := c.Compose(ctx, *o, r, at)
	if err != nil {
		return false, err
	}
	encoded, err := canonicalPacket(p)
	if err != nil {
		return false, err
	}
	var submission sql.NullInt64
	if report.submission != 0 {
		submission = sql.NullInt64{Int64: report.submission, Valid: true}
	}
	result, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE supervisor_messages SET packet=?,event_id=?,submission_no=?,updated_at=? WHERE message_id=? AND state IN ('queued','deferred_busy','withheld_pre_send','sending') AND recipient_task_id=? AND NOT EXISTS (SELECT 1 FROM supervisor_attempts WHERE message_id=? AND attempt_no<>? AND (send_attempted<>'no' OR retry_safe=0))", encoded, eventID, submission, at, row.MessageID, r.Recipient, row.MessageID, currentAttempt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, Refusal{"superseded_revision", "the report was sent; the staged packet cannot be changed"}
	}
	instant := "claim"
	if row.State == "sending" {
		instant = "transport_start"
	}
	detail := evidence.Dumps(contract.OrderedObject{{Key: "fromEvent", Value: row.EventID.String}, {Key: "toEvent", Value: eventID}, {Key: "fromSubmission", Value: optionalNumber(row.SubmissionNo)}, {Key: "toSubmission", Value: optionalNumber(submission)}, {Key: "at", Value: instant}, {Key: "reason", Value: "what the obligation says moved after staging and nothing had been sent, so the message now carries what is owed now"}}, false, false, true)
	_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_restated',?,?)", at, row.MessageID, detail)
	return true, err
}

// ReportableCurrent checks discharge without treating the channel's own staged journal as a second report.
func (c *Channel) ReportableCurrent(ctx context.Context, o Obligation) (bool, string, error) {
	var target string
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT target_ref FROM sync_targets WHERE relationship_id=? AND target='coordination_document'", o.RelationID).Scan(&target)
	if err == sql.ErrNoRows {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	var state, ref string
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT state,target_ref FROM sync_outbox WHERE relationship_id=? AND event_id=? AND subject_kind='verdict' AND target='coordination_document' ORDER BY rowid DESC LIMIT 1", o.RelationID, o.Basis["eventId"]).Scan(&state, &ref)
	if err == sql.ErrNoRows {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	if state == "confirmed" && ref == target {
		return false, "already_in_the_record_the_supervisor_reads", nil
	}
	return true, "", nil
}

func (c *Channel) newestRaising(ctx context.Context, row store.SupervisorMessagesRow) (*Obligation, error) {
	if row.ObligationKind != "blocked" && row.ObligationKind != "decision_request" {
		return c.FromEvent(ctx, row.EventID.String)
	}
	rows, err := c.Store.Q(ctx).QueryContext(ctx, "SELECT event_id FROM events WHERE relationship_id=? ORDER BY first_seen_at DESC,rowid DESC", row.RelationshipID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		o, e := c.FromEvent(ctx, id)
		if e != nil {
			return nil, e
		}
		if o != nil && o.ID == row.ObligationID {
			return o, nil
		}
	}
	return nil, nil
}

func (c *Channel) holdSuperseded(ctx context.Context, id, requestID string, attemptNo int64, at, detail, owner string) error {
	record := evidence.Dumps(map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true, "reason": "what is owed moved between the claim and the transport", "proposal": "obsolete", "detail": detail}, false, true, true)
	if err := c.Store.SettleSupervisorAttempt(ctx, requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
		return err
	}
	if _, err := c.Store.SettleSupervisorMessage(ctx, id, "queued", sql.NullFloat64{}, sql.NullString{String: "superseded_by_report", Valid: true}, at, "sending", attemptNo, sql.NullString{String: owner, Valid: true}); err != nil {
		return err
	}
	_, err := c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_superseded',?,?)", at, id, evidence.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "detail", Value: detail}}, false, false, true))
	return err
}

func (c *Channel) reopenAddressed(ctx context.Context, row store.SupervisorMessagesRow, at string) error {
	return c.Store.Compose(ctx, func(tx context.Context, _ *sql.Conn) error {
		live, err := c.Get(tx, row.MessageID)
		if err != nil || live.HoldReason.String != "hierarchy_unresolved" {
			return err
		}
		r, err := c.Resolve(tx, live.RelationshipID)
		if err != nil || r.Sender != live.SenderTaskID || r.Recipient != live.RecipientTaskID || r.ProjectKey != live.ProjectKey.String {
			return err
		}
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason=NULL,updated_at=? WHERE message_id=? AND hold_reason='hierarchy_unresolved' AND state IN ('queued','deferred_busy','withheld_pre_send')", at, row.MessageID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return err
		}
		detail := evidence.Dumps(contract.OrderedObject{{Key: "proposal", Value: "addressed"}, {Key: "reason", Value: "the hierarchy names this message's endpoints again, so the hierarchy_unresolved hold is released"}}, false, false, true)
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_reopened',?,?)", at, row.MessageID, detail)
		return err
	})
}

func (c *Channel) reopenProposal(ctx context.Context, row store.SupervisorMessagesRow, at string) error {
	return c.Store.Compose(ctx, func(tx context.Context, _ *sql.Conn) error {
		live, err := c.Get(tx, row.MessageID)
		if err != nil || live.HoldReason.String != "superseded_by_report" {
			return err
		}
		r, err := c.Resolve(tx, live.RelationshipID)
		if err != nil {
			return err
		}
		if r.Recipient != live.RecipientTaskID {
			return nil
		}
		_, err = c.refreshProposal(tx, live, r, at, 0)
		if refusal := new(Refusal); errors.As(err, refusal) && refusal.Reason == "superseded_revision" {
			return nil
		}
		if err != nil {
			return err
		}
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason=NULL,next_eligible_at=NULL,updated_at=? WHERE message_id=? AND hold_reason='superseded_by_report' AND state IN ('queued','deferred_busy','withheld_pre_send')", at, row.MessageID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return err
		}
		detail := evidence.Dumps(contract.OrderedObject{{Key: "proposal", Value: "current"}, {Key: "reason", Value: "what this message is for is owed through it again, so the superseded_by_report hold is released"}}, false, false, true)
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_reopened',?,?)", at, row.MessageID, detail)
		return err
	})
}

func (c *Channel) loadSettings(ctx context.Context, task string) (*delivery.TaskSettings, error) {
	if c.settingsLoader != nil {
		return c.settingsLoader(ctx, task)
	}
	return delivery.AuthorizedSettings(ctx, c.Store, task, nil)
}

func sameSettings24(first, second *delivery.TaskSettings) bool {
	if first == nil || second == nil {
		return first == second
	}
	return first.SettingsFreeResume == second.SettingsFreeResume && evidence.Dumps(first.Data, false, true, true) == evidence.Dumps(second.Data, false, true, true)
}

func optionalNumber(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func nullableIntRepr(n sql.NullInt64) string {
	if !n.Valid {
		return "None"
	}
	return fmt.Sprint(n.Int64)
}

func nullableStringRepr(s sql.NullString) string {
	if !s.Valid {
		return "None"
	}
	return store.PyRepr(s.String)
}
