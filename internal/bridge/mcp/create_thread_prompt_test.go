package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/ledger"
)

// CRW-1486/S3: a thread created with no first turn has no rollout, so thread/resume and thread/turns/list on it later fail
// with "missing source rollout". create_thread therefore refuses a call without a prompt before anything is reserved: the
// App Server sees no thread/start and the ledger keeps no receipt, so the same request_id is free for the call that brings
// one. An explicit "" and a blank-only prompt stay refused by the checks that already refused them.
func TestCreateThreadRefusesAMissingPrompt(t *testing.T) {
	host := fakehost.Start(t)
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t, []string{"--socket", host.SocketPath, "--state-dir", filepath.Join(home, "ledger")}, env)
	base := map[string]any{"cwd": cwd, "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
	with := func(id string, extra map[string]any) map[string]any {
		arguments := map[string]any{"request_id": id}
		for key, value := range base {
			arguments[key] = value
		}
		for key, value := range extra {
			arguments[key] = value
		}
		return arguments
	}
	for _, tc := range []struct {
		name, id string
		extra    map[string]any
		said     []string
	}{
		{"missing", "no-prompt", nil, []string{"Error executing tool create_thread: ", "prompt is required", "no rollout", "missing source rollout"}},
		{"null", "null-prompt", map[string]any{"prompt": nil}, []string{"Error executing tool create_thread: ", "prompt is required", "no rollout"}},
		{"empty", "empty-prompt", map[string]any{"prompt": ""}, []string{"Error executing tool create_thread: ", "prompt must contain 1–100000 characters"}},
		{"blank", "blank-prompt", map[string]any{"prompt": "   "}, []string{"Error executing tool create_thread: ", "prompt must contain 1–100000 characters"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := call(t, s, "create_thread", with(tc.id, tc.extra))
			text := result.Content[0].(*sdk.TextContent).Text
			if !result.IsError {
				t.Fatalf("create_thread without a usable prompt was accepted: %s", text)
			}
			for _, want := range tc.said {
				if !strings.Contains(text, want) {
					t.Fatalf("refusal %q does not say %q", text, want)
				}
			}
			// The ledger holds no receipt for the id: get_operation does not know it.
			operation := call(t, s, "get_operation", map[string]any{"request_id": tc.id})
			if said := operation.Content[0].(*sdk.TextContent).Text; !operation.IsError || !strings.Contains(said, "Unknown request_id") {
				t.Fatalf("a refused creation left a receipt for %s: %s", tc.id, said)
			}
		})
	}
	s.finish(t)
	if host.Count("thread/start") != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("a refused creation reached the host: %v", host.Requests())
	}
}

// CRW-1486 fix round 2: a creation the ledger accepted with no prompt before the refusal existed is answered from its receipt
// when it is retried with the same request_id and arguments; the refusal is for a creation that has not happened.
func TestCreateThreadRetriedWithoutAPromptReplaysItsStoredReceipt(t *testing.T) {
	host := fakehost.Start(t)
	home, env := isolated(t)
	cwd, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, "ledger")
	host.Respond("thread/start", fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd}, "cwd": cwd, "runtimeWorkspaceRoots": []any{cwd}, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh", "approvalPolicy": "never", "sandbox": map[string]any{"type": "readOnly", "networkAccess": false}}})
	host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"id": "thread-1", "cwd": cwd, "model": "anthropic/claude-opus-5", "reasoningEffort": "xhigh"}}}
	})
	// Before the upgrade: the bridge accepted create_thread without a prompt and kept its receipt.
	policy, err := execution.FromEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	canonical, store, err := ledger.Endpoint(host.SocketPath, state)
	if err != nil {
		t.Fatal(err)
	}
	client := appserver.New(canonical, appserver.DefaultBounds)
	earlier, err := bridge.New(client, store, policy).CreateThread(context.Background(), bridge.CreateThread{RequestID: "pre-upgrade", CWD: cwd, Sandbox: "read-only", Model: "anthropic/claude-opus-5", Effort: "xhigh"})
	client.Close()
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || earlier["status"] != "accepted" {
		t.Fatalf("the pre-upgrade creation: %v %v", earlier, err)
	}
	starts := host.Count("thread/start")

	s := serve(t, []string{"--socket", host.SocketPath, "--state-dir", state}, env)
	arguments := map[string]any{"request_id": "pre-upgrade", "cwd": cwd, "sandbox": "read-only", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh"}
	result := call(t, s, "create_thread", arguments)
	if result.IsError {
		t.Fatalf("the retry of a stored creation was refused: %s", result.Content[0].(*sdk.TextContent).Text)
	}
	if replay := structured(t, result); replay["replayed"] != true || replay["threadId"] != earlier["threadId"] {
		t.Fatalf("the retry is not the stored receipt: %v, stored %v", replay, earlier)
	}
	// A request_id the ledger does not hold is still refused.
	arguments["request_id"] = "post-upgrade"
	if fresh := call(t, s, "create_thread", arguments); !fresh.IsError || !strings.Contains(fresh.Content[0].(*sdk.TextContent).Text, "prompt is required") {
		t.Fatalf("a new creation without a prompt was not refused: %v", fresh.Content)
	}
	s.finish(t)
	if host.Count("thread/start") != starts {
		t.Fatalf("the retry reached the host: %d thread/start after %d", host.Count("thread/start"), starts)
	}
}
