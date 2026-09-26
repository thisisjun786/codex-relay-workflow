package bridge

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func Test_test_an_exception_is_not_evidence_that_a_pair_matches_its_roles_policy(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	policy := `{"roles":{"parent":{"model":"explicit-model","reasoningEffort":"high"}},"exceptions":{"one-task":{"model":"gpt-6-astra","reasoningEffort":"high","cwd":[` + jsonQuote(cwd) + `],"role":"parent"}}}`
	loaded, err := execution.FromBytes([]byte(policy), "test")
	if err != nil {
		t.Fatal(err)
	}
	b.Policy = loaded
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	input := SendMessage{RequestID: "excepted-send", ThreadID: "thread-1", Message: "work", Exception: "one-task", Role: "parent", Expected: map[string]any{"model": "gpt-6-astra", "reasoning_effort": "high", "cwd": cwd}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "failed" || object(receipt["rpcError"])["code"] != "unverified_pair_for_unloaded_thread" || host.Count("thread/resume") != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_send_that_names_no_role_is_not_guarded_here_and_that_boundary_is_deliberate(t *testing.T) {
	b, host := rolesBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	resume := startReply(t.TempDir())
	resume.Result["model"], resume.Result["reasoningEffort"] = pyModel, pyEffort
	host.Respond("thread/resume", resume)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	input := SendMessage{RequestID: "unrelated-send", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": pyModel, "reasoning_effort": pyEffort}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || receipt["echoIndependence"] != "not_established" {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_verified_role_pair_may_still_be_sent_to_a_thread_the_host_has_not_loaded(t *testing.T) {
	b, host := rolesBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	resume := startReply(t.TempDir())
	resume.Result["model"], resume.Result["reasoningEffort"] = parentModel, parentEffort
	host.Respond("thread/resume", resume)
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	input := SendMessage{RequestID: "verified-send", ThreadID: "thread-1", Message: "work", Role: "parent", Expected: map[string]any{"model": parentModel, "reasoning_effort": parentEffort}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || receipt["echoIndependence"] != "not_established" || host.Count("turn/start") != 1 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_a_host_that_declared_no_roles_keeps_exactly_its_previous_send_behaviour(t *testing.T) {
	b, host := testBridge(t)
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "notLoaded"}}}})
	host.Respond("thread/resume", startReply(t.TempDir()))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	input := SendMessage{RequestID: "legacy-send", ThreadID: "thread-1", Message: "work", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	receipt, err := b.SendMessageToThread(context.Background(), input)
	if err != nil || receipt["status"] != "accepted" || host.Count("turn/start") != 1 {
		t.Fatalf("receipt=%v err=%v", receipt, err)
	}
}

func Test_test_naming_no_role_produces_the_same_request_identity_as_before_roles_existed(t *testing.T) {
	// Python watches the params the bridge hands ledger.begin. The same observation here is the
	// retained row: a caller naming no role must match a fingerprint computed without the key.
	b, host := rolesBridge(t)
	cwd := t.TempDir()
	pairStart(host, cwd, pyModel, pyEffort)
	plainInput := createInput(cwd, "no-role")
	plainInput.Model, plainInput.Effort = pyModel, pyEffort
	if receipt, err := b.CreateThread(context.Background(), plainInput); err != nil || receipt["status"] != "accepted" {
		t.Fatalf("no-role=%v err=%v", receipt, err)
	}
	pairStart(host, cwd, parentModel, parentEffort)
	roleInput := plainInput
	roleInput.RequestID, roleInput.Role, roleInput.Model, roleInput.Effort = "with-role", "parent", parentModel, parentEffort
	if receipt, err := b.CreateThread(context.Background(), roleInput); err != nil || receipt["status"] != "accepted" {
		t.Fatalf("with-role=%v err=%v", receipt, err)
	}
	withoutKey := map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false, "prompt": nil, "title": nil, "model": pyModel, "reasoning_effort": pyEffort}
	if retained, err := b.Ledger.Lookup(context.Background(), "no-role", "create_thread", withoutKey, nil); err != nil || retained == nil {
		t.Fatalf("a caller naming no role was recorded with a role key: %v %v", retained, err)
	}
	named := map[string]any{"cwd": cwd, "sandbox": "read-only", "approvalPolicy": "never", "ephemeral": false, "prompt": nil, "title": nil, "model": parentModel, "reasoning_effort": parentEffort, "role": "parent"}
	if retained, err := b.Ledger.Lookup(context.Background(), "with-role", "create_thread", named, nil); err != nil || retained == nil {
		t.Fatalf("a named role was not part of the identity: %v %v", retained, err)
	}
	base := map[string]any{"cwd": "/w", "sandbox": "read-only", "model": pyModel, "reasoning_effort": pyEffort}
	plain, err := ledger.Fingerprint("create_thread", base)
	if err != nil {
		t.Fatal(err)
	}
	base["role"] = nil
	withRole, err := ledger.Fingerprint("create_thread", base)
	if err != nil || plain == withRole {
		t.Fatalf("plain=%s withRole=%s err=%v", plain, withRole, err)
	}
}
