package store

import (
	"testing"
)

func TestClassifyObservation_python_five_endings(t *testing.T) {
	cases := []struct {
		name, status string
		claim        *ChildClaim
		want         ObservationOutcome
		refused      bool
	}{
		{"test_completed_without_a_child_receipt_is_an_ordinary_turn_end", "completed", nil, OrdinaryTurnEnd, false},
		{"test_a_completed_turn_is_never_success_on_its_own", "completed", nil, OrdinaryTurnEnd, false},
		{"test_failed_and_interrupted_turns_are_observable_without_a_receipt/failed", "failed", nil, Failed, false},
		{"test_failed_and_interrupted_turns_are_observable_without_a_receipt/interrupted", "interrupted", nil, Interrupted, false},
		{"test_a_turn_still_running_is_not_terminal", "inProgress", nil, InProgress, false},
		{"test_a_child_receipt_supplies_the_outcome", "completed", &ChildClaim{Outcome: ReadyForReview, Producer: "child"}, ReadyForReview, false},
		{"test_a_child_receipt_supplies_the_outcome_blocked", "completed", &ChildClaim{Outcome: BlockedNeedsInput, Producer: "child"}, BlockedNeedsInput, false},
		{"test_a_child_receipt_supplies_the_outcome_failed", "completed", &ChildClaim{Outcome: Failed, Producer: "child"}, Failed, false},
		{"test_an_observed_failure_is_never_promoted_to_reviewable", "failed", &ChildClaim{Outcome: ReadyForReview, Producer: "child"}, Contradictory, false},
		{"test_an_observed_failure_is_never_promoted_to_reviewable_failed_blocked", "failed", &ChildClaim{Outcome: BlockedNeedsInput, Producer: "child"}, Contradictory, false},
		{"test_an_observed_failure_is_never_promoted_to_reviewable_interrupted_ready", "interrupted", &ChildClaim{Outcome: ReadyForReview, Producer: "child"}, Contradictory, false},
		{"test_an_observed_failure_is_never_promoted_to_reviewable_interrupted_blocked", "interrupted", &ChildClaim{Outcome: BlockedNeedsInput, Producer: "child"}, Contradictory, false},
		{"test_a_child_may_report_its_own_failure_from_a_normal_turn", "completed", &ChildClaim{Outcome: Failed, Producer: "child"}, Failed, false},
		{"test_a_receipt_that_is_not_child_produced_cannot_assert_an_outcome", "completed", &ChildClaim{Outcome: ReadyForReview, Producer: "daemon_observation"}, "", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := ClassifyObservation(test.status, test.claim)
			if (err != nil) != test.refused || got != test.want {
				t.Fatalf("outcome %q want %q refusal %v: %v", got, test.want, test.refused, err)
			}
		})
	}
}

// Every (turn status, claim) pair classified by Go, as Python's classify_observation classified
// it (the golden): a refusal is "refused:" and its reason.
func TestClassifyObservation_matches_python_for_every_pair(t *testing.T) {
	var rows []any
	for _, status := range []string{"completed", "failed", "interrupted", "inProgress", "bogus"} {
		claims := []*ChildClaim{nil}
		for _, outcome := range []string{"ready_for_review", "failed", "interrupted", "blocked_needs_input", "bogus"} {
			for _, producer := range []string{"child", "daemon_observation"} {
				claims = append(claims, &ChildClaim{Outcome: ObservationOutcome(outcome), Producer: producer})
			}
		}
		for _, claim := range claims {
			got, err := ClassifyObservation(status, claim)
			answer := string(got)
			if err != nil {
				answer = "refused:" + RefusalReason(err)
			}
			var spelled any
			if claim != nil {
				spelled = map[string]string{"outcome": string(claim.Outcome), "producer": claim.Producer}
			}
			rows = append(rows, []any{status, spelled, answer})
		}
	}
	if len(rows) != 55 {
		t.Fatalf("the matrix has %d rows", len(rows))
	}
	checkJSON(t, "status, claim, classification", rows)
}
