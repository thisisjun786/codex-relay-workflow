package ops

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1331 (CRW-1316 §6.3): an executor subagent of a session call that committed in its worktree moved HEAD. The engine keeps
// that commit on the attempt's stray ref and records executor_head_moved on the completed row; crw.commit then commits the pending
// tree on the recorded start commit C with the observed HEAD as the compare-and-swap's old value, and the proof of the node takes
// the stray HEAD whose tree is the pending tree as proven.

// strayCommit makes the commit an executor subagent would make in the worktree and returns it.
func (f *fx) strayCommit() string {
	f.t.Helper()
	gitIn(f.t, f.wt, "add", "-A")
	gitIn(f.t, f.wt, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the subagent committed")
	return gitIn(f.t, f.wt, "rev-parse", "HEAD")
}

// completeMoved ends the call completed with its pending tree and the executor_head_moved record of a session call.
func (f *fx) completeMoved(call, start, head string) string {
	p := f.snapshot()
	f.add(engine.Event{Kind: engine.KindCall, CallID: call, Op: "agent", Event: engine.EvCompleted, Data: map[string]any{"pending_tree": p,
		"executor_head_moved": map[string]any{"start": start, "head": head, "ref": engine.StrayRef("run-1", f.node, 1, call)}}})
	return p
}

func TestCommitOverAStrayHeadKeepsTheRecordedParent(t *testing.T) {
	f := newFx(t)
	f.edit("a.txt", "built\n")
	stray := f.strayCommit()
	snap := f.snapshot()
	res, err := Commit(ctx, ExecGit("git"), f.wt, CommitRequest{Tree: snap, Parent: f.start, Expect: stray, Message: "crw.commit"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitIn(t, f.wt, "rev-parse", res.Commit+"^"); got != f.start || res.Parent != f.start {
		t.Errorf("the commit's parent is %s (result %+v), want the recorded start %s", got, res, f.start)
	}
	if got := gitIn(t, f.wt, "rev-parse", "HEAD"); got != res.Commit {
		t.Errorf("HEAD %s, commit %s", got, res.Commit)
	}
	// the compare-and-swap is against the HEAD it observed: a HEAD that moved again is refused and nothing moves
	f.edit("src/c.txt", "more\n")
	again := f.strayCommit()
	_, err = Commit(ctx, ExecGit("git"), f.wt, CommitRequest{Tree: f.snapshot(), Parent: res.Commit, Expect: stray, Message: "again"})
	if !errors.Is(err, ErrHeadMoved) {
		t.Errorf("a commit whose expected HEAD is stale: %v", err)
	}
	if got := gitIn(t, f.wt, "rev-parse", "HEAD"); got != again {
		t.Errorf("a refused commit moved HEAD to %s", got)
	}
}

func TestStrayHeadWithThePendingTreeIsProven(t *testing.T) {
	t.Run("recorded stray whose tree is the pending tree", func(t *testing.T) {
		f := newFx(t)
		f.edit("a.txt", "built\n")
		stray := f.strayCommit()
		f.completeMoved("t1", f.start, stray)
		np := f.prove()
		expectProven(t, np, map[string]bool{"t1": true})
		if np.Foreign != "" {
			t.Errorf("foreign %q: %s", np.Foreign, np.Describe())
		}
	})
	t.Run("the same commit without the record", func(t *testing.T) {
		f := newFx(t)
		f.edit("a.txt", "built\n")
		f.strayCommit()
		f.complete("t1")
		np := f.prove()
		if np.Foreign != "commit" || np.Proven("t1") {
			t.Errorf("an unrecorded commit is foreign: %+v", np)
		}
	})
	t.Run("a stray whose tree is not the pending tree", func(t *testing.T) {
		f := newFx(t)
		f.edit("a.txt", "built\n")
		stray := f.strayCommit()
		f.edit("src/c.txt", "and an edit it did not commit\n")
		f.completeMoved("t1", f.start, stray)
		np := f.prove()
		if np.Foreign != "commit" || np.Proven("t1") {
			t.Errorf("a stray commit of another tree is not the call's state: %+v", np)
		}
	})
}

func TestCrwCommitOverAStrayHead(t *testing.T) {
	f := newFx(t)
	f.edit("a.txt", "built\n")
	stray := f.strayCommit()
	p := f.completeMoved("t1", f.start, stray)
	j := engine.NewMemoryJournal()
	for _, e := range f.events {
		j.Append(e)
	}
	lane := NewLane(Deps{RunID: "run-1", Repo: f.repo, Journal: j})
	ref, _ := json.Marshal(map[string]any{"callId": "t1"})
	args, _ := json.Marshal([]json.RawMessage{json.RawMessage(`"n1"`), ref})
	out, err := lane.Ops()[NameCommit].Run(ctx, engine.OpRequest{RunID: "run-1", CallID: "k1", RequestID: "req-k1", Args: args, Journal: j})
	if err != nil {
		t.Fatal(err)
	}
	commit, parent := out.Data["commit"].(string), out.Data["parent"].(string)
	if parent != f.start || gitIn(t, f.wt, "rev-parse", commit+"^") != f.start || gitIn(t, f.wt, "rev-parse", commit+"^{tree}") != p {
		t.Errorf("crw.commit made %s on %s; want the pending tree %s on the recorded start %s", commit, parent, p, f.start)
	}
	if gitIn(t, f.wt, "rev-parse", "HEAD") != commit {
		t.Errorf("HEAD is not the new commit")
	}
	if out.Data["stray_head"] != stray {
		t.Errorf("the commit row does not name the stray HEAD it replaced: %v", out.Data)
	}
	// the node is proven on the recorded chain afterwards
	f.add(engine.Event{Kind: engine.KindCall, CallID: "k1", Op: OpCommit, Event: engine.EvCompleted, Data: out.Data})
	expectProven(t, f.prove(), map[string]bool{"t1": true})
}
