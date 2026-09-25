package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_goal_read_validates_id_and_bounds_text_without_mutation(t *testing.T) {
	b, host := testBridge(t)
	if _, err := b.GetGoal(context.Background(), " "); err == nil || host.Count("thread/goal/get") != 0 {
		t.Fatalf("invalid id: %v", err)
	}
	host.Script("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "exact objective\nwith whitespace  ", "status": "active"}}}, fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": strings.Repeat("x", 5000), "status": "active"}}})
	first, err := b.GetGoal(context.Background(), "thread-1")
	if err != nil || object(first["goal"])["objective"] != "exact objective\nwith whitespace  " {
		t.Fatalf("first=%v err=%v", first, err)
	}
	second, err := b.GetGoal(context.Background(), "thread-1")
	if err != nil || object(second["goal"])["objective"] != strings.Repeat("x", 4000)+"\n[truncated; original length 5000 characters]" || object(second["goal"])["status"] != "active" || host.Count("thread/goal/get") != 2 || host.Count("thread/goal/set") != 0 {
		t.Fatalf("second=%v err=%v calls=%v", second, err, host.Requests())
	}
}

func Test_test_invalid_create_has_no_api_effects(t *testing.T) {
	b, host := testBridge(t)
	for _, input := range []CreateThread{
		{RequestID: "valid", CWD: "relative", Sandbox: "read-only", Model: "explicit-model", Effort: "high"},
		{RequestID: "valid", CWD: "/does-not-exist-ctb", Sandbox: "read-only", Model: "explicit-model", Effort: "high"},
		{RequestID: "valid", CWD: t.TempDir(), Prompt: " ", Sandbox: "read-only", Model: "explicit-model", Effort: "high"},
		{RequestID: "valid", CWD: t.TempDir(), Sandbox: "made-up", Model: "explicit-model", Effort: "high"},
		{RequestID: "", CWD: t.TempDir(), Sandbox: "read-only", Model: "explicit-model", Effort: "high"},
	} {
		if receipt, err := b.CreateThread(context.Background(), input); err == nil {
			t.Fatalf("invalid input=%v accepted receipt=%v", input, receipt)
		}
	}
	if len(hostMethods(host)) != 0 {
		t.Fatalf("calls=%v", host.Requests())
	}
}

func Test_test_active_turn_is_derived_from_the_newest_in_progress_turn(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "inProgress"}}}})
	observed, err := b.ActiveTurn(context.Background(), "thread-1")
	if err != nil || observed["observation"] != "active" || observed["steerable"] != true || observed["activeTurnId"] != "turn-1" {
		t.Fatalf("observed=%v err=%v", observed, err)
	}
	if _, present := object(observed["status"])["turnId"]; present || host.Count("thread/resume") != 0 {
		t.Fatalf("observed=%v calls=%v", observed, host.Requests())
	}
}

func Test_test_active_turn_reports_idle_and_a_thread_with_no_turns(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Script("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "completed"}}}}, fakehost.Reply{Result: map[string]any{"data": []any{}}})
	idle, err := b.ActiveTurn(context.Background(), "thread-1")
	if err != nil || idle["observation"] != "idle" || idle["activeTurnId"] != nil || idle["newestTurnId"] != "turn-1" || idle["steerable"] != false {
		t.Fatalf("idle=%v err=%v", idle, err)
	}
	blank, err := b.ActiveTurn(context.Background(), "thread-2")
	if err != nil || blank["activeTurnId"] != nil || blank["newestTurnId"] != nil {
		t.Fatalf("blank=%v err=%v", blank, err)
	}
}
