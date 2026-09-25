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

func settingsSend(t *testing.T) (*Bridge, *fakehost.Server, SendMessage) {
	t.Helper()
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(t.TempDir()))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-2"}}})
	return b, host, SendMessage{RequestID: "same", ThreadID: "thread-1", Message: "hello", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
}

// carriedSend is test_settings.py carried(): a workspace-write thread whose resume states the
// full policy, sent with the sandbox contract as well as the pair.
func carriedSend(t *testing.T) (*Bridge, *fakehost.Server, SendMessage) {
	t.Helper()
	b, host, input := settingsSend(t)
	policy := map[string]any{"type": "workspaceWrite", "writableRoots": []any{}, "networkAccess": false, "excludeTmpdirEnvVar": false, "excludeSlashTmp": false}
	resume := startReply(t.TempDir())
	resume.Result["sandbox"] = policy
	host.Respond("thread/resume", resume)
	input.Expected["sandbox"], input.Expected["expected_sandbox_policy"] = "workspace-write", policy
	return b, host, input
}

func annotationEqual(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func Test_test_a_clean_resume_starts_its_turn_with_no_overrides(t *testing.T) {
	b, host, input := carriedSend(t)
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || object(receipt["settings"])["verification"] != "observed_at_resume" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	params := hostParams(t, host, "turn/start")
	if len(params) != 2 || params["threadId"] != input.ThreadID || object(params["input"].([]any)[0])["text"] != "hello" {
		t.Fatalf("params=%v", params)
	}
}

func Test_test_a_reused_id_replays_without_dispatching_again(t *testing.T) {
	b, host, input := carriedSend(t)
	first, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || first["status"] != "accepted" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	second, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || second["replayed"] != true || second["turnId"] != first["turnId"] || host.Count("turn/start") != 1 {
		t.Fatalf("second=%v err=%v calls=%v", second, err, host.Requests())
	}
}

func Test_test_a_reused_id_with_changed_settings_is_rejected(t *testing.T) {
	b, host, input := carriedSend(t)
	if _, err := b.SendMessageToThread(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	input.Expected["model"] = "openai/gpt-5.6-sol"
	_, err := b.SendMessageToThread(context.Background(), input)
	if !errors.Is(err, ledger.ErrConflict) || host.Count("turn/start") != 1 {
		t.Fatalf("err=%v calls=%v", err, host.Requests())
	}
}

func Test_test_a_misspelled_setting_key_is_refused_not_discarded(t *testing.T) {
	b, host, input := settingsSend(t)
	for _, wrong := range []map[string]any{{"reasoningEffort": "xhigh"}, {"model": "explicit-model", "sandboxPolicy": map[string]any{}}} {
		input.Expected = wrong
		if receipt, err := b.SendMessageToThread(context.Background(), input); err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Fatalf("accepted %v: %v %v", wrong, receipt, err)
		}
	}
	if len(hostMethods(host)) != 0 {
		t.Fatalf("calls=%v", host.Requests())
	}
}

func Test_test_a_replay_answers_from_the_ledger_without_touching_the_host(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	input := createInput(cwd, "replayed")
	input.Prompt, input.Effort = "hello", "xhigh"
	started := startReply(cwd)
	started.Result["reasoningEffort"] = "xhigh"
	host.Respond("thread/start", started)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"cwd": cwd, "model": input.Model, "reasoningEffort": "xhigh"}}})
	first, err := b.CreateThread(context.Background(), input)
	if err != nil || first["status"] != "accepted" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	if object(first["settingsAfterDispatch"])["concurrentChange"] != false {
		t.Fatalf("annotation=%v", first)
	}
	before := len(host.Requests())
	replay, err := b.CreateThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || len(host.Requests()) != before || !annotationEqual(replay["settingsAfterDispatch"], first["settingsAfterDispatch"]) {
		t.Fatalf("replay=%v err=%v calls=%v", replay, err, host.Requests())
	}
}

func Test_test_a_replay_is_answered_even_when_the_host_has_gone_away(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	input := createInput(cwd, "offline")
	input.Prompt, input.Effort = "hello", "xhigh"
	started := startReply(cwd)
	started.Result["reasoningEffort"] = "xhigh"
	host.Respond("thread/start", started)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"cwd": cwd, "model": input.Model, "reasoningEffort": "xhigh"}}})
	first, err := b.CreateThread(context.Background(), input)
	if err != nil || first["status"] != "accepted" {
		t.Fatalf("first=%v err=%v", first, err)
	}
	host.Close()
	replay, err := b.CreateThread(context.Background(), input)
	if err != nil || replay["replayed"] != true || replay["turnId"] != first["turnId"] || !annotationEqual(replay["settingsAfterDispatch"], first["settingsAfterDispatch"]) {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
}
