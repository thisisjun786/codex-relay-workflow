package agent_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

var _ engine.HeadKeeper = (*agent.Allocator)(nil)

// CRW-1331 (CRW-1316 §6.3): the allocator reads a worktree's HEAD and keeps a session executor's own commit on the stray ref of its
// call, refs/crw/flow/<run>/stray/<node>/a<n>-<call> (CRW-1367); it keeps nothing on a ref outside the run's stray refs.
func TestAllocatorKeepsAStrayHead(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	run := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "-q", "-b", "main", repo)
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	run(repo, "add", "-A")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first")
	first := run(repo, "rev-parse", "HEAD")
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(dir, "state"), RunID: "run-s"})
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(dir, "wt")
	run(repo, "worktree", "add", "--detach", "-q", wt, first)
	writeFile(t, filepath.Join(wt, "a.txt"), "changed\n")
	run(wt, "add", "-A")
	run(wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the subagent committed")
	stray := run(wt, "rev-parse", "HEAD")
	ctx := context.Background()
	if head, err := alloc.HeadOf(ctx, wt); err != nil || head != stray {
		t.Fatalf("HeadOf %q %v, want %s", head, err, stray)
	}
	ref := engine.StrayRef("run-s", "n1", 1, "c_000000000001")
	if ref != "refs/crw/flow/run-s/stray/n1/a1-c_000000000001" {
		t.Errorf("stray ref %s", ref)
	}
	if err := alloc.KeepRef(ctx, ref, stray); err != nil {
		t.Fatal(err)
	}
	if got := run(repo, "rev-parse", ref); got != stray {
		t.Errorf("%s is %s, want %s", ref, got, stray)
	}
	if err := alloc.KeepRef(ctx, "refs/heads/main", stray); err == nil {
		t.Errorf("the allocator moved a ref that is not a stray ref of its run")
	}
	if got := run(repo, "rev-parse", "main"); got != first {
		t.Errorf("main moved to %s", got)
	}
}

// CRW-1367: a stray ref is written with a compare-and-swap: KeepRef creates it, keeps it as it is when it already holds the commit,
// and never moves it to a commit that does not descend from the one it holds (CRW-1392), so a second record of the same ref cannot
// overwrite the first.
func TestAllocatorKeepRefNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	run := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "-q", "-b", "main", repo)
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	run(repo, "add", "-A")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first")
	first := run(repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "a.txt"), "b\n")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-am", "second")
	second := run(repo, "rev-parse", "HEAD")
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(dir, "state"), RunID: "run-s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := "refs/crw/flow/run-s/stray/n1/a1-c_000000000001"
	if err := alloc.KeepRef(ctx, ref, first); err != nil {
		t.Fatal(err)
	}
	if err := alloc.KeepRef(ctx, ref, first); err != nil {
		t.Errorf("keeping the commit the ref already holds: %v", err)
	}
	// a commit with no history in common with the record (CRW-1392: the descendant of the record is a fast-forward and moves it)
	unrelated := run(repo, "commit-tree", second+"^{tree}", "-m", "unrelated")
	if err := alloc.KeepRef(ctx, ref, unrelated); err == nil {
		t.Errorf("the allocator moved %s to a commit that does not descend from its record", ref)
	}
	if got := run(repo, "rev-parse", ref); got != first {
		t.Errorf("%s is %s, want the first record %s", ref, got, first)
	}
}

// CRW-1392: the second HEAD of a call whose first was kept (the call's second turn after a crash) is kept on the same ref when it
// descends from the first: KeepRef moves the ref by a fast-forward with the old value as the compare-and-swap's, and refuses a
// commit that does not descend from the record (a fast-forward over a record that moved meanwhile, and a commit that is no commit,
// are TestAllocatorKeepRefRefusesAMovedRecordAndANonCommit's, CRW-1405).
func TestAllocatorKeepRefFastForwards(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	run := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "-q", "-b", "main", repo)
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	run(repo, "add", "-A")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first")
	first := run(repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "a.txt"), "b\n")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-am", "second")
	second := run(repo, "rev-parse", "HEAD")
	sibling := run(repo, "commit-tree", second+"^{tree}", "-p", first, "-m", "a sibling of second")
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(dir, "state"), RunID: "run-s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref := "refs/crw/flow/run-s/stray/n1/a1-c_000000000001"
	if err := alloc.KeepRef(ctx, ref, first); err != nil {
		t.Fatal(err)
	}
	if err := alloc.KeepRef(ctx, ref, second); err != nil {
		t.Fatalf("a descendant of the record: %v", err)
	}
	if got := run(repo, "rev-parse", ref); got != second {
		t.Fatalf("%s is %s, want the descendant %s", ref, got, second)
	}
	if err := alloc.KeepRef(ctx, ref, sibling); err == nil {
		t.Errorf("the allocator moved %s to a commit beside its record", ref)
	}
	if err := alloc.KeepRef(ctx, ref, first); err == nil {
		t.Errorf("the allocator moved %s back to an ancestor of its record", ref)
	}
	if got := run(repo, "rev-parse", ref); got != second {
		t.Errorf("%s is %s, want %s", ref, got, second)
	}
}

// CRW-1405: the two refusals of KeepRef over a ref that already keeps a record that TestAllocatorKeepRefFastForwards does not reach.
// A fast-forward over a record that moved between KeepRef's read of the ref and its update (a second writer of the same ref) is
// refused by the update's compare-and-swap against the commit it read, and the record the other writer left is kept. A commit that
// names no commit (an object id the repository does not hold, a tree, a name that is no object) is refused over an existing ref as
// "no commit", before any ancestry check, and the record is kept.
func TestAllocatorKeepRefRefusesAMovedRecordAndANonCommit(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	run := func(cwd string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "-q", "-b", "main", repo)
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	run(repo, "add", "-A")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first")
	first := run(repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "a.txt"), "b\n")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-am", "second")
	second := run(repo, "rev-parse", "HEAD")
	// what the other writer moved the record to while KeepRef was between its read and its update
	other := run(repo, "commit-tree", second+"^{tree}", "-p", first, "-m", "the other writer's record")
	ref := "refs/crw/flow/run-s/stray/n1/a1-c_000000000001"
	execGit := agent.ExecGit("git")
	var mu sync.Mutex
	moveBeforeUpdate := false
	var updates [][]string
	git := func(ctx context.Context, cwd string, env []string, args ...string) (string, error) {
		mu.Lock()
		move := moveBeforeUpdate && len(args) > 0 && args[0] == "update-ref"
		if len(args) > 0 && args[0] == "update-ref" {
			updates = append(updates, append([]string(nil), args...))
		}
		if move {
			moveBeforeUpdate = false
		}
		mu.Unlock()
		if move {
			run(repo, "update-ref", ref, other)
		}
		return execGit(ctx, cwd, env, args...)
	}
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(dir, "state"), RunID: "run-s", Git: git})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := alloc.KeepRef(ctx, ref, first); err != nil {
		t.Fatal(err)
	}

	t.Run("a fast-forward over a record that moved meanwhile", func(t *testing.T) {
		mu.Lock()
		moveBeforeUpdate, updates = true, nil
		mu.Unlock()
		if err := alloc.KeepRef(ctx, ref, second); err == nil {
			t.Errorf("the allocator moved %s over a record that moved after it read it", ref)
		}
		if got := run(repo, "rev-parse", ref); got != other {
			t.Errorf("%s is %s, want the record the other writer left %s", ref, got, other)
		}
		mu.Lock()
		defer mu.Unlock()
		// the update KeepRef sent is the fast-forward from the record it read, compared against that record
		if len(updates) != 1 || strings.Join(updates[0][len(updates[0])-2:], " ") != second+" "+first {
			t.Errorf("updates %v, want one update-ref %s %s", updates, second, first)
		}
	})

	t.Run("a commit that is no commit over an existing ref", func(t *testing.T) {
		held := run(repo, "rev-parse", ref)
		for _, c := range []struct{ name, commit string }{
			{"an object the repository does not hold", strings.Repeat("0", len(first))},
			{"a tree", run(repo, "rev-parse", second+"^{tree}")},
			{"a name that is no object", "no-such-commit"},
			{"no name", ""},
		} {
			mu.Lock()
			updates = nil
			mu.Unlock()
			err := alloc.KeepRef(ctx, ref, c.commit)
			if err == nil || !strings.Contains(err.Error(), "is no commit") {
				t.Errorf("%s: KeepRef %v, want the refusal that it is no commit", c.name, err)
			}
			if got := run(repo, "rev-parse", ref); got != held {
				t.Errorf("%s: %s is %s, want the record %s", c.name, ref, got, held)
			}
			mu.Lock()
			if len(updates) != 0 {
				t.Errorf("%s: the allocator sent %v", c.name, updates)
			}
			mu.Unlock()
		}
	})
}
