package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The session backend is what a session run's calls need of a backend: the allocator's directories (Placer, Restorer, HeadKeeper,
// DirRemover), the proofs and the source of a QA call; it starts no thread and no turn.
var (
	_ engine.Backend      = (*agent.SessionBackend)(nil)
	_ engine.Placer       = (*agent.SessionBackend)(nil)
	_ engine.Restorer     = (*agent.SessionBackend)(nil)
	_ engine.HeadKeeper   = (*agent.SessionBackend)(nil)
	_ engine.DirRemover   = (*agent.SessionBackend)(nil)
	_ engine.Prover       = (*agent.SessionBackend)(nil)
	_ engine.SourceReader = (*agent.SessionBackend)(nil)
	_ engine.SessionHost  = agent.SessionHost{}
)

// CRW-1348 (CRW-1316 §6.4): the trace digests of a session call. The workspace digest follows P's status and HEAD; the outside
// digest the owner's checkout; the shared digest the refs under refs/crw/ other than the run's own and the binding files of the
// flow-bound directory, not the hook's ledger.
func TestSessionBackendTraceDigests(t *testing.T) {
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
	wt := filepath.Join(dir, "wt")
	run(repo, "worktree", "add", "--detach", "-q", wt, first)
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(dir, "state"), RunID: "run-s"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := agent.NewSessionBackend(alloc, repo)
	if err != nil {
		t.Fatal(err)
	}
	b.BoundDir = filepath.Join(dir, "flow-bound")
	if err := os.MkdirAll(filepath.Join(b.BoundDir, "decisions"), 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	trace := func() engine.TraceDigests {
		t.Helper()
		d, err := b.Trace(ctx, wt)
		if err != nil || d.Workspace == "" || d.Outside == "" || d.Shared == "" {
			t.Fatalf("trace %+v %v", d, err)
		}
		return d
	}
	before := trace()
	if again := trace(); again != before {
		t.Fatalf("two traces of nothing changed differ: %+v %+v", before, again)
	}
	// what the run and the owner's own spawns move is not a change
	run(repo, "update-ref", "refs/crw/flow/run-s/stray/n1/a1", first)
	writeFile(t, filepath.Join(b.BoundDir, "decisions", "0190.jsonl"), "{}\n")
	if now := trace(); now != before {
		t.Fatalf("the run's own ref or the hook's ledger changed the trace: %+v %+v", before, now)
	}
	// an edit in P
	writeFile(t, filepath.Join(wt, "b.txt"), "b\n")
	edited := trace()
	if edited.Workspace == before.Workspace || edited.Outside != before.Outside || edited.Shared != before.Shared {
		t.Fatalf("an edit in P: %+v -> %+v", before, edited)
	}
	// an edit of the owner's checkout
	writeFile(t, filepath.Join(repo, "c.txt"), "c\n")
	outside := trace()
	if outside.Outside == edited.Outside || outside.Workspace != edited.Workspace {
		t.Fatalf("an edit outside P: %+v -> %+v", edited, outside)
	}
	// a ref of another run and a binding file are shared state
	run(repo, "update-ref", "refs/crw/flow/run-other/x", first)
	shared := trace()
	if shared.Shared == outside.Shared {
		t.Fatal("another run's ref did not change the shared digest")
	}
	writeFile(t, filepath.Join(b.BoundDir, "0190a000-0000-7000-8000-00000000000a.jsonl"), "{}\n")
	if now := trace(); now.Shared == shared.Shared {
		t.Fatal("a binding file did not change the shared digest")
	}
	// an owner that is P itself, or no checkout, has no outside digest
	b.OwnerCwd = t.TempDir()
	if d, _ := b.Trace(ctx, wt); d.Outside != "" {
		t.Fatalf("an owner cwd that is no checkout: %+v", d)
	}
	// it starts no thread and no turn
	if _, err := b.StartThread(ctx, engine.ThreadSpec{}); err == nil {
		t.Fatal("a session backend started a thread")
	}
	if _, err := b.RunTurn(ctx, engine.TurnSpec{}); err == nil {
		t.Fatal("a session backend ran a turn")
	}
}

// CRW-1482 (C9): the evidence hook writes its assignment records, claims and attempt counters under `<owner cwd>/.crw/` while a
// subagent works, so the outside digest leaves the owner cwd's .crw out even in an owner checkout that does not ignore it; every
// other path of the owner's repository, a .crw elsewhere in it and the paths outside the owner's subdirectory included, is still
// seen.
func TestSessionTraceIgnoresOwnerCrwState(t *testing.T) {
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
	writeFile(t, filepath.Join(repo, "owner", "sub", "keep.txt"), "k\n")
	run(repo, "add", "-A")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first")
	first := run(repo, "rev-parse", "HEAD")
	if !gitFails(repo, "check-ignore", "-q", "owner/sub/.crw/x") {
		t.Fatal("the owner checkout ignores .crw")
	}
	wt := filepath.Join(dir, "wt")
	run(repo, "worktree", "add", "--detach", "-q", wt, first)
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(dir, "state"), RunID: "run-s"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, owner := range []string{repo, filepath.Join(repo, "owner", "sub")} {
		b, err := agent.NewSessionBackend(alloc, owner)
		if err != nil {
			t.Fatal(err)
		}
		b.BoundDir = filepath.Join(dir, "flow-bound")
		trace := func() engine.TraceDigests {
			t.Helper()
			d, err := b.Trace(ctx, wt)
			if err != nil || d.Workspace == "" || d.Outside == "" || d.Shared == "" {
				t.Fatalf("trace %+v %v", d, err)
			}
			return d
		}
		before := trace()
		// the hook's records, claims and counters under the owner cwd's .crw are not a change outside P
		writeFile(t, filepath.Join(owner, ".crw", "evidence-assignments", "owner-s", "a1.json"), "{}\n")
		writeFile(t, filepath.Join(owner, ".crw", "evidence", "attempts", "n.json"), "{}\n")
		if now := trace(); now != before {
			t.Fatalf("owner %s: the hook's .crw state changed the trace: %+v -> %+v", owner, before, now)
		}
		// anything else in the owner's repository still is
		for _, p := range []string{
			filepath.Join(owner, "edit.txt"),
			filepath.Join(repo, "top.txt"),
			filepath.Join(repo, "elsewhere", ".crw", "x.json"),
			filepath.Join(owner, ".crwx", "y.json"),
		} {
			last := trace()
			writeFile(t, p, "x "+owner+"\n")
			if now := trace(); now.Outside == last.Outside || now.Workspace != last.Workspace {
				t.Fatalf("owner %s: writing %s: %+v -> %+v", owner, p, last, now)
			}
		}
		last := trace()
		writeFile(t, filepath.Join(owner, "sub2", "keep.txt"), "k\n")
		run(repo, "add", "-A")
		staged := trace()
		if staged.Outside == last.Outside {
			t.Fatalf("owner %s: a staged change did not change the trace", owner)
		}
		run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "more")
		// a tracked file under the owner cwd's .crw is left out too: the hook rewrites its own files there
		tracked := trace()
		if tracked.Outside == staged.Outside {
			t.Fatalf("owner %s: a commit did not change the trace", owner)
		}
		writeFile(t, filepath.Join(owner, ".crw", "evidence-assignments", "owner-s", "a1.json"), "{\"claimed\":true}\n")
		if now := trace(); now != tracked {
			t.Fatalf("owner %s: a rewrite of a tracked .crw file changed the trace: %+v -> %+v", owner, tracked, now)
		}
	}
}

// CRW-1493: the owner cwd's .crw is left out of the outside digest relative to the owner cwd itself, not through a repository
// prefix read back from git: an owner whose repository-relative path starts with a space (`repo/" lead"/sub`) keeps its .crw out,
// its other paths in, and the twin path with the space trimmed (`repo/lead/sub/.crw`) is still seen.
func TestSessionTraceKeepsLeadingSpaceOwner(t *testing.T) {
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
	owner := filepath.Join(repo, " lead", "sub")
	writeFile(t, filepath.Join(repo, "a.txt"), "a\n")
	writeFile(t, filepath.Join(owner, "keep.txt"), "k\n")
	run(repo, "add", "-A")
	run(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "first")
	first := run(repo, "rev-parse", "HEAD")
	if !gitFails(repo, "check-ignore", "-q", " lead/sub/.crw/x") {
		t.Fatal("the owner checkout ignores .crw")
	}
	wt := filepath.Join(dir, "wt")
	run(repo, "worktree", "add", "--detach", "-q", wt, first)
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: repo, StateDir: filepath.Join(dir, "state"), RunID: "run-s"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := agent.NewSessionBackend(alloc, owner)
	if err != nil {
		t.Fatal(err)
	}
	b.BoundDir = filepath.Join(dir, "flow-bound")
	ctx := context.Background()
	trace := func() engine.TraceDigests {
		t.Helper()
		d, err := b.Trace(ctx, wt)
		if err != nil || d.Workspace == "" || d.Outside == "" || d.Shared == "" {
			t.Fatalf("trace %+v %v", d, err)
		}
		return d
	}
	before := trace()
	// the hook's record under the owner cwd's .crw is not a change outside P
	writeFile(t, filepath.Join(owner, ".crw", "evidence-assignments", "owner-s", "a1.json"), "{}\n")
	if now := trace(); now != before {
		t.Fatalf("the hook's .crw state under a leading-space owner changed the trace: %+v -> %+v", before, now)
	}
	// a tracked file of the owner and the trimmed twin's .crw still are
	for _, p := range []string{
		filepath.Join(owner, "keep.txt"),
		filepath.Join(repo, "lead", "sub", ".crw", "x"),
	} {
		last := trace()
		writeFile(t, p, "x "+p+"\n")
		if now := trace(); now.Outside == last.Outside || now.Workspace != last.Workspace {
			t.Fatalf("writing %s: %+v -> %+v", p, last, now)
		}
	}
}

// CRW-1327: the session host reads the rollout's token_count with its split (CRW-1332's sessionhost.Usage) and hands the engine all of
// it: the cached input and the reasoning output are kept, not only the input, the output and the total.
func TestSessionHostTokensKeepTheCachedSplit(t *testing.T) {
	rollout := filepath.Join(t.TempDir(), "rollout.jsonl")
	line := `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":4171534,"cached_input_tokens":4006518,"output_tokens":46728,"reasoning_output_tokens":31200,"total_tokens":4218262}}}}` + "\n"
	if err := os.WriteFile(rollout, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	got, ok := agent.SessionHost{}.Tokens(context.Background(), rollout)
	want := engine.Tokens{Input: 4171534, CachedInput: 4006518, Output: 46728, ReasoningOutput: 31200, Total: 4218262}
	if !ok || got != want {
		t.Errorf("Tokens = %+v %v, want %+v", got, ok, want)
	}
}
