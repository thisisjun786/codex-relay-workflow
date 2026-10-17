package executor

import (
	"context"
	"testing"
)

func TestCollectedCandidateKeepsRegisteredRepositoryIdentity(t *testing.T) {
	in, _ := candidateFixture(t)
	report := CheckExecutorResult(context.Background(), in)
	if !report.CandidateValid || report.Candidate == nil {
		t.Fatalf("collection: %+v", report)
	}
	remote, err := candidateCommand(context.Background(), report.Candidate.Checkout, "remote", "get-url", "origin")
	if err != nil || remote != "https://github.com/"+in.Repository.ID+".git" {
		t.Fatalf("registered identity for verify_command lost after collection: %q %v", remote, err)
	}
}
