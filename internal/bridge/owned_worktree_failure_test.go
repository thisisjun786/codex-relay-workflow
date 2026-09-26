package bridge

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_a_prompt_whose_frame_never_went_out_is_not_left_unknown(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "WITHHOLD"
	worktreeHost(host, input.Destination)
	b.RPC.(*appserver.Client).FailBeforeWrite("turn/start")
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "outcome_unknown" || object(receipt["initialPrompt"])["state"] != "not_sent" || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	effects := receipt["attemptedEffects"].([]string)
	if !slices.Contains(effects, "thread/start") || slices.Contains(effects, "turn/start") {
		t.Fatalf("effects=%v", effects)
	}
}

func Test_test_a_prompt_the_host_refused_is_recorded_as_refused(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "WITHHOLD"
	worktreeHost(host, input.Destination)
	host.Respond("turn/start", fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: "turn rejected"}})
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "failed" || object(receipt["initialPrompt"])["state"] != "rejected" || receipt["recoveryRequired"] != true || host.Count("turn/start") != 1 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	effects := receipt["attemptedEffects"].([]string)
	if effects[len(effects)-1] != "turn/start" {
		t.Fatalf("effects=%v", effects)
	}
}

func Test_test_replay_survives_removed_checkout_and_source_paths(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	worktreeHost(host, input.Destination)
	first, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || first["status"] != "accepted" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	if err := os.Rename(input.Destination, input.Destination+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(input.Source, input.Source+"-moved"); err != nil {
		t.Fatal(err)
	}
	replay, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || replay["threadId"] != first["threadId"] || !reflect.DeepEqual(replay["worktree"], first["worktree"]) || host.Count("thread/start") != 1 {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
}

func Test_test_known_thread_failure_retains_worktree_and_prevents_retry(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "WITHHOLD"
	host.Respond("thread/start", fakehost.Reply{Error: &fakehost.RPCError{Code: -32602, Message: "rejected"}})
	first, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || first["status"] != "failed" || first["phase"] != "creating_thread" || host.Count("turn/start") != 0 {
		t.Fatalf("first=%v err=%v", first, err)
	}
	if _, err := os.Stat(input.Destination); err != nil {
		t.Fatalf("worktree lost: %v", err)
	}
	replay, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || host.Count("thread/start") != 1 {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
	}
}

func Test_test_a_lost_project_read_leaves_no_worktree_and_keeps_the_id(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.ProjectID, input.Prompt = "project-1", "hello"
	host.Script("project/read", fakehost.Reply{Close: &fakehost.CloseFrame{Code: 1001, Reason: "lost"}})
	host.Respond("project/read", fakehost.Reply{Result: map[string]any{"project": map[string]any{"id": "project-1"}}})
	lost, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || lost["status"] != "not_attempted" || len(lost["attemptedEffects"].([]string)) != 0 || lost["recoveryRequired"] != nil {
		t.Fatalf("lost=%v err=%v", lost, err)
	}
	if _, err := os.Lstat(input.Destination); !os.IsNotExist(err) {
		t.Fatalf("worktree created: %v", err)
	}
	worktreeHost(host, input.Destination)
	start := worktreeStart(input.Destination)
	start["thread"].(map[string]any)["projectId"] = "project-1"
	host.Respond("thread/start", fakehost.Reply{Result: start})
	accepted, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || accepted["status"] != "accepted" || accepted["attempt"] != 2 || accepted["recoveryRequired"] != false {
		t.Fatalf("accepted=%v err=%v", accepted, err)
	}
	if _, err := os.Stat(input.Destination); err != nil {
		t.Fatal(err)
	}
}

func Test_test_a_known_validation_failure_keeps_its_request_id(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	if err := os.Mkdir(input.Destination, 0700); err != nil {
		t.Fatal(err)
	}
	first, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || first["status"] != "failed" || len(first["attemptedEffects"].([]string)) != 0 || !strings.Contains(text(first["error"]), "must be absent") {
		t.Fatalf("first=%v err=%v", first, err)
	}
	replay, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || replay["status"] != "failed" || host.Count("thread/start") != 0 {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
}

func Test_test_a_reserved_destination_is_an_attempt_even_with_no_request_sent(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "hello"
	worktreeHost(host, input.Destination)
	b.RPC.(*appserver.Client).FailBeforeWrite("thread/start")
	first, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || first["status"] != "outcome_unknown" || first["retrySafe"] != false || first["recoveryRequired"] != true || host.Count("thread/start") != 0 {
		t.Fatalf("first=%v err=%v", first, err)
	}
	if _, err := os.Stat(filepath.Join(input.Destination, "tracked")); err != nil {
		t.Fatal(err)
	}
	if effects := first["attemptedEffects"].([]string); len(effects) != 3 || effects[0] != "worktree/reserve" || effects[1] != "worktree/create" || effects[2] != "worktree/checkout" {
		t.Fatalf("attemptedEffects=%v", effects)
	}
	second, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || second["replayed"] != true || host.Count("thread/start") != 0 {
		t.Fatalf("replay=%v err=%v", second, err)
	}
}
