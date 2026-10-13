package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1330 (CRW-1316 S1): `crw doctor flow-roles` shows the helper role store the flow reads its six pairs from, and warns when the
// execution policy still declares agent-roles, which the flow no longer reads.
func TestCRW1330_DoctorFlowRolesWarnsAboutAnUnreadAgentRolesSection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crw")
	t.Setenv("CRW_HOME", dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(dir, "subagents.json")
	if err := os.WriteFile(store, []byte(`{"roles": {"executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"agent-roles": {
  "architect": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}, "executor": {"model": "anthropic/claude-haiku-5", "reasoningEffort": "xhigh"},
  "executor-t1": {"model": "anthropic/claude-sonnet-5-5", "reasoningEffort": "xhigh"}, "reviewer": {"model": "anthropic/claude-sonnet-5", "reasoningEffort": "high"},
  "reviewer-final": {"model": "gpt-6.1-sol", "reasoningEffort": "xhigh"}, "explorer": {"model": "anthropic/claude-haiku-5", "reasoningEffort": "high"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
	run := func() (int, map[string]any, string) {
		var out, errOut strings.Builder
		code := Run(context.Background(), []string{"flow-roles"}, &out, &errOut)
		var report map[string]any
		if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
			t.Fatalf("%v\n%s\n%s", err, out.String(), errOut.String())
		}
		return code, report, out.String()
	}
	code, report, text := run()
	if code != 0 || report["store"] != store || report["present"] != true || len(report["roles"].([]any)) != 6 || len(report["digest"].(string)) != 64 {
		t.Fatalf("exit %d: %s", code, text)
	}
	for _, want := range []string{"agent-roles", "no longer reads", policy, "family", "executor-t1", "undecided"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report lacks %q:\n%s", want, text)
		}
	}
	// Without the section the file is not warned about.
	if err := os.WriteFile(policy, []byte(`{"allowed": [{"model": "m", "efforts": ["high"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, text = run(); code != 0 || strings.Contains(text, "no longer reads") {
		t.Fatalf("exit %d: %s", code, text)
	}
}

// CRW-1330 (evaluation 5a1a64c2 d1): a policy whose agent-roles does not parse loads; doctor flow-roles exits 0 and names the
// section, that the flow no longer reads it, and why the loader ignored it.
func TestCRW1330_DoctorFlowRolesWarnsAboutAMalformedAgentRolesSection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "crw")
	t.Setenv("CRW_HOME", dir)
	policy := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(policy, []byte(`{"agent-roles": {"architect": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", policy)
	var out, errOut strings.Builder
	code := Run(context.Background(), []string{"flow-roles"}, &out, &errOut)
	var report map[string]any
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out.String(), errOut.String())
	}
	if code != 0 || report["policyError"] != nil {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	for _, want := range []string{"no longer reads", "ignored", "executor-t1", policy} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the report lacks %q:\n%s", want, out.String())
		}
	}
}
