package agent_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1202 (F09): a turn of the architect after its DESIGN that names the head it judges (the REFLECTION at the integrated head, the
// re-plan at the tip) runs in a detached worktree of its own at that head, which the turn names as its cwd. The worktree is removed
// when the call has its terminal row, and a resume that runs the call again - a held reflection that goes on - makes it again at the
// head the call's row records; the path belongs to the call, so no other call's directory overlaps it.

const architectCwdScript = `const arch = await agent.call('plan it', { agentType: 'architect', label: 'arch' })
const refl = await agent.call('reflect on it', { resume: arch, head: 'HEAD_H', label: 'reflect' })
const other = await agent.call('plan again', { agentType: 'architect', label: 'other' })
return [arch.status, refl.status, other.status]`

func TestArchitectTurnWorktreeRecreatedOnResume(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	// H is a commit past the base the DESIGN ran at: the integrated head
	writeFile(t, filepath.Join(e.repo, "a.txt"), "two\n")
	git(t, e.repo, "commit", "-q", "-a", "-m", "integrated")
	h := git(t, e.repo, "rev-parse", "HEAD")
	script := strings.ReplaceAll(architectCwdScript, "HEAD_H", h)

	var mu sync.Mutex
	asked := 0
	headsSeen := map[string]string{}
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		mu.Lock()
		defer mu.Unlock()
		if cwd, _ := r.Params["cwd"].(string); cwd != "" {
			out, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").Output()
			if err != nil {
				headsSeen[r.ClientID] = "error: " + err.Error()
			} else {
				headsSeen[r.ClientID] = strings.TrimSpace(string(out))
			}
		}
		if strings.Contains(r.Text, "reflect on it") {
			asked++
			if asked == 1 {
				return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "busy", CodexErrorInfo: "serverOverloaded"}}
			}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	res := e.run(script)
	if got := value[[]string](t, res); strings.Join(got, ",") != "completed,held,completed" {
		t.Fatalf("the first run: %v", got)
	}
	by := calls(res)
	archDir := filepath.Join(e.state, "flow", e.runID, "calls", by["arch"].CallID, "wt")
	reflectDir := filepath.Join(e.state, "flow", e.runID, "calls", by["reflect"].CallID, "wt")
	otherDir := filepath.Join(e.state, "flow", e.runID, "calls", by["other"].CallID, "wt")
	if reflectDir == archDir || reflectDir == otherDir {
		t.Fatalf("the directories of the calls overlap: %s %s %s", archDir, reflectDir, otherDir)
	}
	turns := e.requests("turn/start")
	var reflectTurns []request
	for _, tr := range turns {
		if strings.Contains(inputText(t, tr), "reflect on it") {
			reflectTurns = append(reflectTurns, tr)
		}
	}
	if len(reflectTurns) != 1 || reflectTurns[0].Params["cwd"] != reflectDir {
		t.Fatalf("the reflection turn named cwd %v, want the worktree of its call, %s (%d turns)", reflectTurns, reflectDir, len(reflectTurns))
	}
	if prepared := e.row("reflect", engine.EvPrepared); str(prepared.Data, "head") != h || str(allocationOf(t, prepared), "path") != reflectDir || str(allocationOf(t, prepared), "start_commit") != h {
		t.Errorf("the prepared row of the reflection: head %v, allocation %v", prepared.Data["head"], prepared.Data["allocation"])
	}
	mu.Lock()
	if head := headsSeen[by["reflect"].CallID+"/t1"]; head != h {
		t.Errorf("the reflection ran in a worktree at %q, want the integrated head %s", head, h)
	}
	mu.Unlock()
	// the call has its terminal row (held): its worktree is gone
	if _, err := os.Stat(reflectDir); err == nil {
		t.Errorf("the worktree of the held call still exists: %s", reflectDir)
	}
	// the architect's other calls kept the directories they started in
	if _, err := os.Stat(archDir); err != nil {
		t.Errorf("the worktree of the DESIGN call is gone: %v", err)
	}

	// the cause is gone: the resume goes on with the held call in place, as the next turn of its thread, in a worktree made again at H
	e.restart("proc-2")
	e.useClock()
	adds := e.worktreeAdds()
	res2 := e.resumeRun(script)
	if got := value[[]string](t, res2); strings.Join(got, ",") != "completed,completed,completed" {
		t.Fatalf("the resume: %v", got)
	}
	// the held reflection's worktree, made again, and the one of the call after it, which the held call's suffix runs again
	if n := e.worktreeAdds() - adds; n != 2 {
		t.Errorf("%d worktrees were added by the resume, want those of the reflection and of the call after it", n)
	}
	var again []request
	for _, tr := range e.requests("turn/start") {
		if strings.Contains(inputText(t, tr), "previous turn stopped") {
			again = append(again, tr)
		}
	}
	if len(again) != 1 || again[0].Params["cwd"] != reflectDir {
		t.Fatalf("the turn that went on named %v, want cwd %s again", again, reflectDir)
	}
	mu.Lock()
	if head := headsSeen[by["reflect"].CallID+"/t2"]; head != h {
		t.Errorf("the turn that went on ran in a worktree at %q, want the head the call's row records, %s", head, h)
	}
	mu.Unlock()
	if _, err := os.Stat(reflectDir); err == nil {
		t.Errorf("the worktree of the call that ended is still there: %s", reflectDir)
	}
	if rows := e.rows("other"); len(rows) == 0 {
		t.Errorf("the call after the reflection did not run")
	}
	// the call's directory is never shared: the call after it, run again, has one of its own
	if _, err := os.Stat(filepath.Join(filepath.Dir(otherDir), "wt-1")); err != nil {
		t.Errorf("the worktree of the call after the reflection, made again: %v", err)
	}
}

// The path of a directory the engine asks the allocator to remove comes from the journal (the terminal row that names it): only a
// call-private directory of the run is removed, never another path a row could name (CRW-1202, the cleanup a resume makes again).
func TestRemoveDirTakesOnlyACallDirectoryOfTheRun(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	var gitCalls int
	alloc, err := agent.NewAllocator(agent.AllocatorConfig{Repo: root, StateDir: state, RunID: "run-x",
		Base: func(string) (string, error) { return "", nil },
		Git: func(context.Context, string, []string, ...string) (string, error) {
			gitCalls++
			return "", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "repo")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, filepath.Join(state, "flow", "run-y", "calls", "c1", "wt"), filepath.Join(state, "flow", "run-x", "calls", "..", "wt", "A")} {
		if err := alloc.RemoveDir(context.Background(), path); err == nil {
			t.Errorf("RemoveDir(%s) was accepted", path)
		}
	}
	own := alloc.CallPath("c1")
	if err := alloc.RemoveDir(context.Background(), own); err != nil || gitCalls != 0 {
		t.Errorf("a call directory that is not there: err=%v git calls=%d", err, gitCalls)
	}
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := alloc.RemoveDir(context.Background(), own); err != nil || gitCalls != 1 {
		t.Errorf("the call directory of the run: err=%v git calls=%d", err, gitCalls)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the path outside is gone: %v", err)
	}
}
