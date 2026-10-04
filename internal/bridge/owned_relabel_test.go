package bridge

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const relabelModel = "relabelled/model"
const relabelEffort = "relabelled-effort"

type relabellingPolicy struct{}

func (relabellingPolicy) Summary() map[string]any {
	return map[string]any{"mode": "allowlist", "digest": "stub"}
}
func (relabellingPolicy) Role(string) (execution.Role, bool) { return execution.Role{}, false }
func (relabellingPolicy) Authorize(in execution.Input) (execution.Authorized, error) {
	return execution.Authorized{Model: relabelModel, Effort: relabelEffort, Receipt: map[string]any{"mode": "allowlist", "digest": "stub", "model": relabelModel, "reasoningEffort": relabelEffort}}, nil
}

func Test_test_the_launch_and_the_comparison_follow_the_authorization_not_the_arguments(t *testing.T) {
	b, host := testBridge(t)
	b.Policy = relabellingPolicy{}
	cwd := t.TempDir()
	input := createInput(cwd, "relabelled")
	input.Prompt = "work"
	start := startReply(cwd)
	start.Result["model"], start.Result["reasoningEffort"] = relabelModel, relabelEffort
	host.Respond("thread/start", start)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", start)
	created, err := b.CreateThread(context.Background(), input)
	if err != nil || created["status"] != "accepted" || hostParams(t, host, "thread/start")["model"] != relabelModel || pyjson.Map(hostParams(t, host, "thread/start")["config"])["model_reasoning_effort"] != relabelEffort || pyjson.Map(pyjson.Map(created["settings"])["requested"])["model"] != relabelModel || pyjson.Map(pyjson.Map(created["settings"])["requested"])["reasoningEffort"] != relabelEffort || pyjson.Map(created["executionPolicy"])["model"] != relabelModel {
		t.Fatalf("created=%v err=%v", created, err)
	}
	message := SendMessage{RequestID: "relabelled-send", ThreadID: "thread-1", Message: "again", Expected: map[string]any{"model": input.Model, "reasoning_effort": input.Effort}}
	delivered, err := b.SendMessageToThread(context.Background(), message)
	if err != nil || delivered["status"] != "accepted" || hostParams(t, host, "thread/resume")["model"] != relabelModel || pyjson.Map(hostParams(t, host, "thread/resume")["config"])["model_reasoning_effort"] != relabelEffort || pyjson.Map(delivered["executionPolicy"])["model"] != relabelModel {
		t.Fatalf("delivered=%v err=%v", delivered, err)
	}
}

func Test_test_a_worktree_launch_follows_the_authorization_not_the_arguments(t *testing.T) {
	b, host := testBridge(t)
	b.Policy = relabellingPolicy{}
	input := worktreeInput(t)
	input.Prompt = "work"
	start := worktreeStart(input.Destination)
	start["model"], start["reasoningEffort"] = relabelModel, relabelEffort
	host.Respond("thread/start", fakehost.Reply{Result: start})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	receipt, err := b.CreateWorktreeThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || hostParams(t, host, "thread/start")["model"] != relabelModel || pyjson.Map(hostParams(t, host, "thread/start")["config"])["model_reasoning_effort"] != relabelEffort || pyjson.Map(pyjson.Map(receipt["settings"])["requested"])["model"] != relabelModel || pyjson.Map(pyjson.Map(receipt["settings"])["requested"])["reasoningEffort"] != relabelEffort || pyjson.Map(receipt["executionPolicy"])["model"] != relabelModel {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}
