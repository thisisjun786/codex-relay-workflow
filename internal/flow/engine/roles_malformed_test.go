package engine

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// CRW-1330 (evaluation 5a1a64c2 d1, criterion c3): an execution policy whose agent-roles or agent-aliases does not parse no
// longer stops a flow command. The table comes from the helper role store, and the reading warns that the file declares the
// section and why the loader ignored it.
func TestCRW1330_MalformedAgentRolesDoNotStopTheFlow(t *testing.T) {
	store := withRoleStore(t, roleStore("anthropic/claude-haiku-5", "gpt-6.1-sol"))
	policyPath := filepath.Join(t.TempDir(), "policy.json")
	// the agent-roles of the time before executor-t1, and an alias table whose rows overlap
	writeFile(t, policyPath, `{"agent-roles": {
  "architect": {"model": "claude-opus", "reasoningEffort": "xhigh"},
  "executor": {"model": "claude-haiku", "reasoningEffort": "xhigh"},
  "reviewer": {"model": "gpt-sol", "reasoningEffort": "high"},
  "explorer": {"model": "claude-haiku", "reasoningEffort": "high"}},
 "agent-aliases": [{"key": "opus", "role": "architect"}, {"key": "opus", "role": "executor"}]}`)
	t.Setenv(execution.EnvPolicy, policyPath)
	table, exec, err := LoadPolicy()
	if err != nil {
		t.Fatalf("a malformed agent-roles section stops the flow: %v", err)
	}
	if table.Source != store || table.Digest != snapshotDigest(t) || table.Roles["executor"].Pair != (Pair{"anthropic/claude-haiku-5", "xhigh"}) {
		t.Fatalf("%+v", table)
	}
	if joined := strings.Join(table.Warnings, "\n"); !strings.Contains(joined, policyPath) || !strings.Contains(joined, "no longer reads") {
		t.Fatalf("warnings %q", table.Warnings)
	}
	if joined := strings.Join(exec.Warnings(), "\n"); !strings.Contains(joined, "executor-t1") || !strings.Contains(joined, "ignored") {
		t.Fatalf("loader warnings %q", exec.Warnings())
	}
	code, out, errOut := runFlow(t, "roles", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d %q %s", code, errOut, out)
	}
	var report PolicyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(report.Warnings, "\n")
	for _, want := range []string{"no longer reads", "executor-t1", "ignored"} {
		if !strings.Contains(joined, want) {
			t.Errorf("crw flow roles warnings lack %q: %q", want, report.Warnings)
		}
	}
}
