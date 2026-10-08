//go:build dev

package ci

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964 review finding (P2): a git repository variable the caller exports, here GIT_DIR, never redirects the
// run to another repository: the record names the commit of the repository the run was started in.
func TestLocal_a_caller_git_variable_never_redirects_the_run(t *testing.T) {
	repo := newLocalFixture(t)
	other := newLocalFixture(t)
	other.write("file.txt", "other\n")
	other.commit()
	want := strings.TrimSpace(string(runGitOrFail(t, repo.root, "rev-parse", "HEAD")))
	t.Setenv("GIT_DIR", filepath.Join(other.root, ".git"))
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if made.HeadCommit != want {
		t.Errorf("the record names %s, the run was started at %s: GIT_DIR redirected it", made.HeadCommit, want)
	}
}

// runGitOrFail runs git in dir and fails the test on error.
func runGitOrFail(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
