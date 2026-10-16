package scenario

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/roles"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// TestReviewerFinalIsNotARoleAnyMoreButOldRowsRead (CRW-1450): reviewer-final is no role of a flow run. The flow's role lists do not hold
// it and it has no prompt; a helper role store and an execution policy file that still hold the key load, the entry is ignored and
// `crw flow roles` names it a retired role key. The rows an old run wrote for it still read: the reviewer-final agent call and the
// crw.reviewerFinal row of an old close are what ReviewerFinalProof proves, the store keeps the call's role (the relay's agent acceptance
// reads its review rows by that role, dagsched/accept_agent.go reviewerRoles, which names the retired key constant), and a trace export
// of the run lists the call with its role.
func TestReviewerFinalIsNotARoleAnyMoreButOldRowsRead(t *testing.T) {
	const retired = "reviewer-final"
	if slices.Contains(roles.Names[:], retired) || slices.Contains(execution.AgentRoleNames[:], retired) || slices.Contains(role.FlowRoles(), role.ReviewerFinal) || slices.Contains(role.FlowOnlyRoles(), role.ReviewerFinal) {
		t.Errorf("reviewer-final is still a flow role: roles.Names %v, AgentRoleNames %v, FlowRoles %v, FlowOnlyRoles %v", roles.Names, execution.AgentRoleNames, role.FlowRoles(), role.FlowOnlyRoles())
	}
	if _, err := roles.Prompt(retired, roles.Options{}); err == nil {
		t.Error("reviewer-final still has a prompt")
	}
	// the retired key constants stay: old rows, policy files and role stores carry the key
	if execution.AgentReviewerFinal != retired || string(role.ReviewerFinal) != retired {
		t.Errorf("the retired key constants: %q %q", execution.AgentReviewerFinal, role.ReviewerFinal)
	}

	t.Run("a store and a policy file holding the key load and name it retired", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "crw")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		writeText(t, filepath.Join(dir, role.StoreFile), `{"roles": {"reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "high"},
  "reviewer-final": {"mode": "model", "model": "gpt-6.1-sol", "effort": "xhigh"}}}`)
		policy := filepath.Join(t.TempDir(), "policy.json")
		writeText(t, policy, `{"agent-roles": {
  "architect": {"model": "claude-opus", "reasoningEffort": "xhigh"}, "executor": {"model": "claude-haiku", "reasoningEffort": "xhigh"},
  "executor-t1": {"model": "claude-sonnet", "reasoningEffort": "xhigh"}, "reviewer": {"model": "gpt-sol", "reasoningEffort": "high"},
  "reviewer-final": {"model": "gpt-sol", "reasoningEffort": "xhigh"}, "explorer": {"model": "claude-haiku", "reasoningEffort": "high"}}}`)
		t.Setenv("CRW_HOME", dir)
		t.Setenv(execution.EnvPolicy, policy)
		var out, errOut bytes.Buffer
		if code := engine.RunCLI(context.Background(), []string{"roles", "--json"}, &out, &errOut); code != 0 {
			t.Fatalf("crw flow roles: exit %d %s", code, errOut.String())
		}
		var report engine.PolicyReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		for _, r := range report.Roles {
			if r.Role == retired {
				t.Errorf("crw flow roles lists reviewer-final: %+v", r)
			}
		}
		named := 0
		for _, w := range report.Warnings {
			if strings.Contains(w, "reviewer-final is a retired role key and is ignored") {
				named++
			}
		}
		if named != 2 {
			t.Errorf("crw flow roles names the retired key in %d warnings, want one for the store and one for the policy file: %q", named, report.Warnings)
		}
	})

	t.Run("an old run's reviewer-final rows read", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("the run: %q; calls %v", got.State, l.Labels())
		}
		old := appendOldReviewerFinal(t, l, "M1")
		if v := ops.ReviewerFinalProof(l.Events(), str(old.Data, "range"), str(old.Data, "criteria_digest")); !v.Proven {
			t.Errorf("ReviewerFinalProof of the old final review: %+v", v)
		}
		call := str(old.Data, "call")
		var stored string
		if err := l.Store.DB.QueryRow(`SELECT role FROM flow_calls WHERE run_id = ? AND call_id = ? AND event = 'completed'`, l.RunID, call).Scan(&stored); err != nil || stored != retired {
			t.Fatalf("the store's completed row of the old reviewer-final call: role %q, %v", stored, err)
		}
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		if _, err := l.Store.DB.Exec(`VACUUM INTO ?`, path); err != nil {
			t.Fatal(err)
		}
		tr, err := trace.Export(context.Background(), trace.Options{Path: path, Run: l.RunID})
		if err != nil {
			t.Fatal(err)
		}
		listed := false
		for _, run := range tr.Runs {
			for _, c := range run.Calls {
				if c.CallID == call && c.Role == retired {
					listed = true
				}
			}
		}
		if !listed {
			t.Errorf("the trace export does not list the old reviewer-final call %s", call)
		}
	})
}

func writeText(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
