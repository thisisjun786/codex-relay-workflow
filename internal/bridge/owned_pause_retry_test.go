package bridge

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_a_lost_steer_response_is_unknown_and_never_sent_again(t *testing.T) {
	b, host := activeSteer(t)
	host.Respond("turn/steer", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	lost, err := b.SteerThread(context.Background(), "lost-steer", "thread-1", "turn-1", "one instruction")
	if err != nil || lost["status"] != "outcome_unknown" || lost["clientUserMessageId"] != "steer:lost-steer" {
		t.Fatalf("lost=%v err=%v", lost, err)
	}
	replay, err := b.SteerThread(context.Background(), "lost-steer", "thread-1", "turn-1", "one instruction")
	if err != nil || replay["replayed"] != true || replay["status"] != "outcome_unknown" || host.Count("turn/steer") != 1 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
	}
}

func Test_test_a_lost_pause_response_is_unknown_and_never_sent_again(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "o", "status": "active"}}})
	host.Respond("thread/goal/set", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	lost, err := b.PauseGoal(context.Background(), "lost-pause", "thread-1")
	if err != nil || lost["status"] != "outcome_unknown" {
		t.Fatalf("lost=%v err=%v", lost, err)
	}
	replay, err := b.PauseGoal(context.Background(), "lost-pause", "thread-1")
	if err != nil || replay["replayed"] != true || host.Count("thread/goal/set") != 1 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
	}
}

func Test_test_a_lost_read_before_a_steer_is_not_an_unknown_steer(t *testing.T) {
	b, host := activeSteer(t)
	host.Script("thread/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	lost, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", "one instruction")
	if err != nil || lost["status"] != "not_attempted" || len(lost["attemptedEffects"].([]string)) != 0 || host.Count("turn/steer") != 0 {
		t.Fatalf("lost=%v err=%v calls=%v", lost, err, host.Requests())
	}
	retried, err := b.SteerThread(context.Background(), "steer", "thread-1", "turn-1", "one instruction")
	if err != nil || retried["status"] != "accepted" || retried["delivery"] != "accepted_not_applied" || host.Count("turn/steer") != 1 {
		t.Fatalf("retry=%v err=%v", retried, err)
	}
}

func Test_test_a_lost_goal_read_never_paused_anything(t *testing.T) {
	b, host := testBridge(t)
	host.Script("thread/goal/get", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	host.Respond("thread/goal/get", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "o", "status": "active"}}})
	host.Respond("thread/goal/set", fakehost.Reply{Result: map[string]any{"goal": map[string]any{"objective": "o", "status": "paused"}}})
	lost, err := b.PauseGoal(context.Background(), "pause", "thread-1")
	if err != nil || lost["status"] != "not_attempted" || len(lost["attemptedEffects"].([]string)) != 0 || host.Count("thread/goal/set") != 0 {
		t.Fatalf("lost=%v err=%v", lost, err)
	}
	retried, err := b.PauseGoal(context.Background(), "pause", "thread-1")
	if err != nil || retried["status"] != "accepted" || retried["pause"] != "goal_paused_turn_may_still_be_running" || host.Count("thread/goal/set") != 1 {
		t.Fatalf("retry=%v err=%v", retried, err)
	}
}
