package store

import (
	"context"
	"database/sql"
	"fmt"
)

type Refusal struct {
	ID             int64
	At             string
	RelationshipID sql.NullString
	EventID        sql.NullString
	Reason         string
	Detail         sql.NullString
	Payload        sql.NullString
}

type DiscoveryCursor struct {
	TaskID    string
	Listing   string
	Cursor    sql.NullString
	Exhausted bool
	Scanned   int64
	UpdatedAt string
}

func (s *Store) DiscoveryCursor(ctx context.Context, taskID, listing string) (DiscoveryCursor, error) {
	var r DiscoveryCursor
	err := s.q(ctx).QueryRowContext(ctx, `SELECT task_id,listing,cursor,exhausted,scanned,updated_at FROM discovery_cursors WHERE task_id=? AND listing=?`, taskID, listing).Scan(&r.TaskID, &r.Listing, &r.Cursor, &r.Exhausted, &r.Scanned, &r.UpdatedAt)
	if err != nil {
		return DiscoveryCursor{}, fmt.Errorf("discovery cursor %q/%q: %w", taskID, listing, err)
	}
	return r, nil
}
