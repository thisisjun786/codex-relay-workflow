package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
)

// The user-decision table in the additive DAG zone (CRW-736): the record of internal/relay/decisions
// persisted with typed reads and writes. The red run of this file, before the appended statement
// exists, is the proof the table is missing.

// userDecision is a well-formed crw-user-decision/1 record whose fingerprint is its own content's.
func userDecision(id, context, project string, state decisions.State) decisions.Record {
	record := decisions.Record{
		Schema:     decisions.Schema,
		DecisionID: id,
		Kind:       decisions.KindPolicy,
		Context:    context,
		Options:    []decisions.Option{{ID: "a", Label: "A", Effect: "e"}, {ID: "b", Label: "B", Effect: "e"}},
		Blocking:   []decisions.Blocking{{Kind: decisions.BlockingIssue, Ref: "CRW-1"}},
		Origin:     decisions.Origin{Issue: "CRW-736", Project: project},
		Source:     decisions.Source{Kind: "report", Ref: "report:1"},
		Authority:  decisions.Authority{Kind: decisions.AuthorityUser, Ref: "user"},
		State:      state,
		RaisedAt:   "2026-10-06T00:00:00Z",
		RaisedVia:  "direct-ask",
		Seen:       []decisions.Seen{{At: "2026-10-06T00:00:00Z", Source: "report:1"}},
	}
	record.Fingerprint = decisions.Fingerprint(record.Context, record.Blocking, record.Options)
	return record
}

// userDecisionRows counts the rows the table holds, so a test can say whether a raise added one.
func userDecisionRows(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM dag_user_decisions").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// The table arrives with the zone: a store this build opened carries dag_user_decisions. Before the
// appended CREATE this test fails with "no such table: dag_user_decisions" - the red evidence.
func TestUserDecisionTableIsCreatedByTheZone(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	rows, err := s.All(context.Background(), "SELECT decision_id FROM dag_user_decisions")
	if err != nil {
		t.Fatalf("reading dag_user_decisions: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a fresh store holds %d user decisions", len(rows))
	}
}

// One raise writes one row; a second raise of the same fingerprint adds no row, appends to seen and
// reports merged true, and the record reads back field for field.
func TestUserDecisionRaiseInsertsOneRowThenMergesTheSameFingerprint(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	first := userDecision("ud-1", "Which default does the retry take?", "PRJ-A", decisions.StateOpen)

	raised, merged, err := s.Raise(ctx, first)
	must(t, err)
	if merged {
		t.Fatal("the first raise of a fingerprint merged into a row that cannot exist")
	}
	if raised.DecisionID != first.DecisionID || len(raised.Seen) != 1 {
		t.Fatalf("first raise returned %+v", raised)
	}
	if got := userDecisionRows(t, s); got != 1 {
		t.Fatalf("rows after the first raise = %d, want 1", got)
	}

	second := first
	second.DecisionID = "ud-2"
	second.Seen = []decisions.Seen{{At: "2026-10-06T01:00:00Z", Source: "report:2"}}
	folded, merged, err := s.Raise(ctx, second)
	must(t, err)
	if !merged {
		t.Fatal("the second raise of the same fingerprint inserted a new row")
	}
	if got := userDecisionRows(t, s); got != 1 {
		t.Fatalf("rows after the second raise = %d, want 1", got)
	}
	if folded.DecisionID != "ud-1" || len(folded.Seen) != 2 || folded.Seen[0].Source != "report:1" || folded.Seen[1].Source != "report:2" {
		t.Fatalf("merged record = %+v, want the stored id and both observations in order", folded)
	}

	listed, err := s.List(ctx, UserDecisionFilter{})
	must(t, err)
	if len(listed) != 1 {
		t.Fatalf("List returned %d records, want 1", len(listed))
	}
	if !reflect.DeepEqual(listed[0], folded) {
		t.Fatalf("the stored record did not round trip:\n stored %+v\n listed %+v", folded, listed[0])
	}
}

// A record that already carries an answer is history: raising its question again is a new record.
func TestUserDecisionRaiseOfAAnsweredQuestionInsertsANewRow(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	first := userDecision("ud-1", "Which default does the retry take?", "PRJ-A", decisions.StateOpen)
	_, _, err := s.Raise(ctx, first)
	must(t, err)
	if _, err := s.DB.ExecContext(ctx, "UPDATE dag_user_decisions SET state = 'answered' WHERE decision_id = ?", first.DecisionID); err != nil {
		t.Fatal(err)
	}

	second := first
	second.DecisionID = "ud-2"
	raised, merged, err := s.Raise(ctx, second)
	must(t, err)
	if merged {
		t.Fatal("a raise merged into an answered record")
	}
	if raised.DecisionID != "ud-2" || userDecisionRows(t, s) != 2 {
		t.Fatalf("raised %+v, rows %d, want the new record and two rows", raised, userDecisionRows(t, s))
	}
}

// List filters by state and by the record's origin project, alone and together.
func TestUserDecisionListFiltersByStateAndProject(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	for _, record := range []decisions.Record{
		userDecision("ud-1", "First question?", "PRJ-A", decisions.StateOpen),
		userDecision("ud-2", "Second question?", "PRJ-B", decisions.StateRaised),
		userDecision("ud-3", "Third question?", "PRJ-B", decisions.StateOpen),
	} {
		_, _, err := s.Raise(ctx, record)
		must(t, err)
	}

	for _, test := range []struct {
		name   string
		filter UserDecisionFilter
		want   []string
	}{
		{"no filter", UserDecisionFilter{}, []string{"ud-1", "ud-2", "ud-3"}},
		{"by state", UserDecisionFilter{State: decisions.StateOpen}, []string{"ud-1", "ud-3"}},
		{"by project", UserDecisionFilter{Project: "PRJ-B"}, []string{"ud-2", "ud-3"}},
		{"by state and project", UserDecisionFilter{State: decisions.StateOpen, Project: "PRJ-B"}, []string{"ud-3"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			listed, err := s.List(ctx, test.filter)
			must(t, err)
			ids := make([]string, 0, len(listed))
			for _, record := range listed {
				ids = append(ids, record.DecisionID)
			}
			if !reflect.DeepEqual(ids, test.want) {
				t.Fatalf("List(%+v) = %v, want %v", test.filter, ids, test.want)
			}
		})
	}
}

// A record the format refuses, and a fingerprint that is not the record's own, write nothing.
func TestUserDecisionRaiseRefusesAnInvalidRecord(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	invalid := userDecision("ud-1", "Which default does the retry take?", "PRJ-A", decisions.StateOpen)
	invalid.Schema = "crw-user-decision/2"
	if _, _, err := s.Raise(ctx, invalid); err == nil {
		t.Fatal("a record of another schema was accepted")
	}
	stale := userDecision("ud-2", "Which default does the retry take?", "PRJ-A", decisions.StateOpen)
	stale.Fingerprint = "0123456789abcdef"
	if _, _, err := s.Raise(ctx, stale); err == nil {
		t.Fatal("a fingerprint that is not the record's own was accepted")
	}
	if got := userDecisionRows(t, s); got != 0 {
		t.Fatalf("a refused raise wrote %d rows", got)
	}
}
