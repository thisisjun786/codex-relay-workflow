package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

func Test_test_first_full_request_can_use_opus_and_xhigh(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	input := createInput(cwd, "opus")
	input.Model, input.Effort, input.Prompt = "anthropic/claude-opus-5", "xhigh", "Do the actual work now."
	start := startReply(cwd)
	start.Result["model"], start.Result["reasoningEffort"] = input.Model, input.Effort
	host.Respond("thread/start", start)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	receipt, err := b.CreateThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" || object(object(receipt["settings"])["actual"])["reasoningEffort"] != "xhigh" || object(hostParams(t, host, "turn/start")["input"].([]any)[0])["text"] != input.Prompt {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_an_omitted_setting_keeps_the_pre_upgrade_fingerprint(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	params := map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false, "prompt": nil, "title": nil}
	_, receipt, err := b.Ledger.Begin(context.Background(), "retained", "create_thread", params, nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt["status"], receipt["threadId"] = "accepted", "older-thread"
	if _, err := b.Ledger.Save(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	replay, err := b.CreateThread(context.Background(), CreateThread{RequestID: "retained", CWD: cwd, Sandbox: "read-only"})
	if err != nil || replay["replayed"] != true || replay["threadId"] != "older-thread" || len(hostMethods(host)) != 0 {
		t.Fatalf("replay=%v err=%v", replay, err)
	}
}

func Test_test_a_failing_annotation_cannot_downgrade_an_accepted_turn(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	input := createInput(cwd, "annot-fail")
	input.Prompt, input.Effort = "hello", "xhigh"
	start := startReply(cwd)
	start.Result["reasoningEffort"] = "xhigh"
	host.Respond("thread/start", start)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Respond("thread/read", fakehost.Reply{Error: &fakehost.RPCError{Code: -32000, Message: "nope"}})
	receipt, err := b.CreateThread(context.Background(), input)
	stored, getErr := b.GetOperation(context.Background(), input.RequestID)
	if err != nil || getErr != nil || receipt["status"] != "accepted" || receipt["turnId"] != "turn-1" || receipt["settingsAfterDispatch"] != nil || stored["status"] != "accepted" {
		t.Fatalf("receipt=%v stored=%v err=%v get=%v", receipt, stored, err, getErr)
	}
}

func Test_test_a_cancelled_annotation_propagates_instead_of_completing(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	input := createInput(cwd, "cancel-prep")
	input.Prompt = "hello"
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	paused, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	host.Respond("thread/read", fakehost.Reply{Paused: paused, Release: release})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := b.CreateThread(ctx, input); result <- err }()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("annotation not requested")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation swallowed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation hung")
	}
	stored, err := b.GetOperation(context.Background(), input.RequestID)
	if err != nil || stored["status"] != "accepted" || stored["turnId"] != "turn-1" {
		t.Fatalf("stored=%v err=%v", stored, err)
	}
}

func Test_test_an_explicit_null_declaration_is_refused_rather_than_read_as_never(t *testing.T) {
	b, host, input := settingsSend(t)
	input.Expected["approval_policy"] = nil
	_, err := b.SendMessageToThread(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "approval_policy must be one of") || len(hostMethods(host)) != 0 {
		t.Fatalf("err=%v calls=%v", err, host.Requests())
	}
}

func Test_test_an_accepted_delivery_is_not_reported_as_completed_work(t *testing.T) {
	b, host, input := settingsSend(t)
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-2", "status": "inProgress"}}}})
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || receipt["delivery"] != "turn_started" || strings.Contains(strings.Split(text(receipt["deliveryMeaning"]), ".")[0], "completed") || !strings.Contains(text(receipt["deliveryMeaning"]), "does not say the peer read it") {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
	observed, err := b.WaitThread(context.Background(), input.ThreadID, text(receipt["turnId"]), 0)
	if err != nil || object(observed["turn"])["status"] != "inProgress" || observed["timedOut"] != true {
		t.Fatalf("wait=%v err=%v", observed, err)
	}
}
