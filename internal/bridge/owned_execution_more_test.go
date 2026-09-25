package bridge

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

func Test_test_naming_no_role_leaves_the_request_identical_to_one_made_before_roles_existed(t *testing.T) {
	b, host := rolesBridge(t)
	cwd := t.TempDir()
	input := createInput(cwd, "stable")
	input.Model, input.Effort, input.Prompt = pyModel, pyEffort, "work"
	pairStart(host, cwd, pyModel, pyEffort)
	first, err := b.CreateThread(context.Background(), input)
	if err != nil || first["status"] != "accepted" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	replayed, err := b.CreateThread(context.Background(), input)
	if err != nil || replayed["replayed"] != true || host.Count("thread/start") != 1 {
		t.Fatalf("replay=%v err=%v", replayed, err)
	}
}

func Test_test_an_authorized_pair_is_the_one_transmitted_and_the_one_compared(t *testing.T) {
	cwd := t.TempDir()
	b, host := guardedBridge(t, cwd)
	input := createInput(cwd, "excepted")
	input.Model, input.Effort, input.Exception, input.Prompt = pyUnapproved, "high", "one-task", "work"
	pairStart(host, cwd, pyUnapproved, "high")
	receipt, err := b.CreateThread(context.Background(), input)
	start := hostParams(t, host, "thread/start")
	policy, requested := object(receipt["executionPolicy"]), object(object(receipt["settings"])["requested"])
	if err != nil || receipt["status"] != "accepted" || start["model"] != pyUnapproved || object(start["config"])["model_reasoning_effort"] != "high" || policy["mode"] != "allowlist" || policy["exception"] != "one-task" || policy["model"] != pyUnapproved || policy["reasoningEffort"] != "high" || requested["model"] != pyUnapproved || requested["reasoningEffort"] != "high" || object(receipt["settings"])["verification"] != "observed_at_creation" || host.Count("turn/start") != 1 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_an_approved_pair_records_the_mode_it_was_approved_under(t *testing.T) {
	cwd := t.TempDir()
	b, host := guardedBridge(t, cwd)
	input := createInput(cwd, "approved")
	input.Model, input.Effort, input.Prompt = pyModel, pyEffort, "work"
	pairStart(host, cwd, pyModel, pyEffort)
	receipt, err := b.CreateThread(context.Background(), input)
	start := hostParams(t, host, "thread/start")
	policy := object(receipt["executionPolicy"])
	if err != nil || receipt["status"] != "accepted" || start["model"] != pyModel || object(start["config"])["model_reasoning_effort"] != pyEffort || len(policy) != 7 || policy["mode"] != "allowlist" || policy["digest"] == nil || policy["exception"] != nil || policy["role"] != nil || policy["model"] != pyModel || policy["reasoningEffort"] != pyEffort || policy["limits"] != execution.Limits || host.Count("turn/start") != 1 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_resume_carries_the_authorized_pair_and_is_checked_against_it(t *testing.T) {
	cwd := t.TempDir()
	b, host := guardedBridge(t, cwd)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	resume := startReply(cwd)
	resume.Result["model"], resume.Result["reasoningEffort"] = pyModel, pyEffort
	host.Respond("thread/resume", resume)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	input := SendMessage{RequestID: "m", ThreadID: "thread-1", Message: "hello", Expected: map[string]any{"model": pyModel, "reasoning_effort": pyEffort}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	params := hostParams(t, host, "thread/resume")
	policy := object(receipt["executionPolicy"])
	if err != nil || receipt["status"] != "accepted" || params["model"] != pyModel || object(params["config"])["model_reasoning_effort"] != pyEffort || policy["model"] != pyModel || policy["reasoningEffort"] != pyEffort || object(receipt["settings"])["verification"] != "observed_at_resume" || host.Count("turn/start") != 1 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_an_unapproved_pair_is_refused_on_the_resume_path_too(t *testing.T) {
	b, host := guardedBridge(t, t.TempDir())
	input := SendMessage{RequestID: "blocked", ThreadID: "thread-1", Message: "hello", Expected: map[string]any{"model": pyUnapproved, "reasoning_effort": "high"}}
	_, err := b.SendMessageToThread(context.Background(), input)
	var refusal *execution.Refusal
	if !errors.As(err, &refusal) || refusal.Code != execution.NotAllowed || len(hostMethods(host)) != 0 {
		t.Fatalf("err=%v calls=%v", err, host.Requests())
	}
}
