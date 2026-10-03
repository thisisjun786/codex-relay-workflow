package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAffordanceComponentHookLifecycle(t *testing.T) {
	t.Setenv("CRW_BIN", "crw")
	t.Setenv("CODEX_HOME", t.TempDir())
	ws := t.TempDir()
	call := func(event, leg, payloadEvent string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"cwd": ws, "session_id": "root", "hook_event_name": payloadEvent})
		var out strings.Builder
		found, code := runComponentHook(invocation{ctx: context.Background(), args: []string{event, "--leg", leg}, stdout: &out}, strings.NewReader(string(raw)), componentHooks())
		if !found || code != 0 {
			t.Fatalf("affordance row %s claimed=%v code=%d", leg, found, code)
		}
		return out.String()
	}
	if out := call("session-start", "session-start-announcing-map-affordance", "SessionStart"); !strings.Contains(out, `"hookEventName":"SessionStart"`) {
		t.Fatal("missing SessionStart envelope")
	}
	if out := call("post-compact", "post-compact-injecting-bg-terminal-affordance.post-compact", "PostCompact"); out != "" {
		t.Fatal("PostCompact emitted unsupported context")
	}
	if out := call("user-prompt-submit", "post-compact-injecting-bg-terminal-affordance.user-prompt-submit", "UserPromptSubmit"); !strings.Contains(out, `"hookEventName":"UserPromptSubmit"`) {
		t.Fatal("missing compact recovery envelope")
	}
	if out := call("user-prompt-submit", "post-compact-injecting-bg-terminal-affordance.user-prompt-submit", "UserPromptSubmit"); out != "" {
		t.Fatal("compact recovery repeated")
	}
}
