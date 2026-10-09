package dagsched

import (
	"context"
	"strings"
	"testing"
)

// CRW-952 c5 on the pull-request path: the record head must be the pull request head, and a --supersedes acceptance needs
// a record of its own.

// TestPremergePullRequestRecordMustNameThePullRequestHead: a record that names another head is premerge_head_mismatch;
// a record that names the pull request head accepts.
func TestPremergePullRequestRecordMustNameThePullRequestHead(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.declare("rp", "I", "i.go")
	k.reportNode("rp", "I", acceptOpts{})
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
	ctx := context.Background()
	pull := &PRRef{Repository: "owner/repo", Number: 7}
	other := strings.Repeat("c", 40)
	_, err := k.sched.Accept(ctx, "rp", "I", "parent", AcceptInput{RuleVersion: verifier, PullRequest: pull, Premerge: premergeAt(k.sched, ctx, "rp", "I", other)})
	if got := refusalReasonOf(err); got != "premerge_head_mismatch" {
		t.Fatalf("a record of another head: reason %q (err %v); want premerge_head_mismatch", got, err)
	}
	if n := k.acceptCount(); n != 0 {
		t.Fatalf("a refused record wrote %d acceptances", n)
	}
	if _, err := k.sched.Accept(ctx, "rp", "I", "parent", AcceptInput{RuleVersion: verifier, PullRequest: pull, Premerge: premergeAt(k.sched, ctx, "rp", "I", head1)}); err != nil {
		t.Fatalf("a record of the pull request head must accept: %v", err)
	}
}

// TestPremergeSupersedesNeedsItsOwnRecord: a --supersedes acceptance without a record is premerge_missing; with a record
// of its own it replaces the active acceptance.
func TestPremergeSupersedesNeedsItsOwnRecord(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.reportNode("rp", "I", acceptOpts{})
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
	first, err := k.accept("rp", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}})
	if err != nil || first.AcceptanceID == "" {
		t.Fatalf("first accept = %v %+v", err, first)
	}
	k.supersedeReport(first.RelationshipID, "I", "rp", "second")
	ctx := context.Background()
	pull := &PRRef{Repository: "owner/repo", Number: 7}
	if _, err := k.sched.Accept(ctx, "rp", "I", "parent", AcceptInput{RuleVersion: verifier, PullRequest: pull, Supersedes: first.AcceptanceID}); refusalReasonOf(err) != "premerge_missing" {
		t.Fatalf("--supersedes without a record: %v; want premerge_missing", err)
	}
	second, err := k.sched.Accept(ctx, "rp", "I", "parent", AcceptInput{RuleVersion: verifier, PullRequest: pull, Supersedes: first.AcceptanceID, Premerge: premergeAt(k.sched, ctx, "rp", "I", head1)})
	if err != nil || second.SupersededID != first.AcceptanceID {
		t.Fatalf("--supersedes with its own record = %v %+v", err, second)
	}
}
