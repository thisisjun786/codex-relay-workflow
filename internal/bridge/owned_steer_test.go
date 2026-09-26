package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func activeSteer(t *testing.T) (*Bridge, *fakehost.Server) {
	t.Helper()
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}})
	host.Respond("turn/steer", fakehost.Reply{Result: map[string]any{"turnId": "turn-1"}})
	return b, host
}

func Test_test_steer_with_a_stale_turn_id_fails_and_does_not_retarget(t *testing.T) {
	b, host := activeSteer(t)
	host.Respond("turn/steer", fakehost.Reply{ErrorObject: map[string]any{"code": "expected_turn_mismatch", "message": "turn moved on"}})
	receipt, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-0", "late instruction")
	if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != "expected_turn_mismatch" || receipt["delivery"] != nil || host.Count("turn/steer") != 1 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func Test_test_a_turn_id_we_did_not_guard_is_a_failure_not_an_acceptance(t *testing.T) {
	b, host := activeSteer(t)
	host.Respond("turn/steer", fakehost.Reply{Result: map[string]any{"turnId": "turn-99"}})
	receipt, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", "scope change")
	if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != "steered_turn_mismatch" || receipt["steeredTurnId"] != "turn-99" || receipt["expectedTurnId"] != "turn-1" || receipt["delivery"] == "accepted_not_applied" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_an_uncertain_steer_replays_and_is_reconciled_by_record(t *testing.T) {
	b, host := activeSteer(t)
	receipt, err := b.SteerThread(context.Background(), "steer-once", "thread-1", "turn-1", "one instruction")
	if err != nil || receipt["clientUserMessageId"] != "steer:steer-once" || hostParams(t, host, "turn/steer")["clientUserMessageId"] != "steer:steer-once" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	replay, err := b.SteerThread(context.Background(), "steer-once", "thread-1", "turn-1", "one instruction")
	if err != nil || replay["replayed"] != true || host.Count("turn/steer") != 1 {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
	stored, err := b.GetOperation(context.Background(), "steer-once")
	if err != nil || stored["clientUserMessageId"] != "steer:steer-once" {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
}

func Test_test_the_same_request_id_cannot_be_aimed_at_another_turn(t *testing.T) {
	b, host := activeSteer(t)
	if _, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", "first"); err != nil {
		t.Fatal(err)
	}
	_, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-2", "first")
	if !errors.Is(err, ledger.ErrConflict) || host.Count("turn/steer") != 1 {
		t.Fatalf("err=%v calls=%v", err, host.Requests())
	}
}

func Test_test_steer_reports_an_unsupported_method_without_claiming_a_host_wide_gap(t *testing.T) {
	b, host := activeSteer(t)
	host.Respond("turn/steer", fakehost.Reply{Error: &fakehost.RPCError{Code: -32601, Message: "turn/steer"}})
	receipt, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", "instruction")
	if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != json.Number("-32601") || host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func Test_test_steer_rejects_empty_and_oversized_input_before_any_call(t *testing.T) {
	b, host := activeSteer(t)
	for _, message := range []string{"", "   ", strings.Repeat("x", 100001)} {
		if receipt, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", message); err == nil {
			t.Fatalf("accepted %d bytes: %v", len(message), receipt)
		}
	}
	if host.Count("thread/read") != 0 || host.Count("turn/steer") != 0 {
		t.Fatalf("calls=%v", host.Requests())
	}
}
