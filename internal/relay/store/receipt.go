package store

import (
	"context"
	"errors"
	"fmt"
)

// Receipt is the original serialized completion claim stored in events.receipt.
// There is no standalone receipts table in schema version 1.
type Receipt struct {
	EventID          string
	Payload          string
	Stage            string
	ObservationCount int64
}

func (s *Store) Receipt(ctx context.Context, eventID string) (Receipt, error) {
	var receipt Receipt
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id,receipt,stage,observation_count FROM events WHERE event_id=?`, eventID).Scan(&receipt.EventID, &receipt.Payload, &receipt.Stage, &receipt.ObservationCount)
	if err != nil {
		return Receipt{}, fmt.Errorf("receipt %q: %w", eventID, err)
	}
	return receipt, nil
}

// ReceiptsForRelationship returns receipt claims in their original first-seen order.
func (s *Store) ReceiptsForRelationship(ctx context.Context, relationshipID string) (_ []Receipt, err error) {
	rows, err := s.q(ctx).QueryContext(ctx, `SELECT event_id,receipt,stage,observation_count FROM events WHERE relationship_id=? ORDER BY first_seen_at,event_id`, relationshipID)
	if err != nil {
		return nil, fmt.Errorf("receipts for %q: %w", relationshipID, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var result []Receipt
	for rows.Next() {
		var receipt Receipt
		if scanErr := rows.Scan(&receipt.EventID, &receipt.Payload, &receipt.Stage, &receipt.ObservationCount); scanErr != nil {
			return nil, fmt.Errorf("scan receipt: %w", scanErr)
		}
		result = append(result, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("receipt rows: %w", err)
	}
	return result, nil
}
