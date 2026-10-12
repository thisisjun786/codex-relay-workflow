package agent_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The resume scenarios of F07 that are about the fork request itself: a first process runs the build; a second process, with
// a script that continues it, runs the continuation inside a suffix, on a fork of the build's thread into the restored worktree.

const forkBuild = `const S = ` + verdictSchema + `
const first = await agent.call('one', { agentType: 'executor', node: 'n1', schema: S, label: 'first' })
`

const forkBuildOnly = forkBuild + `return [first.status]`

const forkContinued = forkBuild + `
const second = await agent.call('two', { resume: first, schema: S, label: 'second' })
return [first.status, second.status]`

// The fork request is the 0.154 shape: the through turn as lastTurnId, the restored worktree as cwd, the pair of the role,
// ephemeral false and excludeTurns true - and nothing asks for the history to be hydrated: no request of the host carries
// includeTurns true, and thread/rollback is never used.
func TestContinuationForksAtPrefixTurn(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.run(forkBuildOnly)
	thread := str(e.row("first", engine.EvThreadStarted).Data, "thread_id")
	turn := str(e.row("first", engine.EvTurnStarted).Data, "turn_id")
	e.restart("proc-2")
	res := e.resumeRun(forkContinued)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	forks := e.requests("thread/fork")
	if len(forks) != 1 {
		t.Fatalf("%d thread/fork requests", len(forks))
	}
	p := forks[0].Params
	restored := e.wt("n1", 2, 1)
	if p["threadId"] != thread || p["lastTurnId"] != turn || p["cwd"] != restored || p["ephemeral"] != false || p["excludeTurns"] != true {
		t.Errorf("thread/fork %v", p)
	}
	if cfg, _ := p["config"].(map[string]any); p["model"] != executorModel || cfg["model_reasoning_effort"] != executorEffort {
		t.Errorf("the fork repeats the role's pair: %v", p)
	}
	for _, r := range e.host.Requests() {
		var params map[string]any
		_ = json.Unmarshal(r.Params, &params)
		if params["includeTurns"] == true {
			t.Errorf("%s asked for the history to be hydrated: %v", r.Method, params)
		}
		if strings.Contains(r.Method, "rollback") {
			t.Errorf("%s was sent", r.Method)
		}
	}
	// the history of the fork is read with thread/turns/list, in full
	lists := e.requests("thread/turns/list")
	if len(lists) == 0 || lists[len(lists)-1].Params["itemsView"] != "full" {
		t.Errorf("thread/turns/list %v", lists)
	}
}

// The boundary of a fork is checked against the history it got, read page after page: the last turn is the through turn. A host
// that kept the turns after it (or a fork anywhere else) is refused, and the fork is archived.
func TestForkBoundaryCheckedByPaging(t *testing.T) {
	scenario := `const S = ` + verdictSchema + `
const first = await agent.call('one', { agentType: 'executor', node: 'n1', schema: S, label: 'first' })
const mid = await agent.call('two', { resume: first, schema: S, label: 'mid' })
`
	firstScript := scenario + `return [first.status, mid.status]`
	// the third call continues the first call's turn, which is not the last turn of the thread: the fork has to stop there
	repairFirst := func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.N == 1 {
			return fakehost.TurnPlan{Message: "not json"}
		}
		return fakehost.TurnPlan{Message: passMessage}
	}
	secondScript := scenario + `const third = await agent.call('three', { resume: first, schema: S, label: 'third' })
return [first.status, mid.status, third.status]`
	t.Run("the history of the fork takes several pages", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.plan(repairFirst)
		e.threads.TurnsPageSize(1)
		e.run(firstScript)
		e.restart("proc-2")
		res := e.resumeRun(secondScript)
		by := calls(res)
		if by["third"].Status != engine.StatusCompleted {
			t.Fatalf("third: %s; rows %v", callStatus(by["third"]), kinds(e.rows("third")))
		}
		forkID := by["third"].Meta.ThreadID
		var pages, cursors int
		for _, r := range e.requests("thread/turns/list") {
			if r.Params["threadId"] == forkID {
				pages++
				if r.Params["cursor"] != nil {
					cursors++
				}
			}
		}
		if pages < 2 || cursors < 1 {
			t.Errorf("the fork's history was read in %d requests, %d of them with a cursor: it takes two pages", pages, cursors)
		}
		// the fork holds the first call's turns and stops there: the later turn of mid is not in it
		view, _ := e.threads.Thread(forkID)
		through := by["first"].Meta.TurnIDs
		if len(through) != 2 || len(view.Turns) != 3 || view.Turns[1].ID != through[1] {
			t.Errorf("the fork's turns %+v, the first call's %v", view.Turns, through)
		}
	})
	t.Run("a host that keeps the turns after the boundary", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.plan(repairFirst)
		e.threads.TurnsPageSize(1)
		e.run(firstScript)
		e.restart("proc-2")
		e.threads.ForkKeepsAllTurns(true)
		res := e.resumeRun(secondScript)
		third := calls(res)["third"]
		if third.Status != engine.StatusRefused {
			t.Fatalf("third: %s, want refused; rows %v", callStatus(third), kinds(e.rows("third")))
		}
		if n := e.count("thread/fork"); n != 1 {
			t.Errorf("%d thread/fork requests", n)
		}
		// nothing was sent to the fork: the call ended before its first turn, and the fork is archived
		if slices.Contains(kinds(e.rows("third")), engine.EvForkStarted) || slices.Contains(kinds(e.rows("third")), engine.EvTurnRequested) {
			t.Errorf("rows %v", kinds(e.rows("third")))
		}
		if got := e.threads.Archived(); len(got) != 1 {
			t.Errorf("archived %v: the fork that is not the boundary must be archived", got)
		}
		if reason := e.row("third", engine.EvRefused).Data["detail"]; !strings.Contains(reason.(string), "not the boundary") {
			t.Errorf("refusal detail %v", reason)
		}
	})
}

// A thread/fork whose answer was lost in a suffix made a fork with the copied prefix at the restored worktree: the next process
// finds it by the cwd and Thread.forkedFromId, sees the history is exactly the recorded prefix and sends the call's first turn
// to it. thread/fork is requested once.
func TestForkAckLostFoundByCwdAndForkedFrom(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.run(forkBuildOnly)
	source := str(e.row("first", engine.EvThreadStarted).Data, "thread_id")
	turn := str(e.row("first", engine.EvTurnStarted).Data, "turn_id")
	e.restart("proc-2")
	e.lostAck("thread/fork", 1)
	restored := e.wt("n1", 2, 1)
	first := e.resumeRun(forkContinued)
	if got := calls(first)["second"]; got.Status != engine.StatusUnknown {
		t.Fatalf("second in the process that lost the answer: %s", callStatus(got))
	}
	forks := e.forkThreads(source)
	if len(forks) != 1 || len(forks[0].Turns) != 1 || forks[0].Cwd != restored || forks[0].Turns[0].ID != turn {
		t.Fatalf("the host holds %+v after the lost answer", forks)
	}
	if prefix, _ := e.row("second", engine.EvForkRequested).Data["prefix"].([]map[string]any); len(prefix) != 1 || prefix[0]["turn_id"] != turn {
		t.Fatalf("the recorded prefix %v", e.row("second", engine.EvForkRequested).Data["prefix"])
	}
	e.restart("proc-3")
	res := e.resumeRun(forkContinued)
	second := calls(res)["second"]
	if second.Status != engine.StatusCompleted || second.Meta.ThreadID != forks[0].ID {
		t.Fatalf("second: %s on %s, want completed on the found fork %s; rows %v", callStatus(second), second.Meta.ThreadID, forks[0].ID, kinds(e.rows("second")))
	}
	if n := e.count("thread/fork"); n != 1 {
		t.Errorf("%d thread/fork requests: the fork was found, not made again", n)
	}
	if n := e.markerCount(second.CallID + "/t1"); n != 1 {
		t.Errorf("%d turns carried the call's operation", n)
	}
	if started := e.row("second", engine.EvForkStarted).Data; started["recovered"] != true || started["thread_id"] != forks[0].ID || started["cwd"] != restored {
		t.Errorf("fork_started %v", started)
	}
	if len(e.forkThreads(source)) != 1 {
		t.Errorf("the host holds %d forks of the build thread", len(e.forkThreads(source)))
	}
}
