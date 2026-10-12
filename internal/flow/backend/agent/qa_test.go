package agent_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
)

// The QA capture call of a milestone's final gate (SP-QA-1, CRW-1206) is an executor call marked qa: true. The backend gives it
// the QA overlay, a scratch worktree at the head the call names (not a node's worktree) that keeps no pending tree, and a line of
// the flow binding that allows no native helper: the hook then refuses the oracle passes the run starts itself.

// The thread of a qa call gets the QA overlay, the call runs in a call-private worktree at the head it names, and what it edits
// there is discarded: the completed row records no pending tree.
func TestQACallScratchWorktreeOverlayAndNoPendingTree(t *testing.T) {
	e := newEnv(t, withBound(nil))
	// the head the gate pinned: a commit that is not the base the node allocation would start from
	if err := os.WriteFile(filepath.Join(e.repo, "gate.txt"), []byte("integrated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, e.repo, "add", "-A")
	git(t, e.repo, "commit", "-q", "-m", "integrated")
	head := git(t, e.repo, "rev-parse", "HEAD")
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		// the capture drives the surface and leaves files behind in its scratch worktree
		if cwd := e.scratchOf(r.Text); cwd != "" {
			_ = os.WriteFile(filepath.Join(cwd, "capture.png"), []byte("png"), 0o644)
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	res := e.run(`const S = ` + verdictSchema + `
const q = await agent.call('capture', { agentType: 'executor', qa: true, head: ` + q(head) + `, schema: S, label: 'qa' })
const s = await agent.call('synthesise', { resume: q, schema: S, label: 'qa-synthesis' })
return [q.status, s.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	start := e.requests("thread/start")[0].Params
	overlay, err := roles.Prompt("executor", roles.Options{QA: true})
	if err != nil {
		t.Fatal(err)
	}
	if dev, _ := start["developerInstructions"].(string); dev != overlay {
		t.Errorf("the thread of the qa call does not carry the QA overlay")
	}
	prep := e.row("qa", engine.EvPrepared)
	alloc, _ := prep.Data["allocation"].(map[string]any)
	if alloc["kind"] != engine.PlaceCall || alloc["start_commit"] != head {
		t.Fatalf("allocation %v, want a call-private worktree at %s", alloc, head)
	}
	path, _ := alloc["path"].(string)
	if !strings.Contains(path, "/calls/") || strings.Contains(path, "/wt/") && !strings.Contains(path, "/calls/") {
		t.Errorf("the scratch worktree is %s, want a call-private directory", path)
	}
	if got := git(t, path, "rev-parse", "HEAD"); got != head {
		t.Errorf("the scratch worktree is at %s, want %s", got, head)
	}
	for _, label := range []string{"qa", "qa-synthesis"} {
		done := e.row(label, engine.EvCompleted)
		if pt, present := done.Data["pending_tree"]; present {
			t.Errorf("%s recorded the pending tree %v: the edits of a qa call are discarded", label, pt)
		}
	}
	// no worktree of a node was made for it
	for _, c := range e.gitCalls() {
		if len(c.Args) > 2 && c.Args[0] == "worktree" && c.Args[1] == "add" && strings.Contains(strings.Join(c.Args, " "), "/wt/call-") {
			t.Errorf("a node worktree was made for the qa call: %v", c.Args)
		}
	}
}

// The binding line of every turn of a qa thread - the capture turn and the synthesis turn that continues it - allows no native
// helper, while the line of a build call of the same run allows one.
func TestQACallBindingAllowsNoNativeHelper(t *testing.T) {
	e := newEnv(t, withBound(nil))
	res := e.run(`const S = ` + verdictSchema + `
const q = await agent.call('capture', { agentType: 'executor', qa: true, head: ` + q(e.base) + `, schema: S, label: 'qa' })
const s = await agent.call('synthesise', { resume: q, schema: S, label: 'qa-synthesis' })
const b = await agent.call('build', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
return [q.status, s.status, b.status]`)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	by := calls(res)
	qa := boundLines(t, by["qa"].Meta.ThreadID)
	if len(qa) != 2 {
		t.Fatalf("%d binding lines on the qa thread, want one for each turn: %+v", len(qa), qa)
	}
	for i, l := range qa {
		if l.NativeAllowance == nil || *l.NativeAllowance != 0 || l.Role != "executor" {
			t.Errorf("line %d of the qa thread: %+v", i, l)
		}
	}
	if qa[0].CallID != by["qa"].CallID || qa[1].CallID != by["qa-synthesis"].CallID {
		t.Errorf("the lines name the calls %s and %s", qa[0].CallID, qa[1].CallID)
	}
	build := boundLines(t, by["build"].Meta.ThreadID)
	if len(build) != 1 || build[0].Allowance() != 1 {
		t.Errorf("the build thread: %+v", build)
	}
}

// scratchOf is the directory the call of a turn runs in, from the allocation its prepared row names (the thread's own cwd is
// asked of the host, which the plan of a turn cannot do while it holds the thread).
func (e *env) scratchOf(turnText string) string {
	line, _, _ := strings.Cut(turnText, "\n")
	op := strings.TrimPrefix(line, "crw-flow-op: ")
	callID, _, _ := strings.Cut(op, "/")
	for _, ev := range e.journal.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && ev.CallID == callID {
			if alloc, ok := ev.Data["allocation"].(map[string]any); ok {
				return str(alloc, "path")
			}
		}
	}
	return ""
}
