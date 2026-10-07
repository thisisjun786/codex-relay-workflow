package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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

// userDecisionColumnList is userDecisionColumns as the slice the writers walk.
var userDecisionColumnList = strings.Split(strings.ReplaceAll(userDecisionColumns, " ", ""), ",")

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

// List reads the records the filter selects, oldest first by the real instant of raised_at (and by
// decision_id when two share one instant). The project predicate selects a record whose origin
// project is P or one of whose observations names it: a question raised from two projects is one
// folded record, so the second project must see it too. A store that predates the DAG zone has no
// dag_user_decisions table and reads as empty, as the other zone reads do (dag.isMissingZone); any
// other SQL error is returned.
func (s *Store) List(ctx context.Context, filter UserDecisionFilter) ([]decisions.Record, error) {
	query := "SELECT " + userDecisionColumns + " FROM dag_user_decisions WHERE 1 = 1"
	args := []any{}
	if filter.State != "" {
		query += " AND state = ?"
		args = append(args, string(filter.State))
	}
	if filter.Project != "" {
		// The origin object and the seen array are JSON text, guarded as the rest of the store
		// guards it: a row written by another writer with text that is not JSON cannot make the
		// whole read fail. The two alternatives are parenthesised as one predicate, so a state
		// filter beside them still applies to both.
		query += " AND ((CASE WHEN json_valid(origin_json) THEN json_extract(origin_json, '$.project') END) = ?" +
			" OR EXISTS (SELECT 1 FROM json_each(CASE WHEN json_valid(seen_json) THEN seen_json ELSE '[]' END) AS entry" +
			"  WHERE (CASE WHEN json_valid(entry.value) THEN json_extract(entry.value, '$.source') END) = 'project:' || ?))"
		args = append(args, filter.Project, filter.Project)
	}
	rows, err := s.All(ctx, query, args...)
	if err != nil {
		if missingUserDecisionTable(err) {
			return []decisions.Record{}, nil
		}
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
	// The order is the instant raised_at names, not its spelling: a stored value that is not a
	// timestamp (a row a hand edit left) sorts after the parseable ones, in text order, and does
	// not fail the read.
	slices.SortStableFunc(out, compareUserDecisions)
	return out, nil
}

// compareUserDecisions orders two records by the instant of their raised_at, then by decision_id.
// An unparseable raised_at ranks after every parseable one; two unparseable ones compare by text.
func compareUserDecisions(first, second decisions.Record) int {
	firstAt, firstOK := userDecisionInstant(first.RaisedAt)
	secondAt, secondOK := userDecisionInstant(second.RaisedAt)
	if firstOK != secondOK {
		if firstOK {
			return -1
		}
		return 1
	}
	if !firstOK {
		if order := strings.Compare(first.RaisedAt, second.RaisedAt); order != 0 {
			return order
		}
		return strings.Compare(first.DecisionID, second.DecisionID)
	}
	if !firstAt.Equal(secondAt) {
		if firstAt.Before(secondAt) {
			return -1
		}
		return 1
	}
	return strings.Compare(first.DecisionID, second.DecisionID)
}

// userDecisionInstant is the instant a stored raised_at names, or false for text that is not one.
func userDecisionInstant(value string) (time.Time, bool) {
	if value == "" || strings.Contains(value, ",") {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	return parsed, err == nil
}

// missingUserDecisionTable reports the one read failure that means the store has no user-decision
// table: the DAG zone never reached it. The judgment is on that table's name alone, as dag's
// isMissingZone judges the zone's, so another missing table is still an error.
func missingUserDecisionTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table: dag_user_decisions")
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
	// The fold's options go back too: a raise that names a reply a stored record does not carry
	// (one written before the reply field existed) fills it in, and the reply an answer is applied
	// against is the one the row must carry. The merged set is the stored set with those replies, so
	// the option ids and the fingerprint are unchanged.
	options, err := json.Marshal(record.Options)
	if err != nil {
		return fmt.Errorf("encode options: %w", err)
	}
	_, err = s.exec(ctx, "UPDATE dag_user_decisions SET state = ?, seen_json = ?, options_json = ? WHERE decision_id = ?",
		string(record.State), string(encoded), string(options), record.DecisionID)
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
	// A stored raised_at the format refuses - a row written before the check existed, or by a
	// hand edit - must not fail the read: List still orders such a record, after the parseable
	// ones. Every other check still applies, and the writer path (Raise, Update) refuses one.
	keptRaisedAt := record.RaisedAt
	record.RaisedAt = ""
	err := validateUserDecision(record)
	record.RaisedAt = keptRaisedAt
	if err != nil {
		return decisions.Record{}, err
	}
	return record, nil
}

// ErrUserDecisionAbsent is Get's refusal of a decision_id no row carries.
var ErrUserDecisionAbsent = errors.New("store: no user decision carries that decision id")

// Get reads one record by its decision_id.
func (s *Store) Get(ctx context.Context, decisionID string) (decisions.Record, error) {
	row, err := s.One(ctx, "SELECT "+userDecisionColumns+" FROM dag_user_decisions WHERE decision_id = ?", decisionID)
	if err != nil {
		if missingUserDecisionTable(err) {
			// A store without the table holds no record, so the answer is the same as for an id no
			// row carries rather than a host failure.
			return decisions.Record{}, fmt.Errorf("%w: %q", ErrUserDecisionAbsent, decisionID)
		}
		return decisions.Record{}, err
	}
	if row == nil {
		return decisions.Record{}, fmt.Errorf("%w: %q", ErrUserDecisionAbsent, decisionID)
	}
	return decodeUserDecision(row)
}

// Update reads the record decisionID names, hands it to mutate, and stores what mutate returns -
// one transaction, so the read the mutation decided on is the row the write replaces (a second
// writer between the two would otherwise be overwritten blind). mutate may refuse, in which case
// nothing is written. The row is written whole: every column is written back from the record the
// store itself read, so no field the reader holds is dropped by a narrower write.
//
// mutate is handed the transaction's context, and every store call it makes must use it: the
// transaction holds the store's one writable connection, so a read on the pool inside it would
// wait for the connection the transaction is holding.
func (s *Store) Update(ctx context.Context, decisionID string, mutate func(context.Context, decisions.Record) (decisions.Record, error)) (decisions.Record, error) {
	var answer decisions.Record
	err := s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		stored, err := s.Get(txCtx, decisionID)
		if err != nil {
			return err
		}
		updated, err := mutate(txCtx, stored)
		if err != nil {
			return err
		}
		// The stored record is written back with the raised_at it was read with: a value the
		// format refuses (a row written before the check, or by a hand edit) must not make the
		// record unanswerable, so the writer validates the record without re-checking the one
		// field the reader deliberately preserves. Every other field, and any raised_at the
		// mutation changed, is still checked.
		// The record keeps the identity Update was asked about: a mutation that returns another
		// decision_id would otherwise leave the requested row untouched and overwrite another.
		if updated.DecisionID != stored.DecisionID {
			return fmt.Errorf("%w: a mutation changed %q to %q", ErrUserDecisionAbsent, stored.DecisionID, updated.DecisionID)
		}
		keptRaisedAt := updated.RaisedAt
		if keptRaisedAt == stored.RaisedAt {
			updated.RaisedAt = ""
		}
		validationErr := validateUserDecision(updated)
		updated.RaisedAt = keptRaisedAt
		if validationErr != nil {
			return validationErr
		}
		if err := s.writeUserDecision(txCtx, updated); err != nil {
			return err
		}
		answer = updated
		return nil
	})
	if err != nil {
		return decisions.Record{}, err
	}
	return answer, nil
}

// writeUserDecision replaces the row decision_id names with the record's every field.
func (s *Store) writeUserDecision(ctx context.Context, record decisions.Record) error {
	values, err := encodeUserDecision(record)
	if err != nil {
		return err
	}
	assignments := make([]string, 0, len(userDecisionColumnList))
	for _, column := range userDecisionColumnList {
		if column == "decision_id" {
			continue
		}
		assignments = append(assignments, column+" = ?")
	}
	args := make([]any, 0, len(values))
	for i, column := range userDecisionColumnList {
		if column == "decision_id" {
			continue
		}
		args = append(args, values[i])
	}
	args = append(args, record.DecisionID)
	_, err = s.exec(ctx, "UPDATE dag_user_decisions SET "+strings.Join(assignments, ", ")+" WHERE decision_id = ?", args...)
	return err
}
