package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// Append a side table instead of changing the shipped decision table. Old
// read-only stores have no notes; writes install it with the ordinary DAG zone.
var userDecisionNotesZone = []string{`CREATE TABLE IF NOT EXISTS dag_user_decision_notes (
    decision_id TEXT PRIMARY KEY REFERENCES dag_user_decisions (decision_id),
    answer_note TEXT NOT NULL
)`}

func (s *Store) decodeUserDecisionWithNote(ctx context.Context, row Row) (decisions.Record, error) {
	record, err := decodeUserDecision(row)
	if err != nil {
		return record, err
	}
	err = s.q(ctx).QueryRowContext(ctx, "SELECT answer_note FROM dag_user_decision_notes WHERE decision_id=?", record.DecisionID).Scan(&record.AnswerNote)
	if errors.Is(err, sql.ErrNoRows) || err != nil && strings.Contains(err.Error(), "no such table: dag_user_decision_notes") {
		return record, nil
	}
	if err != nil {
		return record, err
	}
	at := record.RaisedAt
	record.RaisedAt = ""
	err = validateUserDecision(record)
	record.RaisedAt = at
	return record, err
}

func (s *Store) writeUserDecisionNote(ctx context.Context, record decisions.Record) error {
	_, err := s.exec(ctx, "INSERT INTO dag_user_decision_notes (decision_id,answer_note) VALUES (?,?) ON CONFLICT(decision_id) DO UPDATE SET answer_note=excluded.answer_note", record.DecisionID, record.AnswerNote)
	return err
}
