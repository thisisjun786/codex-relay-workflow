package agent_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func (e *env) wt(node string, attempt, lineage int) string {
	return filepath.Join(e.state, "flow", e.runID, "wt", node, fmt.Sprintf("a%d-l%d", attempt, lineage))
}

func allocationOf(t *testing.T, row engine.Event) map[string]any {
	t.Helper()
	a, ok := row.Data["allocation"].(map[string]any)
	if !ok {
		t.Fatalf("prepared row without an allocation: %v", row.Data)
	}
	return a
}

func num(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return -1
}

// restoreAfter makes the journal behave as F07's suffix restore: when the call labelled after ends, it makes a worktree at
// C with a pending file on top (outside the allocator, so the allocator's own commands stay countable), verifies its
// snapshot and writes the worktree_restored row. It returns the restored path.
func (e *env) restoreAfter(after string, attempt, lineage int, then ...func(path string)) string {
	e.t.Helper()
	path := e.wt("n1", attempt, lineage)
	done := false
	e.journal.onAfter(func(ev engine.Event) {
		if done || ev.Kind != engine.KindCall || ev.Label != after || ev.Event != engine.EvCompleted {
			return
		}
		done = true
		git(e.t, e.repo, "worktree", "add", "--detach", "-q", path, e.base)
		writeFile(e.t, filepath.Join(path, "p.txt"), "pending\n")
		digest, err := e.alloc.Snapshot(context.Background(), path)
		if err != nil {
			e.t.Error(err)
			return
		}
		e.journal.MemoryJournal.Append(engine.Event{RunID: e.runID, Kind: engine.KindFlow, Event: agentRestored, Data: map[string]any{
			"node": "n1", "lineage": lineage, "attempt": attempt, "path": path, "C": e.base, "P": digest, "snapshot_digest": digest}})
		for _, fn := range then {
			fn(path)
		}
	})
	return path
}

const agentRestored = "worktree_restored"

// Every executor call that creates a thread gets a worktree of its own - the first build, the rebuild after a null_model
// and the executor-t1 build - at the node's last commit, with the allocation journaled in prepared before the worktree
// exists; a fix continuation makes none. A retired worktree keeps its HEAD and working snapshot on a ref.
func TestEachThreadCreatingExecutorCallGetsOwnWorktree(t *testing.T) {
	e := newEnv(t)
	second := git(t, e.repo, "commit-tree", "-p", e.base, "-m", "recorded", git(t, e.repo, "rev-parse", e.base+"^{tree}"))
	// the first attempt leaves an uncommitted edit and a null_model
	first := e.wt("n1", 1, 0)
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.ThreadID == "thr-fake-001" {
			writeFile(t, filepath.Join(first, "scratch.txt"), "half done\n")
			return fakehost.TurnPlan{Message: "not json"}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	// crw.commit (F08) records a commit on the node after the first attempt; the next attempt starts from it
	e.journal.onAfter(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Label == "first" && ev.Event == engine.EvNullModel {
			e.journal.MemoryJournal.Append(engine.Event{RunID: e.runID, Kind: engine.KindCall, CallID: "c_commit", Op: "crw.commit", Event: engine.EvCompleted,
				Data: map[string]any{"node": "n1", "commit": second}})
		}
	})
	// the prepared row precedes the worktree, and the worktree precedes thread_requested
	var ordering []string
	preparedChecked, startChecked := 0, 0
	e.journal.onBefore(func(ev engine.Event) {
		if ev.Event != engine.EvPrepared {
			return
		}
		if a, ok := ev.Data["allocation"].(map[string]any); ok && a["kind"] == "fresh" {
			preparedChecked++
			if _, err := os.Lstat(str(a, "path")); err == nil {
				ordering = append(ordering, "the worktree existed before its prepared row: "+str(a, "path"))
			}
		}
	})
	e.threads.OnStart(func(params map[string]any) map[string]any {
		cwd := str(params, "cwd")
		startChecked++
		seen := false
		for _, ev := range e.journal.Events() {
			if ev.Event == engine.EvThreadRequested && str(ev.Data, "cwd") == cwd {
				seen = true
			}
		}
		if _, err := os.Lstat(cwd); err != nil || !seen {
			ordering = append(ordering, "thread/start before the worktree or thread_requested: "+cwd)
		}
		return nil
	})
	res := e.run(`const S = ` + verdictSchema + `
const first = await agent.call('FIRST build', { agentType: 'executor', node: 'n1', schema: S, label: 'first' })
const rebuild = await agent.call('build again', { agentType: 'executor', node: 'n1', schema: S, label: 'rebuild' })
const step = await crw.tierStep('n1', { evidence: [first.callId] })
const t1 = await agent.call('build as t1', { model: 'sonnet', stepDecision: step.id, node: 'n1', schema: S, label: 't1' })
const fix = await agent.call('fix it', { resume: t1, label: 'fix' })
return [first.status, rebuild.status, t1.status, fix.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"null_model", "completed", "completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	if len(ordering) > 0 {
		t.Errorf("%v", ordering)
	}
	if preparedChecked != 3 || startChecked != 3 {
		t.Errorf("the ordering was checked %d and %d times, want 3 and 3", preparedChecked, startChecked)
	}
	starts := e.requests("thread/start")
	if len(starts) != 3 {
		t.Fatalf("%d threads, want the three thread-creating calls", len(starts))
	}
	for i, label := range []string{"first", "rebuild", "t1"} {
		want := e.wt("n1", i+1, 0)
		if starts[i].Params["cwd"] != want {
			t.Errorf("%s: cwd %v, want %s", label, starts[i].Params["cwd"], want)
		}
		a := allocationOf(t, e.row(label, engine.EvPrepared))
		wantStart := second
		if i == 0 {
			wantStart = e.base
		}
		if a["kind"] != "fresh" || num(a["attempt"]) != i+1 || a["path"] != want || a["start_commit"] != wantStart {
			t.Errorf("%s: allocation %v, want fresh attempt %d at %s from %s", label, a, i+1, want, wantStart)
		}
		if head := git(t, want, "rev-parse", "HEAD"); head != wantStart {
			t.Errorf("%s: worktree HEAD %s, want %s", label, head, wantStart)
		}
	}
	// the fix continuation made nothing
	if a := allocationOf(t, e.row("fix", engine.EvPrepared)); a["kind"] != "continued" || a["path"] != nil {
		t.Errorf("fix allocation %v", a)
	}
	if n := e.worktreeAdds(); n != 3 {
		t.Errorf("%d worktrees were made, want 3", n)
	}
	if slices.Contains(kinds(e.rows("fix")), engine.EvThreadRequested) {
		t.Errorf("a continuation starts no thread: %v", kinds(e.rows("fix")))
	}
	if turns := e.requests("turn/start"); turns[len(turns)-1].Params["threadId"] != turns[len(turns)-2].Params["threadId"] {
		t.Errorf("the fix turn must run on the t1 thread")
	}
	// retired worktrees keep their HEAD and working snapshot; the edit of the null_model attempt is not carried forward
	ref := fmt.Sprintf("refs/crw/flow/%s/retired/n1/a1", e.runID)
	if got := git(t, e.repo, "show", ref+":scratch.txt"); got != "half done" {
		t.Errorf("retired a1 holds %q", got)
	}
	if parent := git(t, e.repo, "rev-parse", ref+"^"); parent != e.base {
		t.Errorf("retired a1 sits on %s, want its HEAD %s", parent, e.base)
	}
	if _, err := os.Stat(filepath.Join(e.wt("n1", 2, 0), "scratch.txt")); err == nil {
		t.Errorf("the edit of the null_model attempt reached the next worktree")
	}
	if got := git(t, e.repo, "for-each-ref", "--format=%(refname)", fmt.Sprintf("refs/crw/flow/%s/retired/", e.runID)); got != fmt.Sprintf("refs/crw/flow/%s/retired/n1/a1\nrefs/crw/flow/%s/retired/n1/a2", e.runID, e.runID) {
		t.Errorf("retired refs:\n%s", got)
	}
}

// The first executor call of a suffix adopts the worktree the restore made - as a fresh thread or as a fork - and makes,
// checks out and resets nothing; a null_model rebuild after it is a new allocation.
func TestRestoreAllocationAdoptedNotReallocated(t *testing.T) {
	check := func(t *testing.T, e *env, restored, label string, wantAdds int) {
		t.Helper()
		a := allocationOf(t, e.row(label, engine.EvPrepared))
		if a["kind"] != "restore" || a["path"] != restored || a["start_commit"] != e.base || num(a["attempt"]) != 2 || num(a["lineage"]) != 1 || a["pending"] == nil || a["snapshot_digest"] == nil {
			t.Errorf("%s: allocation %v", label, a)
		}
		if got, err := os.ReadFile(filepath.Join(restored, "p.txt")); err != nil || string(got) != "pending\n" {
			t.Errorf("%s: the pending file is %q (%v): the adopted worktree must stay as it was restored", label, got, err)
		}
		if n := e.worktreeAdds(); n != wantAdds {
			t.Errorf("%d worktrees made by the allocator, want %d", n, wantAdds)
		}
		for _, c := range e.gitCalls() {
			if len(c.Args) > 0 && (c.Args[0] == "checkout" || c.Args[0] == "reset" || c.Args[0] == "clean" || c.Args[0] == "restore") {
				t.Errorf("git %v rewrote files of the adopted worktree", c.Args)
			}
		}
	}
	t.Run("fresh thread", func(t *testing.T) {
		e := newEnv(t)
		restored := e.restoreAfter("first", 2, 1)
		res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const second = await agent.call('two', { agentType: 'executor', node: 'n1', label: 'second' })
const third = await agent.call('three', { agentType: 'executor', node: 'n1', label: 'third' })
return [first.status, second.status, third.status]`)
		if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed", "completed"}) {
			t.Fatalf("%v", got)
		}
		starts := e.requests("thread/start")
		if len(starts) != 3 || starts[1].Params["cwd"] != restored {
			t.Fatalf("thread/start cwds %v, want the second at %s", starts, restored)
		}
		check(t, e, restored, "second", 2)
		// the rebuild that follows is a new allocation: 1 + the two allocations of the node, in the restore's lineage
		third := allocationOf(t, e.row("third", engine.EvPrepared))
		if third["kind"] != "fresh" || num(third["attempt"]) != 3 || num(third["lineage"]) != 1 || third["path"] != e.wt("n1", 3, 1) {
			t.Errorf("third: allocation %v", third)
		}
		if starts[2].Params["cwd"] != e.wt("n1", 3, 1) {
			t.Errorf("the rebuild runs in %v", starts[2].Params["cwd"])
		}
		// the adopted worktree is retired like any other when the next allocation is made
		ref := fmt.Sprintf("refs/crw/flow/%s/retired/n1/a2", e.runID)
		if got := git(t, e.repo, "show", ref+":p.txt"); got != "pending" {
			t.Errorf("retired a2 holds %q", got)
		}
	})
	t.Run("fork", func(t *testing.T) {
		e := newEnv(t)
		restored := e.restoreAfter("first", 2, 1)
		res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const second = await agent.call('two', { resume: first, label: 'second' })
const third = await agent.call('three', { agentType: 'executor', node: 'n1', label: 'third' })
return [first.status, second.status, third.status, second.handle !== first.handle]`)
		if got := value[[]any](t, res); fmt.Sprint(got) != "[completed completed completed true]" {
			t.Fatalf("%v", got)
		}
		forks := e.requests("thread/fork")
		if len(forks) != 1 {
			t.Fatalf("%d thread/fork requests", len(forks))
		}
		by := calls(res)
		fork := forks[0].Params
		if fork["cwd"] != restored || fork["threadId"] != by["first"].Meta.ThreadID || fork["lastTurnId"] != by["first"].Meta.TurnIDs[0] || fork["ephemeral"] != false {
			t.Errorf("thread/fork %v", fork)
		}
		// the original thread got no further turn: the continuation runs on the fork
		var onOriginal int
		for _, r := range e.requests("turn/start") {
			if r.Params["threadId"] == by["first"].Meta.ThreadID {
				onOriginal++
			}
		}
		if onOriginal != 1 || by["second"].Meta.ThreadID == by["first"].Meta.ThreadID {
			t.Errorf("%d turns on the original thread; the continuation's thread is %s", onOriginal, by["second"].Meta.ThreadID)
		}
		if got := kinds(e.rows("second")); !slices.Equal(got, []string{"prepared", "fork_requested", "fork_started", "turn_requested", "turn_started", "completed"}) {
			t.Errorf("rows %v", got)
		}
		requested := e.row("second", engine.EvForkRequested).Data
		if requested["thread_id"] != by["first"].Meta.ThreadID || requested["through_turn_id"] != by["first"].Meta.TurnIDs[0] || requested["cwd"] != restored {
			t.Errorf("fork_requested %v", requested)
		}
		prefix, _ := requested["prefix"].([]map[string]any)
		if len(prefix) != 1 || prefix[0]["turn_id"] != by["first"].Meta.TurnIDs[0] || prefix[0]["op"] != by["first"].CallID+"/t1" {
			t.Errorf("the expected prefix %v", requested["prefix"])
		}
		if started := e.row("second", engine.EvForkStarted).Data; started["cwd"] != restored || started["model"] != "claude-fake-haiku" {
			t.Errorf("fork_started %v", started)
		}
		a := allocationOf(t, e.row("second", engine.EvPrepared))
		if a["kind"] != "restore" || a["path"] != restored || a["fork"] != true {
			t.Errorf("second: allocation %v", a)
		}
		if got, err := os.ReadFile(filepath.Join(restored, "p.txt")); err != nil || string(got) != "pending\n" {
			t.Errorf("the pending file is %q (%v)", got, err)
		}
		if n := e.worktreeAdds(); n != 2 {
			t.Errorf("%d worktrees made by the allocator: the first call's and the third's", n)
		}
		if third := allocationOf(t, e.row("third", engine.EvPrepared)); third["kind"] != "fresh" || num(third["attempt"]) != 3 {
			t.Errorf("third: allocation %v", third)
		}
	})
	t.Run("checkpoint thread", func(t *testing.T) {
		// an executor-t1 call that the suffix starts adopts the same worktree
		e := newEnv(t)
		restored := e.restoreAfter("first", 2, 1)
		res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const step = await crw.tierStep('n1', { evidence: [first.callId] })
const t1 = await agent.call('two', { model: 'sonnet', stepDecision: step.id, node: 'n1', label: 't1' })
return [first.status, t1.status]`)
		if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
			t.Fatalf("%v", got)
		}
		if starts := e.requests("thread/start"); len(starts) != 2 || starts[1].Params["cwd"] != restored {
			t.Fatalf("thread/start %v, want the executor-t1 thread at %s", starts, restored)
		}
		check(t, e, restored, "t1", 1)
	})
}

// An adopted restore is compared with its verified snapshot before the thread is started and again before the call's
// first turn; a difference sends nothing further, leaves the call's last row where it was and escalates once.
func TestRestoreSnapshotRecheckedBeforeFirstTurn(t *testing.T) {
	assert := func(t *testing.T, e *env, res *engine.Result, last string, turns, threads int) {
		t.Helper()
		by := calls(res)
		if by["second"].Status != engine.StatusUnknown || by["second"].Reason != engine.ReasonRestoreSnapshotMismatch {
			t.Errorf("second: %s/%s", by["second"].Status, by["second"].Reason)
		}
		rows := kinds(e.rows("second"))
		if rows[len(rows)-1] != last {
			t.Errorf("rows %v: the last row must stay %s", rows, last)
		}
		if n := len(e.requests("turn/start")); n != turns {
			t.Errorf("%d turn/start requests, want %d", n, turns)
		}
		if n := len(e.requests("thread/start", "thread/fork")); n != threads {
			t.Errorf("%d thread creations, want %d", n, threads)
		}
		esc := e.flowRows("escalation")
		if len(esc) != 1 || esc[0].Data["reason"] != "restore_snapshot_mismatch" || esc[0].Data["call_id"] != by["second"].CallID || esc[0].Data["last_event"] != last {
			t.Errorf("escalations %+v", esc)
		}
	}
	t.Run("changed after fork_started", func(t *testing.T) {
		e := newEnv(t)
		restored := e.restoreAfter("first", 2, 1)
		e.threads.OnStart(func(params map[string]any) map[string]any {
			if _, fork := params["lastTurnId"]; fork {
				writeFile(t, filepath.Join(restored, "p.txt"), "edited during the fork\n")
			}
			return nil
		})
		res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const second = await agent.call('two', { resume: first, label: 'second' })
return [first.status, second.status]`)
		assert(t, e, res, engine.EvForkStarted, 1, 2)
	})
	t.Run("changed after thread_started", func(t *testing.T) {
		e := newEnv(t)
		restored := e.restoreAfter("first", 2, 1)
		e.threads.OnStart(func(params map[string]any) map[string]any {
			if params["cwd"] == restored {
				writeFile(t, filepath.Join(restored, "p.txt"), "edited during the start\n")
			}
			return nil
		})
		res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const second = await agent.call('two', { agentType: 'executor', node: 'n1', label: 'second' })
return [first.status, second.status]`)
		assert(t, e, res, engine.EvThreadStarted, 1, 2)
	})
	t.Run("changed before the thread", func(t *testing.T) {
		e := newEnv(t)
		e.restoreAfter("first", 2, 1, func(path string) {
			writeFile(t, filepath.Join(path, "p.txt"), "edited before the call\n")
		})
		res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const second = await agent.call('two', { agentType: 'executor', node: 'n1', label: 'second' })
return [first.status, second.status]`)
		assert(t, e, res, engine.EvPrepared, 1, 1)
	})
	t.Run("unchanged", func(t *testing.T) {
		e := newEnv(t)
		e.restoreAfter("first", 2, 1)
		res := e.run(`const first = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'first' })
const second = await agent.call('two', { resume: first, label: 'second' })
return [first.status, second.status]`)
		if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
			t.Fatalf("%v", got)
		}
		if len(e.flowRows("escalation")) != 0 {
			t.Errorf("an unchanged restore escalates: %+v", e.flowRows("escalation"))
		}
	})
}

// An executor turn never carries a cwd - not the first turn, a repair, a fix continuation or the turn that resumes a held
// call, since a turn cwd would move the thread out of its worktree - and an architect turn carries its call's directory.
func TestExecutorTurnStartNeverCarriesCwd(t *testing.T) {
	e := newEnv(t)
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		switch {
		case strings.Contains(r.Text, "HELD") && r.N == 1:
			return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "busy", CodexErrorInfo: "serverOverloaded"}}
		case strings.Contains(r.Text, "FIRST") && r.N == 1:
			return fakehost.TurnPlan{Message: "not json"}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	res := e.run(`const S = ` + verdictSchema + `
const a = await agent.call('FIRST build', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
const fix = await agent.call('fix it', { resume: a, label: 'fix' })
const held = await agent.call('HELD build', { agentType: 'executor', node: 'n2', label: 'held' })
const resumed = await agent.call('try again', { resume: held, label: 'resumed' })
const arch = await agent.call('plan it', { agentType: 'architect', label: 'arch' })
const arch2 = await agent.call('replan it', { resume: arch, label: 'arch2' })
return [a.status, fix.status, held.status, resumed.status, arch.status, arch2.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed", "held", "completed", "completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	by := calls(res)
	role := map[string]string{}
	for _, c := range res.Calls {
		role[c.Meta.ThreadID] = c.Meta.Role
	}
	executorTurns, architectTurns := 0, 0
	archCwd := filepath.Join(e.state, "flow", e.runID, "calls", by["arch"].CallID, "wt")
	for _, turn := range e.requests("turn/start") {
		cwd, has := turn.Params["cwd"]
		switch role[str(turn.Params, "threadId")] {
		case "executor":
			executorTurns++
			if has {
				t.Errorf("an executor turn carries cwd %v: %s", cwd, firstLine(inputText(t, turn)))
			}
		case "architect":
			architectTurns++
			if !has || cwd != archCwd {
				t.Errorf("an architect turn carries cwd %v (present %v), want %s", cwd, has, archCwd)
			}
		default:
			t.Errorf("turn on an unknown thread %v", turn.Params["threadId"])
		}
	}
	// first turn + repair + fix, the held call's turn + the turn that resumed it; the architect's two turns
	if executorTurns != 5 || architectTurns != 2 {
		t.Errorf("%d executor turns and %d architect turns", executorTurns, architectTurns)
	}
}
