package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
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
