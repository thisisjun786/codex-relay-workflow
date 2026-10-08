package dagsched

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-906 generation 2, round 14: the in-flight guard matches a turn's repository by the lane's own reading of a
// repository (mergeturn.SameRepository), under every spelling the acceptance has. A turn spelled as the checkout's
// git directory names the same repository as the accepted checkout, so a merging turn of it holds the correction;
// a turn of an unrelated checkout with the same head does not.
func TestACorrectionIsNotRecordedOverAMergingTurnSpelledAsTheCheckoutsGitDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	csInit(t, checkout)
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	k.exec("UPDATE dag_acceptances SET repository = ?, head_sha = 'head-b', pr_number = 5 WHERE relationship_id = ?", checkout, rid)
	k.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id = (SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ?)", rid)
	k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, state, tenure, requested_at, updated_at)"+
		" VALUES ('mtn-gitdir', 'tgt', ?, 'dev', 'P-TEST', 'parent', 'host', NULL, NULL, ' HEAD-B\n', 'merging', 1, 't', 't')", filepath.Join(checkout, ".git"))
	_, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a correction over a merging turn spelled as the checkout's git directory = %v, want disposition_conflict", err)
	}
	if !strings.Contains(err.Error(), "mtn-gitdir") {
		t.Fatalf("the refusal does not name the turn: %v", err)
	}
	if got := acExecution(k, "B", 2); got != "" {
		t.Fatalf("the refused correction wrote the execution %q", got)
	}
}

func TestACorrectionIsNotHeldByAMergingTurnOfAnUnrelatedCheckout(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	unrelated := filepath.Join(root, "unrelated")
	for _, dir := range []string{checkout, unrelated} {
		csInit(t, dir)
	}
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	k.exec("UPDATE dag_acceptances SET repository = ?, head_sha = 'head-b' WHERE relationship_id = ?", checkout, rid)
	k.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id = (SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ?)", rid)
	k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, state, tenure, requested_at, updated_at)"+
		" VALUES ('mtn-unrelated', 'tgt', ?, 'dev', 'P-TEST', 'parent', 'host', NULL, NULL, 'head-b', 'merging', 1, 't', 't')", unrelated)
	_, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if err != nil && strings.Contains(err.Error(), "mtn-unrelated") {
		t.Fatalf("a merging turn of an unrelated checkout held the correction: %v", err)
	}
}

// csInit makes a real git repository with one commit, so git names it as a repository.
func csInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "dev"}, {"commit", "-q", "--allow-empty", "-m", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=crw", "GIT_AUTHOR_EMAIL=crw@example.invalid", "GIT_COMMITTER_NAME=crw", "GIT_COMMITTER_EMAIL=crw@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}
