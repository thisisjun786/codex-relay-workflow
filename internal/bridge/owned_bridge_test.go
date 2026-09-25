package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

func startReply(cwd string) fakehost.Reply {
	return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1"}, "cwd": cwd, "model": "explicit-model", "reasoningEffort": "high", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly"}}}
}
func createInput(cwd, id string) CreateThread {
	return CreateThread{RequestID: id, CWD: cwd, Sandbox: "read-only", Model: "explicit-model", Effort: "high"}
}
func hostParams(t *testing.T, host *fakehost.Server, method string) map[string]any {
	t.Helper()
	for _, req := range host.Requests() {
		if req.Method == method {
			var params map[string]any
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatal(err)
			}
			return params
		}
	}
	t.Fatalf("no %s request", method)
	return nil
}

// hostMethods lists what the bridge asked the host, without the connection handshake, which is
// what the Python fake's calls list records.
func hostMethods(host *fakehost.Server) []string {
	methods := []string{}
	for _, req := range host.Requests() {
		if req.Method != "initialize" && req.Method != "initialized" {
			methods = append(methods, req.Method)
		}
	}
	return methods
}
func hasMethodPrefix(host *fakehost.Server, prefix string) bool {
	return slices.ContainsFunc(hostMethods(host), func(m string) bool { return strings.HasPrefix(m, prefix) })
}

func Test_test_create_and_followup_carry_the_stated_pair_and_exact_messages(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("thread/name/set", fakehost.Reply{})
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-2"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(cwd))
	host.Respond("thread/turns/list", fakehost.Reply{Result: map[string]any{"data": []any{map[string]any{"id": "turn-2", "status": "completed", "items": []any{map[string]any{"text": "followup"}}}}}})
	in := createInput(cwd, "create")
	in.Prompt = "  exact\nmessage  "
	in.Title = "Demo"
	first, err := b.CreateThread(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first["status"] != "accepted" || object(first["creation"])["model"] != "explicit-model" {
		t.Fatalf("creation=%v calls=%v", first, host.Requests())
	}
	sent := hostParams(t, host, "thread/start")
	if sent["model"] != "explicit-model" || len(object(sent["config"])) != 1 || object(sent["config"])["model_reasoning_effort"] != "high" || sent["projectId"] != nil || hasMethodPrefix(host, "thread/goal/") {
		t.Fatalf("creation: %v", sent)
	}
	turn := hostParams(t, host, "turn/start")
	if text(object(turn["input"].([]any)[0])["text"]) != in.Prompt {
		t.Fatalf("turn: %v", turn)
	}
	result, err := b.SendMessageToThread(context.Background(), SendMessage{RequestID: "send", ThreadID: text(first["threadId"]), Message: "followup", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}})
	if err != nil || result["status"] != "accepted" || result["turnId"] != "turn-2" {
		t.Fatalf("send: %v %v", result, err)
	}
	resume := hostParams(t, host, "thread/resume")
	if resume["model"] != "explicit-model" || len(object(resume["config"])) != 1 || object(resume["config"])["model_reasoning_effort"] != "high" || resume["excludeTurns"] != true || resume["threadId"] != first["threadId"] || len(resume) != 4 {
		t.Fatalf("resume: %v", resume)
	}
	observed, err := b.WaitThread(context.Background(), text(first["threadId"]), text(result["turnId"]), 0)
	if err != nil || object(object(observed["turn"])["items"].([]any)[0])["text"] != "followup" {
		t.Fatalf("observed=%v err=%v", observed, err)
	}
}
func Test_test_empty_creation_does_not_dispatch_or_set_goal(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	r, err := b.CreateThread(context.Background(), createInput(cwd, "empty"))
	if err != nil || r["turnId"] != nil || host.Count("turn/start") != 0 || host.Count("thread/goal/set") != 0 {
		t.Fatalf("receipt=%v err=%v calls=%v", r, err, host.Requests())
	}
}
func Test_test_duplicate_create_and_message_do_not_dispatch_twice(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	host.Respond("turn/start", fakehost.Reply{Result: map[string]any{"turn": map[string]any{"id": "turn-1"}}})
	host.Respond("thread/read", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "idle"}}}})
	host.Respond("thread/resume", startReply(cwd))
	in := createInput(cwd, "same")
	in.Prompt = "hello"
	var wg sync.WaitGroup
	results := make(chan ledger.Receipt, 3)
	for range 3 {
		wg.Go(func() {
			receipt, err := b.CreateThread(context.Background(), in)
			if err != nil {
				t.Errorf("create: %v", err)
			}
			results <- receipt
		})
	}
	wg.Wait()
	close(results)
	if host.Count("thread/start") != 1 || host.Count("turn/start") != 1 {
		t.Fatalf("concurrent creates dispatched twice: %v", host.Requests())
	}
	for receipt := range results {
		if receipt["threadId"] != "thread-1" {
			t.Fatalf("thread=%v", receipt)
		}
	}
	msg := SendMessage{RequestID: "same-send", ThreadID: "thread-1", Message: "hello again", Expected: map[string]any{"model": "explicit-model", "reasoning_effort": "high"}}
	for range 3 {
		if _, err := b.SendMessageToThread(context.Background(), msg); err != nil {
			t.Fatal(err)
		}
	}
	if host.Count("thread/start") != 1 || host.Count("turn/start") != 2 {
		t.Fatalf("calls=%v", host.Requests())
	}
}
func Test_test_conflicting_request_id_fails_without_mutation(t *testing.T) {
	b, host := testBridge(t)
	cwd := t.TempDir()
	host.Respond("thread/start", startReply(cwd))
	in := createInput(cwd, "same")
	in.Prompt = "first"
	if _, err := b.CreateThread(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	in.Prompt = "second"
	_, err := b.CreateThread(context.Background(), in)
	if !errors.Is(err, ledger.ErrConflict) || host.Count("thread/start") != 1 {
		t.Fatalf("error=%v calls=%v", err, host.Requests())
	}
}
func Test_test_replay_after_cwd_removal_returns_retained_receipt(t *testing.T) {
	b, host := testBridge(t)
	cwd := filepath.Join(t.TempDir(), "checkout")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	host.Respond("thread/start", startReply(cwd))
	in := createInput(cwd, "create")
	first, err := b.CreateThread(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	calls := len(host.Requests())
	second, err := b.CreateThread(context.Background(), in)
	if err != nil || second["replayed"] != true || second["threadId"] != first["threadId"] || host.Count("thread/start") != 1 || len(host.Requests()) != calls {
		t.Fatalf("replay=%v err=%v", second, err)
	}
	in.Prompt = "different"
	_, err = b.CreateThread(context.Background(), in)
	if !errors.Is(err, ledger.ErrConflict) {
		t.Fatal(err)
	}
}
func Test_test_cwd_symlink_retargeting_does_not_change_request_identity(t *testing.T) {
	b, host := testBridge(t)
	root := t.TempDir()
	original := filepath.Join(root, "original")
	other := filepath.Join(root, "other")
	alias := filepath.Join(root, "checkout")
	for _, p := range []string{original, other} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(original, alias); err != nil {
		t.Fatal(err)
	}
	host.Respond("thread/start", startReply(original))
	in := createInput(alias, "create")
	first, err := b.CreateThread(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if object(first["creation"])["cwd"] != original {
		t.Fatalf("creation=%v", first)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, alias); err != nil {
		t.Fatal(err)
	}
	second, err := b.CreateThread(context.Background(), in)
	if err != nil || second["replayed"] != true || second["threadId"] != first["threadId"] {
		t.Fatalf("replay=%v err=%v", second, err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	third, err := b.CreateThread(context.Background(), in)
	if err != nil || third["replayed"] != true || host.Count("thread/start") != 1 {
		t.Fatalf("replay=%v err=%v", third, err)
	}
}
