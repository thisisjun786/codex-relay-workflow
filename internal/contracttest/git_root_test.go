package contracttest

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1579 (D7): the root of a git contract case is made below tmploc.Dir() (TMPDIR, else /tmp), not
// below /dev/shm. The case's own repository is root/source, which `git worktree list` names first.
func TestRunGit_rootIsBelowTheTempDir(t *testing.T) {
	tmp := testsupport.MkdirTempOutsideRepositories(t, "crw-rungit-root-")
	t.Setenv("TMPDIR", tmp)
	scenario := Scenario{
		ID:    "rungit-root",
		Kind:  RunKind("git"),
		Given: map[string]any{"files": map[string]any{"tracked": "base\n"}},
		Run:   map[string]any{"kind": "git", "steps": []any{map[string]any{"kind": "worktree", "path": "isolated"}}},
	}
	obs, err := runGit(t, scenario)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := obs["worktrees"].(string)
	first, _, _ := strings.Cut(list, "\n")
	repo, ok := strings.CutPrefix(first, "worktree ")
	if !ok {
		t.Fatalf("worktree list starts with %q, want a worktree line", first)
	}
	if filepath.Base(repo) != "source" {
		t.Fatalf("the first worktree is %q, want the case's source repository", repo)
	}
	root := filepath.Dir(repo)
	if filepath.Dir(root) != tmp {
		t.Errorf("git contract root = %q, want a directory directly below the temp dir %q", root, tmp)
	}
}
