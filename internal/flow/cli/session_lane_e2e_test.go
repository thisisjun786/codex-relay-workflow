package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
	"github.com/thisisjun786/codex-relay-workflow/internal/role/spawn"
)

// CRW-1362 (CRW-1316 S5, design sections 15, 16 and 18.6): a project of two issues runs to the end of its milestone on the fake
// App Server as a session run, through the commands its owner thread - the project session (the child, relay role parent) - runs.
// The run is the lane lab's (a temporary relay store with the plan, a temporary git repository whose target branch dev the run
// integrates into, the lab's verifications), started with `crw flow start project-lane` on the session backend; the test is the
// owner: it takes every step with `crw flow next`, spawns each role subagent through the spawn hook on the host and reports it with
// `crw flow dispatched`. On the way the run dies after the architect and `crw flow next` resumes it; the run dies after the design
// audit's subagent was spawned and before it was reported, and the resumed run binds it from its step_published row; the two
// issue nodes (cap 2, disjoint edit regions) run at once; the App Server restarts while one node's executor runs, so that attempt
// is lost (session_attempt_lost) and a new attempt runs in the same worktree, and the report of the lost attempt that comes late is
// answered close; the other node's first review asks for changes and its fix round runs after the restart; the milestone closes
// with both nodes landed on dev, and the relay's acceptance of each node reads its reviewer from the session call's thread_started
// row.

// sessionLabRoles is the helper role store of the scenario: the architect names its model and leaves its effort to the host (the
// decided-parts rule binds it on its model alone), every other role is decided.
const sessionLabRoles = `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "medium"},
  "executor-t1": {"mode": "model", "model": "anthropic/claude-sonnet-5-5", "effort": "xhigh"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "low"},
  "critic": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"}}}`

// sessionLabPairs are the pairs of sessionLabRoles (the architect's effort undecided).
var sessionLabPairs = map[string]engine.Pair{
	"architect": {Model: "anthropic/claude-opus-5-5"}, "executor": {Model: "anthropic/claude-haiku-5", Effort: "medium"},
	"executor-t1": {Model: "anthropic/claude-sonnet-5-5", Effort: "xhigh"}, "reviewer": {Model: "gpt-6.1-sol", Effort: "high"},
	"explorer": {Model: "anthropic/claude-haiku-5", Effort: "low"},
	"critic": {Model: "gpt-6.1-sol", Effort: "xhigh"},
}

// ownerEffort is the reasoning effort of the owner thread: a subagent spawned with no effort runs on it.
const ownerEffort = "medium"

// sessionLab is the lane lab driven as a session run by its owner thread.
type sessionLab struct {
	*laneLab
	owner, run string

	mu   sync.Mutex
	done chan int
	// stopAt kills the current process after it stored the row it names (once).
	stopAt func(engine.Event) bool
	tools  int
	taken  map[string]bool
	// subagents are the subagents the owner spawned, by agent id: the step and the call it was for.
	subagents map[string]sessionSpawn
	order     []string
	closes    []string
	replies   []string
	// the scenario's marks
	auditKilled, restarted, parallel, late bool
	gateA1                                 chan struct{}
	gateOnce                               sync.Once
	lostAgent                              string
	lostStep                               NextStep
	// builders are the subagents of the nodes' first executor attempts, by node.
	builders map[string]string
}

// openGate lets A-1's first executor finish.
func (k *sessionLab) openGate() { k.gateOnce.Do(func() { close(k.gateA1) }) }

// agentTypeFor is the spawn_agent agent_type of a role: the hook's own (bound.AgentTypeOf), so the lab and the hook cannot drift.
func agentTypeFor(role string) string { return bound.AgentTypeOf(role) }

type sessionSpawn struct {
	step NextStep
	call labCall
}

// stopJournal is the journal of the lab's process with a kill at a row: after the row stopAt names is stored the process dies.
type stopJournal struct {
	*labJournal
	s *sessionLab
}

func (j *stopJournal) Append(e engine.Event) engine.Event {
	out := j.labJournal.Append(e)
	j.s.mu.Lock()
	stop := j.s.stopAt
	hit := stop != nil && stop(out)
	if hit {
		j.s.stopAt = nil
	}
	j.s.mu.Unlock()
	if hit {
		j.s.kill()
	}
	return out
}

func newSessionLab(t *testing.T) *sessionLab {
	t.Helper()
	// the owner thread is the first thread the lab's fake host makes
	owner := fmt.Sprintf("0190a000-0000-7000-8000-%012x", 1)
	l := newLaneLabFor(t, owner, "dev", labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	// the run is started in the repository (ProductionWorld's OwnerCwd is the process cwd): the evidence records of its executor steps
	// go under the lab's repository, never under the source tree (CRW-1482)
	t.Chdir(l.repo)
	// nobody declares the nodes' edit regions before the run (CRW-1391): crw.release declares each node's admission regions (the plan's
	// and the DESIGN's edit_regions) before the relay's release, so the two nodes of disjoint regions run at once with no owner's help
	dir := filepath.Join(t.TempDir(), "crw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, role.StoreFile), []byte(sessionLabRoles), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_HOME", dir)
	// the evidence contract of the executor steps is on: the SubagentStop hook accepts each executor's receipt (stopWithReceipt)
	t.Setenv("CRW_PABCD", "on")
	t.Setenv(execution.EnvPolicy, "")
	t.Setenv(execution.EnvDigest, "")
	k := &sessionLab{laneLab: l, owner: owner, taken: map[string]bool{}, subagents: map[string]sessionSpawn{}, builders: map[string]string{}, gateA1: make(chan struct{})}

	// the owner thread: the project session, idle after a turn of its own (its spawn items go into its last turn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := appserver.Dial(ctx, l.host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	raw, err := client.Call(ctx, "thread/start", map[string]any{"cwd": l.repo, "model": "owner-model", "sandbox": "danger-full-access", "approvalPolicy": "never",
		"ephemeral": false, "config": map[string]any{"model_reasoning_effort": ownerEffort}})
	if err != nil {
		t.Fatal(err)
	}
	var started struct{ Thread struct{ ID string } }
	_ = json.Unmarshal(raw, &started)
	if started.Thread.ID != owner {
		t.Fatalf("the owner thread is %s, want %s", started.Thread.ID, owner)
	}
	if _, err := client.Call(ctx, "turn/start", map[string]any{"threadId": owner, "input": []any{map[string]any{"type": "text", "text": "run the milestone"}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the owner's turn to end", func() bool {
		v, ok := l.host.Threads().Thread(owner)
		return ok && len(v.Turns) == 1 && v.Turns[0].Status == "completed"
	})

	l.td.Ready = AwaitLock
	l.td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
		k.process(req.Spec.RunID, req.Resume)
		return os.Getpid(), nil
	}
	l.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		w, err := ProductionWorld(ctx, req)
		if err != nil {
			return World{}, err
		}
		if w.Session == nil {
			return World{}, fmt.Errorf("the run is not a session run")
		}
		w.Session.Poll, w.Session.WakeAfter, w.Session.LostAfter = 50*time.Millisecond, time.Hour, 5*time.Second
		j := &labJournal{Journal: w.Journal}
		l.mu.Lock()
		l.cur = j
		l.mu.Unlock()
		w.Journal = &stopJournal{labJournal: j, s: k}
		return w, nil
	}
	return k
}

// process runs `crw flow _run` in this process, as SpawnDetached starts it.
func (k *sessionLab) process(run string, resume bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	argv := []string{ProcessCommand, "--state", k.w.state, "--socket", k.host.SocketPath}
	if resume {
		argv = append(argv, "--resume")
	}
	argv = append(argv, run)
	done := make(chan int, 1)
	k.laneLab.mu.Lock()
	k.laneLab.cancel, k.laneLab.killed = cancel, false
	k.processes++
	k.laneLab.mu.Unlock()
	k.mu.Lock()
	k.done = done
	k.mu.Unlock()
	go func() {
		defer cancel()
		var errOut bytes.Buffer
		code := RunWith(ctx, k.td.Deps, argv, strings.NewReader(""), &bytes.Buffer{}, &errOut)
		k.laneLab.mu.Lock()
		killed := k.laneLab.killed
		k.laneLab.mu.Unlock()
		if code != 0 && !killed {
			k.t.Logf("process %s (resume %v): exit %d: %s", run, resume, code, errOut.String())
		}
		done <- code
	}()
}

// waitProcess waits until the current process has ended.
func (k *sessionLab) waitProcess() {
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

func (k *sessionLab) cmd(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := RunWith(context.Background(), k.td.Deps, append(args, "--state", k.w.state, "--socket", k.host.SocketPath), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func (k *sessionLab) next(wait int) NextAnswer {
	k.t.Helper()
	code, out, errOut := k.cmd("next", k.run, "--wait", strconv.Itoa(wait), "--json")
	if code != 0 {
		k.t.Fatalf("next: exit %d\n%s\n%s", code, out, errOut)
	}
	var ans NextAnswer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		k.t.Fatalf("next: %v\n%s", err, out)
	}
	return ans
}

// dispatched reports a subagent for a step and answers the run's reply status.
func (k *sessionLab) dispatched(st NextStep, agent string) string {
	k.t.Helper()
	code, out, errOut := k.cmd("dispatched", k.run, st.Call, "--agent", agent, "--k", strconv.Itoa(st.K), "--attempt", strconv.Itoa(st.Attempt), "--wait", "60", "--json")
	var ans map[string]any
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		k.t.Fatalf("dispatched: exit %d %v\n%s\n%s", code, err, out, errOut)
	}
	status, _ := ans["status"].(string)
	k.mu.Lock()
	k.replies = append(k.replies, st.Call+"/a"+strconv.Itoa(st.Attempt)+"/"+agent+":"+status)
	k.mu.Unlock()
	return status
}

// readStore reads the relay store the run writes, read-only.
func (k *sessionLab) readStore(f func(st *store.Store)) {
	k.t.Helper()
	st, err := store.OpenReadOnlyStore(context.Background(), k.w.db)
	if err != nil {
		k.t.Fatal(err)
	}
	defer st.Close()
	f(st)
}

// rows are the flow_calls rows of a call, in order: event and detail.
func (k *sessionLab) rows(call string) []sessionRow {
	k.t.Helper()
	var out []sessionRow
	k.readStore(func(st *store.Store) {
		rows, err := st.DB.Query(`SELECT event_seq, event, COALESCE(detail_json, ''), COALESCE(role, ''), COALESCE(node_id, '') FROM flow_calls WHERE call_id = ? AND run_id = ? ORDER BY event_seq`, call, k.run)
		if err != nil {
			k.t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var r sessionRow
			var detail string
			if err := rows.Scan(&r.seq, &r.event, &detail, &r.role, &r.node); err != nil {
				k.t.Fatal(err)
			}
			_ = json.Unmarshal([]byte(detail), &r.data)
			out = append(out, r)
		}
	})
	return out
}

type sessionRow struct {
	seq               int
	event, role, node string
	data              map[string]any
}

func eventsOfRows(rows []sessionRow) string {
	var out []string
	for _, r := range rows {
		out = append(out, r.event)
	}
	return strings.Join(out, ",")
}

// hasRow is whether a call has stored a row of the event with the attempt (0: any).
func (k *sessionLab) hasRow(call, event string, attempt int) bool {
	for _, r := range k.rows(call) {
		if r.event == event && (attempt == 0 || int(rowNum(r.data["attempt"])) == attempt) {
			return true
		}
	}
	return false
}

func rowNum(v any) float64 {
	f, _ := v.(float64)
	return f
}

// boundTo is whether the run bound the subagent to the step's attempt (its thread_started row names it) and the call has ended.
func (k *sessionLab) boundTo(st NextStep, agent string) bool {
	bound, ended := false, false
	for _, r := range k.rows(st.Call) {
		switch {
		case r.event == engine.EvThreadStarted && int(rowNum(r.data["attempt"])) == st.Attempt && r.data["thread_id"] == agent:
			bound = true
		case engine.IsTerminalEvent(r.event):
			ended = true
		}
	}
	return bound && ended
}

// runningSubagents are the subagents of the owner whose turn is in progress.
func (k *sessionLab) runningSubagents() []string {
	var out []string
	for _, v := range k.host.Threads().All() {
		if !v.Subagent {
			continue
		}
		for _, tu := range v.Turns {
			if tu.Status == "inProgress" {
				out = append(out, v.ID)
			}
		}
	}
	return out
}

// checkNeedsChanges is the first unit check of A-1 (CRW-1465): one finding the fix round answers.
func checkNeedsChanges() labCheck {
	return labCheck{Verdict: reviewadapter.VerdictNeedsChanges, Findings: []reviewadapter.Finding{{Severity: "P2", Impact: "core_function", File: "node-A-1.txt", Line: 1,
		Text: "the file needs the line of the fix round"}}}
}

// spawn is what the owner does for a spawn step: the answer the role gives (the lab's model, with the scenario's turns), the spawn
// hook's decision (the ledger records the role and the op) and the subagent the host spawns with the pair the hook put in.
func (k *sessionLab) spawn(st NextStep) string {
	k.t.Helper()
	c := k.callOf(fakehost.TurnRequest{Text: st.Message})
	if c.ID != st.Call {
		k.t.Fatalf("the step %s names no prepared call of the run (%+v)", st.Op, c)
	}
	plan := k.onTurn(fakehost.TurnRequest{Text: st.Message})
	switch {
	case c.Role == "executor" && c.Label == "build:"+k.nodeOf["A-1"]+":executor" && st.Attempt == 1:
		// A-1's executor finishes only once A-2's executor runs too: the nodes run at once
		plan.Gate = k.gateA1
	case c.Role == "executor" && c.Label == "build:"+k.nodeOf["A-2"]+":executor" && st.Attempt == 1:
		// A-2's first executor attempt is still running when the App Server restarts
		plan = fakehost.TurnPlan{Hang: true}
	}
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
	// an executor step names the evidence receipt its subagent writes (CRW-1482): the subagent writes it, and the host's SubagentStop
	// hands it to the stop hook, which accepts it, before the turn ends; the unit check of the node reads it (CRW-1463, C12)
	receipt := c9ReceiptRe.FindStringSubmatch(st.Message)
	writes := receipt != nil && !plan.Hang && (c.Role == "executor" || c.Role == "executor-t1")
	var stopped chan struct{}
	rollout := ""
	if writes {
		rollout = filepath.Join(k.t.TempDir(), "rollout.jsonl")
		if plan.Gate == nil {
			stopped = make(chan struct{})
			plan.Gate = stopped
		}
	}
	agent := k.host.Threads().Spawn(fakehost.SubagentSpec{Parent: k.owner, ToolUseID: toolUse, AgentType: agentType, Model: model, Effort: effort, Message: st.Message,
		RolloutPath: rollout, Plan: plan})
	if writes {
		k.stopWithReceipt(agent, agentType, st.Message, receipt[1], rollout, plan.Message)
		if stopped != nil {
			close(stopped)
		}
	}
	k.mu.Lock()
	k.subagents[agent] = sessionSpawn{step: st, call: c}
	k.order = append(k.order, agent)
	k.mu.Unlock()
	return agent
}

// stopWithReceipt is an executor subagent's evidence receipt and the SubagentStop of its turn: it writes the receipt at the file its
// message names, the rollout whose first own UserMessage is the message, and sends the stop through the hook's registered leg, which
// accepts the receipt.
func (k *sessionLab) stopWithReceipt(agent, agentType, message, receipt, rollout, final string) {
	k.t.Helper()
	if err := os.WriteFile(receipt, []byte("$ go test ./node\nFAIL\n$ go test ./node\nok\n"), 0o644); err != nil {
		k.t.Fatal(err)
	}
	view, ok := k.host.Threads().Thread(agent)
	if !ok || len(view.Turns) == 0 {
		k.t.Fatalf("the subagent %s: %+v", agent, view)
	}
	turn := view.Turns[len(view.Turns)-1].ID
	var b bytes.Buffer
	for _, line := range []any{
		map[string]any{"type": "session_meta", "payload": map[string]any{"id": agent, "session_id": k.owner}},
		map[string]any{"type": "event_msg", "payload": map[string]any{"type": "item_completed", "thread_id": agent, "turn_id": turn,
			"item": map[string]any{"type": "UserMessage", "id": "u1", "content": []any{map[string]any{"type": "text", "text": message}}}}},
	} {
		raw, _ := json.Marshal(line)
		b.Write(append(raw, '\n'))
	}
	if err := os.WriteFile(rollout, b.Bytes(), 0o644); err != nil {
		k.t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "SubagentStop", "cwd": k.repo, "session_id": k.owner, "agent_type": agentType,
		"agent_id": agent, "turn_id": turn, "last_assistant_message": final, "agent_transcript_path": rollout})
	var out, stderr bytes.Buffer
	if code := harness.Hook(context.Background(), []string{"subagent-stop", "--leg", "subagent-stop-verifying-evidence"}, bytes.NewReader(raw), &out, &stderr,
		os.LookupEnv, harness.Legs()); code != 0 || stderr.Len() != 0 || out.Len() != 0 {
		k.t.Fatalf("the stop of %s: exit %d: %s %s", agent, code, out.String(), stderr.String())
	}
}

// take is the owner's handling of one step it was handed: each step once.
func (k *sessionLab) take(st NextStep) {
	k.t.Helper()
	key := fmt.Sprintf("%s/%s/t%d-a%d/%s", st.Call, st.Action, st.K, st.Attempt, st.AgentID)
	if k.taken[key] {
		return
	}
	switch st.Action {
	case engine.StepClose:
		k.taken[key] = true
		k.closes = append(k.closes, st.AgentID)
		return
	case engine.StepSendInput:
		k.t.Fatalf("the scenario has no schema repair, and the run sent input: %+v", st)
	}
	c := k.callOf(fakehost.TurnRequest{Text: st.Message})
	if strings.HasPrefix(c.Label, "fix:"+k.nodeOf["A-1"]+":") && !k.restarted {
		// the fix round of A-1 runs after the App Server restart: the restart comes when only A-2's executor still runs
		if k.lostAgent == "" {
			k.dumpNode(k.nodeOf["A-2"])
		}
		waitFor(k.t, "A-2's executor bound", func() bool {
			if k.lostAgent == "" {
				return false
			}
			ok := k.hasRow(k.lostStep.Call, engine.EvTurnStarted, 1)
			if !ok {
				k.t.Logf("A-2's executor %s rows: %s", k.lostStep.Call, eventsOfRows(k.rows(k.lostStep.Call)))
			}
			return ok
		})
		waitFor(k.t, "every other subagent turn done", func() bool {
			r := k.runningSubagents()
			return len(r) == 1 && r[0] == k.lostAgent
		})
		k.host.Threads().Restart()
		k.restarted = true
	}
	k.taken[key] = true
	agent := k.spawn(st)
	k.t.Logf("%s: the owner spawned %s for %s (%s, attempt %d)", time.Now().Format("15:04:05.000"), agent, st.Op, c.Label, st.Attempt)
	switch {
	case strings.HasPrefix(c.Label, "design-audit") && !k.auditKilled:
		// the run dies after the subagent was spawned and before the owner reported it
		k.auditKilled = true
		k.kill()
		k.waitProcess()
		return
	case c.Label == "build:"+k.nodeOf["A-2"]+":executor" && st.Attempt == 1:
		k.lostAgent, k.lostStep = agent, st
	}
	if c.Role == "executor" && c.Label == "build:"+c.Node+":executor" && st.Attempt == 1 {
		// the second node's executor is spawned while the first one's still runs: the two nodes run at once
		for node, other := range k.builders {
			if node != c.Node && slices.Contains(k.runningSubagents(), other) {
				k.parallel = true
			}
		}
		k.builders[c.Node] = agent
		if len(k.builders) == 2 {
			k.openGate()
		}
	}
	if status := k.dispatched(st, agent); status != engine.ReplyBound && !(status == engine.ReplyClose && k.boundTo(st, agent)) {
		// a report the run reads after its marker search bound this subagent and the call ended is answered close: the attempt is
		// over, and the subagent is closed (judgeReport); any other answer is wrong
		k.t.Fatalf("dispatched %s for %s: %s", agent, st.Op, status)
	}
	if st.Call == k.lostStep.Call && st.Attempt == 2 && !k.late {
		// the report of the lost attempt comes late: the run answers close
		k.late = true
		if status := k.dispatched(k.lostStep, k.lostAgent); status != engine.ReplyClose {
			k.t.Errorf("the late report of the lost attempt %s was answered %s, want close", k.lostAgent, status)
		}
	}
}

func TestSessionProjectMilestoneEndToEndOnTheFakeHost(t *testing.T) {
	k := newSessionLab(t)
	// A-1's first unit check finds something: its fix round runs after the App Server restart
	k.UnitChecks("A-1", ops.UnitCheckUncommitted, checkNeedsChanges())
	k.w.close() // the run's processes open the store for writing; the test reads it read-only
	// the first process dies right after the architect's completed row
	k.stopAt = func(e engine.Event) bool {
		return e.Kind == engine.KindCall && e.Label == "architect" && e.Event == engine.EvCompleted
	}
	code, out, errOut := k.cmd("start", "project-lane", "--args", fmt.Sprintf(`{"plan":%q,"milestone":"M1","cap":2}`, k.plan), "--handoff", k.file,
		"--thread", k.owner, "--repo", k.repo, "--force", "--json")
	if code != 0 {
		t.Fatalf("start: exit %d\n%s\n%s", code, out, errOut)
	}
	var started StartAnswer
	if err := json.Unmarshal([]byte(out), &started); err != nil {
		t.Fatal(err)
	}
	k.run = started.Run
	if started.Plan.Backend != BackendSession || !started.Bound {
		t.Fatalf("the start: %+v", started)
	}
	if got := bound.Lookup(k.owner); got.State != bound.Orchestrator || got.Line.RunID != k.run {
		t.Fatalf("the owner's binding after the start: %+v", got)
	}

	// the owner's loop: next, and each step it hands out
	resumes := 0
	deadline := time.Now().Add(200 * time.Second)
	// the nodes never ran at once if A-2's executor was not spawned in time: A-1's executor is let go so the test ends with the
	// failure, not a hang
	watchdog := time.AfterFunc(90*time.Second, k.openGate)
	defer watchdog.Stop()
	var ans NextAnswer
	for {
		ans = k.next(5)
		if ans.Resumed != 0 {
			resumes++
			if !strings.Contains(ans.Hint, "crw flow resume "+k.run+" --detach") {
				t.Errorf("next resumed the run without saying so: %s", ans.Hint)
			}
		}
		if ans.State == NextEnded {
			break
		}
		for _, st := range ans.Steps {
			k.take(st)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run did not end: %+v\nreplies %v", ans, k.replies)
		}
	}
	k.waitProcess()
	if ans.Ended == nil || ans.Ended.Status != engine.RunCompleted {
		t.Fatalf("the end: %+v", ans.Ended)
	}
	res, ok := ReadResult(k.w.state, k.run)
	if !ok {
		t.Fatal("the run left no result")
	}
	k.w.open()
	v := k.closed(res)
	if v.Tip != k.git(k.repo, "rev-parse", "dev") {
		t.Errorf("the milestone closed over %s, dev is at %s", v.Tip, k.git(k.repo, "rev-parse", "dev"))
	}
	k.effectsOnce("A-1", "A-2")
	if strings.Join(k.reports, ",") != "plan-1/M1" {
		t.Errorf("the milestone reports sent up: %v", k.reports)
	}
	// run death then `crw flow next` resumed it, twice: after the architect and after the design audit's spawn
	if resumes != 2 || k.processes != 3 {
		t.Errorf("%d resumes by next, %d processes; want 2 and 3", resumes, k.processes)
	}
	if !k.auditKilled || !k.restarted || !k.parallel || !k.late {
		t.Fatalf("the scenario did not run every step: audit kill %v, restart %v, parallel %v, late report %v", k.auditKilled, k.restarted, k.parallel, k.late)
	}

	// the only new top-level thread is the owner's; every role call ran on a subagent thread of the owner
	for _, th := range k.host.Threads().All() {
		if th.ID == k.owner {
			continue
		}
		if !th.Subagent || th.ParentThreadID != k.owner {
			t.Errorf("thread %s is not a subagent of the owner (subagent %v, parent %q)", th.ID, th.Subagent, th.ParentThreadID)
		}
	}

	// every session call's rows: one thread_started per attempt, naming a subagent the owner spawned for that attempt's step, with
	// the pair of the role under the decided-parts rule, cwd P apart from the owner's cwd
	calls := map[string]bool{}
	k.readStore(func(st *store.Store) {
		rows, err := st.DB.Query(`SELECT DISTINCT call_id FROM flow_calls WHERE run_id = ? AND kind = 'agent'`, k.run)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			_ = rows.Scan(&c)
			calls[c] = true
		}
	})
	if len(calls) < 8 {
		t.Fatalf("%d agent calls in the run", len(calls))
	}
	byLabel := map[string]string{}
	for call := range calls {
		rows := k.rows(call)
		perAttempt := map[int]int{}
		completed := 0
		label := ""
		for _, r := range rows {
			if l, _ := r.data["_label"].(string); l != "" {
				label = l
			}
			switch r.event {
			case engine.EvCompleted:
				completed++
			case engine.EvThreadStarted:
				attempt := int(rowNum(r.data["attempt"]))
				perAttempt[attempt]++
				agent, _ := r.data["thread_id"].(string)
				sp, ok := k.subagents[agent]
				if !ok || sp.step.Call != call || sp.step.Attempt != attempt {
					t.Errorf("%s (%s): thread_started names %s, which the owner did not spawn for attempt %d (%+v)", call, label, agent, attempt, sp.step)
				}
				pair := sessionLabPairs[r.role]
				model, _ := r.data["model"].(string)
				effort, _ := r.data["effort"].(string)
				switch {
				case pair.Effort == "":
					if model != pair.Model || effort != ownerEffort || r.data["effort_unchecked"] != true {
						t.Errorf("%s (%s, role %s): bound %s/%s (effort_unchecked %v), want the model %s at the effort the host applied", call, label, r.role, model, effort, r.data["effort_unchecked"], pair.Model)
					}
				case model != pair.Model || effort != pair.Effort:
					t.Errorf("%s (%s, role %s): bound %s/%s, want the role's pair %v", call, label, r.role, model, effort, pair)
				}
				if th, _ := k.host.Threads().Thread(agent); th.Model != model || th.AgentRole != agentTypeFor(r.role) {
					t.Errorf("%s: the subagent %s runs %s as %s, the row says %s for role %s", call, agent, th.Model, th.AgentRole, model, r.role)
				}
				if cwd, _ := r.data["cwd"].(string); cwd == "" || cwd == k.repo || r.data["host_cwd"] != k.repo || r.data["via"] != "session" || r.data["parent_thread_id"] != k.owner {
					t.Errorf("%s (%s): thread_started %v", call, label, r.data)
				}
			}
		}
		byLabel[label] = call
		for attempt, n := range perAttempt {
			if n != 1 {
				t.Errorf("%s (%s): %d thread_started rows for attempt %d: %s", call, label, n, attempt, eventsOfRows(rows))
			}
		}
		if completed != 1 {
			t.Errorf("%s (%s): %d completed rows: %s", call, label, completed, eventsOfRows(rows))
		}
	}

	// the design audit's subagent was spawned before the process died and never reported: the resumed run bound it from its
	// step_published row, through the ledger and the owner turn's spawn item, and showed its step no more
	audit := byLabel["design-audit:r1"]
	if got := eventsOfRows(k.rows(audit)); got != "prepared,step_published,thread_started,turn_started,completed" {
		t.Errorf("the design audit's rows: %s", got)
	}
	for _, r := range k.replies {
		if strings.HasPrefix(r, audit+"/") {
			t.Errorf("the design audit's subagent was reported: %s", r)
		}
	}
	// the App Server restart lost A-2's running executor: session_attempt_lost, and the next attempt in the same allocation
	a2 := byLabel["build:"+k.nodeOf["A-2"]+":executor"]
	a2rows := k.rows(a2)
	if got := eventsOfRows(a2rows); got != "prepared,step_published,thread_started,turn_started,session_attempt_lost,step_published,thread_started,turn_started,completed" {
		t.Errorf("A-2's executor rows: %s", got)
	}
	for _, r := range a2rows {
		if r.event == engine.EvSessionAttemptLost && (r.data["agent_id"] != k.lostAgent || int(rowNum(r.data["attempt"])) != 1) {
			t.Errorf("session_attempt_lost %v, want attempt 1 of %s", r.data, k.lostAgent)
		}
	}
	if !slices.Contains(k.closes, k.lostAgent) {
		t.Errorf("the lost subagent %s was never closed: %v", k.lostAgent, k.closes)
	}
	// the fix round of A-1 ran after the restart, on a new subagent that continues the build
	fix := byLabel["fix:"+k.nodeOf["A-1"]+":executor:r1"]
	if fix == "" || !k.hasRow(fix, engine.EvCompleted, 0) {
		t.Fatalf("A-1's fix round did not complete: %v", byLabel)
	}
	if body, err := exec.Command("git", "-C", k.repo, "show", "dev:node-A-1.txt").CombinedOutput(); err != nil || !strings.Contains(string(body), "fix:") {
		t.Errorf("the fix round's work is not on dev: %v %s", err, body)
	}
	// every node is accepted on its unit check (CRW-1465): the relay's acceptance names the crw.unitCheck call, the checker's model and no
	// reviewer thread (a unit check is a run of the review tool, not an agent), and the check read the executor's directory P, apart
	// from the owner's cwd
	checkedIn := map[string][]string{}
	for _, r := range k.uc.requests() {
		checkedIn[r.Node] = append(checkedIn[r.Node], r.Request.Worktree)
	}
	for _, key := range []string{"A-1", "A-2"} {
		node := k.nodeOf[key]
		var reviewCall, head, revThread, revModel, revEffort string
		k.readStore(func(st *store.Store) {
			if err := st.DB.QueryRow(`SELECT reviewer_call_id, head, reviewer_thread_id, reviewer_model, reviewer_effort FROM dag_agent_acceptances WHERE node_id = ?`, node).
				Scan(&reviewCall, &head, &revThread, &revModel, &revEffort); err != nil {
				t.Fatalf("the acceptance of %s: %v", key, err)
			}
		})
		var check *sessionRow
		for _, r := range k.rows(reviewCall) {
			if r.event == engine.EvCompleted {
				r := r
				check = &r
			}
		}
		if check == nil || revThread != "" || revModel != labCheckModel || revEffort != "medium" {
			t.Errorf("the acceptance of %s: call %s thread %q %s/%s, want a completed unit check of %s with no thread", key, reviewCall, revThread, revModel, revEffort, labCheckModel)
		}
		dirs := checkedIn[node]
		if len(dirs) == 0 {
			t.Errorf("%s was never unit-checked", key)
		}
		for _, d := range dirs {
			if d == "" || d == k.repo {
				t.Errorf("the unit check of %s read %q, the owner's cwd is %s", key, d, k.repo)
			}
		}
		if key == "A-1" && len(dirs) != 2 {
			t.Errorf("A-1 was unit-checked %d times, want the build's and the fix round's", len(dirs))
		}
	}
	// the owner's line was closed when the run ended
	if got := bound.Lookup(k.owner); got.State != bound.Standalone {
		t.Errorf("the owner's binding after the end: %+v", got)
	}
}

// dumpNode logs the run's rows of a node (a diagnosis of a scenario that stalls).
func (k *sessionLab) dumpNode(node string) {
	k.readStore(func(st *store.Store) {
		rows, err := st.DB.Query(`SELECT kind, event, COALESCE(substr(detail_json, 1, 600), ''), COALESCE(substr(CAST(result_bytes AS TEXT), 1, 600), '') FROM flow_calls WHERE run_id = ? AND node_id = ? ORDER BY event_seq`, k.run, node)
		if err != nil {
			k.t.Logf("dump: %v", err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var kind, event, detail, result string
			_ = rows.Scan(&kind, &event, &detail, &result)
			k.t.Logf("node %s: %s %s %s %s", node, kind, event, detail, result)
		}
	})
}
