package testsupport_test

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// gitMeta makes a Git directory at dir: the regular HEAD and objects directory a marker must resolve to
// (CRW-1588: a .git that holds no repository is no marker).
func gitMeta(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// CRW-1034: a directory with a repository among its ancestors (a .git resolving to a Git directory, or a bare
// repository's HEAD and objects) is inside it as the worktree destination guard reads it, and the helper that
// picks a root for worktree tests must not answer one. A .git that resolves to no repository — the empty
// directory a sandbox leaves behind — is no marker (CRW-1588).
func TestInsideRepositoryReadsTheGuardsMarkers(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if testsupport.InsideRepository(base) {
		t.Skip("the temporary location of this host is below a repository marker")
	}
	empty := filepath.Join(base, "empty-dotgit")
	if err := os.MkdirAll(filepath.Join(empty, ".git", "deeper"), 0o700); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(base, "garbage-dotgit")
	if err := os.MkdirAll(garbage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(garbage, ".git"), []byte("not a gitfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(base, "bare")
	gitMeta(t, bare)
	plain := filepath.Join(base, "plain", "child")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir  string
		want bool
	}{{empty, false}, {filepath.Join(empty, "sub"), false}, {garbage, false}, {bare, true}, {filepath.Join(bare, "sub"), true}, {plain, false}} {
		if got := testsupport.InsideRepository(c.dir); got != c.want {
			t.Errorf("InsideRepository(%s) = %v, want %v", c.dir, got, c.want)
		}
	}
}

func TestTempParentOutsideRepositoriesSkipsAParentInsideOne(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	inside := filepath.Join(base, "inside")
	gitMeta(t, filepath.Join(inside, ".git"))
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

// CRW-1034 (post-evaluation d2, d3): a candidate is judged by its physical path, and a clean location nobody can write to does not end the search.
func TestTempParentOutsideRepositoriesJudgesThePhysicalPath(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	real := filepath.Join(base, "repo", "tmp")
	gitMeta(t, filepath.Join(base, "repo", ".git"))
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(base, "clean-target")
	if err := os.Mkdir(clean, 0o700); err != nil {
		t.Fatal(err)
	}
	if testsupport.InsideRepository(clean) {
		t.Skip("the temporary location of this host is below a repository marker")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	cleanAlias := filepath.Join(base, "clean-alias")
	if err := os.Symlink(clean, cleanAlias); err != nil {
		t.Fatal(err)
	}
	// the alias has no marker above it, its target does: it is not clean, and the next candidate is answered
	got, ok := testsupport.TempParentOutsideRepositories([]string{alias, cleanAlias})
	want, _ := filepath.EvalSymlinks(clean)
	if !ok || got != want {
		t.Fatalf("got %q, %v; want the resolved clean path %q", got, ok, want)
	}
	if got, ok = testsupport.TempParentOutsideRepositories([]string{alias}); ok {
		t.Fatalf("a symlink into a repository was answered: %q", got)
	}
}

func TestMkdirTempOutsideRepositoriesAnswersAPhysicalPathWhenTMPDIRIsASymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if testsupport.InsideRepository(target) {
		t.Skip("the temporary location of this host is below a repository marker")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", alias)
	dir := testsupport.MkdirTempOutsideRepositories(t, "crw-outside-")
	if resolved, err := filepath.EvalSymlinks(dir); err != nil || resolved != dir {
		t.Fatalf("%s is not a physical path (%q, %v)", dir, resolved, err)
	}
}

// recordingTB is a testing.TB whose Fatalf and Skipf end the calling goroutine and keep the message, so a test can observe
// the failure of a helper that is handed a testing.TB. Run it in its own goroutine through failureOf.
type recordingTB struct {
	testing.TB
	message  string
	skipped  bool
	cleanups []func()
}

func (r *recordingTB) Helper()          {}
func (r *recordingTB) Cleanup(f func()) { r.cleanups = append(r.cleanups, f) }
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
func (r *recordingTB) Skipf(format string, args ...any) {
	r.message, r.skipped = fmt.Sprintf(format, args...), true
	runtime.Goexit()
}
func (r *recordingTB) Errorf(format string, args ...any) { r.message = fmt.Sprintf(format, args...) }

// failureOf runs fn against a recordingTB and answers what it recorded; its cleanups run before it returns.
func failureOf(t *testing.T, fn func(testing.TB)) *recordingTB {
	t.Helper()
	r := &recordingTB{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(r)
	}()
	<-done
	for i := len(r.cleanups) - 1; i >= 0; i-- {
		r.cleanups[i]()
	}
	return r
}

// CRW-1579 (R1-3): TMPDIR below a repository marker is a failure with its reason, not a move to /var/tmp or /dev/shm and not
// a skip: the host sets TMPDIR to a directory outside every repository.
func TestMkdirTempOutsideRepositories_failsWithTheReasonInsideARepository(t *testing.T) {
	base := t.TempDir()
	gitMeta(t, filepath.Join(base, ".git"))
	tmp := filepath.Join(base, "tmp")
	if err := os.Mkdir(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", tmp)
	var made string
	r := failureOf(t, func(tb testing.TB) { made = testsupport.MkdirTempOutsideRepositories(tb, "crw-outside-") })
	if r.skipped {
		t.Fatalf("the test was skipped (%s); a TMPDIR inside a repository is a failure", r.message)
	}
	if made != "" {
		t.Fatalf("a directory was made (%s) although TMPDIR=%s is inside a repository", made, tmp)
	}
	if !strings.Contains(r.message, "set TMPDIR to a directory outside every repository") || !strings.Contains(r.message, tmp) {
		t.Fatalf("the failure %q does not name TMPDIR %s and say to set TMPDIR to a directory outside every repository", r.message, tmp)
	}
}

// A relative TMPDIR is tmploc's refusal, not a directory made below the working directory.
func TestMkdirTempOutsideRepositories_failsOnARelativeTMPDIR(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.Mkdir("rel", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", "rel")
	r := failureOf(t, func(tb testing.TB) { testsupport.MkdirTempOutsideRepositories(tb, "crw-outside-") })
	if !strings.Contains(r.message, "tmpdir_not_absolute") {
		t.Fatalf("the failure %q is not tmploc's tmpdir_not_absolute", r.message)
	}
	if entries, _ := os.ReadDir("rel"); len(entries) != 0 {
		t.Fatalf("a directory was made below the relative TMPDIR: %v", entries)
	}
}
