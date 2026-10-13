package sessionhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/sessionhost"
)

// env is a fake App Server with one owner thread (the project orchestrator) whose turn is in progress, an App Server
// client and a flow-bound directory of its own.
type env struct {
	t        *testing.T
	server   *fakehost.Server
	threads  *fakehost.Threads
	client   *appserver.Client
	host     *sessionhost.Host
	boundDir string
	owner    string
	turn     string
	release  chan struct{}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, server: fakehost.Start(t), release: make(chan struct{})}
	e.threads = e.server.Threads()
	e.threads.ThreadIDs(func(n int) string { return fmt.Sprintf("01a10000-0000-7000-8000-%012x", n) })
	e.boundDir = filepath.Join(t.TempDir(), "flow-bound")
	if err := os.Mkdir(e.boundDir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.connect()
	raw, err := e.client.Call(context.Background(), "thread/start", map[string]any{"cwd": "/work/orchestrator", "model": "owner-model",
		"sandbox": "danger-full-access", "approvalPolicy": "never", "ephemeral": false})
	if err != nil {
		t.Fatal(err)
	}
	var started struct{ Thread struct{ ID string } }
	if err := json.Unmarshal(raw, &started); err != nil {
		t.Fatal(err)
	}
	e.owner = started.Thread.ID
	e.threads.OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Gate: e.release, Message: "owner"}
	})
	raw, err = e.client.Call(context.Background(), "turn/start", map[string]any{"threadId": e.owner, "input": []any{map[string]any{"type": "text", "text": "run"}}})
	if err != nil {
		t.Fatal(err)
	}
	var turn struct{ Turn struct{ ID string } }
	if err := json.Unmarshal(raw, &turn); err != nil {
		t.Fatal(err)
	}
	e.turn = turn.Turn.ID
	t.Cleanup(func() {
		select {
		case <-e.release:
		default:
			close(e.release)
		}
	})
	return e
}

// connect dials a fresh client (the old one is closed by its cleanup), as a run does after an App Server restart.
func (e *env) connect() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := appserver.Dial(ctx, e.server.SocketPath)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = client.Close() })
	e.client = client
	e.host = sessionhost.New(client, e.boundDir)
}

func (e *env) spawn(spec fakehost.SubagentSpec) string {
	e.t.Helper()
	if spec.Parent == "" {
		spec.Parent = e.owner
	}
	id := e.threads.Spawn(spec)
	if id == "" {
		e.t.Fatal("the fake made no subagent")
	}
	return id
}

func (e *env) decide(d bound.Decision) {
	e.t.Helper()
	if d.ThreadID == "" {
		d.ThreadID = e.owner
	}
	if d.RunID == "" {
		d.RunID = "run-1"
	}
	if d.ThreadKind == "" {
		d.ThreadKind = bound.KindOrchestrator
	}
	if d.Decision == "" {
		d.Decision = bound.DecisionAllowed
	}
	if err := bound.AppendDecision(e.boundDir, d); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) waitLast(thread, status string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, ok := e.threads.Thread(thread)
		if ok && len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Status == status {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("thread %s: the last turn did not reach %s", thread, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func message(op, role, body string) string {
	return "crw-flow-op: " + op + "\nCRW-ROLE: " + role + "\n\n" + body
}

// ReadAgent reads a subagent with thread/read (includeTurns false): the parent, the configured pair, the cwd, the role, the
// status and the rollout path, through the life of the subagent: active, notLoaded, lost on a restart, and absent.
func TestReadAgent(t *testing.T) {
	e := newEnv(t)
	gate := make(chan struct{})
	child := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-1", AgentType: "reviewer", Nickname: "Scout", Model: "gpt-6.1-sol", Effort: "xhigh",
		Message: message("c_abc/t1-a1", "reviewer-final", "packet"), RolloutPath: "/r/child.jsonl", Plan: fakehost.TurnPlan{Gate: gate, Message: "{}"}})
	ctx := context.Background()
	got, err := e.host.ReadAgent(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	want := sessionhost.Agent{ThreadID: child, ParentThreadID: e.owner, Model: "gpt-6.1-sol", Effort: "xhigh", Cwd: "/work/orchestrator", AgentRole: "reviewer",
		Status: sessionhost.StatusActive, RolloutPath: "/r/child.jsonl", ThreadSource: "subagent", Nickname: "Scout", CLIVersion: fakehost.CLIVersion}
	if got != want {
		t.Errorf("ReadAgent\n got %+v\nwant %+v", got, want)
	}
	if e.server.Count("thread/read") != 1 {
		t.Errorf("thread/read requests: %d", e.server.Count("thread/read"))
	}
	for _, r := range e.server.Requests() {
		if r.Method == "thread/read" && !strings.Contains(string(r.Params), `"includeTurns":false`) {
			t.Errorf("thread/read asked %s", r.Params)
		}
	}
	close(gate)
	e.waitLast(child, "completed")
	if got, err := e.host.ReadAgent(ctx, child); err != nil || got.Status != sessionhost.StatusNotLoaded {
		t.Errorf("a finished subagent: %+v %v", got, err)
	}
	// a null pair reads as empty strings
	e.threads.Readback(func(fakehost.ThreadView) (*string, *string) { return nil, nil })
	if got, err := e.host.ReadAgent(ctx, child); err != nil || got.Model != "" || got.Effort != "" {
		t.Errorf("a null pair: %+v %v", got, err)
	}
	e.threads.Readback(nil)
	// a subagent running when the App Server restarts reads notLoaded on the new connection
	lost := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-2", AgentType: "executor", Message: message("c_abc/t1-a2", "executor", "p"), Plan: fakehost.TurnPlan{Hang: true}})
	e.waitLast(lost, "inProgress")
	e.threads.Restart()
	e.connect()
	if got, err := e.host.ReadAgent(ctx, lost); err != nil || got.Status != sessionhost.StatusNotLoaded || got.ParentThreadID != e.owner {
		t.Errorf("a lost subagent: %+v %v", got, err)
	}
	// a thread the host does not have
	if _, err := e.host.ReadAgent(ctx, "01a1ffff-0000-7000-8000-000000000000"); !errors.Is(err, sessionhost.ErrNotFound) {
		t.Errorf("an unknown thread: %v", err)
	}
	// a transport failure is not "not found"
	e.server.Script("thread/read", fakehost.Reply{Error: &fakehost.RPCError{Code: -32603, Message: "internal error"}})
	if _, err := e.host.ReadAgent(ctx, child); err == nil || errors.Is(err, sessionhost.ErrNotFound) {
		t.Errorf("an internal error: %v", err)
	}
}

// Turns lists a thread's turns newest first with summary or full items, and derives what a run needs from each: the first
// user text and its marker, the final answer and its phase, the error and the time it last changed.
func TestTurns(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	child := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-1", AgentType: "reviewer", Message: message("c_abc/t1-a1", "reviewer", "packet"),
		Plan: fakehost.TurnPlan{Commentary: "reading", Message: `{"verdict":"fail"}`}})
	e.waitLast(child, "completed")
	// a send_input turn (schema repair), then a failed one
	e.threads.OnTurn(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.N == 3 {
			return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "quota", CodexErrorInfo: "usageLimitExceeded"}}
		}
		return fakehost.TurnPlan{Message: `{"verdict":"pass"}`, NullPhase: true}
	})
	for i, text := range []string{message("c_abc/t2-a1", "reviewer", "repair"), "plain"} {
		if _, err := e.client.Call(ctx, "turn/start", map[string]any{"threadId": child, "input": []any{map[string]any{"type": "text", "text": text}}}); err != nil {
			t.Fatal(err)
		}
		e.waitLast(child, map[int]string{0: "completed", 1: "failed"}[i])
	}
	for _, full := range []bool{false, true} {
		turns, err := e.host.Turns(ctx, child, 0, full)
		if err != nil {
			t.Fatal(err)
		}
		if len(turns) != 3 {
			t.Fatalf("full=%v: %d turns", full, len(turns))
		}
		failed, repair, first := turns[0], turns[1], turns[2]
		if first.Status != "completed" || first.FirstUserText != message("c_abc/t1-a1", "reviewer", "packet") || first.Marker != "c_abc/t1-a1" ||
			!first.Answered || first.FinalText != `{"verdict":"fail"}` || first.FinalPhase != "final_answer" || first.ErrorInfo != nil || first.UpdatedAt.IsZero() {
			t.Errorf("full=%v: the first turn %+v", full, first)
		}
		// a provider that sets no phase: the last message without a phase is the answer
		if repair.Marker != "c_abc/t2-a1" || !repair.Answered || repair.FinalText != `{"verdict":"pass"}` || repair.FinalPhase != "" {
			t.Errorf("full=%v: the repair turn %+v", full, repair)
		}
		if failed.Status != "failed" || failed.Marker != "" || failed.Answered || failed.ErrorInfo == nil ||
			*failed.ErrorInfo != (sessionhost.TurnError{Message: "quota", CodexErrorInfo: "usageLimitExceeded"}) {
			t.Errorf("full=%v: the failed turn %+v", full, failed)
		}
		wantView := map[bool]string{false: "summary", true: "full"}[full]
		for _, tu := range turns {
			if tu.ItemsView != wantView || tu.ReadError != nil {
				t.Errorf("full=%v: turn %s view %q, read error %v", full, tu.ID, tu.ItemsView, tu.ReadError)
			}
		}
	}
	// a limit caps the turns, read across pages when the host caps the page size
	e.threads.TurnsPageSize(1)
	turns, err := e.host.Turns(ctx, child, 2, false)
	if err != nil || len(turns) != 2 || turns[0].Status != "failed" || turns[1].Marker != "c_abc/t2-a1" {
		t.Errorf("limit 2 over pages of 1: %+v %v", turns, err)
	}
	all, err := e.host.Turns(ctx, child, 0, true)
	if err != nil || len(all) != 3 {
		t.Errorf("all turns over pages of 1: %d %v", len(all), err)
	}
	e.threads.TurnsPageSize(0)
	var params []map[string]any
	for _, r := range e.server.Requests() {
		if r.Method == "thread/turns/list" {
			var p map[string]any
			_ = json.Unmarshal(r.Params, &p)
			params = append(params, p)
		}
	}
	if params[0]["itemsView"] != "summary" || params[1]["itemsView"] != "full" || params[0]["sortDirection"] != "desc" || params[2]["limit"] != float64(2) {
		t.Errorf("turns/list params: %v", params)
	}
	// a full read the host answers without the items is completed by thread/items/list; one it cannot complete says so
	e.threads.ReadAs(child, 0, fakehost.TurnPlan{NotLoaded: true})
	turns, err = e.host.Turns(ctx, child, 0, true)
	if err != nil || turns[2].ItemsView != "full" || turns[2].FinalText != `{"verdict":"fail"}` || turns[2].ReadError != nil {
		t.Errorf("a turn listed without items: %+v %v", turns[2], err)
	}
	e.threads.ReadAs(child, 0, fakehost.TurnPlan{NotLoaded: true, ItemsListError: map[string]any{"code": -32603, "message": "gone"}})
	turns, err = e.host.Turns(ctx, child, 0, true)
	if err != nil || turns[2].ReadError == nil || turns[2].Answered || turns[2].ItemsView != "notLoaded" {
		t.Errorf("a turn whose items cannot be read: %+v %v", turns[2], err)
	}
	if _, err := e.host.Turns(ctx, "01a1ffff-0000-7000-8000-000000000000", 1, false); !errors.Is(err, sessionhost.ErrNotFound) {
		t.Errorf("an unknown thread: %v", err)
	}
}

// A subagent lost on an App Server restart: its marker turn never completes and has no answer; one that had finished keeps
// its answer, readable while the thread is notLoaded.
func TestTurnsOfLostAndNotLoadedSubagents(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	done := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-1", AgentType: "explorer", Message: message("c_x/t1-a1", "explorer", "q"), Plan: fakehost.TurnPlan{Message: "found"}})
	e.waitLast(done, "completed")
	lost := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-2", AgentType: "executor", Message: message("c_y/t1-a1", "executor", "p"), Plan: fakehost.TurnPlan{Hang: true}})
	e.waitLast(lost, "inProgress")
	running, err := e.host.Turns(ctx, lost, 2, false)
	if err != nil || len(running) != 1 || running[0].Status != "inProgress" || running[0].Answered || running[0].Marker != "c_y/t1-a1" {
		t.Errorf("a running subagent: %+v %v", running, err)
	}
	e.threads.Restart()
	e.connect()
	for id, want := range map[string]struct {
		status   string
		answered bool
	}{done: {"completed", true}, lost: {"interrupted", false}} {
		agent, err := e.host.ReadAgent(ctx, id)
		if err != nil || agent.Status != sessionhost.StatusNotLoaded {
			t.Errorf("%s: %+v %v", id, agent, err)
		}
		for _, full := range []bool{false, true} {
			turns, err := e.host.Turns(ctx, id, 2, full)
			if err != nil || len(turns) != 1 || turns[0].Status != want.status || turns[0].Answered != want.answered {
				t.Errorf("%s full=%v: %+v %v", id, full, turns, err)
			}
		}
	}
}

// Interrupt sends turn/interrupt for the subagent's turn; the turn ends interrupted.
func TestInterrupt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	child := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-1", AgentType: "executor", Message: message("c_x/t1-a1", "executor", "p"), Plan: fakehost.TurnPlan{Hang: true}})
	e.waitLast(child, "inProgress")
	turns, err := e.host.Turns(ctx, child, 1, false)
	if err != nil || len(turns) != 1 {
		t.Fatal(turns, err)
	}
	if err := e.host.Interrupt(ctx, child, turns[0].ID); err != nil {
		t.Fatal(err)
	}
	e.waitLast(child, "interrupted")
	if got := e.threads.Interrupts(); !slices.Equal(got, []string{child}) {
		t.Errorf("interrupts: %v", got)
	}
	var p map[string]any
	for _, r := range e.server.Requests() {
		if r.Method == "turn/interrupt" {
			_ = json.Unmarshal(r.Params, &p)
		}
	}
	if p["threadId"] != child || p["turnId"] != turns[0].ID {
		t.Errorf("turn/interrupt params %v", p)
	}
	if err := e.host.Interrupt(ctx, "01a1ffff-0000-7000-8000-000000000000", "t"); !errors.Is(err, sessionhost.ErrNotFound) {
		t.Errorf("interrupting an unknown thread: %v", err)
	}
}

// FindByMarker finds the subagent of a published step from the owner's side: the hook's ledger names the tool use and the
// op, the owner turn's collabAgentToolCall names the thread it created.
func TestFindByMarker(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	reviewer := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-r", AgentType: "reviewer", Message: message("c_rev/t1-a1", "reviewer", "p"), Plan: fakehost.TurnPlan{Hang: true}})
	executor := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-e", AgentType: "executor", Message: message("c_exe/t1-a1", "executor", "p"), Plan: fakehost.TurnPlan{Hang: true}})
	e.decide(bound.Decision{ToolUseID: "exec-r", Op: "c_rev/t1-a1", AppliedModel: "gpt-6.1-sol", AppliedEffort: "xhigh"})
	e.decide(bound.Decision{ToolUseID: "exec-e", AppliedModel: "haiku", AppliedEffort: "xhigh"}) // no op recorded

	got, err := e.host.FindByMarker(ctx, e.owner, "c_rev/t1-a1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Found || got.Spawn.ThreadID != reviewer || got.Spawn.ToolUseID != "exec-r" || got.Spawn.OwnerTurnID != e.turn || got.Spawn.Op != "c_rev/t1-a1" ||
		got.Spawn.Via != sessionhost.ViaLedger || got.Spawn.Decision == nil || got.Spawn.Decision.AppliedModel != "gpt-6.1-sol" || len(got.Warnings) != 0 || len(got.Others) != 0 {
		t.Errorf("by the ledger's op: %+v", got)
	}
	// a ledger line without the op: the spawn message's marker (the item's prompt) names it
	got, err = e.host.FindByMarker(ctx, e.owner, "c_exe/t1-a1")
	if err != nil || !got.Found || got.Spawn.ThreadID != executor || got.Spawn.Via != sessionhost.ViaPrompt || got.Spawn.Decision == nil {
		t.Errorf("by the prompt beside the ledger line: %+v %v", got, err)
	}
	// an op nobody spawned
	if got, err := e.host.FindByMarker(ctx, e.owner, "c_rev/t1-a2"); err != nil || got.Found {
		t.Errorf("an op not spawned: %+v %v", got, err)
	}
	// a spawn the hook did not record is still found, with a warning (warn and record, never a block)
	unrecorded := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-u", AgentType: "explorer", Message: message("c_exp/t1-a1", "explorer", "q")})
	got, err = e.host.FindByMarker(ctx, e.owner, "c_exp/t1-a1")
	if err != nil || !got.Found || got.Spawn.ThreadID != unrecorded || got.Spawn.Via != sessionhost.ViaUnrecorded || got.Spawn.Decision != nil || !hasWarning(got, sessionhost.WarnNoLedgerLine) {
		t.Errorf("an unrecorded spawn: %+v %v", got, err)
	}
	// the ledger's op wins over a prompt that names another, with a warning
	e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-m", AgentType: "reviewer", Message: message("c_other/t1-a1", "reviewer", "p")})
	e.decide(bound.Decision{ToolUseID: "exec-m", Op: "c_mis/t1-a1"})
	got, err = e.host.FindByMarker(ctx, e.owner, "c_mis/t1-a1")
	if err != nil || !got.Found || got.Spawn.ToolUseID != "exec-m" || !hasWarning(got, sessionhost.WarnOpDiffers) {
		t.Errorf("ledger and prompt disagree: %+v %v", got, err)
	}
	if got, _ := e.host.FindByMarker(ctx, e.owner, "c_other/t1-a1"); got.Found {
		t.Errorf("the prompt's op was taken over the ledger's: %+v", got)
	}
	// a refused decision is not an allowed spawn: the host ran it anyway, so it is found with a warning
	e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-x", AgentType: "executor", Message: message("c_ref/t1-a1", "executor", "p")})
	e.decide(bound.Decision{ToolUseID: "exec-x", Op: "c_ref/t1-a1", Decision: bound.DecisionRefused, Reason: bound.ReasonFullHistoryFork})
	if got, err := e.host.FindByMarker(ctx, e.owner, "c_ref/t1-a1"); err != nil || !got.Found || got.Spawn.Via != sessionhost.ViaUnrecorded || !hasWarning(got, sessionhost.WarnRefusedDecision) {
		t.Errorf("a spawn the hook refused: %+v %v", got, err)
	}
	// the same op spawned twice: the newest is the one, the older is named beside it
	again := e.spawn(fakehost.SubagentSpec{ToolUseID: "exec-r2", AgentType: "reviewer", Message: message("c_rev/t1-a1", "reviewer", "p")})
	e.decide(bound.Decision{ToolUseID: "exec-r2", Op: "c_rev/t1-a1"})
	got, err = e.host.FindByMarker(ctx, e.owner, "c_rev/t1-a1")
	if err != nil || got.Spawn.ThreadID != again || len(got.Others) != 1 || got.Others[0].ThreadID != reviewer || !hasWarning(got, sessionhost.WarnSeveralSpawns) {
		t.Errorf("an op spawned twice: %+v %v", got, err)
	}
	// the owner's earlier turn is searched too, and a restart does not hide the spawn: the lost subagent is still found
	close(e.release)
	e.waitLast(e.owner, "completed")
	e.threads.OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Hang: true} })
	if _, err := e.client.Call(ctx, "turn/start", map[string]any{"threadId": e.owner, "input": []any{map[string]any{"type": "text", "text": "next"}}}); err != nil {
		t.Fatal(err)
	}
	e.waitLast(e.owner, "inProgress")
	e.threads.Restart()
	e.connect()
	got, err = e.host.FindByMarker(ctx, e.owner, "c_exe/t1-a1")
	if err != nil || !got.Found || got.Spawn.ThreadID != executor || got.Spawn.OwnerTurnID != e.turn {
		t.Errorf("after a restart: %+v %v", got, err)
	}
	// a ledger the run cannot read is a warning, and the prompts still find the spawn
	f, err := os.OpenFile(filepath.Join(e.boundDir, bound.DecisionsDir, e.owner+bound.FileSuffix), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not a decision\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got, err = e.host.FindByMarker(ctx, e.owner, "c_rev/t1-a1")
	if err != nil || !got.Found || got.Spawn.ThreadID != again || got.Spawn.Via != sessionhost.ViaUnrecorded || !hasWarning(got, sessionhost.WarnLedgerUnreadable) {
		t.Errorf("an unreadable ledger: %+v %v", got, err)
	}
	// an op that is no marker is an input error
	if _, err := e.host.FindByMarker(ctx, e.owner, "not an op"); err == nil {
		t.Errorf("a malformed op was searched")
	}
	// an owner the host does not have
	if _, err := e.host.FindByMarker(ctx, "01a1ffff-0000-7000-8000-000000000000", "c_rev/t1-a1"); !errors.Is(err, sessionhost.ErrNotFound) {
		t.Errorf("an unknown owner: %v", err)
	}
}

func hasWarning(s sessionhost.MarkerSearch, code string) bool {
	for _, w := range s.Warnings {
		if strings.HasPrefix(w, code) {
			return true
		}
	}
	return false
}

// The answers the live 0.154.0 host gave for a finished subagent (fixtures from the read-only probe of CRW-1332) read as
// the package expects, and match the schema copy although they carry members it does not list.
func TestLiveSubagentAnswers(t *testing.T) {
	fixtures := filepath.Join("..", "..", "bridge", "appserver", "testdata", "fixtures")
	load := func(name string) map[string]any {
		raw, err := os.ReadFile(filepath.Join(fixtures, name))
		if err != nil {
			t.Fatal(err)
		}
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	read, full, summary := load("subagent-thread-read.json"), load("subagent-turns-list-full.json"), load("subagent-turns-list-summary.json")
	for name, c := range map[string]struct {
		schema string
		answer map[string]any
	}{"thread/read": {"ThreadReadResponse.json", read}, "turns full": {"ThreadTurnsListResponse.json", full}, "turns summary": {"ThreadTurnsListResponse.json", summary}} {
		if err := schemaOf(t, c.schema).Validate(c.answer); err != nil {
			t.Errorf("%s does not match %s: %v", name, c.schema, err)
		}
	}
	server := fakehost.Start(t)
	server.Respond("thread/read", fakehost.Reply{Result: read})
	server.Handle("thread/turns/list", func(params json.RawMessage) fakehost.Reply {
		if strings.Contains(string(params), `"itemsView":"full"`) {
			return fakehost.Reply{Result: full}
		}
		return fakehost.Reply{Result: summary}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := appserver.Dial(ctx, server.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	host := sessionhost.New(client, t.TempDir())
	const id = "01a13587-5221-76c0-bbb4-44e8411968fa"
	agent, err := host.ReadAgent(ctx, id)
	want := sessionhost.Agent{ThreadID: id, ParentThreadID: "01a13586-a2b7-7260-b5bf-3965812bfd46", Model: "anthropic/claude-haiku-5-5", Effort: "high",
		Cwd: "/home/user/.local/state/codex-session-relay/e4f02742e7163a2a/flow/run-39ba54c04cc5/calls/c_bf3498ca537a/wt", AgentRole: "explorer",
		Status: sessionhost.StatusIdle, RolloutPath: "/home/user/.codex/sessions/2026/10/13/rollout-2026-10-13T22-50-23-01a13587-5221-76c0-bbb4-44e8411968fa.jsonl",
		ThreadSource: "subagent", Nickname: "Scout", CLIVersion: "0.154.0"}
	if err != nil || agent != want {
		t.Errorf("ReadAgent of the live answer\n got %+v %v\nwant %+v", agent, err, want)
	}
	for _, isFull := range []bool{false, true} {
		turns, err := host.Turns(ctx, id, 2, isFull)
		if err != nil || len(turns) != 1 {
			t.Fatalf("full=%v: %+v %v", isFull, turns, err)
		}
		tu := turns[0]
		if tu.ID != "01a13587-52a7-7d90-b1cc-d1c34f56b0db" || tu.Status != "completed" || !tu.Answered || tu.FinalPhase != sessionhost.PhaseFinalAnswer ||
			tu.FinalText != "(final_answer message, text shortened in the fixture)" || !strings.HasPrefix(tu.FirstUserText, "TASK: ") || tu.Marker != "" ||
			tu.ErrorInfo != nil || tu.UpdatedAt != time.Unix(1791899587, 0).UTC() {
			t.Errorf("full=%v: the live turn %+v", isFull, tu)
		}
	}
}

func schemaOf(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "bridge", "appserver", "testdata", "schema-0.154", "v2", name))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
