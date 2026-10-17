package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The upstream recordings stay immutable. Only their public command spellings
// and help titles change under the CRW-1527 name map; all other bytes still compare.
func workOracleNames(s string) string {
	return strings.NewReplacer(
		"crw pabcd orchestrate", "crw work state",
		"crw pabcd loop", "crw work continue",
		"crw pabcd plan", "crw work outline",
		"crw pabcd review-round", "crw work assessment",
		"crw pabcd", "crw work",
		"multi-cycle PABCD loop", "multi-cycle task work",
	).Replace(s)
}

// CRW-1627: the presentation map rewrites fixed templates only. Session ids, plan-unit paths,
// receipt paths and dynamic error text keep every byte — a refusal's recovery hint must still
// name the object the user gave.

func TestWorkDiagnosticsPreserveUnbindSession(t *testing.T) {
	for _, id := range []string{"PABCD-1", "IPABCD-1"} {
		t.Run(id, func(t *testing.T) {
			cwd := orchestrateTransitionRoot(t)
			s := state.DefaultState(id, "bound-plan")
			s.Phase = state.PhaseB
			s.OrchestrationActive = true
			if err := state.WriteState(cwd, s); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(state.StatePath(cwd, id))
			if err != nil {
				t.Fatal(err)
			}
			got := loopRun(t, cwd, "unbind", "--session", id)
			if got.Code != 1 {
				t.Fatalf("unbind exits %d, want 1: %s", got.Code, got.Output)
			}
			if !strings.Contains(got.Output, "a task work cycle is in flight at phase B") {
				t.Errorf("output lacks the rendered cycle reason: %s", got.Output)
			}
			if !strings.Contains(got.Output, "`crw work state reset --session "+id+"`") {
				t.Errorf("recovery hint lost the session id %q: %s", id, got.Output)
			}
			if strings.Contains(got.Output, "--session task work-") {
				t.Errorf("session id %q was rewritten by the name map: %s", id, got.Output)
			}
			after, err := os.ReadFile(state.StatePath(cwd, id))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("the refusal wrote to the session state")
			}
		})
	}
}

func TestWorkDiagnosticsPreservePlanPaths(t *testing.T) {
	for _, unit := range []string{
		"PABCD-migration",
		"IPABCD-migration",
		filepath.Join("PABCD-migration", "crw pabcd plan", "PABCD-%s"),
	} {
		t.Run(unit, func(t *testing.T) {
			cwd := orchestrateTransitionRoot(t)
			orchestrateTransitionSession(t, cwd, "plan-names", `{"phase":"P"}`)
			before, err := os.ReadFile(state.StatePath(cwd, "plan-names"))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(map[string]any{
				"from": "P", "to": "A", "did": "audited plan", "planUnit": unit,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := orchestrateTransitionRun(t, cwd, "A", "--session", "plan-names", "--attest", string(raw))
			if got.Code != 1 {
				t.Fatalf("transition exits %d, want 1: %s", got.Code, got.Output)
			}
			if !strings.Contains(got.Output, "planUnit "+unit+" does not exist") {
				t.Errorf("the plan-unit path lost bytes: %s", got.Output)
			}
			if !strings.Contains(got.Output, "`crw work outline init <slug>`") {
				t.Errorf("the fixed hint is not rendered with the new name: %s", got.Output)
			}
			after, err := os.ReadFile(state.StatePath(cwd, "plan-names"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Error("the refusal wrote to the session state")
			}
		})
	}
}

func TestWorkDiagnosticsPreserveReceiptPath(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "receipt-names", "receipt-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	att := orchestrateDcloseAttest(id)
	att.TestReceiptPath = filepath.Join("PABCD-evidence", "crw pabcd receipt", "PABCD-%s.json")
	raw, err := json.Marshal(att)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	got := orchestrateTransitionRun(t, cwd, "D", "--session", id, "--attest", string(raw))
	if got.Code != 1 {
		t.Fatalf("dclose exits %d, want 1: %s", got.Code, got.Output)
	}
	if !strings.Contains(got.Output, att.TestReceiptPath) {
		t.Errorf("the receipt path lost bytes: %s", got.Output)
	}
	if !strings.Contains(got.Output, "crw work receipt test --session <id>") {
		t.Errorf("the fixed hint is not rendered with the new name: %s", got.Output)
	}
	after, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the refusal wrote to the session state")
	}
}

// A source refusal outside the renamed guidance stays intact, and the persisted binding is unchanged.
func TestWorkDiagnosticsPreserveOtherSourceRefusals(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "source-names"
	missing := filepath.Join(cwd, "PABCD-missing")
	raw, err := json.Marshal(map[string]any{"phase": "IDLE", "boundSourceRoot": missing})
	if err != nil {
		t.Fatal(err)
	}
	orchestrateTransitionSession(t, cwd, id, string(raw))
	before, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	got := loopRun(t, cwd, "init", "--objective", "source refusal", "--session", id,
		"--criterion", "source resolves")
	if got.Code != 1 {
		t.Fatalf("init exits %d, want 1: %s", got.Code, got.Output)
	}
	want := "loop init: Source binding is missing for the pinned worktree; restore the same binding before continuing.\nNothing was written."
	if got.Output != want {
		t.Errorf("source refusal changed: got %q, want %q", got.Output, want)
	}
	after, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the refusal wrote to the session state")
	}
}
