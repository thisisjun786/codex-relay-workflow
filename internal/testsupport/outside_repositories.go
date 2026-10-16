package testsupport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/gitprobe"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// InsideRepository reports whether dir or one of its ancestors holds what the worktree destination guard (internal/bridge/worktrees, Validate) reads as a repository: a ".git" that resolves to a
// Git directory (a directory holding HEAD and objects, a gitfile naming one, a linked worktree's commondir pair), or the HEAD file and objects directory of a bare one. A ".git" that resolves to no
// repository — the empty directory a Codex sandbox leaves in the writable roots it covers, /tmp/.git and $HOME/.git among them, or a file that is no gitfile — is no marker (CRW-1588). A read
// error counts as inside, as the guard refuses on it.
func InsideRepository(dir string) bool {
	root, err := gitprobe.RepositoryAbove(dir, gitprobe.RepositorySearch{IncludeBare: true})
	return root != "" || err != nil
}

// cleanTempParents answers, in order, each of candidates that resolves (symlinks followed, as the destination guard reads the physical path) to an existing directory with no repository above it
// (InsideRepository). An empty candidate is skipped; the paths answered are the resolved ones.
func cleanTempParents(candidates []string) []string {
	var clean []string
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			continue
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() || InsideRepository(resolved) {
			continue
		}
		clean = append(clean, resolved)
	}
	return clean
}

// TempParentOutsideRepositories answers the first of candidates that is an existing directory, once its symlinks are resolved, with no repository above it (InsideRepository). An empty candidate
// is skipped.
func TempParentOutsideRepositories(candidates []string) (string, bool) {
	if clean := cleanTempParents(candidates); len(clean) > 0 {
		return clean[0], true
	}
	return "", false
}

// MkdirTempOutsideRepositories makes the temporary directory of a test that creates a Git worktree, which the destination guard refuses below any repository. It is os.MkdirTemp below
// tmploc.Dir() (TMPDIR, else /tmp), judged by its physical path, which is what the guard validates, and the directory answered lies below that path. There is no other place: when TMPDIR is
// relative, missing, unwritable or has a repository (a .git resolving to a Git directory, or a bare repository's HEAD and objects) among its physical ancestors, the test fails with that reason
// ("set TMPDIR to a directory outside every repository"); it neither moves to another location nor skips, so the guard is not weakened and the host is told what to fix. The directory is removed
// when the test ends.
func MkdirTempOutsideRepositories(t testing.TB, pattern string) string {
	t.Helper()
	tmp, err := tmploc.Dir()
	if err != nil {
		t.Fatalf("%v", err)
		return ""
	}
	physical, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("temp_dir_unusable: %s: %v; set TMPDIR to an existing directory outside every repository", tmp, err)
		return ""
	}
	if InsideRepository(physical) {
		t.Fatalf("temp_dir_inside_repository: %s (physical %s) is inside a repository, which the worktree destination guard refuses; set TMPDIR to a directory outside every repository", tmp, physical)
		return ""
	}
	dir, err := tmploc.MkdirTempIn(physical, pattern)
	if err != nil {
		t.Fatalf("temp_dir_unusable: %s: %v; set TMPDIR to a writable directory outside every repository", tmp, err)
		return ""
	}
	t.Cleanup(func() {
		if err := RemoveTempTree(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
