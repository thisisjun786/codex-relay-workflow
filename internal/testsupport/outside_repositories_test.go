package testsupport_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1034: a directory with an empty .git (or a bare repository's HEAD and objects) among its ancestors is inside a repository as the
// worktree destination guard reads it; the helper that picks a root for worktree tests must not answer one.
func TestInsideRepositoryReadsTheGuardsMarkers(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	empty := filepath.Join(base, "empty-dotgit")
	if err := os.MkdirAll(filepath.Join(empty, ".git", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(base, "bare")
	if err := os.MkdirAll(filepath.Join(bare, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bare, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(base, "plain", "child")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir  string
		want bool
	}{{empty, true}, {filepath.Join(empty, "sub"), true}, {bare, true}, {filepath.Join(bare, "sub"), true}} {
		if got := testsupport.InsideRepository(c.dir); got != c.want {
			t.Errorf("InsideRepository(%s) = %v, want %v", c.dir, got, c.want)
		}
	}
	// the temporary base of this test binary is a clean location unless the host has a .git above it, which the guard would also refuse
	if got, want := testsupport.InsideRepository(plain), testsupport.InsideRepository(base); got != want {
		t.Errorf("InsideRepository(plain) = %v, but its parent says %v", got, want)
	}
}

func TestTempParentOutsideRepositoriesSkipsAParentInsideOne(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	if err := os.MkdirAll(filepath.Join(inside, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	// "outside" is made clean by cutting the search at base: it has no marker of its own, and the search stops at the first marker it finds
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if testsupport.InsideRepository(outside) {
		t.Skip("the temporary location of this host is below a repository marker")
	}
	got, ok := testsupport.TempParentOutsideRepositories([]string{filepath.Join(inside, "tmp"), inside, "", filepath.Join(base, "missing"), outside})
	if !ok || got != outside {
		t.Fatalf("got %q, %v; want %q", got, ok, outside)
	}
	if got, ok = testsupport.TempParentOutsideRepositories([]string{inside}); ok {
		t.Fatalf("a parent inside a repository was answered: %q", got)
	}
}

func TestMkdirTempOutsideRepositoriesMakesACleanDirectory(t *testing.T) {
	dir := testsupport.MkdirTempOutsideRepositories(t, "crw-outside-")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("%s: %v", dir, err)
	}
	if testsupport.InsideRepository(filepath.Dir(dir)) {
		t.Fatalf("%s is inside a repository", dir)
	}
}
