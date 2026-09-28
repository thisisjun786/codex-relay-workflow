package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (c *Channel) Stranded(ctx context.Context, now float64) ([]store.SupervisorMessagesRow, error) {
	rows, err := c.Store.Q(ctx).QueryContext(ctx, "SELECT "+supervisorColumns+" FROM supervisor_messages WHERE state='sending' AND (lease_until IS NULL OR lease_until<=?) ORDER BY staged_at,message_id", now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []store.SupervisorMessagesRow{}
	for rows.Next() {
		var r store.SupervisorMessagesRow
		if err := rows.Scan(&r.MessageID, &r.ObligationID, &r.ObligationKind, &r.RelationshipID, &r.ProjectKey, &r.Purpose, &r.Kind, &r.SenderTaskID, &r.RecipientTaskID, &r.Subject, &r.Packet, &r.State, &r.AttemptCount, &r.NextEligibleAt, &r.HoldReason, &r.LeaseOwner, &r.LeaseUntil, &r.StagedAt, &r.UpdatedAt, &r.EventID, &r.SubmissionNo, &r.Reading); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

const supervisorColumns = "message_id,obligation_id,obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,attempt_count,next_eligible_at,hold_reason,lease_owner,lease_until,staged_at,updated_at,event_id,submission_no,reading"

func (c *Channel) recoverStranded(ctx context.Context, row store.SupervisorMessagesRow, now float64) (store.SupervisorMessagesRow, error) {
	if row.State != "sending" || row.LeaseUntil.Valid && row.LeaseUntil.Float64 > now {
		return row, nil
	}
	at := delivery.ISOOf(now)
	if c.clockISO != nil {
		at = c.clockISO()
	}
	err := c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		current, err := c.Get(tx, row.MessageID)
		if err != nil {
			return err
		}
		if current.State != "sending" || current.AttemptCount != row.AttemptCount || current.LeaseUntil.Valid && current.LeaseUntil.Float64 > now {
			return nil
		}
		attempt, err := c.Store.LatestSupervisorAttempt(tx, row.MessageID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && !attempt.TransportStartedAt.Valid {
			result, e := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET state='queued',next_eligible_at=NULL,lease_owner=NULL,lease_until=NULL,updated_at=? WHERE message_id=? AND state='sending' AND attempt_count=? AND (lease_until IS NULL OR lease_until<=?)", at, row.MessageID, row.AttemptCount, now)
			if e != nil {
				return e
			}
			moved, e := result.RowsAffected()
			if e != nil {
				return e
			}
			if moved == 0 {
				return nil
			}
			record := map[string]any{"requestId": attempt.RequestID, "messageId": row.MessageID, "attemptNo": row.AttemptCount, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true, "reason": "the claim's lease expired before its transport started, so nothing was sent"}
			if e = c.Store.SettleSupervisorAttempt(tx, attempt.RequestID, "withheld_pre_send", "no", 1, sql.NullString{}, evidence.Dumps(record, false, true, true), at); e != nil {
				return e
			}
			detail := evidence.Dumps(contract.OrderedObject{{Key: "attemptNo", Value: row.AttemptCount}, {Key: "leaseOwner", Value: optionalText(row.LeaseOwner)}, {Key: "leaseUntil", Value: row.LeaseUntil.Float64}, {Key: "reason", Value: "the lease expired before the transport started, so nothing was sent and the report is queued again"}}, false, false, true)
			_, e = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_released',?,?)", at, row.MessageID, string(detail))
			return e
		}
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET state='held_uncertain',lease_owner=NULL,lease_until=NULL,updated_at=? WHERE message_id=? AND state='sending' AND attempt_count=? AND (lease_until IS NULL OR lease_until<=?)", at, row.MessageID, row.AttemptCount, now)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_stranded',?,?)", at, row.MessageID, evidence.Dumps(contract.OrderedObject{{Key: "attemptNo", Value: row.AttemptCount}, {Key: "leaseOwner", Value: optionalText(row.LeaseOwner)}, {Key: "leaseUntil", Value: row.LeaseUntil.Float64}, {Key: "reason", Value: "the lease expired after the transport started and with no receipt, so what that send did is unknown and is not repeated"}}, false, false, true))
		return err
	})
	if err != nil {
		return row, err
	}
	return c.Get(ctx, row.MessageID)
}
