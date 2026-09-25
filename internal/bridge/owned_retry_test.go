package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func Test_test_list_preserves_long_cursor(t *testing.T) {
	b, host := testBridge(t)
	cursor := "cursor:" + strings.Repeat("x", 5000)
	host.Script("thread/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "thread-2"}}, "nextCursor": cursor}}, fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "thread-1"}}}})
	first, err := b.ListThreads(context.Background(), "", 1, nil)
	if err != nil || first["nextCursor"] != cursor {
		t.Fatalf("first=%v err=%v", first, err)
	}
	second, err := b.ListThreads(context.Background(), "", 1, cursor)
	if err != nil || object(first["data"].([]any)[0])["id"] == object(second["data"].([]any)[0])["id"] || hostParams(t, host, "thread/list")["limit"] != float64(1) {
		t.Fatalf("second=%v err=%v calls=%v", second, err, host.Requests())
	}
	var params map[string]any
	count := 0
	for _, req := range host.Requests() {
		if req.Method == "thread/list" {
			count++
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatal(err)
			}
		}
	}
	if count != 2 || params["cursor"] != cursor {
		t.Fatalf("last cursor=%v requests=%v", params, host.Requests())
	}
}

func Test_test_a_lost_read_before_a_message_leaves_the_request_id_usable(t *testing.T) {
	b, host := testBridge(t)
	host.Script("thread/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	message := SendMessage{RequestID: "message", ThreadID: "thread-1", Message: "instruction", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	lost, err := b.SendMessageToThread(context.Background(), message)
	if err != nil || lost["status"] != "not_attempted" || lost["retrySafe"] != true || len(lost["attemptedEffects"].([]string)) != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("lost=%v err=%v calls=%v", lost, err, host.Requests())
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(t.TempDir()))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-2"}}})
	retried, err := b.SendMessageToThread(context.Background(), message)
	if err != nil || retried["status"] != "accepted" || retried["replayed"] == true || fmt.Sprint(retried["attempt"]) != "2" || host.Count("turn/start") != 1 {
		t.Fatalf("retry=%v err=%v calls=%v", retried, err, host.Requests())
	}
	stored, err := b.GetOperation(context.Background(), "message")
	if err != nil || object(stored["priorAttempts"].([]any)[0])["status"] != "not_attempted" {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
}

func Test_test_a_lost_project_read_never_created_a_thread(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Script("project/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	host.Respond("project/read", fakehost.Reply{Result: map[string]any{"project": map[string]any{"id": "project-1"}}})
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	input := createInput(cwd, "project")
	input.ProjectID = "project-1"
	input.Prompt = "hello"
	lost, err := b.CreateThread(context.Background(), input)
	if err != nil || lost["status"] != "not_attempted" || len(lost["attemptedEffects"].([]string)) != 0 || host.Count("thread/start") != 0 {
		t.Fatalf("lost=%v err=%v calls=%v", lost, err, host.Requests())
	}
	accepted, err := b.CreateThread(context.Background(), input)
	if err != nil || accepted["status"] != "accepted" || host.Count("thread/start") != 1 {
		t.Fatalf("accepted=%v err=%v calls=%v", accepted, err, host.Requests())
	}
}

func Test_test_a_request_that_never_reached_a_socket_can_be_retried(t *testing.T) {
	host := fakehost.Start(t)
	store, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	absent := appserver.New(filepath.Join(t.TempDir(), "absent.sock"), appserver.DefaultBounds)
	input := createInput(t.TempDir(), "offline")
	input.Prompt = "hello"
	lost, err := New(absent, store, executionPolicy()).CreateThread(context.Background(), input)
	if err != nil || lost["status"] != "not_attempted" || len(lost["attemptedEffects"].([]string)) != 0 || host.Count("thread/start") != 0 {
		t.Fatalf("lost=%v err=%v", lost, err)
	}
	client, err := appserver.Dial(context.Background(), host.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	host.Respond("thread/start", startReply(input.CWD))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	accepted, err := New(client, store, executionPolicy()).CreateThread(context.Background(), input)
	if err != nil || accepted["status"] != "accepted" || host.Count("thread/start") != 1 {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
}

func Test_test_a_lost_handshake_is_not_an_unknown_creation(t *testing.T) {
	host := fakehost.Start(t)
	client := appserver.New(host.SocketPath, appserver.DefaultBounds)
	t.Cleanup(func() { _ = client.Close() })
	store, err := ledger.Open(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	b := New(client, store, executionPolicy())
	cwd := t.TempDir()
	input := createInput(cwd, "handshake")
	input.Prompt = "hello"
	host.Script("initialize", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	lost, err := b.CreateThread(context.Background(), input)
	if err != nil || lost["status"] != "not_attempted" || len(lost["attemptedEffects"].([]string)) != 0 || host.Count("thread/start") != 0 {
		t.Fatalf("lost=%v err=%v calls=%v", lost, err, host.Requests())
	}
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	accepted, err := b.CreateThread(context.Background(), input)
	if err != nil || accepted["status"] != "accepted" || host.Count("thread/start") != 1 {
		t.Fatalf("accepted=%v err=%v calls=%v", accepted, err, host.Requests())
	}
}

func Test_test_a_lost_resume_response_is_unknown_and_never_resent(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	created := createInput(cwd, "create")
	created.Prompt = "work"
	if first, err := b.CreateThread(context.Background(), created); err != nil || first["status"] != "accepted" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	input := SendMessage{RequestID: "resume-lost", ThreadID: "thread-1", Message: "instruction", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	lost, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || lost["status"] != "outcome_unknown" || lost["retrySafe"] != false || len(lost["attemptedEffects"].([]string)) != 1 || lost["attemptedEffects"].([]string)[0] != "thread/resume" {
		t.Fatalf("lost=%v err=%v calls=%v", lost, err, host.Requests())
	}
	replay, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || replay["status"] != "outcome_unknown" || host.Count("thread/resume") != 1 || host.Count("turn/start") != 1 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
	}
}

func Test_test_a_lost_message_turn_response_is_unknown_and_never_resent(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	created := createInput(cwd, "create")
	created.Prompt = "work"
	if first, err := b.CreateThread(context.Background(), created); err != nil || first["status"] != "accepted" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	input := SendMessage{RequestID: "turn-lost", ThreadID: "thread-1", Message: "instruction", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	lost, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || lost["status"] != "outcome_unknown" || strings.Join(lost["attemptedEffects"].([]string), ",") != "thread/resume,turn/start" {
		t.Fatalf("lost=%v err=%v calls=%v", lost, err, host.Requests())
	}
	replay, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || host.Count("turn/start") != 2 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
	}
}

func Test_test_ledger_survives_restarts_and_is_private(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "state.sqlite3")
	first, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh, receipt, err := first.Begin(context.Background(), "key", "create", map[string]any{"cwd": "/example"}, nil)
	if err != nil || !fresh {
		t.Fatalf("begin=%v %v", fresh, err)
	}
	receipt["threadId"] = "known-id"
	if _, err := first.Save(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	fresh, receipt, err = second.Begin(context.Background(), "key", "create", map[string]any{"cwd": "/example"}, nil)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || fresh || receipt["threadId"] != "known-id" || receipt["status"] != "in_progress_or_unknown" || info.Mode().Perm() != 0600 {
		t.Fatalf("fresh=%v receipt=%v err=%v stat=%v", fresh, receipt, err, statErr)
	}
}
