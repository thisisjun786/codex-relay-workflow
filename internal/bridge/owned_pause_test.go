package bridge

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_pause_sets_status_only_and_never_claims_the_turn_stopped(t *testing.T) {
	b, host := testBridge(t)
	goal := map[string]any{"objective": "original objective", "status": "active", "tokenBudget": 100}
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": goal}})
	host.Respond("thread/goal/set", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "original objective", "status": "paused", "tokenBudget": 100}}})
	receipt, err := b.PauseGoal(context.Background(), "pause", "thread-1")
	if err != nil || receipt["status"] != "accepted" || receipt["delivery"] != "applied_by_host" || receipt["pause"] != "goal_paused_turn_may_still_be_running" || receipt["concurrency"] != "no_host_precondition_for_goal_status" || object(receipt["goalAfter"])["objective"] != "original objective" || object(receipt["goalAfter"])["status"] != "paused" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	params := hostParams(t, host, "thread/goal/set")
	if len(params) != 2 || params["threadId"] != "thread-1" || params["status"] != "paused" {
		t.Fatalf("params=%v", params)
	}
}

func Test_test_pause_then_steer_the_observed_turn_to_finish_safely(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "keep going", "status": "active"}}})
	host.Respond("thread/goal/set", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "keep going", "status": "paused"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active"}}}})
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-1", "status": "inProgress"}}}})
	host.Respond("turn/steer", fakehost.Reply{Result: map[string]any{"turnId": "turn-1"}})
	paused, err := b.PauseGoal(context.Background(), "pause", "thread-1")
	if err != nil || paused["status"] != "accepted" {
		t.Fatalf("paused=%v err=%v", paused, err)
	}
	observed, err := b.ActiveTurn(context.Background(), "thread-1")
	if err != nil || observed["observation"] != "active" || observed["activeTurnId"] != "turn-1" {
		t.Fatalf("observed=%v err=%v", observed, err)
	}
	finished, err := b.SteerThread(context.Background(), "finish", "thread-1", text(observed["activeTurnId"]), "Finish the current step safely and stop.")
	if err != nil || finished["status"] != "accepted" {
		t.Fatalf("finished=%v err=%v", finished, err)
	}
	order := []string{}
	for _, req := range host.Requests() {
		if req.Method == "thread/goal/set" || req.Method == "turn/steer" {
			order = append(order, req.Method)
		}
	}
	if strings.Join(order, ",") != "thread/goal/set,turn/steer" {
		t.Fatalf("order=%v", order)
	}
}

func Test_test_a_goal_that_moved_under_the_pause_is_a_known_failure(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change map[string]any
		code   string
	}{
		{"objective", map[string]any{"objective": "someone else rewrote this", "status": "paused", "tokenBudget": 10}, "goal_changed_under_pause"},
		{"budget", map[string]any{"objective": "original", "status": "paused", "tokenBudget": 999}, "goal_changed_under_pause"},
		{"status", map[string]any{"objective": "original", "status": "active", "tokenBudget": 10}, "goal_not_paused"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			b, host := testBridge(t)
			host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "original", "status": "active", "tokenBudget": 10}}})
			host.Respond("thread/goal/set", fakehost.Reply{Result: map[string]any{"goal": scenario.change}})
			receipt, err := b.PauseGoal(context.Background(), "pause", "thread-1")
			if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != scenario.code || object(receipt["goalBefore"])["objective"] != "original" || receipt["goalAfter"] == nil || receipt["delivery"] == "applied_by_host" || host.Count("thread/goal/set") != 1 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
		})
	}
}

func Test_test_pause_refuses_a_thread_with_no_goal(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{}})
	receipt, err := b.PauseGoal(context.Background(), "pause", "thread-1")
	if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != "no_goal" || host.Count("thread/goal/set") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func Test_test_an_already_paused_goal_is_not_written_again(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "o", "status": "paused"}}})
	receipt, err := b.PauseGoal(context.Background(), "pause", "thread-1")
	if err != nil || receipt["status"] != "accepted" || receipt["pause"] != "already_paused" || receipt["delivery"] != "no_change" || host.Count("thread/goal/set") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
	}
}

func Test_test_pause_refuses_a_goal_that_is_not_active(t *testing.T) {
	for _, status := range []string{"complete", "blocked", "usageLimited", "budgetLimited"} {
		t.Run(status, func(t *testing.T) {
			b, host := testBridge(t)
			host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "o", "status": status}}})
			receipt, err := b.PauseGoal(context.Background(), "pause", "thread-1")
			if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != "goal_not_active" || !strings.Contains(text(object(receipt["rpcError"])["message"]), status) || host.Count("thread/goal/set") != 0 {
				t.Fatalf("receipt=%v err=%v calls=%v", receipt, err, host.Requests())
			}
		})
	}
}
