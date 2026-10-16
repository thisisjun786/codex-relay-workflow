package doctor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
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
  "explorer": {"model": "anthropic/claude-haiku-5", "reasoningEffort": "high"}}}`), 0o600); err != nil {
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
	for _, want := range []string{"agent-roles", "no longer reads", policy, "family", "executor-t1", "critic", "undecided"} {
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

// CRW-1450: reviewer-final is no role of a flow run any more. A helper role store or an execution policy file that still holds the key
// loads; `crw doctor flow-roles` names the retired key once, in a warning that says where it is, and lists the six roles.

// retiredNotice is the text of role.RetiredKeyNotice and execution.RetiredRoleKeyNotice (TestRetiredKeyNoticesAreOneText).
const retiredNotice = "reviewer-final is a retired role key and is ignored"

const sixRoleStore = `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "executor-t1": {"mode": "model", "model": "anthropic/claude-sonnet-5-5", "effort": "xhigh"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "critic": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`

// retiredEntry is the reviewer-final entry an old store holds.
const retiredEntry = `"reviewer-final": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"},
  "critic"`

const retiredPolicy = `{"agent-roles": {
  "architect": {"model": "claude-opus", "reasoningEffort": "xhigh"}, "executor": {"model": "claude-haiku", "reasoningEffort": "xhigh"},
  "executor-t1": {"model": "claude-sonnet", "reasoningEffort": "xhigh"}, "reviewer": {"model": "gpt-sol", "reasoningEffort": "high"},
  "reviewer-final": {"model": "gpt-sol", "reasoningEffort": "xhigh"}, "explorer": {"model": "claude-haiku", "reasoningEffort": "high"},
  "critic": {"model": "gpt-sol", "reasoningEffort": "xhigh"}},
 "agent-aliases": [{"key": "opus", "role": "architect"}, {"key": "haiku", "isolation": "worktree", "role": "executor"},
  {"key": "sonnet", "role": "executor-t1"}, {"key": "ocx-gpt-6-1-sol", "effort": "xhigh", "role": "reviewer-final"},
  {"key": "ocx-gpt-6-1-sol", "role": "reviewer"}]}`

const plainPolicy = `{"allowed": [{"model": "m", "efforts": ["high"]}]}`

// doctorFlowRoles points CRW_HOME at a store holding store and the execution policy at a file holding policy, and runs
// `crw doctor flow-roles`; it returns the store's path and the report.
func doctorFlowRoles(t *testing.T, store, policy string) (string, map[string]any) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "crw")
	t.Setenv("CRW_HOME", dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "subagents.json")
	if err := os.WriteFile(path, []byte(store), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(file, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", file)
	var out, errOut strings.Builder
	code := Run(context.Background(), []string{"flow-roles"}, &out, &errOut)
	var report map[string]any
	if err := json.Unmarshal([]byte(out.String()), &report); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out.String(), errOut.String())
	}
	if code != 0 || report["policyError"] != nil {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	return path, report
}

// retiredWarnings are the warnings of the report that hold the notice.
func retiredWarnings(report map[string]any) []string {
	var out []string
	for _, w := range report["warnings"].([]any) {
		if s, _ := w.(string); strings.Contains(s, retiredNotice) {
			out = append(out, s)
		}
	}
	return out
}

func assertSixRoles(t *testing.T, report map[string]any, digest string) {
	t.Helper()
	roles := report["roles"].([]any)
	if len(roles) != 6 {
		t.Errorf("roles %d, want the six of a flow run: %v", len(roles), roles)
	}
	for _, r := range roles {
		if r.(map[string]any)["role"] == "reviewer-final" {
			t.Errorf("the report lists reviewer-final: %v", r)
		}
	}
	if report["digest"] != digest {
		t.Errorf("digest %v, want %s (the same store without the retired entry)", report["digest"], digest)
	}
}

func TestRetiredReviewerFinalKeyInTheRoleStoreIsNamed(t *testing.T) {
	_, plain := doctorFlowRoles(t, sixRoleStore, plainPolicy)
	path, report := doctorFlowRoles(t, strings.Replace(sixRoleStore, `"critic"`, retiredEntry, 1), plainPolicy)
	if named := retiredWarnings(report); len(named) != 1 || !strings.Contains(named[0], path) {
		t.Errorf("the notice is in %d warnings, want one naming the store %s: %v", len(named), path, report["warnings"])
	}
	assertSixRoles(t, report, plain["digest"].(string))
}

func TestRetiredReviewerFinalKeyInThePolicyFileIsNamed(t *testing.T) {
	_, report := doctorFlowRoles(t, sixRoleStore, retiredPolicy)
	if named := retiredWarnings(report); len(named) != 1 || !strings.Contains(named[0], "execution policy") {
		t.Errorf("the notice is in %d warnings, want one naming the execution policy: %v", len(named), report["warnings"])
	}
	for _, w := range report["warnings"].([]any) {
		if strings.Contains(w.(string), "agent-roles/agent-aliases ignored") {
			t.Errorf("the agent sections were ignored: %v", w)
		}
	}
	_, plain := doctorFlowRoles(t, sixRoleStore, plainPolicy)
	assertSixRoles(t, report, plain["digest"].(string))
}

func TestNoRetiredKeyNoticeWithoutTheKey(t *testing.T) {
	for _, policy := range []string{plainPolicy, strings.Replace(strings.Replace(retiredPolicy, `
  "reviewer-final": {"model": "gpt-sol", "reasoningEffort": "xhigh"}, "explorer"`, ` "explorer"`, 1), `{"key": "ocx-gpt-6-1-sol", "effort": "xhigh", "role": "reviewer-final"},
  `, "", 1)} {
		_, report := doctorFlowRoles(t, sixRoleStore, policy)
		for _, w := range report["warnings"].([]any) {
			if strings.Contains(w.(string), "retired") {
				t.Errorf("a notice without the key: %v", w)
			}
		}
	}
}

// TestRetiredKeyNoticesAreOneText: the role store's notice and the policy loader's are one text, which the commands' readers match.
// role and execution import neither each other, so this package, which imports both, pins them.
func TestRetiredKeyNoticesAreOneText(t *testing.T) {
	for _, pair := range [][2]string{{role.RetiredKeyNotice, execution.RetiredRoleKeyNotice}, {role.RetiredKeyNotice, retiredNotice}, {string(role.ReviewerFinal), execution.AgentReviewerFinal}} {
		if pair[0] != pair[1] {
			t.Errorf("the retired key texts differ: %q and %q", pair[0], pair[1])
		}
	}
}
