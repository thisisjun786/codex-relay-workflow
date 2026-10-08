package dagsched

import "testing"

// The batch identity follows the record digest of each candidate (CRW-952 review, finding d5): a re-judged record is a
// new batch, so an old intent that froze another digest is never reused for the new one.
func TestPremergeBatchIdentityFollowsTheRecordDigest(t *testing.T) {
	in := IntegrationBatchInput{Plan: "g", Checkout: "/c", IntegrationRef: "refs/heads/x", BaseRef: "dev"}
	first := []Candidate{{NodeID: "I", AcceptanceID: "a1", EventID: "e1", HeadSHA: "h1", PremergeDigest: "sha256:1"}}
	second := []Candidate{{NodeID: "I", AcceptanceID: "a1", EventID: "e1", HeadSHA: "h1", PremergeDigest: "sha256:2"}}
	if integrationBatchID(in, "old", first) == integrationBatchID(in, "old", second) {
		t.Fatal("a candidate whose record digest changed must be a different batch")
	}
}
