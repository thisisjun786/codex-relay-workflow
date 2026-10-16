package worktrees

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
func repository(t *testing.T) (string, string, string) {
	t.Helper()
	root := testsupport.MkdirTempOutsideRepositories(t, "crw-worktree-")
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "init")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "tracked"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", "tracked")
	runGit(t, source, "commit", "-m", "base")
	return source, runGit(t, source, "rev-parse", "HEAD"), filepath.Join(root, "isolated")
}
func Test_Worktree_creates_locked_detached_checkout_when_destination_absent(t *testing.T) {
	// Given
	source, revision, destination := repository(t)
	ctx := context.Background()
	w, err := Validate(ctx, source, revision, destination)
	if err != nil {
		t.Fatal(err)
	}
	// When
	if err = w.Reserve(); err != nil {
		t.Fatal(err)
	}
	if err = w.Create(ctx); err != nil {
		t.Fatal(err)
	}
	if err = w.Checkout(ctx); err != nil {
		t.Fatal(err)
	}
	// Then
	actual, err := w.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Matches(ctx, actual) {
		t.Fatalf("checkout mismatch: %+v", actual)
	}
	mode, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if mode.Mode().Perm() != 0700 {
		t.Fatalf("mode %o", mode.Mode().Perm())
	}
	lock := filepath.Join(w.CommonDir, "worktrees", filepath.Base(destination), "locked")
	if _, err = os.Stat(lock); err != nil {
		t.Fatalf("locked worktree: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "tracked"))
	if err != nil || string(data) != "base\n" {
		t.Fatalf("checkout: %q %v", data, err)
	}
}
func Test_Worktree_refuses_destination_inside_repository_without_side_effect(t *testing.T) {
	// Given
	source, revision, _ := repository(t)
	destination := filepath.Join(source, "nested")
	// When
	_, err := Validate(context.Background(), source, revision, destination)
	// Then
	if err == nil || err.Error() != "destination must be outside existing repositories" {
		t.Fatalf("refusal: %v", err)
	}
	if _, err = os.Lstat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination touched: %v", err)
	}
}
func Test_Worktree_preserves_colliding_file_when_destination_exists(t *testing.T) {
	// Given
	source, revision, destination := repository(t)
	if err := os.WriteFile(destination, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	// When
	_, err := Validate(context.Background(), source, revision, destination)
	// Then
	if err == nil || err.Error() != "destination must be absent with an existing parent directory" {
		t.Fatalf("refusal: %v", err)
	}
	body, err := os.ReadFile(destination)
	if err != nil || string(body) != "preserve" {
		t.Fatalf("collision modified: %q %v", body, err)
	}
}

// CRW-1588: the ancestor walk judges a .git by what it resolves to. A marker that holds no repository
// (the empty directory a sandbox leaves behind, a file that is no gitfile) is no marker, while a real
// repository — a clone's .git directory, a linked worktree's gitfile, a bare repository's own HEAD and
// objects — still refuses.
func Test_Worktree_validate_judges_the_marker_by_its_repository(t *testing.T) {
	// Given
	source, revision, _ := repository(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitMeta := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "objects"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	parent := func(parts ...string) string {
		t.Helper()
		dir := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	// Markers that hold no repository do not refuse the destination below them.
	empty := parent("empty")
	if err := os.Mkdir(filepath.Join(empty, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	garbage := parent("garbage")
	if err := os.WriteFile(filepath.Join(garbage, ".git"), []byte("not a gitfile\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{empty, garbage} {
		if _, err := Validate(context.Background(), source, revision, filepath.Join(dir, "new-checkout")); err != nil {
			t.Errorf("a destination below %s (its .git holds no repository) is refused: %v", dir, err)
		}
	}

	// A linked worktree's gitfile, and a bare repository's HEAD and objects, still refuse.
	linked := parent("linked")
	gitMeta(filepath.Join(root, "meta"))
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+filepath.Join(root, "meta")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	bare := parent("bare")
	gitMeta(bare)
	for _, dir := range []string{linked, bare} {
		if _, err := Validate(context.Background(), source, revision, filepath.Join(dir, "new-checkout")); err == nil || err.Error() != "destination must be outside existing repositories" {
			t.Errorf("a destination below %s (a repository) is not refused: %v", dir, err)
		}
	}
}
