package fakehost_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// subagentHost is a fake host with one owner thread whose turn is in progress, the way a project orchestrator's turn
// spawns its role subagents.
type subagentHost struct {
	t       *testing.T
	server  *fakehost.Server
	threads *fakehost.Threads
	client  *appserver.Client
	owner   string
	turn    string
	release chan struct{}
}

func newSubagentHost(t *testing.T) *subagentHost {
	t.Helper()
	h := &subagentHost{t: t, server: fakehost.Start(t), release: make(chan struct{})}
	h.threads = h.server.Threads()
	h.threads.ThreadIDs(func(n int) string { return fmt.Sprintf("01a10000-0000-7000-8000-%012x", n) })
	h.client = h.dial()
	start := h.call("thread/start", map[string]any{"cwd": "/work/orchestrator", "model": "owner-model", "sandbox": "danger-full-access",
		"approvalPolicy": "never", "ephemeral": false, "config": map[string]any{"model_reasoning_effort": "xhigh"}})
	h.owner = start["thread"].(map[string]any)["id"].(string)
	h.threads.OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Gate: h.release, Message: "owner done"}
	})
	started := h.call("turn/start", map[string]any{"threadId": h.owner, "input": []any{map[string]any{"type": "text", "text": "run the milestone"}}})
	h.turn = started["turn"].(map[string]any)["id"].(string)
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
	})
	return h
}

func (h *subagentHost) dial() *appserver.Client {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := appserver.Dial(ctx, h.server.SocketPath)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = client.Close() })
	return client
}

func (h *subagentHost) call(method string, params map[string]any) map[string]any {
	h.t.Helper()
	raw, err := h.client.Call(context.Background(), method, params)
	if err != nil {
		h.t.Fatalf("%s: %v", method, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

// waitStatus waits until the last turn of thread has the status.
func (h *subagentHost) waitStatus(thread, status string) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, ok := h.threads.Thread(thread)
		if ok && len(v.Turns) > 0 && v.Turns[len(v.Turns)-1].Status == status {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("thread %s: the last turn did not reach %s: %+v", thread, status, v.Turns)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func schemaOf(t *testing.T, name string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "schema-0.154", "v2", name))
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

func turnItems(turn any) []map[string]any {
	var out []map[string]any
	for _, it := range turn.(map[string]any)["items"].([]any) {
		out = append(out, it.(map[string]any))
	}
	return out
}

// A spawned subagent reads as the host's subagent thread: parentThreadId, threadSource subagent, the thread_spawn source,
// agentRole and nickname, its rollout path and the parent's cwd; the owner's turn carries the collabAgentToolCall that
// created it; the subagent is active while its turn runs and notLoaded once it ended, and still reads in full.
func TestSubagent_readsAsASubagentThreadAndIsNotLoadedAfterItsTurn(t *testing.T) {
	h := newSubagentHost(t)
	gate := make(chan struct{})
	child := h.threads.Spawn(fakehost.SubagentSpec{Parent: h.owner, ToolUseID: "exec-0001", AgentType: "reviewer", Nickname: "Scout",
		Model: "gpt-6.1-sol", Effort: "xhigh", Message: "crw-flow-op: c_abc/t1-a1\nCRW-ROLE: critic\n\nread the packet",
		RolloutPath: "/rollouts/child.jsonl", Plan: fakehost.TurnPlan{Gate: gate, Commentary: "looking", Message: `{"verdict":"pass"}`}})
	if child == "" {
		t.Fatal("Spawn made no thread")
	}
	read := h.call("thread/read", map[string]any{"threadId": child, "includeTurns": false})
	thread := read["thread"].(map[string]any)
	if thread["parentThreadId"] != h.owner || thread["threadSource"] != "subagent" || thread["agentRole"] != "reviewer" ||
		thread["agentNickname"] != "Scout" || thread["path"] != "/rollouts/child.jsonl" || thread["cwd"] != "/work/orchestrator" ||
		thread["model"] != "gpt-6.1-sol" || thread["reasoningEffort"] != "xhigh" {
		t.Errorf("subagent thread/read: %v", thread)
	}
	spawn, _ := thread["source"].(map[string]any)["subAgent"].(map[string]any)["thread_spawn"].(map[string]any)
	if spawn["parent_thread_id"] != h.owner || spawn["agent_role"] != "reviewer" || spawn["depth"] != float64(1) {
		t.Errorf("subagent source: %v", thread["source"])
	}
	if status := thread["status"].(map[string]any)["type"]; status != "active" {
		t.Errorf("a subagent whose turn runs reads %v", status)
	}
	if err := schemaOf(t, "ThreadReadResponse.json").Validate(read); err != nil {
		t.Errorf("subagent thread/read does not match the schema: %v", err)
	}
	// the owner's turn holds the spawn
	owner := h.call("thread/turns/list", map[string]any{"threadId": h.owner, "itemsView": "full"})
	var collab map[string]any
	for _, it := range turnItems(owner["data"].([]any)[0]) {
		if it["type"] == "collabAgentToolCall" {
			collab = it
		}
	}
	if collab == nil || collab["id"] != "exec-0001" || collab["tool"] != "spawnAgent" || collab["senderThreadId"] != h.owner ||
		fmt.Sprint(collab["receiverThreadIds"]) != fmt.Sprint([]any{child}) || collab["prompt"] != "crw-flow-op: c_abc/t1-a1\nCRW-ROLE: critic\n\nread the packet" ||
		collab["model"] != "gpt-6.1-sol" || collab["reasoningEffort"] != "xhigh" {
		t.Errorf("the owner's collabAgentToolCall: %v", collab)
	}
	if err := schemaOf(t, "ThreadTurnsListResponse.json").Validate(owner); err != nil {
		t.Errorf("the owner's turns do not match the schema: %v", err)
	}
	// the subagent is not in the default listing (it is not an interactive source)
	for _, entry := range h.call("thread/list", map[string]any{})["data"].([]any) {
		if entry.(map[string]any)["id"] == child {
			t.Errorf("the default listing shows the subagent")
		}
	}

	close(gate)
	h.waitStatus(child, "completed")
	read = h.call("thread/read", map[string]any{"threadId": child, "includeTurns": false})
	if status := read["thread"].(map[string]any)["status"].(map[string]any)["type"]; status != "notLoaded" {
		t.Errorf("a finished subagent reads %v, want notLoaded", status)
	}
	if err := schemaOf(t, "ThreadReadResponse.json").Validate(read); err != nil {
		t.Errorf("notLoaded subagent thread/read: %v", err)
	}
	full := h.call("thread/turns/list", map[string]any{"threadId": child, "itemsView": "full"})
	turns := full["data"].([]any)
	if len(turns) != 1 || turns[0].(map[string]any)["status"] != "completed" || turns[0].(map[string]any)["itemsView"] != "full" {
		t.Fatalf("subagent turns: %v", turns)
	}
	items := turnItems(turns[0])
	if len(items) != 3 || items[0]["type"] != "userMessage" || items[1]["phase"] != "commentary" || items[2]["phase"] != "final_answer" || items[2]["text"] != `{"verdict":"pass"}` {
		t.Errorf("subagent items: %v", items)
	}
	if err := schemaOf(t, "ThreadTurnsListResponse.json").Validate(full); err != nil {
		t.Errorf("subagent turns/list full: %v", err)
	}
	// the summary view (the default) gives the first user message and the last agent message, as the live host does
	summary := h.call("thread/turns/list", map[string]any{"threadId": child, "limit": 2})
	st := summary["data"].([]any)[0].(map[string]any)
	sitems := turnItems(st)
	if st["itemsView"] != "summary" || len(sitems) != 2 || sitems[0]["type"] != "userMessage" || sitems[1]["phase"] != "final_answer" {
		t.Errorf("subagent summary turn: %v", st)
	}
	if err := schemaOf(t, "ThreadTurnsListResponse.json").Validate(summary); err != nil {
		t.Errorf("subagent turns/list summary: %v", err)
	}
	// StayLoaded keeps a finished subagent idle (the live host reports idle while its parent keeps it loaded)
	kept := h.threads.Spawn(fakehost.SubagentSpec{Parent: h.owner, ToolUseID: "exec-0002", AgentType: "explorer", Message: "x", StayLoaded: true})
	h.waitStatus(kept, "completed")
	if status := h.call("thread/read", map[string]any{"threadId": kept})["thread"].(map[string]any)["status"].(map[string]any)["type"]; status != "idle" {
		t.Errorf("a StayLoaded subagent reads %v", status)
	}
	if v, _ := h.threads.Thread(child); !v.Subagent || v.AgentRole != "reviewer" || v.ParentThreadID != h.owner || v.RolloutPath != "/rollouts/child.jsonl" {
		t.Errorf("the thread view: %+v", v)
	}
}

// thread/turns/list lists a subagent's turns newest first unless asked otherwise, a send_input turn after the first.
func TestSubagent_turnsListNewestFirst(t *testing.T) {
	h := newSubagentHost(t)
	child := h.threads.Spawn(fakehost.SubagentSpec{Parent: h.owner, ToolUseID: "exec-0003", AgentType: "executor", Message: "first"})
	h.waitStatus(child, "completed")
	h.threads.OnTurn(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: "second answer"} })
	h.call("turn/start", map[string]any{"threadId": child, "input": []any{map[string]any{"type": "text", "text": "second"}}})
	h.waitStatus(child, "completed")
	first := func(turn any) string {
		return turnItems(turn)[0]["content"].([]any)[0].(map[string]any)["text"].(string)
	}
	desc := h.call("thread/turns/list", map[string]any{"threadId": child, "itemsView": "full"})["data"].([]any)
	asc := h.call("thread/turns/list", map[string]any{"threadId": child, "itemsView": "full", "sortDirection": "asc"})["data"].([]any)
	if len(desc) != 2 || first(desc[0]) != "second" || first(desc[1]) != "first" || first(asc[0]) != "first" {
		t.Errorf("turn order: desc %v, asc %v", desc, asc)
	}
}

// An App Server restart loses the running subagent: its turn never ends, the thread reads notLoaded, and the cut turn
// reads interrupted (or the status LostTurnStatus names); a subagent that had finished keeps its answer.
func TestSubagent_runningSubagentIsLostOnRestart(t *testing.T) {
	h := newSubagentHost(t)
	done := h.threads.Spawn(fakehost.SubagentSpec{Parent: h.owner, ToolUseID: "exec-0004", AgentType: "explorer", Message: "quick", Plan: fakehost.TurnPlan{Message: "answer"}})
	h.waitStatus(done, "completed")
	gate := make(chan struct{})
	running := h.threads.Spawn(fakehost.SubagentSpec{Parent: h.owner, ToolUseID: "exec-0005", AgentType: "executor", Message: "slow", Plan: fakehost.TurnPlan{Gate: gate, Message: "late"}})
	stuck := h.threads.Spawn(fakehost.SubagentSpec{Parent: h.owner, ToolUseID: "exec-0006", AgentType: "executor", Message: "hang", Plan: fakehost.TurnPlan{Hang: true}})
	h.waitStatus(running, "inProgress")
	h.threads.LostTurnStatus("inProgress")
	h.threads.Restart()
	close(gate) // a gate that opens after the restart ends nothing: the turn is gone with the process
	h.client = h.dial()
	time.Sleep(50 * time.Millisecond)
	for id, want := range map[string]string{running: "inProgress", stuck: "inProgress", done: "completed"} {
		read := h.call("thread/read", map[string]any{"threadId": id})["thread"].(map[string]any)
		if status := read["status"].(map[string]any)["type"]; status != "notLoaded" {
			t.Errorf("%s after the restart reads %v, want notLoaded", id, status)
		}
		turns := h.call("thread/turns/list", map[string]any{"threadId": id, "itemsView": "full"})["data"].([]any)
		if got := turns[0].(map[string]any)["status"]; got != want {
			t.Errorf("%s: the turn reads %v after the restart, want %s", id, got, want)
		}
	}
	// the default status of a cut turn is interrupted
	h2 := newSubagentHost(t)
	cut := h2.threads.Spawn(fakehost.SubagentSpec{Parent: h2.owner, ToolUseID: "exec-0007", AgentType: "executor", Message: "hang", Plan: fakehost.TurnPlan{Hang: true}})
	h2.waitStatus(cut, "inProgress")
	h2.threads.Restart()
	h2.client = h2.dial()
	turns := h2.call("thread/turns/list", map[string]any{"threadId": cut, "itemsView": "full"})
	if got := turns["data"].([]any)[0].(map[string]any)["status"]; got != "interrupted" {
		t.Errorf("a cut turn reads %v, want interrupted", got)
	}
	if err := schemaOf(t, "ThreadTurnsListResponse.json").Validate(turns); err != nil {
		t.Errorf("a cut turn: %v", err)
	}
	if v, _ := h.threads.Thread(done); len(v.Turns) != 1 || v.Turns[0].Status != "completed" {
		t.Errorf("the finished subagent changed: %+v", v.Turns)
	}
}
