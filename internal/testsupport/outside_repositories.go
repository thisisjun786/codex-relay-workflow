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

// TempParentOutsideRepositories answers the first of candidates that is an existing directory with no repository above it (InsideRepository). An empty candidate is skipped.
func TempParentOutsideRepositories(candidates []string) (string, bool) {
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if abs, err := filepath.Abs(candidate); err == nil {
			candidate = abs
		}
		if info, err := os.Stat(candidate); err != nil || !info.IsDir() || InsideRepository(candidate) {
			continue
		}
		return candidate, true
	}
	return "", false
}

// MkdirTempOutsideRepositories makes the temporary directory of a test that creates a Git worktree, which the destination guard refuses below any repository. It is os.MkdirTemp(TMPDIR) when
// nothing above TMPDIR is a repository, so the usual place and the isolation root keep serving. Otherwise (a host whose /tmp holds an empty .git, TMPDIR unset) it takes the first of
// /var/tmp and /dev/shm that is clean, and when there is none the test is skipped with that reason: the guard is not weakened, and the test is not reported as a defect of the code under test.
// The directory is removed when the test ends.
func MkdirTempOutsideRepositories(t testing.TB, pattern string) string {
	t.Helper()
	parent, ok := TempParentOutsideRepositories(append([]string{os.TempDir()}, fallbackTempParents...))
	if !ok {
		t.Skipf("no temporary location outside every Git repository: %s and %v each have a .git (or a bare repository's HEAD and objects) among their ancestors, which the worktree destination guard refuses", os.TempDir(), fallbackTempParents)
	}
	dir, err := os.MkdirTemp(parent, pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := RemoveTempTree(dir); err != nil {
			t.Error(err)
		}
	})
	return dir
}
