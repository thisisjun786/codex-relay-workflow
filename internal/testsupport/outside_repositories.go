package testsupport

import (
	"os"
	"path/filepath"
	"testing"
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

// fallbackTempParents are the places looked at, after TMPDIR, for a root outside every repository. They are system temporary locations a test may use; none of them is created.
var fallbackTempParents = []string{"/var/tmp", "/dev/shm"}

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

// MkdirTempOutsideRepositories makes the temporary directory of a test that creates a Git worktree, which the destination guard refuses below any repository. It is os.MkdirTemp(TMPDIR) when
// nothing above TMPDIR is a repository, so the usual place and the isolation root keep serving. Otherwise (a host whose /tmp holds an empty .git, TMPDIR unset) it takes the first of
// /var/tmp and /dev/shm in which a directory can be made and that is clean, and when there is none the test is skipped with that reason: the guard is not weakened, and the test is not reported as a
// defect of the code under test. Candidates are judged by their physical path and the directory answered is that path, which is what the guard validates. The directory is removed when the test ends.
func MkdirTempOutsideRepositories(t testing.TB, pattern string) string {
	t.Helper()
	candidates := append([]string{os.TempDir()}, fallbackTempParents...)
	for _, parent := range cleanTempParents(candidates) {
		dir, err := os.MkdirTemp(parent, pattern)
		if err != nil {
			continue // a clean location the test cannot write to: the next one is tried
		}
		t.Cleanup(func() {
			if err := RemoveTempTree(dir); err != nil {
				t.Error(err)
			}
		})
		return dir
	}
	t.Skipf("no temporary location outside every Git repository where a directory can be made: %s and %v are each missing, unwritable or have a .git (or a bare repository's HEAD and objects) among their physical ancestors, which the worktree destination guard refuses", os.TempDir(), fallbackTempParents)
	return ""
}
