package store

import (
	"encoding/json"
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

// Every (turn status, claim) pair classified by Python's classify_observation and by Go.
func TestClassifyObservation_matches_python_for_every_pair(t *testing.T) {
	out := pythonStoreValue(t, `
import json
from codex_session_relay.receipts import classify_observation, ReceiptRefused
rows = []
for status in ("completed", "failed", "interrupted", "inProgress", "bogus"):
    for claim in [None] + [{"outcome": o, "producer": p} for o in ("ready_for_review", "failed", "interrupted", "blocked_needs_input", "bogus") for p in ("child", "daemon_observation")]:
        try:
            got = classify_observation(status, claim).value
        except ReceiptRefused as refused:
            got = "refused:" + refused.reason.value
        rows.append([status, claim, got])
print(json.dumps(rows))
`)
	var rows []struct {
		Status string
		Claim  *ChildClaim
		Want   string
	}
	var raw [][3]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, r := range raw {
		var row struct {
			Status string
			Claim  *ChildClaim
			Want   string
		}
		var claim *struct{ Outcome, Producer string }
		if json.Unmarshal(r[0], &row.Status) != nil || json.Unmarshal(r[1], &claim) != nil || json.Unmarshal(r[2], &row.Want) != nil {
			t.Fatalf("row %s", r)
		}
		if claim != nil {
			row.Claim = &ChildClaim{Outcome: ObservationOutcome(claim.Outcome), Producer: claim.Producer}
		}
		rows = append(rows, row)
	}
	if len(rows) != 55 {
		t.Fatalf("python matrix has %d rows", len(rows))
	}
	for _, row := range rows {
		got, err := ClassifyObservation(row.Status, row.Claim)
		answer := string(got)
		if err != nil {
			answer = "refused:" + RefusalReason(err)
		}
		if answer != row.Want {
			t.Errorf("%s %+v: go %q python %q", row.Status, row.Claim, answer, row.Want)
		}
	}
}
