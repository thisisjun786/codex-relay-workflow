package store

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/decisions"
	"strings"
	"testing"
)

func TestDecisionNoteFailureRollsBackTheAnswer(t *testing.T) {
	s := recordStore(t)
	ctx := context.Background()
	r := userDecision("ud-note", "Which option?", "P", decisions.StateRaised)
	if _, _, err := s.Raise(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("CREATE TRIGGER refuse_note BEFORE UPDATE ON dag_user_decision_notes BEGIN SELECT RAISE(ABORT,'note write failed'); END"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Update(ctx, r.DecisionID, func(_ context.Context, r decisions.Record) (decisions.Record, error) {
		return decisions.ValidateAnswer(r, decisions.Answer{Option: "a", Text: "keep this explanation", By: "task-a", Via: decisions.ViaDots})
	})
	if err == nil || !strings.Contains(err.Error(), "note write failed") {
		t.Fatalf("write: %v", err)
	}
	got, err := s.Get(ctx, r.DecisionID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != decisions.StateRaised || got.AnswerText != "" || got.AnswerNote != "" {
		t.Fatalf("partial answer: %+v", got)
	}
}

func TestDecisionBeforeNoteTableStillReads(t *testing.T) {
	s := recordStore(t)
	ctx := context.Background()
	r := userDecision("ud-old", "Which option?", "P", decisions.StateRaised)
	if _, _, err := s.Raise(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DROP TABLE dag_user_decision_notes"); err != nil {
		t.Fatal(err)
	}
	// This is an old store read, not a writable open that installs the zone.
	got, err := s.Get(ctx, r.DecisionID)
	if err != nil || got.AnswerNote != "" || got.State != r.State {
		t.Fatalf("legacy read: %+v %v", got, err)
	}
	rows, err := s.List(ctx, UserDecisionFilter{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("legacy listing: %+v %v", rows, err)
	}
}
