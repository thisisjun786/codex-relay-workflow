package store

import (
	"errors"
	"strings"
	"testing"
)

// ParseReceipt reads independentReview as an optional object and refuses any other shape the same
// way it refuses a malformed manifestRef: the statement's meaning is the parent's to grade, not the
// intake's.
func TestParseReceiptReadsAnIndependentReviewObject(t *testing.T) {
	t.Parallel()
	receipt := func(member string) []byte {
		return []byte(`{"eventId": "` + strings.Repeat("a", 32) + `", "relationshipId": "rel-1", "executionGeneration": 1, "attempt": 1, "revisionHash": "` + strings.Repeat("b", 64) +
			`", "outcome": "ready_for_review", "producer": "child", "turnRef": {"threadId": "t", "turnId": "u", "turnStatus": "inProgress"}, "emittedAt": "2026-10-04T00:00:00Z", "manifest": []` + member + `}`)
	}
	for name, row := range map[string]struct {
		member string
		ok     bool
	}{
		"no member":       {"", true},
		"an object":       {`, "independentReview": {"status": "complete"}`, true},
		"an empty object": {`, "independentReview": {}`, true},
		"null":            {`, "independentReview": null`, false},
		"a string":        {`, "independentReview": "reviewed"`, false},
		"a list":          {`, "independentReview": []`, false},
		"a number":        {`, "independentReview": 1`, false},
	} {
		_, err := ParseReceipt(receipt(row.member))
		var refused *RefusedError
		switch {
		case row.ok && err != nil:
			t.Errorf("%s: %v", name, err)
		case !row.ok && (!errors.As(err, &refused) || refused.Reason != ReasonMalformedReceipt || !strings.Contains(refused.Detail, "independentReview")):
			t.Errorf("%s: want a malformed_receipt refusal naming independentReview, got %v", name, err)
		}
	}
}
