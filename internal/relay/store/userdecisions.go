package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// The user-decision record (crw-user-decision/1, internal/relay/decisions) in the additive DAG
// zone: dag_user_decisions. The table is one column per field of the record's Q1 field list, with
// the object and array fields held as JSON text, decision_id as the primary key and fingerprint
// indexed. The format, the fingerprint and the state machine belong to the decisions package; this
// file only stores a record and reads it back.

// UserDecisionFilter narrows List. The zero value lists every record.
type UserDecisionFilter struct {
	// State, when not empty, restricts the answer to records in that state.
	State decisions.State
	// Project, when not empty, restricts the answer to records whose origin project is this key.
	Project string
}

// mergeStates are the states a second raise of the same fingerprint merges into. A record that
// carries an answer or a terminal state is history: a later raise of its question is a new record,
// because decisions.Merge refuses two records whose answer differs.
var mergeStates = []decisions.State{decisions.StateOpen, decisions.StateRaised}

// userDecisionColumns is the row's column list, in the order both the INSERT and the SELECT use.
const userDecisionColumns = "decision_id, fingerprint, kind, context, options_json," +
	" recommendation_json, blocking_json, needed_by, origin_json, source_json, authority_json," +
	" state, raised_at, raised_via, seen_json, answered_at, answered_by, answered_via, answer_text," +
	" applied_at, applied_event, applied_generation, withdrawn_reason, expired_reason"

// Raise records a user decision. It validates the record, requires its fingerprint to be its own
// content's, and then, under one writer transaction, either appends the record's observations to
// the open or raised record that already carries this fingerprint (merged true, no new row) or
// inserts the record as a new row (merged false).
func (s *Store) Raise(ctx context.Context, record decisions.Record) (decisions.Record, bool, error) {
	if err := validateUserDecision(record); err != nil {
		return decisions.Record{}, false, err
	}
	var merged decisions.Record
	var didMerge bool
	err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		open, err := s.userDecisionByFingerprint(ctx, record.Fingerprint)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := s.insertUserDecision(ctx, record); err != nil {
				return err
			}
			merged, didMerge = record, false
			return nil
		case err != nil:
			return err
		}
		folded, err := decisions.Merge(open, record)
		if err != nil {
			return err
		}
		if err := s.updateUserDecisionSeen(ctx, folded.DecisionID, folded.Seen); err != nil {
			return err
		}
		merged, didMerge = folded, true
		return nil
	})
	if err != nil {
		return decisions.Record{}, false, err
	}
	return merged, didMerge, nil
}

// List reads the records the filter selects, oldest first. The project predicate reads the stored
// origin object, guarded as the rest of the store guards JSON text.
func (s *Store) List(ctx context.Context, filter UserDecisionFilter) ([]decisions.Record, error) {
	query := "SELECT " + userDecisionColumns + " FROM dag_user_decisions WHERE 1 = 1"
	args := []any{}
	if filter.State != "" {
		query += " AND state = ?"
		args = append(args, string(filter.State))
	}
	if filter.Project != "" {
		query += " AND (CASE WHEN json_valid(origin_json) THEN json_extract(origin_json, '$.project') END) = ?"
		args = append(args, filter.Project)
	}
	query += " ORDER BY raised_at, decision_id"
	rows, err := s.All(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := make([]decisions.Record, 0, len(rows))
	for _, row := range rows {
		record, err := decodeUserDecision(row)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, nil
}

// userDecisionByFingerprint is the merge candidate: the open or raised record of a fingerprint.
func (s *Store) userDecisionByFingerprint(ctx context.Context, fingerprint string) (decisions.Record, error) {
	row, err := s.One(ctx, "SELECT "+userDecisionColumns+" FROM dag_user_decisions"+
		" WHERE fingerprint = ? AND state IN ('open','raised') ORDER BY raised_at, decision_id LIMIT 1", fingerprint)
	if err != nil {
		return decisions.Record{}, err
	}
	return decodeUserDecision(row)
}

func (s *Store) insertUserDecision(ctx context.Context, record decisions.Record) error {
	values, err := encodeUserDecision(record)
	if err != nil {
		return err
	}
	_, err = s.exec(ctx, "INSERT INTO dag_user_decisions ("+userDecisionColumns+")"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", values...)
	return err
}

func (s *Store) updateUserDecisionSeen(ctx context.Context, decisionID string, seen []decisions.Seen) error {
	encoded, err := json.Marshal(seenOrEmpty(seen))
	if err != nil {
		return fmt.Errorf("encode seen: %w", err)
	}
	_, err = s.exec(ctx, "UPDATE dag_user_decisions SET seen_json = ? WHERE decision_id = ?", string(encoded), decisionID)
	return err
}

// validateUserDecision is decisions.Validate plus the one thing the store must not store: a
// fingerprint field that is not the record's own content's fingerprint, which would file two
// different questions under one key.
func validateUserDecision(record decisions.Record) error {
	if err := decisions.Validate(record); err != nil {
		return err
	}
	if content := decisions.Fingerprint(record.Context, record.Blocking, record.Options); content != record.Fingerprint {
		return fmt.Errorf("user decision %q: fingerprint is %q, its content's is %q", record.DecisionID, record.Fingerprint, content)
	}
	return nil
}

func seenOrEmpty(seen []decisions.Seen) []decisions.Seen {
	if seen == nil {
		return []decisions.Seen{}
	}
	return seen
}

// encodeUserDecision is the row's values in userDecisionColumns order: the scalar fields as they
// are and the object and array fields as JSON text.
func encodeUserDecision(record decisions.Record) ([]any, error) {
	recommendation := ""
	if record.Recommendation != nil {
		encoded, err := json.Marshal(record.Recommendation)
		if err != nil {
			return nil, fmt.Errorf("encode recommendation: %w", err)
		}
		recommendation = string(encoded)
	}
	arrays := []any{}
	for _, value := range []any{record.Options, record.Blocking, seenOrEmpty(record.Seen)} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode %T: %w", value, err)
		}
		arrays = append(arrays, string(encoded))
	}
	objects := []any{}
	for _, value := range []any{record.Origin, record.Source, record.Authority} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode %T: %w", value, err)
		}
		objects = append(objects, string(encoded))
	}
	return []any{
		record.DecisionID, record.Fingerprint, string(record.Kind), record.Context,
		arrays[0], recommendation, arrays[1], record.NeededBy,
		objects[0], objects[1], objects[2],
		string(record.State), record.RaisedAt, record.RaisedVia, arrays[2],
		record.AnsweredAt, record.AnsweredBy, record.AnsweredVia, record.AnswerText,
		record.AppliedAt, record.AppliedEvent, record.AppliedGeneration,
		record.WithdrawnReason, record.ExpiredReason,
	}, nil
}

// decodeUserDecision reads one row back into a record. The row is a boundary: a value that does
// not decode, or a record that no longer validates, is an error rather than a half-read record.
func decodeUserDecision(row Row) (decisions.Record, error) {
	if row == nil {
		return decisions.Record{}, sql.ErrNoRows
	}
	record := decisions.Record{
		Schema:          decisions.Schema,
		DecisionID:      row.Text("decision_id"),
		Fingerprint:     row.Text("fingerprint"),
		Kind:            decisions.Kind(row.Text("kind")),
		Context:         row.Text("context"),
		NeededBy:        row.Text("needed_by"),
		State:           decisions.State(row.Text("state")),
		RaisedAt:        row.Text("raised_at"),
		RaisedVia:       row.Text("raised_via"),
		AnsweredAt:      row.Text("answered_at"),
		AnsweredBy:      row.Text("answered_by"),
		AnsweredVia:     row.Text("answered_via"),
		AnswerText:      row.Text("answer_text"),
		AppliedAt:       row.Text("applied_at"),
		AppliedEvent:    row.Text("applied_event"),
		WithdrawnReason: row.Text("withdrawn_reason"),
		ExpiredReason:   row.Text("expired_reason"),
	}
	if generation := row.Get("applied_generation"); generation != nil {
		number, ok := generation.(int64)
		if !ok {
			return decisions.Record{}, fmt.Errorf("user decision %q: applied_generation is %T", record.DecisionID, generation)
		}
		record.AppliedGeneration = number
	}
	if err := decodeJSONColumn(row.Text("options_json"), &record.Options); err != nil {
		return decisions.Record{}, err
	}
	if err := decodeJSONColumn(row.Text("blocking_json"), &record.Blocking); err != nil {
		return decisions.Record{}, err
	}
	if err := decodeJSONColumn(row.Text("seen_json"), &record.Seen); err != nil {
		return decisions.Record{}, err
	}
	if err := decodeJSONColumn(row.Text("origin_json"), &record.Origin); err != nil {
		return decisions.Record{}, err
	}
	if err := decodeJSONColumn(row.Text("source_json"), &record.Source); err != nil {
		return decisions.Record{}, err
	}
	if err := decodeJSONColumn(row.Text("authority_json"), &record.Authority); err != nil {
		return decisions.Record{}, err
	}
	if recommendation := row.Text("recommendation_json"); recommendation != "" {
		record.Recommendation = &decisions.Recommendation{}
		if err := decodeJSONColumn(recommendation, record.Recommendation); err != nil {
			return decisions.Record{}, err
		}
	}
	if err := validateUserDecision(record); err != nil {
		return decisions.Record{}, err
	}
	return record, nil
}

func decodeJSONColumn(text string, into any) error {
	if text == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(text), into); err != nil {
		return fmt.Errorf("decode %T: %w", into, err)
	}
	return nil
}
