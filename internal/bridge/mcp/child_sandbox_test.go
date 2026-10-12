package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
)

// CRW-1296: a parent that names no sandbox gets create_thread's schema default, read-only (as in r2 F0-2). For a relay
// child that is refused through the tool too, with the recreate guidance, and no thread is started.
func TestCreateThreadWithTheDefaultSandboxRefusesARelayChild(t *testing.T) {
	host := fakehost.Start(t)
	home, env := isolated(t)
	policy := filepath.Join(home, "policy.json")
	if err := os.WriteFile(policy, []byte(`{"roles":{"child":{"model":"anthropic/claude-opus-5","reasoningEffort":"xhigh"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	env["CODEX_THREAD_BRIDGE_EXECUTION_POLICY"] = policy
	s := serve(t, []string{"--socket", host.SocketPath, "--state-dir", filepath.Join(home, "ledger")}, env)
	result := call(t, s, "create_thread", map[string]any{"request_id": "default-sandbox-child", "cwd": t.TempDir(), "prompt": "READY", "model": "anthropic/claude-opus-5", "reasoning_effort": "xhigh", "role": "child"})
	s.finish(t)
	text := ""
	for _, content := range result.Content {
		if raw, err := content.MarshalJSON(); err == nil {
			text += string(raw)
		}
	}
	if !result.IsError || !strings.Contains(text, "recreate") || !strings.Contains(text, "writable sandbox") || !strings.Contains(text, "danger-full-access") {
		t.Fatalf("expected the recreate-writable refusal, got error=%v %s", result.IsError, text)
	}
	if host.Count("thread/start") != 0 || host.Count("turn/start") != 0 {
		t.Fatalf("refused child reached the host: %v", host.Requests())
	}
}
