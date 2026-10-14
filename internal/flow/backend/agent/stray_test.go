package agent_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
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
// and never moves it to another commit, so a second record of the same ref cannot overwrite the first.
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
	if err := alloc.KeepRef(ctx, ref, second); err == nil {
		t.Errorf("the allocator moved %s to another commit", ref)
	}
	if got := run(repo, "rev-parse", ref); got != first {
		t.Errorf("%s is %s, want the first record %s", ref, got, first)
	}
}
