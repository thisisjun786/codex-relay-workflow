package dagsched

import (
	"context"
	"database/sql"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-952 c5, the pull-request path after a base refresh: the refreshed head needs its own record. The correction that
// accepts the reworked result names the refreshed head; a record of the head the acceptance carried before the refresh is
// premerge_head_mismatch, and a record of the refreshed head is accepted in place of the first.
func TestPremergeRefreshedHeadNeedsItsOwnRecordOnThePullRequestPath(t *testing.T) {
	t.Parallel()
	s := newRefreshScenario(t)
	s.openGeneration()
	s.refreshBase()
	if _, err := s.record("shared.json"); err != nil {
		t.Fatal(err)
	}
	s.invRevise("g", "I", "g-r3", invTitle("I revised after its base refresh"))
	roots, err := relationshipRoots(context.Background(), s.s.Q(context.Background()), s.rid)
	if err != nil || len(roots) == 0 {
		t.Fatalf("the artifact roots of the child: %v %v", roots, err)
	}
	notes := writeFile(t, roots[0], "rework-notes.md", "the notes of the rework")
	prepared, err := s.sched.PrepareCorrection(context.Background(), "g", "I", "parent", ManifestInput{Base: &BaseRef{Repository: s.target(), Ref: "dev"}, RuleVersion: s.request(false).RuleVersion,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: notes, SHA256: shaOf([]byte("the notes of the rework")), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: roots})
	if err != nil {
		t.Fatalf("prepare I: %v", err)
	}
	reg := &registry.Registry{Store: s.s}
	if _, err := reg.OpenGeneration(context.Background(), s.rid, prepared.DispatchRequestID, "needs_changes_revision", sql.NullString{}); err != nil {
		t.Fatalf("generation-open: %v", err)
	}
	if _, err := reg.BindAnchor(context.Background(), s.rid, 3, "turn-dispatch-3-"+s.rid, "dispatch_receipt"); err != nil {
		t.Fatalf("generation-bind: %v", err)
	}
	if _, err := s.sched.RecordCorrection(context.Background(), "g", "I", "parent", prepared.ManifestDigest); err != nil {
		t.Fatalf("dag-correct after the refresh: %v", err)
	}
	s.rvReportGeneration(s.rid, "I", 3, s.criteria)
	ctx := context.Background()
	pull := &PRRef{Repository: "owner/repo", Number: 7}
	first := AcceptInput{PullRequest: pull, Supersedes: s.accepted.Acceptance.AcceptanceID, Premerge: premergeAt(s.sched, ctx, "g", "I", s.h1)}
	if _, err := s.sched.Accept(ctx, "g", "I", "parent", AcceptInput{RuleVersion: verifier, PullRequest: first.PullRequest, Supersedes: first.Supersedes, Premerge: first.Premerge}); refusalReasonOf(err) != "premerge_head_mismatch" {
		t.Fatalf("the record of the head before the refresh must be refused: %v", err)
	}
	second, err := s.sched.Accept(ctx, "g", "I", "parent", AcceptInput{RuleVersion: verifier, PullRequest: pull, Supersedes: s.accepted.Acceptance.AcceptanceID, Premerge: premergeAt(s.sched, ctx, "g", "I", s.head)})
	if err != nil || second.SupersededID != s.accepted.Acceptance.AcceptanceID {
		t.Fatalf("the refreshed head with its own record must be accepted: %v %+v", err, second)
	}
}
