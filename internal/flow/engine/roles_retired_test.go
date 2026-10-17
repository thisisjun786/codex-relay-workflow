package engine

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// CRW-1450: reviewer-final is no role of a flow run any more. A helper role store or an execution policy file that still holds the key
// loads; the key is a retired role key, accepted and ignored, and `crw flow roles` says so in one warning that names where the key is.

// retiredNotice is the text both sources give (role.RetiredKeyNotice and execution.RetiredRoleKeyNotice; the doctor's tests pin the two
// constants to one text).
const retiredNotice = "reviewer-final is a retired role key and is ignored"

// sixRoleStore is a helper role store with the six roles of a flow run, and the retired reviewer-final entry when retired is set.
func sixRoleStore(retired bool) string {
	text := `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "xhigh"},
  "executor-t1": {"mode": "model", "model": "anthropic/claude-sonnet-5-5", "effort": "xhigh"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},`
	if retired {
		text += `
  "reviewer-final": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"},`
	}
	return text + `
  "critic": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`
}

// retiredPolicyFile is an execution policy whose agent-roles holds a reviewer-final entry and whose agent-aliases has a row naming it.
const retiredPolicyFile = `{"agent-roles": {
  "architect": {"model": "claude-opus", "reasoningEffort": "xhigh"},
  "executor": {"model": "claude-haiku", "reasoningEffort": "xhigh"},
  "executor-t1": {"model": "claude-sonnet", "reasoningEffort": "xhigh"},
  "reviewer": {"model": "gpt-sol", "reasoningEffort": "high"},
  "reviewer-final": {"model": "gpt-sol", "reasoningEffort": "xhigh"},
  "explorer": {"model": "claude-haiku", "reasoningEffort": "high"},
  "critic": {"model": "gpt-sol", "reasoningEffort": "xhigh"}},
 "agent-aliases": [
  {"key": "opus", "role": "architect"},
  {"key": "haiku", "isolation": "worktree", "role": "executor"},
  {"key": "haiku", "refuse": "alias_ambiguous"},
  {"key": "sonnet", "role": "executor-t1"},
  {"key": "ocx-gpt-6-1-sol", "effort": "xhigh", "role": "reviewer-final"},
  {"key": "ocx-gpt-6-1-sol", "role": "reviewer"}]}`

// plainPolicyFile is an execution policy without agent sections.
const plainPolicyFile = `{"allowed": [{"model": "m", "efforts": ["high"]}]}`

// flowRolesReport runs `crw flow roles --json` and `crw flow roles` and returns the report and the text.
func flowRolesReport(t *testing.T) (PolicyReport, string) {
	t.Helper()
	code, out, errOut := runFlow(t, "roles", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("crw flow roles --json: exit %d %q\n%s", code, errOut, out)
	}
	var report PolicyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	code, text, errOut := runFlow(t, "roles")
	if code != 0 || errOut != "" {
		t.Fatalf("crw flow roles: exit %d %q\n%s", code, errOut, text)
	}
	return report, text
}

// assertRetiredNamedOnce checks that the report and the text name the retired key once, in a warning that names source, list no
// reviewer-final role and pin the digest want.
func assertRetiredNamedOnce(t *testing.T, report PolicyReport, text, source, want string) {
	t.Helper()
	var named []string
	for _, w := range report.Warnings {
		if strings.Contains(w, retiredNotice) {
			named = append(named, w)
		}
	}
	if len(named) != 1 || !strings.Contains(named[0], source) {
		t.Errorf("--json: the notice is in %d warnings, want one naming %q: %q", len(named), source, report.Warnings)
	}
	if n := strings.Count(text, retiredNotice); n != 1 || !strings.Contains(text, "warning: ") {
		t.Errorf("text: the notice appears %d times, want once:\n%s", n, text)
	}
	for _, r := range report.Roles {
		if r.Role == "reviewer-final" {
			t.Errorf("crw flow roles lists reviewer-final: %+v", r)
		}
	}
	if len(report.Roles) != len(execution.AgentRoleNames)+1 {
		t.Errorf("roles %d, the flow has %d agent roles and the unit check's tool row", len(report.Roles), len(execution.AgentRoleNames))
	}
	if report.Digest == nil || *report.Digest != want {
		t.Errorf("digest %v, want %s (the same store without the retired entry)", report.Digest, want)
	}
	for _, row := range report.Aliases {
		if row.Role == "reviewer-final" {
			t.Errorf("an alias row resolves to reviewer-final: %+v", row)
		}
	}
}

func TestRetiredReviewerFinalKeyInTheRoleStoreIsNamed(t *testing.T) {
	withRoleStore(t, sixRoleStore(false))
	want := snapshotDigest(t)
	store := withRoleStore(t, sixRoleStore(true))
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeFile(t, policy, plainPolicyFile)
	t.Setenv(execution.EnvPolicy, policy)
	report, text := flowRolesReport(t)
	assertRetiredNamedOnce(t, report, text, store, want)
	if report.Source == nil || *report.Source != store {
		t.Errorf("source %v, want the store %s", report.Source, store)
	}
}

func TestRetiredReviewerFinalKeyInThePolicyFileIsNamed(t *testing.T) {
	withRoleStore(t, sixRoleStore(false))
	want := snapshotDigest(t)
	exec, err := execution.FromBytes([]byte(retiredPolicyFile), "policy.json")
	if err != nil {
		t.Fatalf("a policy file holding the retired key does not load: %v", err)
	}
	if _, ok := exec.AgentRoles(); !ok {
		t.Fatal("the agent sections of a policy file holding the retired key were ignored")
	}
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeFile(t, policy, retiredPolicyFile)
	t.Setenv(execution.EnvPolicy, policy)
	report, text := flowRolesReport(t)
	assertRetiredNamedOnce(t, report, text, "execution policy", want)
	for _, w := range report.Warnings {
		if strings.Contains(w, "agent-roles/agent-aliases ignored") {
			t.Errorf("the agent sections were ignored: %q", w)
		}
	}
}

func TestNoRetiredKeyNoticeWithoutTheKey(t *testing.T) {
	withRoleStore(t, sixRoleStore(false))
	policy := filepath.Join(t.TempDir(), "policy.json")
	writeFile(t, policy, strings.Replace(strings.Replace(retiredPolicyFile, `
  "reviewer-final": {"model": "gpt-sol", "reasoningEffort": "xhigh"},`, "", 1), `
  {"key": "ocx-gpt-6-1-sol", "effort": "xhigh", "role": "reviewer-final"},`, "", 1))
	t.Setenv(execution.EnvPolicy, policy)
	report, text := flowRolesReport(t)
	if strings.Contains(strings.Join(report.Warnings, "\n"), "retired") || strings.Contains(text, "retired") {
		t.Errorf("a notice without the key:\n%s", text)
	}
	writeFile(t, policy, plainPolicyFile)
	report, text = flowRolesReport(t)
	if strings.Contains(strings.Join(report.Warnings, "\n"), "retired") || strings.Contains(text, "retired") {
		t.Errorf("a notice without the key:\n%s", text)
	}
}
