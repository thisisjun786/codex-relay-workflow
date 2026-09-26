package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

func Test_test_environment_mismatch_retains_actual_receipt_and_withholds_prompt(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change map[string]any
	}{
		{"cwd", map[string]any{"cwd": "/wrong"}},
		{"roots", map[string]any{"runtimeWorkspaceRoots": []any{"/wrong"}}},
		{"approval", map[string]any{"approvalPolicy": "on-request"}},
		{"sandbox", map[string]any{"sandbox": map[string]any{"type": "readOnly", "networkAccess": true}}},
		{"model", map[string]any{"model": "different"}},
		{"effort", map[string]any{"reasoningEffort": "different"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			b, host := testBridge(t)
			input := worktreeInput(t)
			input.Prompt = "WITHHOLD"
			input.Model = "expected"
			created := worktreeStart(input.Destination)
			created["model"] = input.Model
			for key, value := range scenario.change {
				created[key] = value
			}
			host.Respond("thread/start", fakehost.Reply{Result: created})
			receipt, err := b.CreateWorktreeThread(context.Background(), input)
			if err != nil || receipt["status"] != "failed" || receipt["recoveryRequired"] != true || object(receipt["initialPrompt"])["state"] != "not_sent" || host.Count("turn/start") != 0 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			for key, value := range scenario.change {
				if key == "runtimeWorkspaceRoots" {
					value = []string{"/wrong"}
				}
				if !reflect.DeepEqual(object(receipt["creation"])[key], value) {
					t.Fatalf("creation %s=%v want %v", key, object(receipt["creation"])[key], value)
				}
			}
			if _, err := os.Stat(filepath.Join(input.Destination, "tracked")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func Test_test_checkout_change_during_thread_start_withholds_prompt(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "WITHHOLD"
	paused, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	host.Respond("thread/start", fakehost.Reply{Result: worktreeStart(input.Destination), Paused: paused, Release: release})
	result := make(chan map[string]any, 1)
	go func() { receipt, _ := b.CreateWorktreeThread(context.Background(), input); result <- receipt }()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("thread/start never arrived")
	}
	gitAt(t, input.Destination, "checkout", "-b", "unexpected")
	close(release)
	select {
	case receipt := <-result:
		if receipt["status"] != "failed" || receipt["threadId"] != "thread-1" || object(receipt["checkoutBeforeDispatch"])["detached"] != false || host.Count("turn/start") != 0 {
			t.Fatalf("receipt=%v calls=%v", receipt, host.Requests())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("launch did not finish")
	}
}

func Test_test_a_worktree_exception_covers_only_the_destination_it_names(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	policy := `{"allowed":[{"model":"explicit-model","efforts":["high"]}],"exceptions":{"this-worktree":{"model":"openai/gpt-6-astra","reasoningEffort":"high","cwd":[` + jsonQuote(input.Destination) + `]}}}`
	loaded, err := execution.FromBytes([]byte(policy), "test")
	if err != nil {
		t.Fatal(err)
	}
	b.Policy = loaded
	input.Model, input.Effort, input.Exception = "openai/gpt-6-astra", "high", "this-worktree"
	input.Destination = filepath.Join(filepath.Dir(input.Destination), "somewhere-else")
	_, err = b.CreateWorktreeThread(context.Background(), input)
	var refusal *execution.Refusal
	if !errors.As(err, &refusal) || refusal.Code != execution.ExceptionOutOfScope || len(hostMethods(host)) != 0 {
		t.Fatalf("err=%v calls=%v", err, host.Requests())
	}
	input.Destination = filepath.Join(filepath.Dir(input.Destination), "isolated")
	start := worktreeStart(input.Destination)
	start["model"] = input.Model
	host.Respond("thread/start", fakehost.Reply{Result: start})
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || hostParams(t, host, "thread/start")["model"] != input.Model || object(hostParams(t, host, "thread/start")["config"])["model_reasoning_effort"] != input.Effort || object(receipt["executionPolicy"])["exception"] != "this-worktree" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_worktree_launch_transmits_the_pair_it_was_authorized_for(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "work"
	worktreeHost(host, input.Destination)
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	start := hostParams(t, host, "thread/start")
	if start["model"] != input.Model || object(start["config"])["model_reasoning_effort"] != input.Effort || object(receipt["executionPolicy"])["model"] != input.Model || object(receipt["executionPolicy"])["reasoningEffort"] != input.Effort || object(object(receipt["settings"])["requested"])["model"] != input.Model || object(object(receipt["settings"])["requested"])["reasoningEffort"] != input.Effort || object(receipt["creation"])["model"] != input.Model || object(receipt["settings"])["verification"] != "observed_at_creation" {
		t.Fatalf("receipt=%v start=%v", receipt, start)
	}
}

func Test_test_a_dispatched_worktree_task_is_annotated_like_the_other_paths(t *testing.T) {
	b, host := testBridge(t)
	input := worktreeInput(t)
	input.Prompt = "do the work"
	worktreeHost(host, input.Destination)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"cwd": input.Destination, "model": input.Model, "reasoningEffort": input.Effort}}})
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" || object(receipt["settings"])["verification"] != "observed_at_creation" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	annotation := object(receipt["settingsAfterDispatch"])
	if annotation["concurrentChange"] != false || !reflect.DeepEqual(annotation["covers"], []string{"cwd", "model", "reasoningEffort"}) || len(annotation["unobserved"].([]string)) != 0 {
		t.Fatalf("annotation=%v", annotation)
	}
}
