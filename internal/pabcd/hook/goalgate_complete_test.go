package hook

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	sourcesession "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The goal-complete cases of CXC v0.2.40 pabcd-state/test/goal-gate.test.ts (3c1459ac): the GOAL-COMPLETE-GATE-01
// block (:290-455) and the #252 PABCD-off case, plus the evidence signals the oracle's fixtures freeze. Every case
// runs against temporary homes (goalGateTestEnv) and a temporary cwd, so no case can read or write the real
// ~/.codex, ~/.crw or ~/.codexclaw.

// goalCompleteTestState writes one session state under cwd; mutate sets the fields a case needs.
func goalCompleteTestState(t *testing.T, cwd, sessionID string, mutate func(*state.State)) {
	t.Helper()
	s := state.DefaultState(sessionID, "")
	if mutate != nil {
		mutate(&s)
	}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

// goalCompleteTestGuard runs the guard the way the dispatcher does, with the readers a hook process has.
func goalCompleteTestGuard(t *testing.T, cwd, sessionID string, pabcdEnabled bool, input any) string {
	t.Helper()
	return goalCompleteApplyGuard(goalGatePreToolUse{SessionID: sessionID, Cwd: cwd, ToolName: goalGateUpdateGoalToolName, ToolInput: input}, pabcdEnabled, goalCompleteProcessDeps())
}

// goalCompleteTestWritePlan writes a plan and binds the session to it, then leaves the state as the caller set it.
func goalCompleteTestWritePlan(t *testing.T, cwd, sessionID string, plan *goalplan.Goalplan, mutate func(*state.State)) {
	t.Helper()
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	goalCompleteTestState(t, cwd, sessionID, func(s *state.State) {
		s.Slug = plan.Slug
		if mutate != nil {
			mutate(s)
		}
	})
}

// goal-gate.test.ts:290-294 - only update_goal status complete is judged.
func TestGoalGateCompleteGuardPassthrough(t *testing.T) {
	_, env := goalGateTestEnv(t)
	cwd := t.TempDir()
	_ = env
	for _, c := range []struct {
		name  string
		tool  string
		input any
	}{
		{"create_goal", goalGateCreateGoalToolName, map[string]any{"objective": "x"}},
		{"blocked", goalGateUpdateGoalToolName, map[string]any{"status": "blocked"}},
		{"non-object input", goalGateUpdateGoalToolName, "complete"},
		{"nil input", goalGateUpdateGoalToolName, nil},
	} {
		p := goalGatePreToolUse{SessionID: "gc0", Cwd: cwd, ToolName: c.tool, ToolInput: c.input}
		if out := goalCompleteApplyGuard(p, true, goalCompleteProcessDeps()); out != "" {
			t.Errorf("%s: %q", c.name, out)
		}
	}
}

// goal-gate.test.ts:296-301 - a complete with no session state has nothing to judge.
func TestGoalGateCompleteGuardNoStatePasses(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	if out := goalCompleteTestGuard(t, cwd, "gc0", true, map[string]any{"status": "complete"}); out != "" {
		t.Errorf("an empty workspace: %q", out)
	}
}

// goal-gate.test.ts:303-312 - a PABCD cycle in flight denies, naming the phase and the session.
func TestGoalGateCompleteGuardMidCycle(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	goalCompleteTestState(t, cwd, "gc1", func(s *state.State) { s.Phase = state.PhaseB; s.OrchestrationActive = true })
	out := goalCompleteTestGuard(t, cwd, "gc1", true, map[string]any{"status": "complete"})
	reason, context := goalGateTestDeny(t, out)
	if reason != context {
		t.Errorf("reason and context differ: %q / %q", reason, context)
	}
	for _, want := range []string{"GOAL-COMPLETE-GATE-01", "phase B", "--session gc1", "pabcd orchestrate reset"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the reason does not name %q: %q", want, reason)
		}
	}
	// The same state with PABCD off skips the cycle check (goal-gate.test.ts:252).
	if out := goalCompleteTestGuard(t, cwd, "gc1", false, map[string]any{"status": "complete"}); out != "" {
		t.Errorf("a mid-cycle complete with PABCD off: %q", out)
	}
}

// goal-gate.test.ts:314-345 - the E8 gate, the dependency reason, the empty plan, and the unreadable plan.
func TestGoalGateCompleteGuardBoundGoalplan(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()

	// One open criterion: the plan fails the E8 gate and the denial carries the reason and the validate remedy.
	open := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "Ship it", Criteria: []goalplan.NewGoalplanCriterion{{Scenario: "tests", ExpectedEvidence: "green"}}})
	goalCompleteTestWritePlan(t, cwd, "gc2", open, nil)
	reason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, cwd, "gc2", true, map[string]any{"status": "complete"}))
	for _, want := range []string{"fails the E8 quality/integrity gate", "unmet criterion", "pabcd loop validate", "blocked"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the E8 denial does not name %q: %q", want, reason)
		}
	}
	if !strings.Contains(reason, ".crw/goalplans/"+open.Slug+"/goalplan.json") {
		t.Errorf("the E8 denial does not name the plan file: %q", reason)
	}
	// blocked is the honest escape hatch and is never denied.
	if out := goalCompleteTestGuard(t, cwd, "gc2", true, map[string]any{"status": "blocked"}); out != "" {
		t.Errorf("blocked on a failing plan: %q", out)
	}

	// A dependency cycle is a structural reason and is exposed in the denial.
	cycleCwd := t.TempDir()
	cycle := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "dependency cycle"})
	two := 2.0
	cycle.SchemaVersion = &two
	cycle.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "b", Title: "b", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}, DependsOn: []string{"a"}},
		{ID: "a", Title: "a", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}, DependsOn: []string{"b"}},
	}
	goalCompleteTestWritePlan(t, cycleCwd, "gc-integrity", cycle, nil)
	cycleReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, cycleCwd, "gc-integrity", true, map[string]any{"status": "complete"}))
	for _, want := range []string{"fails the E8 quality/integrity gate", "work phase dependency cycle: a -> b -> a", "Repair invalid dependency, outcome, and criteria references first"} {
		if !strings.Contains(cycleReason, want) {
			t.Errorf("the integrity denial does not name %q: %q", want, cycleReason)
		}
	}

	// The reason the recorded fixture freezes: a bound plan with one open work phase at IDLE.
	openPhaseCwd := t.TempDir()
	openPhase := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "Ship the export feature"})
	openPhase.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "Exporter", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	goalCompleteTestWritePlan(t, openPhaseCwd, "rec-s1", openPhase, func(s *state.State) { s.Phase = state.PhaseIdle; s.OrchestrationActive = false })
	phaseReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, openPhaseCwd, "rec-s1", true, map[string]any{"status": "complete"}))
	for _, want := range []string{"the session-bound goalplan 'ship-the-export-feature' fails the E8 quality/integrity gate: 1 work phase(s) not done: wp1.", "pabcd loop validate --session rec-s1 --slug \"ship-the-export-feature\""} {
		if !strings.Contains(phaseReason, want) {
			t.Errorf("the open-work-phase denial does not name %q: %q", want, phaseReason)
		}
	}
	if strings.Contains(phaseReason, "\"\"") {
		t.Errorf("the open-work-phase denial left an unresolved invocation: %q", phaseReason)
	}

	// An empty registered plan denies.
	emptyCwd := t.TempDir()
	empty := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "Shell only"})
	goalCompleteTestWritePlan(t, emptyCwd, "gc3", empty, nil)
	emptyReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, emptyCwd, "gc3", true, map[string]any{"status": "complete"}))
	if !strings.Contains(emptyReason, "plan is empty") {
		t.Errorf("the empty plan denial: %q", emptyReason)
	}

	// A bound slug with no plan file denies.
	missingCwd := t.TempDir()
	goalCompleteTestState(t, missingCwd, "gc-missing", func(s *state.State) { s.Slug = "missing-goalplan" })
	missingReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, missingCwd, "gc-missing", true, map[string]any{"status": "complete"}))
	if !strings.Contains(missingReason, "could not be read") {
		t.Errorf("the missing plan denial: %q", missingReason)
	}

	// A bound slug whose plan file is malformed denies the same way.
	malformedCwd := t.TempDir()
	slug := "malformed-goalplan"
	goalCompleteTestState(t, malformedCwd, "gc-malformed", func(s *state.State) { s.Slug = slug })
	dir, err := goalplan.GoalplanDir(malformedCwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, goalplan.GoalplanFile), []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	malformedReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, malformedCwd, "gc-malformed", true, map[string]any{"status": "complete"}))
	if !strings.Contains(malformedReason, "could not be read") {
		t.Errorf("the malformed plan denial: %q", malformedReason)
	}
}

// goal-gate.test.ts:405-421 - a complete legacy v1 plan at IDLE passes.
func TestGoalGateCompleteGuardCompletePlanPasses(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "Done for real", Criteria: []goalplan.NewGoalplanCriterion{{Scenario: "tests", ExpectedEvidence: "green"}}})
	one := 1.0
	plan.SchemaVersion = &one
	evidence := "go test: 0 fail"
	plan.Criteria[0].Status = goalplan.CriterionMet
	plan.Criteria[0].CapturedEvidence = &evidence
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "All", Status: goalplan.WorkPhaseDone,
		Tasks: []goalplan.GoalplanTask{{ID: "t-1", Title: "x", Status: goalplan.TaskDone}}, CriteriaIDs: []string{"c-1"}}}
	goalCompleteTestWritePlan(t, cwd, "gc4", plan, func(s *state.State) { s.Phase = state.PhaseIdle; s.OrchestrationActive = false })
	if out := goalCompleteTestGuard(t, cwd, "gc4", true, map[string]any{"status": "complete"}); out != "" {
		t.Errorf("a complete plan: %q", out)
	}
}

// goal-gate.test.ts:423-438 - a schema-v2 marker blocks a stored downgrade.
func TestGoalGateCompleteGuardSchemaMarker(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "marker downgrade", Criteria: []goalplan.NewGoalplanCriterion{{Scenario: "tests"}}})
	two := 2.0
	plan.SchemaVersion = &two
	met := "green"
	plan.Criteria[0].Status = goalplan.CriterionMet
	plan.Criteria[0].CapturedEvidence = &met
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	markerPath, err := goalplan.SchemaMarkerPath(cwd, plan.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(filepath.Dir(markerPath), goalplan.GoalplanFile)
	stored := map[string]any{}
	raw, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	delete(stored, "schemaVersion")
	rewritten, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}
	goalCompleteTestState(t, cwd, "gc-marker", func(s *state.State) { s.Slug = plan.Slug })
	reason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, cwd, "gc-marker", true, map[string]any{"status": "complete"}))
	if !strings.Contains(reason, `restore "schemaVersion": 2`) {
		t.Errorf("the downgrade denial: %q", reason)
	}
}

// goal-gate.test.ts:218-271 - the evidence signals, each with its own denial text.
func TestGoalGateCompleteGuardEvidenceSignals(t *testing.T) {
	goalGateTestEnv(t)

	// Unreadable session state: a file that exists but is not a state document.
	unreadableCwd := t.TempDir()
	goalCompleteTestState(t, unreadableCwd, "gc-bad", nil)
	if err := os.WriteFile(state.StatePath(unreadableCwd, "gc-bad"), []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	reason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, unreadableCwd, "gc-bad", true, map[string]any{"status": "complete"}))
	if !strings.Contains(reason, "state is unreadable") {
		t.Errorf("the unreadable state denial: %q", reason)
	}

	// unverifiedCorrupt: the verification record could not be trusted.
	corruptCwd := t.TempDir()
	goalCompleteTestState(t, corruptCwd, "gc-corrupt", func(s *state.State) { s.UnverifiedCorrupt = true })
	corruptReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, corruptCwd, "gc-corrupt", true, map[string]any{"status": "complete"}))
	if !strings.Contains(corruptReason, "verification record for this session is unreadable or overflowed") {
		t.Errorf("the corrupt record denial: %q", corruptReason)
	}

	// An unrecordable verdict marker present for this session.
	markerCwd := t.TempDir()
	goalCompleteTestState(t, markerCwd, "gc-marker", nil)
	markerDir := filepath.Join(markerCwd, ".crw", evidence.UnrecordableSubdir)
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, state.SanitizeKey("gc-marker")+"-a1-1.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	markerReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, markerCwd, "gc-marker", true, map[string]any{"status": "complete"}))
	if !strings.Contains(markerReason, "verdict could not be confirmed") {
		t.Errorf("the marker denial: %q", markerReason)
	}

	// Unverified subagents: the first three are named, an unresolvable one as <no agent id>.
	subCwd := t.TempDir()
	goalCompleteTestState(t, subCwd, "gc-sub", func(s *state.State) {
		s.UnverifiedSubagents = []state.UnverifiedSubagent{
			{AgentID: "a1", Resolvable: true},
			{AgentID: "a2", Resolvable: true},
			{AgentID: "a3", Resolvable: false},
			{AgentID: "a4", Resolvable: true},
		}
	})
	subReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, subCwd, "gc-sub", true, map[string]any{"status": "complete"}))
	for _, want := range []string{"4 delegated subagent completion(s)", "(a1, a2, <no agent id>)", "pabcd evidence resolve", "--session gc-sub"} {
		if !strings.Contains(subReason, want) {
			t.Errorf("the unverified denial does not name %q: %q", want, subReason)
		}
	}
	if strings.Contains(subReason, "a4") {
		t.Errorf("the unverified denial names a fourth entry: %q", subReason)
	}

	// A spent evidence budget: a counter at the cap.
	budgetCwd := t.TempDir()
	goalCompleteTestState(t, budgetCwd, "gc-budget", nil)
	if !evidence.WriteAttempts(budgetCwd, "gc-budget", "a1", evidence.MaxAttempts, "t1") {
		t.Fatal("the attempts counter was not written")
	}
	budgetReason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, budgetCwd, "gc-budget", true, map[string]any{"status": "complete"}))
	if !strings.Contains(budgetReason, "exhausted its evidence-verification budget") {
		t.Errorf("the spent budget denial: %q", budgetReason)
	}
}

// The evidence signals are not gated by PABCD: they deny with PABCD off too (goal-gate.test.ts:440-455).
func TestGoalGateCompleteGuardEvidenceSignalsWithPabcdOff(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	goalCompleteTestState(t, cwd, "switch", func(s *state.State) {
		s.Phase = state.PhaseB
		s.OrchestrationActive = true
		s.UnverifiedSubagents = []state.UnverifiedSubagent{{AgentID: "a1", Resolvable: true}}
	})
	if out := goalCompleteTestGuard(t, cwd, "switch", false, map[string]any{"status": "complete"}); out == "" {
		t.Error("an unverified subagent with PABCD off was allowed")
	}
}

// goal-gate.test.ts:440-447 - the dispatcher reaches the guard on the same head.
func TestGoalGateCompleteGuardThroughDispatcher(t *testing.T) {
	_, env := goalGateTestEnv(t)
	cwd := t.TempDir()
	goalCompleteTestState(t, cwd, "gc5", func(s *state.State) { s.Phase = state.PhaseC; s.OrchestrationActive = true })
	raw := goalGateTestPayload(t, cwd, "gc5", goalGateUpdateGoalToolName, map[string]any{"status": "complete"})
	reason, _ := goalGateTestDeny(t, goalGateHandle(raw, true, goalGateRealDeps(env)))
	if !strings.Contains(reason, "GOAL-COMPLETE-GATE-01") {
		t.Errorf("the dispatcher: %q", reason)
	}
}

// The oracle's catch is total: an unexpected error in a reader fails open, so the gate never traps a session.
func TestGoalGateCompleteGuardFailsOpen(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	deps := goalCompleteDeps{
		Invocation: func() (string, error) { return host.Invocation(os.LookupEnv) },
		ReadState:  func(string, string) (state.State, bool) { panic("the state reader cannot answer") },
	}
	p := goalGatePreToolUse{SessionID: "gc6", Cwd: cwd, ToolName: goalGateUpdateGoalToolName, ToolInput: map[string]any{"status": "complete"}}
	if out := goalCompleteApplyGuard(p, true, deps); out != "" {
		t.Errorf("a panicking reader: %q", out)
	}

	// A resolver that fails leaves the reason as it is and does not change the decision.
	deny := goalCompleteDeps{Invocation: func() (string, error) { return "", os.ErrNotExist }}
	midCycle := goalCompleteDeps{Invocation: deny.Invocation, ReadState: state.ReadStateStrict}
	goalCompleteTestState(t, cwd, "gc7", func(s *state.State) { s.Phase = state.PhaseB; s.OrchestrationActive = true })
	p = goalGatePreToolUse{SessionID: "gc7", Cwd: cwd, ToolName: goalGateUpdateGoalToolName, ToolInput: map[string]any{"status": "complete"}}
	reason, _ := goalGateTestDeny(t, goalCompleteApplyGuard(p, true, midCycle))
	if !strings.Contains(reason, "phase B") || !strings.Contains(reason, "`crw pabcd orchestrate") {
		t.Errorf("a failing resolver changed the reason: %q", reason)
	}
}

// goalCompleteTestGitRepo makes cwd a repository with .crw ignored, so the state, plan and receipt this file
// writes leave the captured tree clean: the identity the guard captures is then exactly the one the plan and
// the receipt record, and the passing v2 case can assert the receipt callback the guard wires.
func goalCompleteTestGitRepo(t *testing.T) string {
	t.Helper()
	cwd := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+cwd,
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(cwd, ".gitignore"), []byte(".crw/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "initial")
	return cwd
}

// A complete schema-v2 plan with an approved final gate and a matching receipt passes: this is the case that
// reaches the receipt reader and the source capture together, which the deny cases cannot.
func TestGoalGateCompleteGuardPassingV2PlanWithReceipt(t *testing.T) {
	goalGateTestEnv(t)
	cwd := goalCompleteTestGitRepo(t)
	sessionID := "gc-v2"
	current := goalCompleteCaptureSourceIdentity(cwd, sessionID)
	if current.Kind != "resolved" || current.CommitSha == "" || current.Dirty {
		t.Fatalf("the fixture tree is not a clean resolved capture: %#v", current)
	}

	ts := "2026-01-01T00:00:00.000Z"
	two := 2.0
	met := "go test: 0 fail"
	roundID := "r1"
	receiptPath := ".crw/evidence/test.json"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "v2 pass", Criteria: []goalplan.NewGoalplanCriterion{{Scenario: "tests", ExpectedEvidence: "green"}}})
	plan.SchemaVersion = &two
	plan.Criteria = []goalplan.GoalplanCriterion{{ID: "c-1", Scenario: "tests", ExpectedEvidence: "green", CapturedEvidence: &met, Status: goalplan.CriterionMet, Surface: goalplan.SurfaceLogic}}
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "t", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{"c-1"}}}
	identity := current
	plan.FinalGate = &goalplan.FinalGateState{Status: goalplan.GateApproved, QaRequired: false, UpdatedAt: ts,
		ReviewRoundID: &roundID, TestReceiptPath: &receiptPath, Verdict: goalplan.VerdictPass, SourceIdentity: &identity}
	plan.ReviewRounds = []goalplan.ReviewRoundState{{RoundID: roundID, Purpose: goalplan.PurposeFinalGate, PlanPath: "p.md",
		PlanSha256: strings.Repeat("a", 64), Status: goalplan.ReviewApproved, OpenedAt: ts,
		Lane: goalplan.ReviewLane{LaunchID: "r1-x", Verdict: goalplan.VerdictPass, SourceIdentity: &identity}}}
	goalCompleteTestWritePlan(t, cwd, sessionID, plan, func(s *state.State) { s.Phase = state.PhaseIdle; s.OrchestrationActive = false })

	receipt, err := json.Marshal(map[string]any{"kind": "test", "createdAt": ts,
		"sourceIdentity": map[string]any{"kind": string(current.Kind), "commitSha": current.CommitSha, "dirty": current.Dirty, "capturedAt": current.CapturedAt}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cwd, ".crw", "evidence")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test.json"), receipt, 0o644); err != nil {
		t.Fatal(err)
	}

	if out := goalCompleteTestGuard(t, cwd, sessionID, true, map[string]any{"status": "complete"}); out != "" {
		t.Errorf("a complete v2 plan with a matching receipt: %q", out)
	}

	// The receipt callback is load-bearing: a receipt captured against another tree denies, which is the
	// final gate's own reading of what ParseSourceBoundReceipt returned.
	mismatch, err := json.Marshal(map[string]any{"kind": "test", "createdAt": ts,
		"sourceIdentity": map[string]any{"kind": "resolved", "commitSha": strings.Repeat("b", 40), "dirty": false, "capturedAt": ts}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test.json"), mismatch, 0o644); err != nil {
		t.Fatal(err)
	}
	reason, _ := goalGateTestDeny(t, goalCompleteTestGuard(t, cwd, sessionID, true, map[string]any{"status": "complete"}))
	if !strings.Contains(reason, "the test receipt describes a different source") {
		t.Errorf("a receipt captured against another tree: %q", reason)
	}
}

// goalCompleteTestGit runs one git command in dir with a fixed identity and no system config.
func goalCompleteTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+dir,
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// A complete schema-v2 plan with an approved final gate passes when the session is bound to a clean source
// worktree and the gate and the receipt record exactly that tree. This is the case that reaches the source
// capture, the source comparison and the receipt reader together; a bound session is what makes it possible,
// because only a bound capture excludes the .crw state directory (session-source-identity.ts:6-10).
func TestGoalGateCompleteGuardPassingV2PlanWithBoundSource(t *testing.T) {
	goalGateTestEnv(t)
	repo := goalCompleteTestGitRepo(t)
	linked := filepath.Join(t.TempDir(), "wt")
	goalCompleteTestGit(t, repo, "worktree", "add", "-q", "-b", "wt-branch", linked)
	sessionID := "gc-v2pass"
	goalCompleteTestState(t, repo, sessionID, nil)
	if _, err := sourcesession.Bind(repo, sessionID, linked); err != nil {
		t.Fatalf("bind: %v", err)
	}
	identity := goalCompleteCaptureSourceIdentity(repo, sessionID)
	if identity.Kind != "resolved" || identity.CommitSha == "" || identity.Dirty || identity.SourceRoot == nil || *identity.SourceRoot != linked {
		t.Fatalf("the bound source is not a clean resolved capture: %#v", identity)
	}

	ts := "2026-01-01T00:00:00.000Z"
	two := 2.0
	met := "go test: 0 fail"
	roundID := "r1"
	receiptPath := ".crw/evidence/test.json"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "v2 pass bound", Criteria: []goalplan.NewGoalplanCriterion{{Scenario: "tests", ExpectedEvidence: "green"}}})
	plan.SchemaVersion = &two
	plan.Criteria = []goalplan.GoalplanCriterion{{ID: "c-1", Scenario: "tests", ExpectedEvidence: "green", CapturedEvidence: &met, Status: goalplan.CriterionMet, Surface: goalplan.SurfaceLogic}}
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp1", Title: "t", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{"c-1"}}}
	plan.FinalGate = &goalplan.FinalGateState{Status: goalplan.GateApproved, QaRequired: false, UpdatedAt: ts,
		ReviewRoundID: &roundID, TestReceiptPath: &receiptPath, Verdict: goalplan.VerdictPass, SourceIdentity: &identity}
	plan.ReviewRounds = []goalplan.ReviewRoundState{{RoundID: roundID, Purpose: goalplan.PurposeFinalGate, PlanPath: "p.md",
		PlanSha256: strings.Repeat("a", 64), Status: goalplan.ReviewApproved, OpenedAt: ts,
		Lane: goalplan.ReviewLane{LaunchID: "r1-x", Verdict: goalplan.VerdictPass, SourceIdentity: &identity}}}
	goalCompleteTestWritePlan(t, repo, sessionID, plan, func(s *state.State) { s.Phase = state.PhaseIdle; s.OrchestrationActive = false })

	receipt, err := json.Marshal(map[string]any{"kind": "test", "createdAt": ts, "sourceIdentity": map[string]any{
		"kind": string(identity.Kind), "commitSha": identity.CommitSha, "dirty": identity.Dirty,
		"capturedAt": identity.CapturedAt, "sourceRoot": *identity.SourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, ".crw", "evidence")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "test.json"), receipt, 0o644); err != nil {
		t.Fatal(err)
	}

	if out := goalCompleteTestGuard(t, repo, sessionID, true, map[string]any{"status": "complete"}); out != "" {
		t.Errorf("a complete v2 plan with a bound clean source and a matching receipt: %q", out)
	}
}

// The oracle wraps the invocation resolution in its own try/catch (goal-gate.ts:176-181): a resolver that
// fails, throws included, never changes the deny decision. A resolver that panics must not reach the guard's
// outer catch, where it would turn a deny into a pass.
func TestGoalGateCompleteGuardResolverPanicStillDenies(t *testing.T) {
	goalGateTestEnv(t)
	cwd := t.TempDir()
	goalCompleteTestState(t, cwd, "gc8", func(s *state.State) { s.Phase = state.PhaseB; s.OrchestrationActive = true })
	deps := goalCompleteDeps{
		Invocation: func() (string, error) { panic("the resolver cannot answer") },
		ReadState:  state.ReadStateStrict,
	}
	p := goalGatePreToolUse{SessionID: "gc8", Cwd: cwd, ToolName: goalGateUpdateGoalToolName, ToolInput: map[string]any{"status": "complete"}}
	reason, _ := goalGateTestDeny(t, goalCompleteApplyGuard(p, true, deps))
	if !strings.Contains(reason, "phase B") {
		t.Errorf("a panicking resolver changed the decision: %q", reason)
	}
	if !strings.Contains(reason, "`crw pabcd orchestrate") {
		t.Errorf("a panicking resolver should leave the reason as it is: %q", reason)
	}
}
