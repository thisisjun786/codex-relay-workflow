package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// CRW-1330 (CRW-1316 S1): the lane's rule stamps and the start plan come from the same helper role snapshot as the run's role table.

const laneRoleStore = `{"roles": {
  "architect": {"mode": "model", "model": "anthropic/claude-opus-5-5", "effort": "xhigh"},
  "executor": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "medium"},
  "reviewer": {"mode": "model", "model": "gpt-6.1-sol", "effort": "low"},
  "explorer": {"mode": "model", "model": "anthropic/claude-haiku-5", "effort": "high"}}}`

func laneStore(t *testing.T) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "crw")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, role.StoreFile), []byte(laneRoleStore), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_HOME", dir)
	t.Setenv(execution.EnvPolicy, "")
}

func TestCRW1330_BuildLaneStampsTheSnapshotPairs(t *testing.T) {
	laneStore(t)
	table, _, err := engine.LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	j, err := journal.Open(context.Background(), w.st, "run-stamps", journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	laneful := filepath.Join(t.TempDir(), "laneful.js")
	if err := os.WriteFile(laneful, []byte("export const meta = { name: 'laneful' }\nawait crw.commit({ node: 'n1' })\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := LaneRequest{WorldRequest: WorldRequest{Spec: Spec{Schema: SpecSchema, RunID: "run-stamps", Workflow: "x", Script: laneful, Thread: "thread-owner"}, State: w.state},
		Store: w.st, Journal: j, Policy: table}
	var got *SchedRelay
	if _, err := BuildLane(context.Background(), req, LaneOptions{SkillsDigest: "sha256:s", Relay: func(r *SchedRelay) ops.Relay { got = r; return r }}); err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Rule.Model != "anthropic/claude-haiku-5" || got.Rule.Effort != "medium" || got.Verifier.Model != "gpt-6.1-sol" || got.Verifier.Effort != "low" {
		t.Fatalf("stamps %+v %+v", got.Rule, got.Verifier)
	}
}

func TestCRW1330_StartPlanCarriesTheSnapshotAndItsWarnings(t *testing.T) {
	laneStore(t)
	w := newWorld(t)
	td := w.deps()
	lane := []string{"--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", defaultHandoff(t)}
	_, out, errOut := td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--preview", "--json"}, lane...)...)
	var ans StartAnswer
	if err := json.Unmarshal([]byte(out), &ans); err != nil {
		t.Fatalf("%v: %s%s", err, out, errOut)
	}
	if !strings.HasSuffix(ans.Plan.Policy, role.StoreFile) || len(ans.Plan.Roles) != 6 {
		t.Fatalf("plan %+v", ans.Plan)
	}
	if joined := strings.Join(ans.Plan.PolicyWarnings, "\n"); !strings.Contains(joined, "executor-t1") || !strings.Contains(joined, "reviewer-final") {
		t.Fatalf("warnings %q", ans.Plan.PolicyWarnings)
	}
	_, text, _ := td.do(append([]string{"start", "project-lane", "--thread", "thread-owner", "--state", w.state, "--preview"}, lane...)...)
	if !strings.Contains(text, "warning: ") || !strings.Contains(text, "executor-t1") {
		t.Fatalf("the printed plan:\n%s", text)
	}
}
