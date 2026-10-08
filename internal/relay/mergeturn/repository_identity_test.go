package mergeturn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-906 generation 2, round 16: the repository identity is the common git directory git reports, so the tests
// drive real repositories made with git itself: a main work tree, a linked worktree, a directory whose name ends in
// a space, an unrelated repository and a directory that is not a repository at all.

// riRepos are the spellings the identity tests name: each entry is one repository spelled several ways.
type riRepos struct {
	main, linked, adminDir, link, spaced, other, plain string
}

func riGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=crw", "GIT_AUTHOR_EMAIL=crw@example.invalid",
		"GIT_COMMITTER_NAME=crw", "GIT_COMMITTER_EMAIL=crw@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, dir, err, out)
	}
}

func riInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	riGit(t, dir, "init", "-q", "-b", "dev")
	riGit(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
}

func riSetup(t *testing.T) riRepos {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var r riRepos
	r.main = filepath.Join(root, "main")
	riInit(t, r.main)
	r.linked = filepath.Join(root, "linked")
	riGit(t, r.main, "worktree", "add", "-q", "--detach", r.linked, "HEAD")
	r.adminDir = filepath.Join(r.main, ".git", "worktrees", "linked")
	r.link = filepath.Join(root, "link")
	if err := os.Symlink(r.main, r.link); err != nil {
		t.Fatal(err)
	}
	r.spaced = filepath.Join(root, "space dir ")
	riInit(t, r.spaced)
	r.other = filepath.Join(root, "other")
	riInit(t, r.other)
	r.plain = filepath.Join(root, "plain")
	if err := os.MkdirAll(r.plain, 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSameRepositoryNamesTheRepositoryGitNamesForEverySpelling pins the identity itself.
func TestSameRepositoryNamesTheRepositoryGitNamesForEverySpelling(t *testing.T) {
	r := riSetup(t)
	same := []string{
		r.main, r.main + "/", r.main + "/.", r.main + "/.git", r.link, r.link + "/.",
		r.linked, r.linked + "/", r.adminDir, filepath.Join(r.main, "..", "main"),
	}
	for _, spelling := range same {
		if !SameRepository(r.main, spelling) || !SameRepository(spelling, r.main) {
			t.Fatalf("%q does not name the repository %q that git names", spelling, r.main)
		}
	}
	if !SameRepository(r.spaced, r.spaced+"/.") {
		t.Fatal("a directory whose name ends in a space is not named by its own /. spelling")
	}
	for _, spelling := range []string{r.other, r.plain, filepath.Join(r.main, "missing"), strings.TrimSpace(r.spaced)} {
		if SameRepository(r.main, spelling) || SameRepository(r.spaced, spelling) {
			t.Fatalf("%q names a repository it does not reach", spelling)
		}
	}
	if SameRepository(r.plain, r.plain+"/.") {
		t.Fatal("a path that is not a repository is compared by something other than its exact text")
	}
	if !SameRepository(r.plain, r.plain) {
		t.Fatal("a path that is not a repository is not equal to itself")
	}
	if !SameRepository(" Owner/Repo ", "owner/repo") || SameRepository(r.main, "owner/repo") || SameRepository("", "") {
		t.Fatal("forge slugs or empty spellings are named wrongly")
	}
}

// TestTheLaneGateHoldsEveryGitSpellingOfTheRepository: the lane gate holds a head for a turn whose repository is any
// git spelling of the accepted repository, and for no other repository.
func TestTheLaneGateHoldsEveryGitSpellingOfTheRepository(t *testing.T) {
	r := riSetup(t)
	cases := []struct {
		name, repository string
		held             bool
	}{
		{"the main work tree", r.main, true},
		{"the main work tree with a trailing slash", r.main + "/", true},
		{"the main work tree through a dot segment", r.main + "/./", true},
		{"the .git directory", filepath.Join(r.main, ".git"), true},
		{"a symlink to the main work tree", r.link, true},
		{"a linked worktree", r.linked, true},
		{"a linked worktree with a trailing slash", r.linked + "/", true},
		{"the linked worktree's administrative directory", r.adminDir, true},
		{"an unrelated repository with the same head", r.other, false},
		{"the forge slug of a path-accepted node", fxRepo, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newFx(t)
			w.ucLaneRelationship("rel-lane", 2)
			w.exec("UPDATE dag_acceptances SET repository = ? WHERE acceptance_id = 'acc-rel-lane'", r.main)
			turn := store.MergeTurnsRow{TurnID: "mtn-riwt", TargetKey: "tgt-x", Repository: c.repository, BaseRef: fxBase, ProjectKey: fxA,
				HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
			refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
			if err != nil {
				t.Fatal(err)
			}
			if c.held != (refusal != nil) {
				t.Fatalf("turn repository %q: held = %v, want %v", c.repository, refusal != nil, c.held)
			}
			if refusal != nil && refusal.Reason != contract.RefusalDispositionConflict {
				t.Fatalf("the refusal is %q, want disposition_conflict", refusal.Reason)
			}
		})
	}
}

// TestTheLaneGateHoldsADirectoryWhoseNameEndsInASpace: the acceptance names the directory with a trailing space; its
// own spelling and its /. spelling hold, and the trimmed name, which is another directory, does not.
func TestTheLaneGateHoldsADirectoryWhoseNameEndsInASpace(t *testing.T) {
	r := riSetup(t)
	for _, c := range []struct {
		repository string
		held       bool
	}{{r.spaced, true}, {r.spaced + "/.", true}, {strings.TrimSpace(r.spaced), false}} {
		w := newFx(t)
		w.ucLaneRelationship("rel-lane", 2)
		w.exec("UPDATE dag_acceptances SET repository = ? WHERE acceptance_id = 'acc-rel-lane'", r.spaced)
		turn := store.MergeTurnsRow{TurnID: "mtn-space", TargetKey: "tgt-x", Repository: c.repository, BaseRef: fxBase, ProjectKey: fxA,
			HolderTaskID: alpha.TaskID, CandidateHead: "head-a", State: Holding}
		refusal, err := underCorrectionRefusal(w.ctx, w.s.Querier(w.ctx), turn)
		if err != nil {
			t.Fatal(err)
		}
		if c.held != (refusal != nil) {
			t.Fatalf("turn repository %q: held = %v, want %v", c.repository, refusal != nil, c.held)
		}
	}
}
