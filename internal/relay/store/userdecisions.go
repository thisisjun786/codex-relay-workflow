package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// The user-decision record (crw-user-decision/1, internal/relay/decisions) in the additive DAG zone:
// dag_user_decisions, one column per field of the record, the object and array fields as JSON text.
// The format, the fingerprint and the state machine belong to the decisions package. Raise owns the
// two states a question has before it is answered, open and raised; recording an answer, applying it
// or withdrawing it is the answer/apply/withdraw path, so a raise carrying one of those states is
// refused rather than stored.

// UserDecisionFilter narrows List. The zero value lists every record.
type UserDecisionFilter struct {
	// State, when not empty, restricts the answer to records in that state.
	State decisions.State
	// Project, when not empty, restricts the answer to records whose origin project is this key.
	Project string
}

// The named refusals Raise makes beyond decisions.Validate. Use errors.Is.
var (
	ErrUserDecisionState       = errors.New("store: a raise carries an open or raised record")
	ErrUserDecisionGeneration  = errors.New("store: the user decision's applied_generation is negative")
	ErrUserDecisionFingerprint = errors.New("store: the user decision's fingerprint is not its content's")
)

// raiseStates are the states a raise carries and the states a second raise of one fingerprint folds
// into: a question that is open or raised has no answer yet, so the same question raised again is
// the same record. A record that carries an answer is history, and a later raise of its question is
// a new record with its own decision_id.
var raiseStates = []decisions.State{decisions.StateOpen, decisions.StateRaised}

// userDecisionColumns is the row's column list, in the order the INSERT and the SELECT use.
const userDecisionColumns = "decision_id, fingerprint, kind, context, options_json," +
	" recommendation_json, blocking_json, needed_by, origin_json, source_json, authority_json," +
	" state, raised_at, raised_via, seen_json, answered_at, answered_by, answered_via, answer_text," +
	" applied_at, applied_event, applied_generation, withdrawn_reason, expired_reason"

// Raise records a user decision: it validates the record and requires its fingerprint to be its own
// content's, then, under one writer transaction, either folds the record into the open or raised
// record that already carries this fingerprint (no new row, merged true) or inserts it (merged
// false). What it returns is what it stored, so a caller can compare it with what List reads back.
func (s *Store) Raise(ctx context.Context, record decisions.Record) (decisions.Record, bool, error) {
	// An absent list is stored as an empty array, so the record Raise returns and the record List
	// reads back are the same value.
	if record.Options == nil {
		record.Options = []decisions.Option{}
	}
	if record.Blocking == nil {
		record.Blocking = []decisions.Blocking{}
	}
	if record.Seen == nil {
		record.Seen = []decisions.Seen{}
	}
	if err := validateUserDecision(record); err != nil {
		return decisions.Record{}, false, err
	}
	if !slices.Contains(raiseStates, record.State) {
		return decisions.Record{}, false, fmt.Errorf("%w: %q", ErrUserDecisionState, record.State)
	}
	var answer decisions.Record
	var merged bool
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		stored, err := s.userDecisionByFingerprint(ctx, record.Fingerprint)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if err := s.insertUserDecision(ctx, record); err != nil {
				return err
			}
			answer, merged = record, false
			return nil
		case err != nil:
			return err
		}
		folded, err := foldUserDecision(stored, record)
		if err != nil {
			return err
		}
		if err := s.updateUserDecisionObservation(ctx, folded); err != nil {
			return err
		}
		answer, merged = folded, true
		return nil
	})
	if err != nil {
		return decisions.Record{}, false, err
	}
	return answer, merged, nil
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

// userDecisionByFingerprint is the fold candidate: the open or raised record of a fingerprint.
func (s *Store) userDecisionByFingerprint(ctx context.Context, fingerprint string) (decisions.Record, error) {
	row, err := s.One(ctx, "SELECT "+userDecisionColumns+" FROM dag_user_decisions"+
		" WHERE fingerprint = ? AND state IN ("+raiseStatesSQL()+") ORDER BY raised_at, decision_id LIMIT 1", fingerprint)
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

// updateUserDecisionObservation writes what a fold changes: the observations, and the state when the
// fold advanced it.
func (s *Store) updateUserDecisionObservation(ctx context.Context, record decisions.Record) error {
	encoded, err := json.Marshal(record.Seen)
	if err != nil {
		return fmt.Errorf("encode seen: %w", err)
	}
	_, err = s.exec(ctx, "UPDATE dag_user_decisions SET state = ?, seen_json = ? WHERE decision_id = ?",
		string(record.State), string(encoded), record.DecisionID)
	return err
}

// validateUserDecision is decisions.Validate plus the two things the table would otherwise answer
// with a constraint failure: a generation its CHECK refuses, and a fingerprint that is not the
// record's own content's, which would file two different questions under one key.
func validateUserDecision(record decisions.Record) error {
	if err := decisions.Validate(record); err != nil {
		return err
	}
	if record.AppliedGeneration < 0 {
		return fmt.Errorf("%w: %d", ErrUserDecisionGeneration, record.AppliedGeneration)
	}
	if content := decisions.Fingerprint(record.Context, record.Blocking, record.Options); content != record.Fingerprint {
		return fmt.Errorf("%w: %q is not its content's %q", ErrUserDecisionFingerprint, record.Fingerprint, content)
	}
	return nil
}

// foldUserDecision folds a second raise of one question into the row that already holds it. The
// format owns what makes two statements one question: the fingerprint (its key fields are the
// context, the blocking subjects and the option ids) and the answer, and the format's Merge appends
// the new observations to the row that carries them, refusing a record whose answer differs. The
// stored state then advances through the format's state machine when the transition is allowed,
// which is how a second raise of an open question raises it (open -> raised).
func foldUserDecision(stored, incoming decisions.Record) (decisions.Record, error) {
	advance := incoming.State != stored.State && decisions.CanTransition(stored.State, incoming.State)
	sameState := incoming
	sameState.State = stored.State
	folded, err := decisions.Merge(stored, sameState)
	if err != nil {
		return decisions.Record{}, err
	}
	if advance {
		if err := decisions.Transition(&folded, incoming.State); err != nil {
			return decisions.Record{}, err
		}
	}
	return folded, nil
}

// raiseStatesSQL is raiseStates as the lookup's IN list, so the state check and the query cannot
// drift apart. The values are the vocabulary's own constants.
func raiseStatesSQL() string {
	quoted := make([]string, len(raiseStates))
	for i, state := range raiseStates {
		quoted[i] = "'" + string(state) + "'"
	}
	return strings.Join(quoted, ",")
}

// encodeUserDecision is the row's values in userDecisionColumns order: the scalar fields as they are
// and the object and array fields as JSON text. An absent recommendation is the empty text.
func encodeUserDecision(record decisions.Record) ([]any, error) {
	recommendation := ""
	if record.Recommendation != nil {
		encoded, err := json.Marshal(record.Recommendation)
		if err != nil {
			return nil, fmt.Errorf("encode recommendation: %w", err)
		}
		recommendation = string(encoded)
	}
	encoded := make([]string, 0, 6)
	for _, value := range []any{record.Options, record.Blocking, record.Seen, record.Origin, record.Source, record.Authority} {
		text, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode %T: %w", value, err)
		}
		encoded = append(encoded, string(text))
	}
	return []any{
		record.DecisionID, record.Fingerprint, string(record.Kind), record.Context,
		encoded[0], recommendation, encoded[1], record.NeededBy,
		encoded[3], encoded[4], encoded[5],
		string(record.State), record.RaisedAt, record.RaisedVia, encoded[2],
		record.AnsweredAt, record.AnsweredBy, record.AnsweredVia, record.AnswerText,
		record.AppliedAt, record.AppliedEvent, record.AppliedGeneration,
		record.WithdrawnReason, record.ExpiredReason,
	}, nil
}

// decodeUserDecision reads one row back into a record. The row is a boundary: a value that does not
// decode, or a record that no longer validates, is an error rather than a half-read record.
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
	var recommendation decisions.Recommendation
	for _, column := range []struct {
		name string
		into any
	}{
		{"options_json", &record.Options},
		{"blocking_json", &record.Blocking},
		{"seen_json", &record.Seen},
		{"origin_json", &record.Origin},
		{"source_json", &record.Source},
		{"authority_json", &record.Authority},
		{"recommendation_json", &recommendation},
	} {
		if text := row.Text(column.name); text != "" {
			if err := json.Unmarshal([]byte(text), column.into); err != nil {
				return decisions.Record{}, fmt.Errorf("decode %s: %w", column.name, err)
			}
		}
	}
	if row.Text("recommendation_json") != "" {
		record.Recommendation = &recommendation
	}
	if err := validateUserDecision(record); err != nil {
		return decisions.Record{}, err
	}
	return record, nil
}
