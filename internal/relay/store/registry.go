package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Relationship statuses and generation vocabulary shared with Python's registry.py.
const (
	StatusActive = "active"
	AnchorBound  = "bound"
)

// CurrentRelationship is registry.get: the relationship with its generation invariant enforced,
// so a pointer at a generation that is not retained is refused on every public read.
func (s *Store) CurrentRelationship(ctx context.Context, id string) (Relationship, error) {
	relationship, err := s.Relationship(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Relationship{}, refuse(ReasonUnregisteredRelationship, "no relationship %q", id)
	}
	if err != nil {
		return Relationship{}, err
	}
	if _, err := s.Generation(ctx, id, relationship.Generation); errors.Is(err, sql.ErrNoRows) {
		return Relationship{}, refuse(ReasonUnknownGeneration, "%q points at generation %d which is not retained", id, relationship.Generation)
	} else if err != nil {
		return Relationship{}, err
	}
	return relationship, nil
}

func journal(ctx context.Context, conn *sql.Conn, kind, subject, detail, at string) error {
	if _, err := conn.ExecContext(ctx, `INSERT INTO journal (at,kind,subject,detail) VALUES (?,?,?,?)`, at, kind, subject, detail); err != nil {
		return fmt.Errorf("journal %s: %w", kind, err)
	}
	return nil
}

func quoteJSON(text string) string {
	var buf strings.Builder
	appendPythonString(&buf, text)
	return buf.String()
}
