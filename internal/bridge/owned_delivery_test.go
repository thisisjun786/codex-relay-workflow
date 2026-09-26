package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func interactiveSend(t *testing.T) (*Bridge, *fakehost.Server, SendMessage) {
	t.Helper()
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	// The host test_settings.py calls honour_resume_policy: a transmitted approvalPolicy is
	// applied, so only a resume that omits it leaves the thread on-request.
	host.Handle("thread/resume", func(raw json.RawMessage) fakehost.Reply {
		var params map[string]any
		if err := json.Unmarshal(raw, &params); err != nil {
			t.Error(err)
		}
		resume := startReply(cwd)
		resume.Result["approvalPolicy"] = "on-request"
		if sent, ok := params["approvalPolicy"]; ok {
			resume.Result["approvalPolicy"] = sent
		}
		return resume
	})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-2"}}})
	return b, host, SendMessage{RequestID: "m", ThreadID: "thread-1", Message: "hello", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
}

func Test_test_transmitting_a_policy_would_have_relaxed_an_interactive_thread(t *testing.T) {
	b, host, input := interactiveSend(t)
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != "unsupported_approval_policy" || hostParams(t, host, "thread/resume")["approvalPolicy"] != nil || host.Count("turn/start") != 0 || object(object(receipt["settings"])["actual"])["approvalPolicy"] != "on-request" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_declared_delivery_still_leaves_a_setter_host_untouched(t *testing.T) {
	b, host, input := interactiveSend(t)
	input.Expected["approval_policy"] = "on-request"
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 || hostParams(t, host, "thread/resume")["approvalPolicy"] != nil {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_refused_send_is_preserved_as_undelivered(t *testing.T) {
	b, host, input := interactiveSend(t)
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "failed" || receipt["delivery"] != "not_delivered" || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	for _, effect := range receipt["attemptedEffects"].([]string) {
		if effect == "turn/start" {
			t.Fatal("message dispatched")
		}
	}
}

func Test_test_recovery_processes_one_refused_message_exactly_once(t *testing.T) {
	b, host, input := interactiveSend(t)
	input.RequestID, input.Message = "deliver-1", "parent report"
	first, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || first["status"] != "failed" || host.Count("turn/start") != 0 {
		t.Fatalf("first=%v err=%v", first, err)
	}
	for range 5 {
		replay, err := b.SendMessageToThread(context.Background(), input)
		if err != nil || replay["replayed"] != true || replay["delivery"] != "not_delivered" {
			t.Fatalf("replay=%v err=%v", replay, err)
		}
	}
	input.Expected = map[string]any{"model": "explicit-model", "reasoning_effort": "high", "approval_policy": "on-request"}
	if _, err := b.SendMessageToThread(context.Background(), input); !errors.Is(err, ledger.ErrConflict) {
		t.Fatalf("changed declaration=%v", err)
	}
	input.RequestID = "deliver-2"
	fixed, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || fixed["status"] != "accepted" || fixed["delivery"] != "turn_started" || host.Count("turn/start") != 1 {
		t.Fatalf("fixed=%v err=%v", fixed, err)
	}
	for range 5 {
		replay, err := b.SendMessageToThread(context.Background(), input)
		if err != nil || replay["replayed"] != true || replay["turnId"] != fixed["turnId"] {
			t.Fatalf("replay=%v err=%v", replay, err)
		}
	}
	if host.Count("turn/start") != 1 {
		t.Fatalf("duplicated turn: %v", host.Requests())
	}
}

func Test_test_an_approval_request_during_the_turn_is_left_undecided(t *testing.T) {
	b, host, input := interactiveSend(t)
	input.Expected["approval_policy"] = "on-request"
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-2"}}, ServerRequests: []string{"item/commandExecution/requestApproval"}})
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || len(host.ServerRequests()) == 0 || len(host.Answers()) != 0 || object(receipt["approvals"])["servicedByThisBridge"] != false || object(receipt["approvals"])["onApprovalRequest"] != "left_for_thread_approver" {
		t.Fatalf("receipt=%v err=%v raised=%v answers=%v", receipt, err, host.ServerRequests(), host.Answers())
	}
}
