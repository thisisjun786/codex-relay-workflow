package cli

// The CRW-1109 cases of the pre-merge evaluation of 3fceb240 that live in the orchestrate CLI: the reset that took -h for its
// attestation, and a gated edge past a plan whose schemaVersion is refused.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Red on 3fceb240: `reset --attest -h` took -h for the attestation (not JSON, which the reset exempts), so the session was reset
// where the oracle answered help before any mutation. -h is an option, not a value, so the call is refused.
func TestOrchestrateResetRefusesTheShortHelpAsAnAttestValue(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "attest-help"
	orchestrateTransitionSession(t, cwd, id, `{"phase":"P","orchestrationActive":true}`)
	before, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{
		{"reset", "--session", id, "--attest", "-h"},
		{"reset", "--session", id, "--attest-file", "-h"},
		{"P", "--session", id, "--attest", "-h"},
	} {
		if p := ParseOrchestrateCliArgs(argv, cwd); p.Error == nil || p.Help != nil {
			t.Fatalf("%v parsed: %+v", argv, p)
		}
		got, err := orchestrateCommitTry(t, cwd, nil, argv...)
		if err != nil || got.Code == 0 {
			t.Fatalf("%v did not refuse: %+v %v", argv, got, err)
		}
		if after, err := os.ReadFile(state.StatePath(cwd, id)); err != nil || string(after) != string(before) {
			t.Fatalf("%v changed the session: %v\n%s", argv, err, after)
		}
	}
	// -h as a command's own option is still help, and the word help is still a session name.
	if p := ParseOrchestrateCliArgs([]string{"reset", "--session", id, "-h"}, cwd); p.Help == nil {
		t.Fatalf("-h in an option position is help: %+v", p)
	}
	if p := ParseOrchestrateCliArgs([]string{"status", "--session", "help"}, cwd); p.Args == nil || *p.Args.Session != "help" {
		t.Fatalf("a session named help: %+v", p)
	}
}

// Red on 3fceb240: a bound plan whose schemaVersion is refused (0) was reported by the write lock as unreadable and not refused, so the
// gated P>A edge treated it as an absent plan and published past the work-phase gate: an attestation naming another work phase
// advanced the session, where it is refused with the plan readable. The plan's bytes stay as they are and nothing is published.
func TestOrchestrateGatedEdgeRefusesAPlanWithAnInvalidSchemaVersion(t *testing.T) {
	cwd, id := orchestrateTransitionRoot(t), "schema-gate"
	unit := orchestrateReplanSeed(t, cwd, id)
	planPath := filepath.Join(cwd, ".crw", "goalplans", id, goalplan.GoalplanFile)
	raw, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var plan map[string]any
	if err := json.Unmarshal(raw, &plan); err != nil {
		t.Fatal(err)
	}
	plan["schemaVersion"] = 0
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	attest := `{"from":"P","to":"A","did":"audited the plan","planUnit":"` + unit + `","workPhaseId":"wp-other"}`
	got := orchestrateTransitionRun(t, cwd, "A", "--session", id, "--attest", attest)
	if got.Code == 0 || !strings.Contains(got.Output, "schemaVersion") || !strings.Contains(got.Output, "Nothing was written") {
		t.Fatalf("P>A past a plan with schemaVersion 0: %+v", got)
	}
	if s := state.ReadState(cwd, id); s.Phase != state.PhaseP {
		t.Fatalf("the session moved: %+v", s)
	}
	if after, err := os.ReadFile(planPath); err != nil || string(after) != string(body) {
		t.Fatalf("the plan changed: %v", err)
	}
}
