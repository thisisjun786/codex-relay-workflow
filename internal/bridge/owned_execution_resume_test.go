package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

func Test_test_a_resume_that_disagrees_withholds_the_message(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change map[string]any
		code   string
	}{
		{"override", map[string]any{"model": "some-other-model"}, "settings_not_preserved"},
		{"unreported", map[string]any{"reasoningEffort": nil}, "setting_unobservable"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			b, host, input := settingsSend(t)
			resume := startReply(t.TempDir())
			for key, value := range scenario.change {
				resume.Result[key] = value
			}
			host.Respond("thread/resume", resume)
			receipt, err := b.SendMessageToThread(context.Background(), input)
			findings := object(receipt["settings"])["findings"].([]settings.Finding)
			if err != nil || receipt["status"] != "failed" || len(findings) == 0 || findings[0].Code != scenario.code || host.Count("turn/start") != 0 {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
		})
	}
}

func Test_test_a_loaded_thread_reports_its_own_pair_and_the_message_is_withheld(t *testing.T) {
	b, host, input := settingsSend(t)
	input.Expected = map[string]any{"model": "devin/swe-2", "reasoning_effort": "max"}
	actual := startReply(t.TempDir())
	host.Respond("thread/resume", actual)
	receipt, err := b.SendMessageToThread(context.Background(), input)
	findings := object(receipt["settings"])["findings"].([]settings.Finding)
	if err != nil || receipt["status"] != "failed" || receipt["statusBeforeResume"] != "idle" || len(findings) == 0 || findings[0].Code != settings.NotPreserved || receipt["echoIndependence"] != nil || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_an_unloaded_thread_never_lets_an_echo_stand_as_proof_of_preservation(t *testing.T) {
	for _, adopts := range []bool{true, false} {
		t.Run(fmt.Sprint(adopts), func(t *testing.T) {
			b, host, input := settingsSend(t)
			input.Expected = map[string]any{"model": "devin/swe-2", "reasoning_effort": "max"}
			host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
			actual := startReply(t.TempDir())
			if adopts {
				actual.Result["model"], actual.Result["reasoningEffort"] = "devin/swe-2", "max"
			}
			host.Respond("thread/resume", actual)
			receipt, err := b.SendMessageToThread(context.Background(), input)
			if err != nil || receipt["statusBeforeResume"] != "notLoaded" || receipt["echoIndependence"] != "not_established" {
				t.Fatalf("receipt=%v err=%v", receipt, err)
			}
			if adopts {
				if receipt["status"] != "accepted" || len(object(receipt["settings"])["findings"].([]settings.Finding)) != 0 {
					t.Fatalf("adopted=%v", receipt)
				}
			} else if receipt["status"] != "failed" || object(receipt["settings"])["findings"].([]settings.Finding)[0].Code != settings.NotPreserved || host.Count("turn/start") != 0 {
				t.Fatalf("not adopted=%v", receipt)
			}
		})
	}
}

func Test_test_an_exception_on_the_resume_path_must_state_its_directory(t *testing.T) {
	cwd := t.TempDir()
	b, host := guardedBridge(t, cwd)
	input := SendMessage{RequestID: "no-cwd", ThreadID: "thread-1", Message: "hello", Exception: "one-task", Expected: map[string]any{"model": "openai/gpt-6-astra", "reasoning_effort": "high"}}
	_, err := b.SendMessageToThread(context.Background(), input)
	var refusal *execution.Refusal
	if !errors.As(err, &refusal) || refusal.Code != execution.ExceptionOutOfScope || refusal.Field != "cwd" || !strings.Contains(err.Error(), "must also state its cwd") || len(hostMethods(host)) != 0 {
		t.Fatalf("refusal=%v calls=%v", err, host.Requests())
	}
	input.RequestID = "with-cwd"
	input.Expected["cwd"] = cwd
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	resume := startReply(cwd)
	resume.Result["model"] = "openai/gpt-6-astra"
	host.Respond("thread/resume", resume)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || object(receipt["executionPolicy"])["exception"] != "one-task" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_resume_for_the_wrong_role_never_reads_or_resumes_the_thread(t *testing.T) {
	b, host := rolesBridge(t)
	input := SendMessage{RequestID: "send-wrong-role", ThreadID: "thread-1", Message: "work", Role: "parent", Expected: map[string]any{"model": supersededM, "reasoning_effort": supersededEff}}
	_, err := b.SendMessageToThread(context.Background(), input)
	var refusal *execution.Refusal
	if !errors.As(err, &refusal) || refusal.Code != execution.RoleMismatch || len(hostMethods(host)) != 0 {
		t.Fatalf("err=%v calls=%v", err, host.Requests())
	}
	if _, err := b.GetOperation(context.Background(), input.RequestID); err == nil {
		t.Fatal("refusal wrote ledger row")
	}
}

func Test_test_a_named_supervisor_the_host_has_not_loaded_is_not_resumed_at_all(t *testing.T) {
	b, host := testBridge(t)
	policy := `{"roles":{"supervisor":{"expectation":"record"}}}`
	loaded, err := execution.FromBytes([]byte(policy), "test")
	if err != nil {
		t.Fatal(err)
	}
	b.Policy = loaded
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	input := SendMessage{RequestID: "supervisor-send", ThreadID: "thread-1", Message: "work", Role: "supervisor", Expected: map[string]any{"model": "gpt-6-astra", "reasoning_effort": "high"}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != "unverified_pair_for_unloaded_thread" || host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}
