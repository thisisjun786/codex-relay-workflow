package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_active_turn_reports_non_runnable_status_without_raising(t *testing.T) {
	for _, kind := range []string{"notLoaded", "systemError"} {
		t.Run(kind, func(t *testing.T) {
			b, host := testBridge(t)
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": kind}}}})
			host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "completed"}}}})
			observed, err := b.ActiveTurn(context.Background(), "thread-1")
			if err != nil || observed["observation"] != kind || observed["activeTurnId"] != nil || observed["steerable"] != false {
				t.Fatalf("observed=%v err=%v", observed, err)
			}
		})
	}
}

func Test_test_active_turn_names_a_disagreement_between_status_and_turns(t *testing.T) {
	b, host := testBridge(t)
	host.Script("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}}, fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Script("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "completed"}}}}, fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-2", "status": "inProgress"}}}})
	stale, err := b.ActiveTurn(context.Background(), "thread-1")
	if err != nil || stale["observation"] != "active_without_in_progress_turn" || stale["activeTurnId"] != nil || stale["steerable"] != false {
		t.Fatalf("stale=%v err=%v", stale, err)
	}
	behind, err := b.ActiveTurn(context.Background(), "thread-1")
	if err != nil || behind["observation"] != "in_progress_turn_without_active_status" || behind["activeTurnId"] != "turn-2" || behind["steerable"] != false {
		t.Fatalf("behind=%v err=%v", behind, err)
	}
}

func Test_test_steer_reaches_the_guarded_turn_and_claims_only_acceptance(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}})
	host.Respond("turn/steer", fakehost.Reply{Result: map[string]any{"turnId": "turn-1"}})
	receipt, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", "narrow the scope")
	if err != nil || receipt["status"] != "accepted" || receipt["delivery"] != "accepted_not_applied" || receipt["steeredTurnId"] != "turn-1" || object(receipt["settings"])["verification"] != "not_observable" || !strings.Contains(text(receipt["deliveryMeaning"]), "does not say the peer read it") || !strings.Contains(text(receipt["deliveryMeaning"]), "does not say the peer acted on it") {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	params := hostParams(t, host, "turn/steer")
	if params["expectedTurnId"] != "turn-1" || object(params["input"].([]any)[0])["type"] != "text" || object(params["input"].([]any)[0])["text"] != "narrow the scope" || params["model"] != nil || params["config"] != nil || host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 || hasMethodPrefix(host, "thread/goal/") || len(params["input"].([]any)) != 1 || len(object(params["input"].([]any)[0])) != 2 {
		t.Fatalf("params=%v calls=%v", params, host.Requests())
	}
}

func Test_test_steer_refuses_each_non_active_status_by_name(t *testing.T) {
	for kind, code := range map[string]string{"idle": "thread_idle", "notLoaded": "thread_not_loaded", "systemError": "thread_system_error"} {
		t.Run(kind, func(t *testing.T) {
			b, host := testBridge(t)
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": kind}}}})
			receipt, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", "stop")
			if err != nil || receipt["status"] != "failed" || host.Count("turn/steer") != 0 || object(receipt["rpcError"])["code"] != code || (strings.Contains(text(object(receipt["rpcError"])["message"]), "send_message_to_thread") != (kind == "idle")) {
				t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
			}
		})
	}
}
