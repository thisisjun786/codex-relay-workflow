package store

import (
	"context"
	"database/sql"
)

func (s *Store) RecordRefusal(ctx context.Context, refusal Refusal) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO refusals (at,relationship_id,event_id,reason,detail,payload) VALUES (?,?,?,?,?,?)`, refusal.At, refusal.RelationshipID, refusal.EventID, refusal.Reason, refusal.Detail, refusal.Payload)
		return err
	})
}

func (s *Store) RecordDiscoveryCursor(ctx context.Context, cursor DiscoveryCursor) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO discovery_cursors (task_id,listing,cursor,exhausted,scanned,updated_at) VALUES (?,?,?,?,?,?) ON CONFLICT(task_id,listing) DO UPDATE SET cursor=excluded.cursor,exhausted=excluded.exhausted,scanned=excluded.scanned,updated_at=excluded.updated_at`, cursor.TaskID, cursor.Listing, cursor.Cursor, cursor.Exhausted, cursor.Scanned, cursor.UpdatedAt)
		return err
	})
}
