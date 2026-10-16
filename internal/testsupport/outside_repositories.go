package testsupport

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

// InsideRepository reports whether dir or one of its ancestors holds what the worktree destination guard (internal/bridge/worktrees, Validate) reads as a repository: a ".git" entry of any
// kind (a Codex sandbox leaves an empty .git directory in the writable roots it covers, /tmp/.git and $HOME/.git among them), or the HEAD file and objects directory of a bare one. A stat
// error other than absence counts as inside, as the guard refuses on it.
func InsideRepository(dir string) bool {
	for parent := dir; ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil || !os.IsNotExist(err) {
			return true
		}
		if head, err := os.Stat(filepath.Join(parent, "HEAD")); err == nil && head.Mode().IsRegular() {
			if objects, err := os.Stat(filepath.Join(parent, "objects")); err == nil && objects.IsDir() {
				return true
			}
		}
		if parent == filepath.Dir(parent) {
			return false
		}
	}
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
// relative, missing, unwritable or has a .git (or a bare repository's HEAD and objects) among its physical ancestors, the test fails with that reason ("set TMPDIR to a directory with no .git
// above"); it neither moves to another location nor skips, so the guard is not weakened and the host is told what to fix. The directory is removed when the test ends.
func MkdirTempOutsideRepositories(t testing.TB, pattern string) string {
	t.Helper()
	tmp, err := tmploc.Dir()
	if err != nil {
		t.Fatalf("%v", err)
		return ""
	}
	physical, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatalf("temp_dir_unusable: %s: %v; set TMPDIR to an existing directory with no .git above", tmp, err)
		return ""
	}
	if InsideRepository(physical) {
		t.Fatalf("temp_dir_inside_repository: %s (physical %s) has a .git (or a bare repository's HEAD and objects) among its ancestors, which the worktree destination guard refuses; set TMPDIR to a directory with no .git above", tmp, physical)
		return ""
	}
	dir, err := tmploc.MkdirTempIn(physical, pattern)
	if err != nil {
		t.Fatalf("temp_dir_unusable: %s: %v; set TMPDIR to a writable directory with no .git above", tmp, err)
		return ""
	}
	t.Cleanup(func() {
		if err := RemoveTempTree(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
