package mergeturn

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, out, err)
	}
	return strings.TrimSpace(string(out))
}
func commitObject(t *testing.T, dir, text string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "hash-object", "-w", "-t", "commit", "--stdin")
	cmd.Stdin = strings.NewReader("tree 4b825dc642cb6eb9a060e54bf8d69288fbee4904\nauthor Test <test@example.org> 0 +0000\ncommitter Test <test@example.org> 0 +0000\n\n" + text + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hash-object: %s: %v", out, err)
	}
	return strings.TrimSpace(string(out))
}
func repo(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo.git")
	cmd := exec.Command("git", "init", "--bare", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s: %v", out, err)
	}
	sha := commitObject(t, dir, "first")
	git(t, dir, "update-ref", "refs/heads/main", sha)
	return dir, sha
}
func Test26_MTG_1_local_exact_branch(t *testing.T) {
	dir, sha := repo(t)
	tip, err := (TargetReader{}).Tip(context.Background(), dir, "main")
	if err != nil || tip.SHA != sha || tip.Source != "local_git" || tip.Reference != "refs/heads/main" || tip.Repository != dir {
		t.Fatal(tip, err)
	}
	next := commitObject(t, dir, "second")
	git(t, dir, "update-ref", "refs/heads/main", next)
	tip, err = (TargetReader{}).Tip(context.Background(), dir, "main")
	if err != nil || tip.SHA != next {
		t.Fatal(tip, err)
	}
}
func Test26_MTG_2_never_evaluate_revision_syntax(t *testing.T) {
	dir, _ := repo(t)
	for _, base := range []string{"main~1", "main^", "main@{1}", "-main", "main.lock", "ma*in", " main", "a..b", "x/.hidden", "a//b", "main.", "@", "ma in", "ma\tin", "x/"} {
		_, err := (TargetReader{}).Tip(context.Background(), dir, base)
		var target *TargetUnreadable
		if !errors.As(err, &target) {
			t.Fatalf("%q: %v", base, err)
		}
	}
	for _, base := range []string{"topic#42", "50%off", "release/1.2"} {
		if err := branch(base); err != nil {
			t.Fatal(base, err)
		}
	}
}
func Test26_MTG_3_unreadable_local_paths(t *testing.T) {
	dir, _ := repo(t)
	for _, path := range []string{"relative", filepath.Join(t.TempDir(), "missing"), t.TempDir(), filepath.Join(dir, "objects")} {
		_, err := (TargetReader{}).Tip(context.Background(), path, "main")
		var target *TargetUnreadable
		if !errors.As(err, &target) {
			t.Fatalf("%q: %v", path, err)
		}
	}
	_, err := (TargetReader{}).Tip(context.Background(), dir, "nope")
	if err == nil || !strings.Contains(err.Error(), "refs/heads/nope") {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", dir)
	plain := t.TempDir()
	if err := os.Mkdir(filepath.Join(plain, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = (TargetReader{}).Tip(context.Background(), plain, "main")
	if err == nil {
		t.Fatal("GIT_DIR redirected the read")
	}
}
func Test26_MTG_5_same_commit_never_prefix(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, row := range []struct {
		a, b string
		want bool
	}{{strings.ToUpper(sha), sha, true}, {"abcdef1", sha, false}, {sha, "abcdef1", false}, {"base-0", "base-00", false}, {"base-0", "base-0", true}, {"", "", false}} {
		if got := SameCommit(row.a, row.b); got != row.want {
			t.Fatal(row, got)
		}
	}
}
