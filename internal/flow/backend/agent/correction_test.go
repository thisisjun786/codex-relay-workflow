package agent_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// inheritRepository makes the test process look like a git hook of another repository: the variables that route git to
// that repository are set. The returned function clears them again, so the test's own git commands are unaffected.
func inheritRepository(t *testing.T, foreign string) (clear func()) {
	t.Helper()
	vars := map[string]string{
		"GIT_DIR":        filepath.Join(foreign, ".git"),
		"GIT_WORK_TREE":  foreign,
		"GIT_INDEX_FILE": filepath.Join(foreign, ".git", "index"),
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
	return func() {
		for k := range vars {
			_ = os.Unsetenv(k)
		}
	}
}

// The allocator's git commands run in the directory they are given and nowhere else, whatever repository the process
// inherits from its environment (a git hook, a shell with GIT_DIR set).
func TestAllocatorGitIgnoresInheritedRepositoryEnvironment(t *testing.T) {
	e := newEnv(t)
	wt := filepath.Join(e.root, "flow-wt")
	git(t, e.repo, "worktree", "add", "--detach", "-q", wt, e.base)
	foreign, foreignHead := newRepo(t, t.TempDir())
	writeFile(t, filepath.Join(wt, "b.txt"), "flow edit\n")

	clear := inheritRepository(t, foreign)
	head, err := e.alloc.Commit(context.Background(), wt, "flow commit", false)
	snap, snapErr := e.alloc.Snapshot(context.Background(), wt)
	read, readErr := e.alloc.Head(context.Background(), wt)
	clear()

	if err != nil || snapErr != nil || readErr != nil {
		t.Fatalf("commit %v, snapshot %v, head %v", err, snapErr, readErr)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != head || read != head || head == e.base {
		t.Errorf("the flow worktree is at %s, Commit returned %s, Head %s, base %s", got, head, read, e.base)
	}
	if got := git(t, wt, "show", "--name-only", "--format=", "HEAD"); got != "b.txt" {
		t.Errorf("the flow commit holds %q", got)
	}
	if got := git(t, foreign, "rev-parse", "HEAD"); got != foreignHead {
		t.Errorf("the inherited repository moved from %s to %s", foreignHead, got)
	}
	if got := git(t, foreign, "status", "--porcelain"); got != "" {
		t.Errorf("the inherited repository was touched: %q", got)
	}
	if want := git(t, wt, "rev-parse", "HEAD^{tree}"); snap != want {
		t.Errorf("snapshot %s, the worktree's tree is %s", snap, want)
	}
}

// A tracked file that an ignore rule matches is still part of the snapshot: an edit to it changes the digest and the
// retired ref keeps its working content.
func TestSnapshotCoversTrackedFilesThatAreIgnored(t *testing.T) {
	e := newEnv(t)
	writeFile(t, filepath.Join(e.repo, ".gitignore"), "generated.txt\n")
	writeFile(t, filepath.Join(e.repo, "generated.txt"), "tracked v1\n")
	git(t, e.repo, "add", ".gitignore")
	git(t, e.repo, "add", "-f", "generated.txt")
	git(t, e.repo, "commit", "-q", "-m", "track generated")
	head := git(t, e.repo, "rev-parse", "HEAD")
	wt := filepath.Join(e.root, "flow-wt")
	git(t, e.repo, "worktree", "add", "--detach", "-q", wt, head)

	ctx := context.Background()
	before, err := e.alloc.Snapshot(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(t, e.repo, "rev-parse", head+"^{tree}"); before != got {
		t.Errorf("an unchanged worktree has snapshot %s, its commit's tree is %s", before, got)
	}
	writeFile(t, filepath.Join(wt, "generated.txt"), "tracked v2, edited\n")
	after, err := e.alloc.Snapshot(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Errorf("an edit to a tracked ignored file left the snapshot at %s", before)
	}
	if err := e.alloc.Retire(ctx, "n1", 1, wt); err != nil {
		t.Fatal(err)
	}
	if got := git(t, e.repo, "show", e.alloc.RetiredRef("n1", 1)+":generated.txt"); got != "tracked v2, edited" {
		t.Errorf("the retired ref holds %q", got)
	}
}

// A notification stream that lost the end of a turn does not decide its answer: the host's full items do. A commentary
// message seen on the bus is not the answer when the final answer was lost.
func TestPartialNotificationsDoNotOverrideTheRecoveredAnswer(t *testing.T) {
	cases := map[string]fakehost.TurnPlan{
		"final item lost, turn completed seen":    {Message: passMessage, Commentary: "thinking out loud", DropFinalItem: true},
		"stream lost after the commentary":        {Message: passMessage, Commentary: "thinking out loud", SilentAfterCommentary: true},
		"final item lost, no commentary seen":     {Message: passMessage, DropFinalItem: true},
		"commentary and final both seen (normal)": {Message: passMessage, Commentary: "thinking out loud"},
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return plan })
			info := startedThread(t, e, "explorer")
			out, err := runTurn(t, e, info, "explorer", 1, "one")
			if err != nil {
				t.Fatal(err)
			}
			if out.Text != passMessage {
				t.Errorf("the turn answered %q, want the final answer %q", out.Text, passMessage)
			}
		})
	}
}

// An interrupt the engine journaled decides the call even when the host's turn completed with a valid answer: the call
// is held engine_paused, not completed.
func TestEngineInterruptIntentGovernsACompletedTurn(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if strings.Contains(r.Text, "quota-call") {
			return fakehost.TurnPlan{Gate: gate, Fail: &fakehost.Failure{Message: "quota", CodexErrorInfo: "usageLimitExceeded"}}
		}
		return fakehost.TurnPlan{Hang: true, FinishDespiteInterrupt: true, Message: passMessage}
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.host.WaitCount(ctx, "turn/start", 2)
		close(gate)
	}()
	res := e.run(`const out = await parallel([
  async () => agent.call('quota-call', { agentType: 'executor', node: 'a', label: 'quota' }),
  async () => agent.call('long-call', { agentType: 'executor', node: 'b', label: 'long' }),
])
return out.map((r) => [r.status, r.reason])`)
	if got := value[[][]string](t, res); fmt.Sprint(got) != "[[held host_infra_usageLimitExceeded] [held engine_paused]]" {
		t.Fatalf("%v", got)
	}
	if h := e.row("long", engine.EvHeld); h.Data["input_state"] != "delivered" {
		t.Errorf("held %v", h.Data)
	}
}

// A restored worktree that changed since it was verified stops its node for the rest of the run: later calls of the node
// send nothing, make no worktree and resume no thread.
func TestRestoreMismatchStopsTheNode(t *testing.T) {
	e := newEnv(t)
	e.restoreAfter("first", 2, 1, func(path string) {
		writeFile(t, filepath.Join(path, "p.txt"), "edited before the call\n")
	})
	res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const second = await agent.call('two', { agentType: 'executor', node: 'n1', label: 'second' })
const third = await agent.call('three', { agentType: 'executor', node: 'n1', label: 'third' })
const fix = await agent.call('fix', { resume: first, label: 'fix' })
const other = await agent.call('four', { agentType: 'executor', node: 'n2', label: 'other' })
return [first.status, second.status, third.status, fix.status, other.status]`)
	by := calls(res)
	if by["second"].Reason != engine.ReasonRestoreSnapshotMismatch {
		t.Fatalf("second: %s/%s", by["second"].Status, by["second"].Reason)
	}
	for _, label := range []string{"third", "fix"} {
		if by[label].Status == engine.StatusCompleted || by[label].Reason != engine.ReasonRestoreSnapshotMismatch {
			t.Errorf("%s: %s/%s, want the node stopped with %s", label, by[label].Status, by[label].Reason, engine.ReasonRestoreSnapshotMismatch)
		}
		if by[label].Meta.ThreadID != "" && label == "third" {
			t.Errorf("%s started the thread %s", label, by[label].Meta.ThreadID)
		}
	}
	if by["other"].Status != engine.StatusCompleted {
		t.Errorf("another node: %s/%s", by["other"].Status, by["other"].Reason)
	}
	// first (1 thread, 1 turn) and the other node (1 thread, 1 turn); nothing for second, third or fix
	if n := len(e.requests("turn/start")); n != 2 {
		t.Errorf("%d turn/start requests, want 2", n)
	}
	if n := len(e.requests("thread/start", "thread/fork")); n != 2 {
		t.Errorf("%d thread creations, want 2", n)
	}
	if esc := e.flowRows("escalation"); len(esc) != 1 {
		t.Errorf("escalations %+v", esc)
	}
}

// A turn whose token usage was never reported is not a turn that used none: it is journaled as unmeasured and, under a
// token ceiling, stops the run the way an exhausted budget does.
func TestUnmeasuredTokensAreNotCountedAsZero(t *testing.T) {
	e := newEnv(t)
	e.budget = 1000
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: passMessage, NoTokenUsage: true}
	})
	res := e.run(`const a = await agent.call('one', { agentType: 'explorer', label: 'a' })
const b = await agent.call('two', { agentType: 'explorer', label: 'b' })
const c = await agent.call('three', { agentType: 'explorer', label: 'c' })
return [a.status, b.status, c.status, b.reason]`)
	got := value[[]string](t, res)
	if !slices.Equal(got, []string{"completed", "held", "held", "budget_exhausted"}) {
		t.Fatalf("%v", got)
	}
	if rows := e.flowRows("tokens_unmeasured"); len(rows) != 1 || rows[0].Data["call_id"] != calls(res)["a"].CallID {
		t.Errorf("tokens_unmeasured rows %+v", rows)
	}
	if n := len(e.requests("turn/start")); n != 1 {
		t.Errorf("%d turn/start requests, want 1", n)
	}
}

// Without a ceiling an unmeasured turn changes nothing for the run, but it is still journaled as unmeasured.
func TestUnmeasuredTokensWithoutCeilingKeepRunning(t *testing.T) {
	e := newEnv(t)
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: passMessage, NoTokenUsage: true}
	})
	res := e.run(`const a = await agent.call('one', { agentType: 'explorer', label: 'a' })
const b = await agent.call('two', { agentType: 'explorer', label: 'b' })
return [a.status, b.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	if n := len(e.flowRows("tokens_unmeasured")); n != 2 {
		t.Errorf("%d tokens_unmeasured rows, want 2", n)
	}
}

// The worktree's own index may mark a tracked file assume-unchanged or skip-worktree; the snapshot still hashes the file's
// working content, and the worktree's own index keeps its flags.
func TestSnapshotSeesEditsBehindIndexFlags(t *testing.T) {
	for flag, tag := range map[string]string{"--assume-unchanged": "h", "--skip-worktree": "S"} {
		t.Run(flag, func(t *testing.T) {
			e := newEnv(t)
			wt := filepath.Join(e.root, "flow-wt")
			git(t, e.repo, "worktree", "add", "--detach", "-q", wt, e.base)
			ctx := context.Background()
			before, err := e.alloc.Snapshot(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			git(t, wt, "update-index", flag, "a.txt")
			writeFile(t, filepath.Join(wt, "a.txt"), "an edit of another length\n")
			after, err := e.alloc.Snapshot(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			if after == before {
				t.Errorf("an edit behind %s left the snapshot at %s", flag, before)
			}
			if err := e.alloc.Retire(ctx, "n1", 1, wt); err != nil {
				t.Fatal(err)
			}
			if got := git(t, e.repo, "show", e.alloc.RetiredRef("n1", 1)+":a.txt"); got != "an edit of another length" {
				t.Errorf("the retired ref holds %q", got)
			}
			if got := git(t, wt, "ls-files", "-v", "a.txt"); got != tag+" a.txt" {
				t.Errorf("the worktree's own index lists %q, want its flag kept (%q)", got, tag+" a.txt")
			}
		})
	}
}

// A skip-worktree entry whose file is not in the worktree (a sparse checkout) is held with its index content: it is not a
// deletion.
func TestSnapshotKeepsSparseEntriesAbsentFromTheWorktree(t *testing.T) {
	e := newEnv(t)
	wt := filepath.Join(e.root, "flow-wt")
	git(t, e.repo, "worktree", "add", "--detach", "-q", wt, e.base)
	ctx := context.Background()
	before, err := e.alloc.Snapshot(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	git(t, wt, "update-index", "--skip-worktree", "a.txt")
	if err := os.Remove(filepath.Join(wt, "a.txt")); err != nil {
		t.Fatal(err)
	}
	after, err := e.alloc.Snapshot(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a sparse entry absent from the worktree changed the snapshot from %s to %s", before, after)
	}
}

// A provider that sets no message phase (0.154 MessagePhase is nullable) still answers over the bus: a completed turn whose
// last message has no phase, with no commentary seen, is answered by that message without reading the turn, so a read
// that fails cannot spoil a valid answer.
func TestPhaselessFinalAnswerComesFromTheBus(t *testing.T) {
	e := newEnv(t, withPoll(time.Hour))
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: passMessage, NullPhase: true, SummaryOnly: true, ItemsListError: map[string]any{"code": -32600, "message": "items are not available"}}
	})
	info := startedThread(t, e, "explorer")
	for k := 1; k <= 2; k++ {
		out, err := runTurn(t, e, info, "explorer", k, "one")
		if err != nil {
			t.Fatalf("turn %d: %v", k, err)
		}
		if out.Text != passMessage {
			t.Errorf("turn %d answered %q", k, out.Text)
		}
	}
	if n := len(e.requests("thread/turns/list", "thread/items/list")); n != 0 {
		t.Errorf("%d turn reads: a phase-less answer heard on the bus needs none", n)
	}
}

// A phase-less answer does not lower the guard of a partial stream: a commentary seen on the bus, or no message at all,
// still sends the backend to the host's items.
func TestPhaselessAnswerAfterPartialStreamIsRead(t *testing.T) {
	cases := map[string]fakehost.TurnPlan{
		"final item lost":                  {Message: passMessage, NullPhase: true, DropFinalItem: true},
		"commentary seen, final item lost": {Message: passMessage, NullPhase: true, Commentary: "thinking out loud", DropFinalItem: true},
		"commentary and final seen":        {Message: passMessage, NullPhase: true, Commentary: "thinking out loud"},
	}
	for name, plan := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, withPoll(time.Hour))
			e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return plan })
			info := startedThread(t, e, "explorer")
			out, err := runTurn(t, e, info, "explorer", 1, "one")
			if err != nil {
				t.Fatal(err)
			}
			if out.Text != passMessage {
				t.Errorf("the turn answered %q, want %q", out.Text, passMessage)
			}
			if n := len(e.requests("thread/turns/list")); n != 1 {
				t.Errorf("%d turn reads, want the one read of the host's items", n)
			}
		})
	}
}
