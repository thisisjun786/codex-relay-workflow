package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Message statuses reported for one attempt's frozen bytes (delivery.py _message_status).
const (
	MessageUnavailable     = "unavailable"
	MessagePrepared        = "prepared"
	MessageDispatched      = "dispatched"
	MessageHostLostTurn    = "host_lost_turn"
	MessageConfirmedUnsent = "confirmed_unsent"
	MessageUncertain       = "uncertain"
)

type FrozenAttempt struct {
	RequestID     string
	Number        int64
	Status        string
	DeliveryState sql.NullString
	SentAt        sql.NullString
	Message       sql.NullString
}

// FrozenAttempts never substitutes a preview for bytes absent from an old row, and reads each
// status from the attempt's settled record rather than from the bytes existing.
func (s *Store) FrozenAttempts(ctx context.Context, eventID string) (_ []FrozenAttempt, err error) {
	rows, err := s.q(ctx).QueryContext(ctx, `SELECT a.request_id,a.attempt_no,a.internal_state,a.state,a.record,a.sent_at,m.message FROM attempts AS a LEFT JOIN attempt_messages AS m ON m.request_id=a.request_id WHERE a.event_id=? ORDER BY a.attempt_no`, eventID)
	if err != nil {
		return nil, fmt.Errorf("frozen attempts: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var result []FrozenAttempt
	for rows.Next() {
		var r FrozenAttempt
		var internal string
		var record sql.NullString
		if scanErr := rows.Scan(&r.RequestID, &r.Number, &internal, &r.DeliveryState, &record, &r.SentAt, &r.Message); scanErr != nil {
			return nil, fmt.Errorf("frozen attempt row: %w", scanErr)
		}
		status, statusErr := messageStatus(internal, r, record)
		if statusErr != nil {
			return nil, statusErr
		}
		r.Status = status
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("frozen attempt rows: %w", err)
	}
	return result, nil
}

func messageStatus(internal string, row FrozenAttempt, record sql.NullString) (string, error) {
	if !row.Message.Valid {
		return MessageUnavailable, nil
	}
	if internal != "settled" {
		return MessagePrepared, nil
	}
	switch row.DeliveryState.String {
	case "dispatched", "acknowledged":
		return MessageDispatched, nil
	case MessageHostLostTurn:
		return MessageHostLostTurn, nil
	}
	if record.Valid && record.String != "" {
		var settled struct {
			SendAttempted string `json:"sendAttempted"`
		}
		if err := json.Unmarshal([]byte(record.String), &settled); err != nil {
			return "", fmt.Errorf("attempt %q record: %w", row.RequestID, err)
		}
		if settled.SendAttempted == "no" {
			return MessageConfirmedUnsent, nil
		}
	}
	return MessageUncertain, nil
}
