package ops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var ctx = context.Background()

// gitIn runs git in dir with a commit identity (the tests commit with it) and fails the test on an error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := ExecGit("git")(ctx, dir, identity, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func write(t *testing.T, dir, name, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// newRepo makes a temporary repository with three tracked files and returns its directory and first commit.
func newRepo(t *testing.T) (dir, first string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "config", "core.fileMode", "true")
	write(t, dir, "a.txt", "alpha\n", 0o644)
	write(t, dir, "b.sh", "#!/bin/sh\n", 0o644)
	write(t, dir, "src/c.txt", "gamma\n", 0o644)
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first")
	return dir, gitIn(t, dir, "rev-parse", "HEAD")
}

// addWorktree makes a detached worktree of the repository at commit and returns its directory.
func addWorktree(t *testing.T, repo, name, commit string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), name)
	gitIn(t, repo, "worktree", "add", "--detach", "-q", wt, commit)
	return wt
}

func indexBytes(t *testing.T, dir string) []byte {
	t.Helper()
	p := gitIn(t, dir, "rev-parse", "--path-format=absolute", "--git-path", "index")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSnapshotCoversModeAndDeletion(t *testing.T) {
	repo, _ := newRepo(t)
	before := indexBytes(t, repo)
	write(t, repo, "b.sh", "#!/bin/sh\n", 0o755) // the mode alone
	if err := os.Remove(filepath.Join(repo, "a.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, repo, "src/c.txt", "gamma two\n", 0o644)
	write(t, repo, "src/new.txt", "fresh\n", 0o644) // untracked, not ignored
	snap, err := Snapshot(ctx, ExecGit("git"), repo)
	if err != nil {
		t.Fatal(err)
	}
	listing := gitIn(t, repo, "ls-tree", "-r", snap)
	if strings.Contains(listing, "a.txt") {
		t.Errorf("a deleted file is in the snapshot:\n%s", listing)
	}
	if !strings.Contains(listing, "100755 blob") || !strings.Contains(listing, "\tb.sh") {
		t.Errorf("the mode change of b.sh is not in the snapshot:\n%s", listing)
	}
	if !strings.Contains(listing, "\tsrc/new.txt") {
		t.Errorf("an untracked file is not in the snapshot:\n%s", listing)
	}
	if got := gitIn(t, repo, "show", snap+":src/c.txt"); got != "gamma two" {
		t.Errorf("c.txt in the snapshot: %q", got)
	}
	// the real index is neither read nor written: it still holds what was committed, byte for byte
	if after := indexBytes(t, repo); string(after) != string(before) {
		t.Error("the snapshot changed the real index")
	}
	if out := gitIn(t, repo, "diff", "--cached", "--name-only"); out != "" {
		t.Errorf("the snapshot staged %q", out)
	}
	// the same files give the same snapshot, and a change gives another
	again, err := Snapshot(ctx, ExecGit("git"), repo)
	if err != nil || again != snap {
		t.Errorf("a second snapshot of the same files: %s %v, want %s", again, err, snap)
	}
	write(t, repo, "src/new.txt", "fresh two\n", 0o644)
	if changed, _ := Snapshot(ctx, ExecGit("git"), repo); changed == snap {
		t.Error("a changed untracked file did not change the snapshot")
	}
}

func TestIgnoredFilesOutsideSnapshot(t *testing.T) {
	repo, first := newRepo(t)
	write(t, repo, ".gitignore", "*.log\nbuild/\nsrc/c.txt\n", 0o644) // c.txt is tracked: the ignore rule does not hide it
	gitIn(t, repo, "add", ".gitignore")
	gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "ignore rules")
	head := gitIn(t, repo, "rev-parse", "HEAD")
	write(t, repo, "run.log", "noise\n", 0o644)
	write(t, repo, "build/out.bin", "bin\n", 0o644)
	write(t, repo, "src/c.txt", "gamma three\n", 0o644)
	snap, err := Snapshot(ctx, ExecGit("git"), repo)
	if err != nil {
		t.Fatal(err)
	}
	listing := gitIn(t, repo, "ls-tree", "-r", "--name-only", snap)
	if strings.Contains(listing, "run.log") || strings.Contains(listing, "build/") {
		t.Errorf("an ignored file is in the snapshot:\n%s", listing)
	}
	if got := gitIn(t, repo, "show", snap+":src/c.txt"); got != "gamma three" {
		t.Errorf("an ignored file that is tracked keeps its changes in the snapshot, got %q", got)
	}
	// ... and so in a commit of it
	res, err := Commit(ctx, ExecGit("git"), repo, CommitRequest{Tree: snap, Parent: head, Message: "snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	if out := gitIn(t, repo, "ls-tree", "-r", "--name-only", res.Commit); strings.Contains(out, "run.log") || strings.Contains(out, "build/") {
		t.Errorf("the commit holds an ignored file:\n%s", out)
	}
	_ = first
}

func TestCommitCommitsExactSnapshot(t *testing.T) {
	repo, first := newRepo(t)
	wt := addWorktree(t, repo, "wt", first)
	write(t, wt, "a.txt", "alpha two\n", 0o644)
	write(t, wt, "src/new.txt", "fresh\n", 0o644)
	if err := os.Remove(filepath.Join(wt, "src", "c.txt")); err != nil {
		t.Fatal(err)
	}
	// something staged in the real index that the snapshot does not hold: the commit drops it from the index
	write(t, wt, "staged.txt", "staged\n", 0o644)
	gitIn(t, wt, "add", "staged.txt")
	if err := os.Remove(filepath.Join(wt, "staged.txt")); err != nil {
		t.Fatal(err)
	}
	snap, err := Snapshot(ctx, ExecGit("git"), wt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gitIn(t, wt, "ls-tree", "-r", "--name-only", snap), "staged.txt") {
		t.Fatal("the snapshot read the real index")
	}
	// the working files change after the snapshot was recorded: the commit is still of the recorded tree
	write(t, wt, "a.txt", "alpha three, after the snapshot\n", 0o644)
	res, err := Commit(ctx, ExecGit("git"), wt, CommitRequest{Tree: snap, Parent: first, Message: "crw.commit"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, wt, "rev-parse", res.Commit+"^{tree}"); got != snap {
		t.Errorf("the commit's tree is %s, the snapshot is %s", got, snap)
	}
	if res.Tree != snap || res.Parent != first {
		t.Errorf("result %+v", res)
	}
	if got := gitIn(t, wt, "rev-parse", "HEAD"); got != res.Commit {
		t.Errorf("HEAD %s, commit %s", got, res.Commit)
	}
	if got := gitIn(t, wt, "rev-parse", res.Commit+"^"); got != first {
		t.Errorf("the commit's parent %s, want %s", got, first)
	}
	// the real index is the new HEAD's tree: nothing staged, the staged file gone from the index
	if got := gitIn(t, wt, "write-tree"); got != snap {
		t.Errorf("the real index writes tree %s, the new HEAD's tree is %s", got, snap)
	}
	if out := gitIn(t, wt, "diff", "--cached", "--name-only"); out != "" {
		t.Errorf("the index differs from the new HEAD: %q", out)
	}
	// the working files are not touched: the later edit shows as a change against the new HEAD
	if out := gitIn(t, wt, "diff", "--name-only"); out != "a.txt" {
		t.Errorf("working changes after the commit: %q", out)
	}
	// a commit on a parent that is no longer HEAD is refused and moves nothing
	_, err = Commit(ctx, ExecGit("git"), wt, CommitRequest{Tree: snap, Parent: first, Message: "again"})
	if !errors.Is(err, ErrHeadMoved) {
		t.Errorf("a commit on a stale parent: %v", err)
	}
	if got := gitIn(t, wt, "rev-parse", "HEAD"); got != res.Commit {
		t.Error("a refused commit moved HEAD")
	}
	if _, err := Commit(ctx, ExecGit("git"), wt, CommitRequest{Tree: first, Parent: res.Commit, Message: "x"}); err == nil {
		t.Error("a commit's tree must be a tree, not a commit id")
	}
}

func TestScopeRecordsPathsOutsideTheEditRegions(t *testing.T) {
	repo, first := newRepo(t)
	wt := addWorktree(t, repo, "wt", first)
	write(t, wt, "src/c.txt", "inside\n", 0o644)
	write(t, wt, "src/deep/d.txt", "inside too\n", 0o644)
	write(t, wt, "a.txt", "outside\n", 0o644)
	snap, err := Snapshot(ctx, ExecGit("git"), wt)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Scope(ctx, ExecGit("git"), wt, first, snap, []string{"src/"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(rep.Changed, ",") != "a.txt,src/c.txt,src/deep/d.txt" || strings.Join(rep.OutOfScope, ",") != "a.txt" {
		t.Errorf("%+v", rep)
	}
	if rep, _ = Scope(ctx, ExecGit("git"), wt, first, snap, []string{"src/c.txt", "*.txt", "src/deep/*"}); len(rep.OutOfScope) != 0 {
		t.Errorf("a file path and a pattern are regions: %+v", rep)
	}
	if rep, _ = Scope(ctx, ExecGit("git"), wt, first, snap, nil); len(rep.OutOfScope) != 0 {
		t.Errorf("no declared region leaves nothing out of scope: %+v", rep)
	}
}

// trimmingGit is the git runner of the agent package's allocator: it hands back standard output trimmed as text. The
// snapshot and the scope must give the same answer through it as through the raw ExecGit.
func trimmingGit(inner GitFunc) GitFunc {
	return func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
		out, err := inner(ctx, dir, env, args...)
		return strings.TrimSpace(out), err
	}
}

// A worktree whose directory name ends in a space is a valid worktree: the snapshot reads the directory it was given, not a
// path git printed and the runner trimmed.
func TestSnapshotOfAWorktreeWhoseNameEndsInASpace(t *testing.T) {
	for name, git := range map[string]GitFunc{"exec": ExecGit("git"), "trimmed": trimmingGit(ExecGit("git"))} {
		t.Run(name, func(t *testing.T) {
			repo, first := newRepo(t)
			wt := addWorktree(t, repo, "attempt ", first)
			write(t, wt, "src/new.txt", "added\n", 0o644)
			snap, err := Snapshot(ctx, git, wt)
			if err != nil {
				t.Fatalf("the snapshot of %q: %v", wt, err)
			}
			if want := gitIn(t, wt, "rev-parse", "HEAD^{tree}"); snap == want {
				t.Errorf("the snapshot %s is the tree of HEAD: the added file is missing", snap)
			}
			if listing := gitIn(t, repo, "ls-tree", "-r", "--name-only", snap); !strings.Contains(listing, "src/new.txt") {
				t.Errorf("the snapshot lists %q", listing)
			}
		})
	}
}

// The paths of a scope report are exact: a changed file whose name begins with a space keeps it, and an edit region names it.
func TestScopeKeepsPathsWithLeadingSpace(t *testing.T) {
	for name, git := range map[string]GitFunc{"exec": ExecGit("git"), "trimmed": trimmingGit(ExecGit("git"))} {
		t.Run(name, func(t *testing.T) {
			repo, _ := newRepo(t)
			write(t, repo, " a.txt", "spaced\n", 0o644)
			gitIn(t, repo, "add", "-A")
			gitIn(t, repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "spaced")
			base := gitIn(t, repo, "rev-parse", "HEAD")
			write(t, repo, " a.txt", "spaced and changed\n", 0o644)
			snap, err := Snapshot(ctx, ExecGit("git"), repo)
			if err != nil {
				t.Fatal(err)
			}
			rep, err := Scope(ctx, git, repo, base, snap, []string{" a.txt"})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Changed) != 1 || rep.Changed[0] != " a.txt" || len(rep.OutOfScope) != 0 {
				t.Errorf("%q", rep)
			}
			rep, err = Scope(ctx, git, repo, base, snap, []string{"a.txt"})
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.OutOfScope) != 1 || rep.OutOfScope[0] != " a.txt" {
				t.Errorf("the region a.txt is not the file ' a.txt': %q", rep)
			}
		})
	}
}

// A region is read by its normal form (CRW-1445): a symbol region `path#key` is its file (the change set has files, not
// symbols), and the blanks, `./` and doubled slashes of the text are cleaned as RegionOf cleans them. A name with no
// trailing slash is a file, as the relay reads it, so `src` does not cover `src/c.txt`.
func TestScopeTreatsASymbolRegionAsItsFile(t *testing.T) {
	repo, first := newRepo(t)
	wt := addWorktree(t, repo, "wt", first)
	write(t, wt, "src/c.txt", "inside\n", 0o644)
	write(t, wt, "src/deep/d.txt", "inside too\n", 0o644)
	write(t, wt, "a.txt", "outside\n", 0o644)
	snap, err := Snapshot(ctx, ExecGit("git"), wt)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		regions []string
		out     string
	}{
		{[]string{"a.txt#F", "src/c.txt#G"}, "src/deep/d.txt"},
		{[]string{"./a.txt", "src/"}, ""},
		{[]string{"src"}, "a.txt,src/c.txt,src/deep/d.txt"},
		{[]string{"src/c.txt#G"}, "a.txt,src/deep/d.txt"},
		{[]string{"src//c.txt"}, "a.txt,src/deep/d.txt"},
		{[]string{"src/./"}, "a.txt"},
	} {
		rep, err := Scope(ctx, ExecGit("git"), wt, first, snap, c.regions)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(rep.Changed, ",") != "a.txt,src/c.txt,src/deep/d.txt" {
			t.Errorf("%q: changed %+v", c.regions, rep)
		}
		if got := strings.Join(rep.OutOfScope, ","); got != c.out {
			t.Errorf("%q: out of scope %q, want %q", c.regions, got, c.out)
		}
	}
}

// inRegions (the inline routing check) reads its regions as Scope does.
func TestInRegionsReadsASymbolRegionAsItsFile(t *testing.T) {
	for _, c := range []struct {
		files, regions []string
		want           bool
	}{
		{[]string{"a.go"}, []string{"a.go#F"}, true},
		{[]string{"a.go", "b.go"}, []string{"a.go#F"}, false},
		{[]string{"c/1"}, []string{"./c/"}, true},
		// A planned tree or pattern whose path holds "#" keeps the place its text spells.
		{[]string{"gen#1/x.go"}, []string{"gen#1/"}, true},
		{[]string{"a#b.go"}, []string{"a#*.go"}, true},
		{[]string{"pkg/a#b.go"}, []string{"pkg/a#b.go"}, true},
	} {
		if got := inRegions(c.files, c.regions); got != c.want {
			t.Errorf("inRegions(%q, %q) = %v, want %v", c.files, c.regions, got, c.want)
		}
	}
}
