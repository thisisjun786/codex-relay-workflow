package dagsched

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-906 generation 2, round 16: the in-flight guard names a merging turn's repository by git's own identity of it
// (mergeturn.SameRepository), so every git spelling of the accepted repository holds the correction, a linked
// worktree included, and an unrelated repository does not.
func TestACorrectionIsHeldByEveryGitSpellingOfTheAcceptedRepository(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(root, "main")
	csInit(t, main)
	linked := filepath.Join(root, "linked")
	riGitRun(t, main, "worktree", "add", "-q", "--detach", linked, "HEAD")
	link := filepath.Join(root, "link")
	if err := os.Symlink(main, link); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	csInit(t, other)
	cases := []struct {
		name, repository string
		held             bool
	}{
		{"the main work tree", main, true},
		{"the main work tree with a trailing slash", main + "/", true},
		{"the main work tree through a dot segment", main + "/./", true},
		{"the .git directory", filepath.Join(main, ".git"), true},
		{"a symlink to the main work tree", link, true},
		{"a linked worktree", linked, true},
		{"a linked worktree with a trailing slash", linked + "/", true},
		{"the linked worktree's administrative directory", filepath.Join(main, ".git", "worktrees", "linked"), true},
		{"an unrelated repository with the same head", other, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, accepted := rvSettledSharedRoot(t)
			rid := accepted["B"].RelationshipID
			prepared := k.rvPrepare("sr", "B")
			acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
			k.exec("UPDATE dag_acceptances SET repository = ?, head_sha = 'head-b', pr_number = 5 WHERE relationship_id = ?", main, rid)
			k.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id = (SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ?)", rid)
			k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, state, tenure, requested_at, updated_at)"+
				" VALUES ('mtn-riwt', 'tgt', ?, 'dev', 'P-TEST', 'parent', 'host', NULL, NULL, 'head-b', 'merging', 1, 't', 't')", c.repository)
			_, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
			if c.held {
				if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "mtn-riwt") {
					t.Fatalf("a merging turn spelled %q was not held: %v", c.repository, err)
				}
				if got := acExecution(k, "B", 2); got != "" {
					t.Fatalf("the refused correction wrote the execution %q", got)
				}
				return
			}
			if err != nil && strings.Contains(err.Error(), "mtn-riwt") {
				t.Fatalf("a merging turn of an unrelated repository held the correction: %v", err)
			}
		})
	}
}

func riGitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=crw", "GIT_AUTHOR_EMAIL=crw@example.invalid", "GIT_COMMITTER_NAME=crw", "GIT_COMMITTER_EMAIL=crw@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
