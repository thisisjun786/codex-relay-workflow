package worktrees

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
	root, err := os.MkdirTemp("/dev/shm", "crw-worktree-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
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
