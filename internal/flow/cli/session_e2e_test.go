package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/role/spawn"
)

// CRW-1348 (CRW-1316 S3): a session run end to end on the fake App Server, through the commands the owner thread runs. `crw flow
// start` (the default backend, session) binds the owner thread and starts the run process (ProductionWorld: the session host
// adapter, the run's FileStepBus, the owner); the owner takes each step with `crw flow next`, runs the spawn hook and spawns the
// subagent on the host, and reports it with `crw flow dispatched`; the run binds, harvests and writes the completed row. The run
// process dies after the first call's completed row, and the next `crw flow next` resumes it; it dies again after the third call's
// subagent was spawned and before it was reported, and the resumed run finds that subagent from its step_published row through the
// hook's ledger and the owner turn's spawn item. The relay acceptance's read of a call's pair is the newest thread_started row.

type sessionE2E struct {
	t     *testing.T
	w     *world
	td    *testDeps
	host  *fakehost.Server
	repo  string
	owner string

	mu       sync.Mutex
	cur      *dyingRunJournal
	cancel   context.CancelFunc
	done     chan int
	starts   []SpawnRequest
	tools    int
	killWhen func(engine.Event) bool
}

// dyingRunJournal is the run's journal as a process that dies sees it: after the death it stores nothing.
type dyingRunJournal struct {
	engine.Journal
	mu   sync.Mutex
	dead bool
	stop func(engine.Event) bool
	kill func()
}

func (j *dyingRunJournal) Append(e engine.Event) engine.Event {
	j.mu.Lock()
	dead := j.dead
	j.mu.Unlock()
	if dead {
		return e
	}
	out := j.Journal.Append(e)
	if j.stop != nil && j.stop(out) {
		j.kill()
	}
	return out
}

func (j *dyingRunJournal) SetOnCommit(notify func()) {
	if n, ok := j.Journal.(engine.CommitNotifier); ok {
		n.SetOnCommit(notify)
	}
}

func (j *dyingRunJournal) Err() error {
	if f, ok := j.Journal.(engine.FailingJournal); ok {
		return f.Err()
	}
	return nil
}

func newSessionE2E(t *testing.T) *sessionE2E {
	t.Helper()
	isolateGit(t)
	k := &sessionE2E{t: t, w: newWorld(t)}
	k.w.close() // the process opens the store for writing
	k.repo = filepath.Join(t.TempDir(), "repo")
	for _, args := range [][]string{{"init", "-q", "-b", "main", k.repo}, {"-C", k.repo, "commit", "-q", "--allow-empty", "-m", "base"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// the run is started in the repository (ProductionWorld's OwnerCwd is the process cwd): the evidence records of its executor steps
	// go under the test's repository, never under the source tree (CRW-1482)
	t.Chdir(k.repo)
	k.host = fakehost.Start(t)
	k.host.Threads().ThreadIDs(func(n int) string { return fmt.Sprintf("01a20000-0000-7000-8000-%012x", n) })
	k.host.Threads().OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: "owner"} })
	// the owner thread: the project orchestrator, idle after a turn of its own (its spawn items go into its last turn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := appserver.Dial(ctx, k.host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := client.Call(ctx, "thread/start", map[string]any{"cwd": k.repo, "model": "owner-model", "sandbox": "danger-full-access", "approvalPolicy": "never", "ephemeral": false})
	if err != nil {
		t.Fatal(err)
	}
	var started struct{ Thread struct{ ID string } }
	_ = json.Unmarshal(raw, &started)
	k.owner = started.Thread.ID
	if _, err := client.Call(ctx, "turn/start", map[string]any{"threadId": k.owner, "input": []any{map[string]any{"type": "text", "text": "run the project"}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the owner's turn to end", func() bool {
		v, ok := k.host.Threads().Thread(k.owner)
		return ok && len(v.Turns) == 1 && v.Turns[0].Status == "completed"
	})

	k.td = k.w.deps()
	k.td.Probe = ProberFunc(func(ctx context.Context, req PreflightRequest) []Fact { return k.td.facts })
	k.td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		k.mu.Lock()
		k.starts = append(k.starts, req)
		k.mu.Unlock()
		k.process(req.Spec.RunID, req.Resume)
		return os.Getpid(), nil
	}
	k.td.Ready = AwaitLock
	k.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		w, err := ProductionWorld(ctx, req)
		if err != nil {
			return World{}, err
		}
		if w.Session == nil {
			return World{}, fmt.Errorf("the run is not a session run")
		}
		w.Session.Poll, w.Session.WakeAfter = 200*time.Millisecond, time.Hour
		k.mu.Lock()
		j := &dyingRunJournal{Journal: w.Journal, stop: k.killWhen}
		j.kill = func() { k.kill() }
		k.cur = j
		k.mu.Unlock()
		w.Journal = j
		return w, nil
	}
	return k
}

// process runs `crw flow _run` in this process, as SpawnDetached starts it.
func (k *sessionE2E) process(run string, resume bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	argv := []string{ProcessCommand, "--state", k.w.state, "--socket", k.host.SocketPath}
	if resume {
		argv = append(argv, "--resume")
	}
	argv = append(argv, run)
	done := make(chan int, 1)
	k.mu.Lock()
	k.cancel, k.done = cancel, done
	k.mu.Unlock()
	go func() {
		defer cancel()
		var errOut bytes.Buffer
		code := RunWith(ctx, k.td.Deps, argv, strings.NewReader(""), &bytes.Buffer{}, &errOut)
		if code != 0 && ctx.Err() == nil {
			k.t.Logf("process %s (resume %v): exit %d: %s", run, resume, code, errOut.String())
		}
		done <- code
	}()
}

// kill ends the current process the way a crash does: it stores no more rows and its context ends.
func (k *sessionE2E) kill() {
	k.mu.Lock()
	j, cancel := k.cur, k.cancel
	k.killWhen = nil
	k.mu.Unlock()
	if j != nil {
		j.mu.Lock()
		j.dead = true
		j.stop = nil
		j.mu.Unlock()
	}
	if cancel != nil {
		cancel()
	}
}

// waitProcess waits until the current process has ended.
func (k *sessionE2E) waitProcess() {
	k.t.Helper()
	k.mu.Lock()
	done := k.done
	k.mu.Unlock()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		k.t.Fatal("the run process did not end")
	}
}

func (k *sessionE2E) cmd(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := RunWith(context.Background(), k.td.Deps, append(args, "--state", k.w.state, "--socket", k.host.SocketPath), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func (k *sessionE2E) next(run string, wait int) NextAnswer {
	k.t.Helper()
	code, out, errOut := k.cmd("next", run, "--wait", strconv.Itoa(wait), "--json")
	if code != 0 {
		k.t.Fatalf("next: exit %d\n%s\n%s", code, out, errOut)
	}
	var ans NextAnswer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		k.t.Fatalf("next: %v\n%s", err, out)
	}
	return ans
}

// nextSpawn waits until next hands out a spawn step.
func (k *sessionE2E) nextSpawn(run string) (NextAnswer, NextStep) {
	k.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		ans := k.next(run, 10)
		for _, st := range ans.Steps {
			if st.Action == engine.StepSpawn {
				return ans, st
			}
		}
	}
	k.t.Fatal("next handed out no spawn step")
	return NextAnswer{}, NextStep{}
}

// spawnStep is what the owner does for a spawn step: the spawn hook decides it (the ledger records the role and the op), and the host
// spawns the subagent with the pair the hook put in.
func (k *sessionE2E) spawnStep(st NextStep, answer string) string {
	k.t.Helper()
	k.mu.Lock()
	k.tools++
	toolUse := fmt.Sprintf("call-tool-%d", k.tools)
	k.mu.Unlock()
	input, _ := json.Marshal(st.Tool.Arguments)
	payload := `{"hook_event_name":"PreToolUse","tool_name":"spawn_agent","session_id":` + strconv.Quote(k.owner) + `,"tool_use_id":` + strconv.Quote(toolUse) +
		`,"cwd":` + strconv.Quote(k.repo) + `,"tool_input":` + string(input) + `}`
	out := spawn.RunSpawnAttachHook(payload, func(string) (string, bool) { return "", false })
	var hook struct {
		Out struct {
			Decision string         `json:"permissionDecision"`
			Updated  map[string]any `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &hook); err != nil || hook.Out.Decision != "allow" {
		k.t.Fatalf("the hook did not allow the step's spawn: %q", out)
	}
	model, _ := hook.Out.Updated["model"].(string)
	effort, _ := hook.Out.Updated["reasoning_effort"].(string)
	agentType, _ := hook.Out.Updated["agent_type"].(string)
	return k.host.Threads().Spawn(fakehost.SubagentSpec{Parent: k.owner, ToolUseID: toolUse, AgentType: agentType, Model: model, Effort: effort,
		Message: st.Message, Plan: fakehost.TurnPlan{Message: answer}})
}

func (k *sessionE2E) dispatched(run string, st NextStep, agent string) map[string]any {
	k.t.Helper()
	code, out, errOut := k.cmd("dispatched", run, st.Call, "--agent", agent, "--k", strconv.Itoa(st.K), "--attempt", strconv.Itoa(st.Attempt), "--wait", "60", "--json")
	var ans map[string]any
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		k.t.Fatalf("dispatched: exit %d %v\n%s\n%s", code, err, out, errOut)
	}
	if code != 0 || ans["status"] != engine.ReplyBound {
		k.t.Fatalf("dispatched: exit %d %v", code, ans)
	}
	return ans
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (k *sessionE2E) readStore(f func(st *store.Store)) {
	k.t.Helper()
	st, err := store.OpenReadOnlyStore(context.Background(), k.w.db)
	if err != nil {
		k.t.Fatal(err)
	}
	defer st.Close()
	f(st)
}

func TestSessionRunEndToEndOnTheFakeHost(t *testing.T) {
	k := newSessionE2E(t)
	script := filepath.Join(t.TempDir(), "three.js")
	if err := os.WriteFile(script, []byte(`export const meta = { name: 'three', phases: ['one'] }
phase('one')
const a = await agent.call('look at a', { agentType: 'explorer', label: 'a' })
const b = await agent.call('look at b', { agentType: 'explorer', label: 'b' })
const c = await agent.call('look at c', { agentType: 'explorer', label: 'c' })
return [a.status, b.status, c.status, a.output, b.output, c.output]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// the first process dies right after the completed row of the first call
	k.killWhen = func(e engine.Event) bool {
		return e.Kind == engine.KindCall && e.Label == "a" && e.Event == engine.EvCompleted
	}

	code, out, errOut := k.cmd("start", script, "--thread", k.owner, "--repo", k.repo, "--json")
	if code != 0 {
		t.Fatalf("start: exit %d\n%s\n%s", code, out, errOut)
	}
	var started StartAnswer
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatal(err)
	}
	run := started.Run
	if started.Plan.Backend != BackendSession || !started.Bound || !strings.Contains(started.Next, "crw flow next "+run) || len(started.Plan.Spawns) != 6 {
		t.Fatalf("the start: %+v", started)
	}
	if got := bound.Lookup(k.owner); got.State != bound.Orchestrator || got.Line.RunID != run || len(got.Line.HelperPairs) != 6 {
		t.Fatalf("the owner's binding after the start: %+v", got)
	}

	// call a: the run publishes its step and, the owner being idle, stages the owner's wake (run: supervisor message)
	_, stA := k.nextSpawn(run)
	waitFor(t, "the wake of the owner", func() bool {
		n := 0
		k.readStore(func(st *store.Store) {
			_ = st.DB.QueryRow(`SELECT COUNT(*) FROM supervisor_messages WHERE obligation_kind = 'session_step_waiting' AND recipient_task_id = ? AND relationship_id = ?`,
				k.owner, "run:"+run).Scan(&n)
		})
		return n == 1
	})
	if stA.Tool.Name != "spawn_agent" || stA.Tool.Arguments["fork_context"] != false || stA.Role != "explorer" || stA.Workspace == "" || stA.Packet == "" ||
		!strings.HasPrefix(stA.Message, "crw-flow-op: "+stA.Op+"\nCRW-ROLE: explorer\n") || !strings.Contains(stA.Then, "crw flow dispatched "+run+" "+stA.Call) {
		t.Fatalf("step a: %+v", stA)
	}
	agentA := k.spawnStep(stA, "the answer of a")
	k.dispatched(run, stA, agentA)
	k.waitProcess() // the process died after a's completed row

	// the next `crw flow next` finds no process: it resumes the run, and the resumed process publishes b
	ans := k.next(run, 30)
	if ans.Resumed == 0 || !strings.Contains(ans.Hint, "crw flow resume "+run+" --detach") {
		t.Fatalf("next did not resume the dead run: %+v", ans)
	}
	var stB NextStep
	for _, st := range ans.Steps {
		if st.Action == engine.StepSpawn {
			stB = st
		}
	}
	if stB.Call == "" {
		_, stB = k.nextSpawn(run)
	}
	agentB := k.spawnStep(stB, "the answer of b")
	k.dispatched(run, stB, agentB)

	// call c: spawned, and the process dies before the owner reports it (and before its marker search, every 6 polls, finds it)
	_, stC := k.nextSpawn(run)
	agentC := k.spawnStep(stC, "the answer of c")
	k.kill()
	k.waitProcess()
	// the resumed run finds c's subagent through the ledger and the owner's spawn item and binds it with no report
	deadline := time.Now().Add(90 * time.Second)
	for {
		ans = k.next(run, 5)
		if ans.State == NextEnded {
			break
		}
		for _, st := range ans.Steps {
			if st.Action == engine.StepSpawn {
				t.Fatalf("the step of c was shown again: its subagent was not found: %+v", st)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run did not end: %+v", ans)
		}
	}
	k.waitProcess()
	if ans.Ended == nil || ans.Ended.Status != engine.RunCompleted {
		t.Fatalf("the end: %+v", ans)
	}
	res, ok := ReadResult(k.w.state, run)
	if !ok || res.Status != engine.RunCompleted || string(compactJSON(res.Value)) != `["completed","completed","completed","the answer of a","the answer of b","the answer of c"]` {
		t.Fatalf("the result: %+v %v", res, ok)
	}
	k.mu.Lock()
	starts := len(k.starts)
	k.mu.Unlock()
	if starts != 3 {
		t.Fatalf("%d process starts, want 3 (the start and two resumes)", starts)
	}
	// the run closed the owner's line when it ended
	if got := bound.Lookup(k.owner); got.State != bound.Standalone {
		t.Fatalf("the owner's binding after the end: %+v", got)
	}
	// the ledger records the role and the op of each spawn
	decisions, err := bound.ReadDecisions(mustBoundDir(t), k.owner)
	if err != nil || len(decisions) != 3 {
		t.Fatalf("the ledger: %+v %v", decisions, err)
	}
	for i, st := range []NextStep{stA, stB, stC} {
		if d := decisions[i]; d.Role != "explorer" || d.Op != st.Op || d.Decision != bound.DecisionAllowed || d.Warning != "" {
			t.Fatalf("ledger line %d: %+v", i, d)
		}
	}
	// the rows the relay acceptance reads: the newest thread_started row of each call names the subagent, the pair the host read and
	// cwd P; c's report was never written, and its row is still there
	pair := engine.FakePolicy().Roles["explorer"].Pair
	for _, c := range []struct {
		st    NextStep
		agent string
	}{{stA, agentA}, {stB, agentB}, {stC, agentC}} {
		k.readStore(func(st *store.Store) {
			var detail string
			if err := st.DB.QueryRow(`SELECT detail_json FROM flow_calls WHERE call_id = ? AND run_id = ? AND event = 'thread_started' ORDER BY event_seq DESC LIMIT 1`, c.st.Call, run).Scan(&detail); err != nil {
				t.Fatalf("call %s: %v", c.st.Call, err)
			}
			var row struct {
				ThreadID string `json:"thread_id"`
				Model    string `json:"model"`
				Effort   string `json:"effort"`
				Cwd      string `json:"cwd"`
				Via      string `json:"via"`
				Parent   string `json:"parent_thread_id"`
			}
			_ = json.Unmarshal([]byte(detail), &row)
			if row.ThreadID != c.agent || row.Model != pair.Model || row.Effort != pair.Effort || row.Cwd != c.st.Workspace || row.Via != "session" || row.Parent != k.owner {
				t.Fatalf("call %s: thread_started %s", c.st.Call, detail)
			}
			var events []string
			rows, _ := st.DB.Query(`SELECT event FROM flow_calls WHERE call_id = ? AND run_id = ? ORDER BY event_seq`, c.st.Call, run)
			for rows.Next() {
				var e string
				_ = rows.Scan(&e)
				events = append(events, e)
			}
			rows.Close()
			if got := strings.Join(events, ","); !strings.HasPrefix(got, "prepared,step_published,") || !strings.HasSuffix(got, "thread_started,turn_started,completed") {
				t.Fatalf("call %s rows: %s", c.st.Call, got)
			}
		})
	}
	inbox, _ := os.ReadDir(engine.NewFileStepBus(RunDir(k.w.state, run)).InboxDir())
	replies, _ := os.ReadDir(engine.NewFileStepBus(RunDir(k.w.state, run)).RepliesDir())
	if len(inbox) != 0 || len(replies) != 2 {
		t.Fatalf("inbox %d, replies %d: want every report answered and none for c", len(inbox), len(replies))
	}
}

func mustBoundDir(t *testing.T) string {
	t.Helper()
	dir, err := bound.Dir()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
