package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// These are the normal-path D close cases of CXC v0.2.40 pabcd-state/test/orchestrate-cli.test.ts
// (:830-1000, :1094-1180, :2570-2700). The oracle's commit hooks become orchestrateDcloseSeam, so a
// case can stop the close after any durable write; every case drives orchestrateDclose directly with
// the session already at C, as the oracle's parsedDclose() helper does.
//
// The tests reuse the temporary-world helpers orchestrate_transition_test.go defines
// (orchestrateTransitionRoot, orchestrateTransitionRepo, orchestrateTransitionSession), so HOME,
// CODEX_HOME and CRW_HOME point into temporary directories and no run can reach the real Codex home.

// orchestrateDcloseTestCwd is boundCwd(): a one-commit git repository, because the C>D gate captures a
// source identity and a bound close needs a resolvable one.
func orchestrateDcloseTestCwd(t *testing.T) string {
	t.Helper()
	// The packet's real-state rule: HOME, CODEX_HOME and CRW_HOME point into temporary directories
	// before anything this file runs can reach an install, config or hook path.
	orchestrateTransitionRoot(t)
	return orchestrateTransitionRepo(t)
}

// orchestrateDcloseSeedPlan writes the two-phase plan seedBoundCycleAtC builds.
func orchestrateDcloseSeedPlan(t *testing.T, cwd, slug string, taskStatus goalplan.TaskStatus) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "cycle completion gate"})
	plan.Slug = slug
	task := goalplan.GoalplanTask{ID: "t-1", Title: "the work", Status: taskStatus}
	if taskStatus == goalplan.TaskDone {
		task.Outcome = "focused tests passed"
	}
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{task}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "second", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	active := "wp-1"
	plan.ActiveWorkPhaseID = &active
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// orchestrateDcloseSeedReceipt writes the receipt the C>D gate accepts for this epoch, as seedReceipt does.
func orchestrateDcloseSeedReceipt(t *testing.T, cwd, id, epoch string) {
	t.Helper()
	path := filepath.Join(cwd, ".crw", "evidence", id, "test-receipt.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"kind": "test", "sourceIdentity": source.Capture(cwd, source.Options{ExcludeStateArtifacts: true}),
		"command": "go test ./...", "exitCode": 0, "createdAt": "2026-01-01T00:00:00.000Z",
		"ownerSessionId": id, "checkEpoch": epoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// orchestrateDcloseSeedAtC is seedBoundCycleAtC: a bound session at C with a valid receipt and a plan
// whose first phase is the fixed close target.
func orchestrateDcloseSeedAtC(t *testing.T, cwd, id, slug string, taskStatus goalplan.TaskStatus) {
	t.Helper()
	orchestrateDcloseSeedPlan(t, cwd, slug, taskStatus)
	s := state.DefaultState(id, slug)
	s.Phase = state.PhaseC
	epoch := "c-test-epoch"
	s.CheckEpoch = &epoch
	s.OrchestrationActive = true
	s.Flags = state.Flags{AuditPassed: true}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)
}

// orchestrateDcloseAttest is dAttest(id): the C>D attestation the oracle's helpers build.
func orchestrateDcloseAttest(id string) *attest.Attestation {
	zero := float64(0)
	return &attest.Attestation{
		From: state.PhaseC, To: state.PhaseD, Did: "ran the suite", CheckOutput: "722 pass", ExitCode: &zero,
		WorkPhaseID: "wp-1", TestReceiptPath: ".crw/evidence/" + id + "/test-receipt.json",
	}
}

// orchestrateDcloseRun is parsedDclose(cwd, id) plus runOrchestrateCli: the close the harness would run.
func orchestrateDcloseRun(t *testing.T, cwd, id string, seam orchestrateDcloseSeam) (CliResult, error) {
	t.Helper()
	return orchestrateDclose(cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, seam)
}

// orchestrateDcloseGoalplanRows reads the plan's own ledger rows.
func orchestrateDcloseGoalplanRows(t *testing.T, cwd, slug string) []map[string]any {
	t.Helper()
	rows, err := orchestrateDcloseReadJSONLObjects(filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// orchestrateDcloseGoalplan is the stored plan.
func orchestrateDcloseGoalplan(t *testing.T, cwd, slug string) *goalplan.Goalplan {
	t.Helper()
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		t.Fatalf("goalplan %s could not be read", slug)
	}
	return plan
}

// orchestrateDclosePhaseStatus is one phase's stored status.
func orchestrateDclosePhaseStatus(t *testing.T, cwd, slug, id string) goalplan.WorkPhaseStatus {
	t.Helper()
	for _, wp := range orchestrateDcloseGoalplan(t, cwd, slug).WorkPhases {
		if wp.ID == id {
			return wp.Status
		}
	}
	t.Fatalf("work phase %s not found", id)
	return ""
}

// orchestrateDcloseDoneRows counts this session's C -> IDLE rows.
func orchestrateDcloseDoneRows(t *testing.T, cwd, id string) int {
	t.Helper()
	n := 0
	for _, row := range orchestrateTransitionLedger(t, cwd) {
		if row["sessionId"] == id && row["from"] == "C" && row["to"] == "IDLE" {
			n++
		}
	}
	return n
}

// orchestrateDcloseSnapshot records the bytes of the named files, with "<absent>" for a missing one.
func orchestrateDcloseSnapshot(t *testing.T, paths ...string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			out[p] = "<absent>"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[p] = string(raw)
	}
	return out
}

func orchestrateDcloseAssertUnchanged(t *testing.T, before map[string]string) {
	t.Helper()
	for p, want := range before {
		raw, err := os.ReadFile(p)
		got := "<absent>"
		if err == nil {
			got = string(raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s changed:\n%s", p, got)
		}
	}
}

// orchestrateDcloseTree lists the files under a root, sorted.
func orchestrateDcloseTree(t *testing.T, root string) []string {
	t.Helper()
	names := []string{}
	if root == "" {
		return names
	}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, path)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

// TestOrchestrateDcloseRefusesWhileATaskIsOpen is "D-close is refused while the active work-phase still
// has open tasks, and writes nothing" (:832): the pending task is named, the phase stays at C, no ledger
// row is written and the plan file is byte-identical.
func TestOrchestrateDcloseRefusesWhileATaskIsOpen(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-pending", "cycle-gate-pending"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskPending)
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath, state.StatePath(cwd, id))

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "open task") || !strings.Contains(got.Output, "t-1") ||
		!strings.Contains(got.Output, "CYCLE-COMPLETION-01") {
		t.Fatalf("refusal: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseC {
		t.Fatal("the refusal moved the session")
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refusal wrote a ledger row: %+v", rows)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseClosesAndStartsTheNext is "D-close succeeds once the tasks are done, closing the
// phase and starting the next" (:853): wp-1 done, wp-2 in_progress, the cursor on wp-2, one done row.
func TestOrchestrateDcloseClosesAndStartsTheNext(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-done", "cycle-gate-done"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "close target wp-1 is complete") {
		t.Fatalf("close: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the close did not reach IDLE")
	}
	plan := orchestrateDcloseGoalplan(t, cwd, slug)
	if plan.WorkPhases[0].Status != goalplan.WorkPhaseDone || plan.WorkPhases[1].Status != goalplan.WorkPhaseInProgress {
		t.Fatalf("plan: %+v", plan.WorkPhases)
	}
	if plan.ActiveWorkPhaseID == nil || *plan.ActiveWorkPhaseID != "wp-2" {
		t.Fatalf("cursor: %v", plan.ActiveWorkPhaseID)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows = %d, want 1", n)
	}
	// The row's key order is the oracle's: the close key first, the evidence last
	// (orchestrate-cli.ts:899 and :1040; the recorded fixture holds the same order).
	line := orchestrateTransitionRowLine(t, cwd, 0)
	shape := "\",\"from\":\"C\",\"to\":\"IDLE\",\"reason\":\"done\",\"checkEpoch\":\"c-test-epoch\"," +
		"\"closedWorkPhaseId\":\"wp-1\",\"evidence\":\"ran the suite\"}"
	if !strings.Contains(line, shape) {
		t.Fatalf("done row shape:\n%s", line)
	}
	var done, started int
	for _, row := range orchestrateDcloseGoalplanRows(t, cwd, slug) {
		switch row["event"] {
		case "workphase_done":
			if row["detail"] == "closed wp-1" {
				done++
			}
		case "workphase_started":
			if row["detail"] == "started wp-2" {
				started++
			}
		}
	}
	if done != 1 || started != 1 {
		t.Fatalf("goalplan rows: done=%d started=%d", done, started)
	}
}

// TestOrchestrateDcloseRefusesAnUnreadableGoalplan is "D-close on a bound session is refused when the
// goalplan cannot be read" (:869): a hand-removed plan file must not become the cheapest way past the gate.
func TestOrchestrateDcloseRefusesAnUnreadableGoalplan(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-unreadable", "cycle-gate-gone"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	if err := os.Remove(filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")); err != nil {
		t.Fatal(err)
	}
	before := orchestrateDcloseSnapshot(t, state.StatePath(cwd, id))

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "could not be read") {
		t.Fatalf("refusal: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseC {
		t.Fatal("the refusal moved the session")
	}
	if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
		t.Fatalf("the refusal wrote a ledger row: %+v", rows)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseAllDoneClosesTheCycleOnly is "D-close succeeds when every work-phase is already
// done" (:886): an all-done plan closes the cycle, mints no marker, writes one row with a null closed
// work phase, and leaves the plan's own ledger alone.
func TestOrchestrateDcloseAllDoneClosesTheCycleOnly(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-all-done", "cycle-gate-complete"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "all phases done"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "closed", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch, s.OrchestrationActive = state.PhaseC, &epoch, true
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 || strings.Contains(got.Output, "blocked or superseded") {
		t.Fatalf("all-done close: %+v", got)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseIdle || after.DcloseRecovery != nil {
		t.Fatalf("state: %+v", after)
	}
	rows := orchestrateTransitionLedger(t, cwd)
	if len(rows) != 1 || rows[0]["checkEpoch"] != epoch {
		t.Fatalf("rows: %+v", rows)
	}
	if key, present := rows[0]["closedWorkPhaseId"]; !present || key != nil {
		t.Fatalf("closedWorkPhaseId = %v (present %v), want an explicit null", key, present)
	}
	if rows := orchestrateDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
		t.Fatalf("all-done wrote a goalplan row: %+v", rows)
	}
}

// TestOrchestrateDcloseAllDoneTakesNoFinalizeLock is "all-done close writes its PABCD row inside the
// first lock and takes no finalization lock" (:1397): a lock directory created from afterStateWrite
// would break a second critical section, so the output must stay silent about pending finalization.
func TestOrchestrateDcloseAllDoneTakesNoFinalizeLock(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "all-done-single-lock", "all-done-single-lock-plan"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "already finished"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-all-done"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch, s.OrchestrationActive = state.PhaseC, &epoch, true
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)

	stateWrites := 0
	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{afterStateWrite: func() error {
		stateWrites++
		// A lock held from here on would break a finalization pass. all-done must be finished already.
		lockDir, err := goalplan.GoalplanWriteLockDir(cwd, slug)
		if err != nil {
			return err
		}
		return os.Mkdir(lockDir, 0o700)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 || strings.Contains(got.Output, "finalization is pending") {
		t.Fatalf("all-done close: %+v", got)
	}
	if stateWrites != 1 {
		t.Fatalf("state writes = %d, want 1", stateWrites)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseIdle || after.DcloseRecovery != nil {
		t.Fatalf("state: %+v", after)
	}
}

// TestOrchestrateDcloseRefusesAnEmptyPlan is "D-close is refused when the bound goalplan is empty" (:910).
func TestOrchestrateDcloseRefusesAnEmptyPlan(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-empty-plan", "cycle-gate-empty"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "no phases registered"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{}
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch = state.PhaseC, &epoch
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "the plan is empty") {
		t.Fatalf("refusal: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseC {
		t.Fatal("the refusal moved the session")
	}
}

// TestOrchestrateDcloseReportsADeadlock is "D-close is refused when every remaining work-phase is
// blocked" (:934) and "wp4: D-close reports dependency deadlock and writes nothing" (:1155).
func TestOrchestrateDcloseReportsADeadlock(t *testing.T) {
	t.Run("blocked-only", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "cycle-all-blocked", "cycle-gate-blocked"
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "everything blocked"})
		plan.Slug = slug
		plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "stuck", Status: goalplan.WorkPhaseBlocked, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
		plan.ActiveWorkPhaseID = nil
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		epoch := "c-test-epoch"
		s := state.DefaultState(id, slug)
		s.Phase, s.CheckEpoch = state.PhaseC, &epoch
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		orchestrateDcloseSeedReceipt(t, cwd, id, epoch)

		got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "Dependency deadlock: work-phase wp-1 is blocked") {
			t.Fatalf("refusal: %+v", got)
		}
		if state.ReadState(cwd, id).Phase != state.PhaseC {
			t.Fatal("the refusal moved the session")
		}
	})
	t.Run("unmet-dependency", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "cycle-dependency-deadlock", "cycle-dependency-deadlock"
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "dependency deadlock"})
		plan.Slug = slug
		version := float64(3)
		plan.SchemaVersion = &version
		reason := "vendor"
		plan.WorkPhases = []goalplan.GoalplanWorkPhase{
			{ID: "wp-1", Title: "upstream", Status: goalplan.WorkPhaseBlocked, BlockedReason: &reason, DependsOn: []string{}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
			{ID: "wp-2", Title: "downstream", Status: goalplan.WorkPhasePending, DependsOn: []string{"wp-1"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		}
		plan.ActiveWorkPhaseID = nil
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		epoch := "c-test-epoch"
		s := state.DefaultState(id, slug)
		s.Phase, s.CheckEpoch = state.PhaseC, &epoch
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		orchestrateDcloseSeedReceipt(t, cwd, id, epoch)
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "Dependency deadlock") ||
			!strings.Contains(got.Output, "wp-2 waits for work-phase wp-1 (blocked)") {
			t.Fatalf("refusal: %+v", got)
		}
		if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
			t.Fatalf("the refusal wrote a ledger row: %+v", rows)
		}
		orchestrateDcloseAssertUnchanged(t, before)
	})
}

// TestOrchestrateDcloseRefusesAnInvalidPlan is "CLI D-close rejects an invalid v3 dependency plan before
// every write" (:2585): the integrity reasons are computed inside the lock and refuse before any write.
func TestOrchestrateDcloseRefusesAnInvalidPlan(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "invalid-v3-cli-close", "invalid-v3-cli-close-plan"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "invalid dependency close"})
	plan.Slug = slug
	version := float64(3)
	plan.SchemaVersion = &version
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "broken", Status: goalplan.WorkPhaseInProgress, DependsOn: []string{"missing"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	active := "wp-1"
	plan.ActiveWorkPhaseID = &active
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-invalid"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch = state.PhaseC, &epoch
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)
	before := orchestrateDcloseSnapshot(t,
		filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json"),
		state.StatePath(cwd, id),
		filepath.Join(cwd, ".crw", "ledger.jsonl"),
		filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"))

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "invalid goalplan") {
		t.Fatalf("refusal: %+v", got)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseRefusesALockTimeout is "D-close lock timeout returns code 1 and leaves phase, plan,
// and both ledgers unchanged" (:2570).
func TestOrchestrateDcloseRefusesALockTimeout(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-lock-timeout", "cycle-gate-lock-timeout"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	lockDir, err := goalplan.GoalplanWriteLockDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "owner.json"), []byte("{\"pid\":4242}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := orchestrateDcloseSnapshot(t,
		filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json"),
		filepath.Join(cwd, ".crw", "ledger.jsonl"),
		filepath.Join(cwd, ".crw", "goalplans", slug, "ledger.jsonl"))

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, ".goalplan.lock") || !strings.Contains(got.Output, "D-close was not applied") {
		t.Fatalf("refusal: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseC {
		t.Fatal("the lock refusal moved the session")
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseUnboundCloseIsUnchanged is "an unbound (HITL) session closes its cycle exactly as
// before" (:972): the pre-wp5 wording, no goalplan lock, one cleared-IDLE write and one done row.
func TestOrchestrateDcloseUnboundCloseIsUnchanged(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id := "cycle-hitl"
	s := state.DefaultState(id, "")
	s.Phase = state.PhaseC
	s.Flags = state.Flags{AuditPassed: true}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}

	zero := float64(0)
	got, err := orchestrateDclose(cwd, id, "", state.ReadState(cwd, id), &attest.Attestation{
		From: state.PhaseC, To: state.PhaseD, Did: "ran the suite", CheckOutput: "722 pass", ExitCode: &zero,
	}, false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	want := "orchestrate D: current=C -> IDLE (C \u2192 IDLE, cycle closed, session " + id + ")"
	if got.Code != 0 || got.Output != want {
		t.Fatalf("output = %q, want %q", got.Output, want)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the unbound close did not reach IDLE")
	}
}

// TestOrchestrateDcloseMissingWorkPhaseIDRefused is the §35-5 input check: a bound, non-all-done plan
// with no workPhaseId in the attestation is refused with the oracle's text and writes nothing.
func TestOrchestrateDcloseMissingWorkPhaseIDRefused(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-no-target", "cycle-no-target-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	before := orchestrateDcloseSnapshot(t, state.StatePath(cwd, id), filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json"))

	att := orchestrateDcloseAttest(id)
	att.WorkPhaseID = ""
	got, err := orchestrateDclose(cwd, id, "", state.ReadState(cwd, id), att, false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || got.Output != "orchestrate D: attest.workPhaseId is required. Nothing was written." {
		t.Fatalf("refusal: %+v", got)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseTargetNotInPlanRefused is the §35-5 membership check.
func TestOrchestrateDcloseTargetNotInPlanRefused(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-wrong-target", "cycle-wrong-target-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	before := orchestrateDcloseSnapshot(t, state.StatePath(cwd, id), filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json"))

	got, err := orchestrateDclose(cwd, id, "wp-9", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || got.Output != "orchestrate D: work-phase wp-9 is not in the bound goalplan. Nothing was written." {
		t.Fatalf("refusal: %+v", got)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseTargetNotTheActivePhaseRefused is the §35-6 fixed-target check: a past, already
// done phase cannot be closed again, and the refusal names the active one ("past done phase id in C does
// not become a recovery marker", :1189).
func TestOrchestrateDcloseTargetNotTheActivePhaseRefused(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "past-done-c", "past-done-c-plan"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "past done phase"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "past", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "current", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	active := "wp-2"
	plan.ActiveWorkPhaseID = &active
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-past"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch = state.PhaseC, &epoch
	s.Flags = state.Flags{AuditPassed: true, CheckPassed: true}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	// The oracle's own case (:1189) reaches this through the work-phase binding gate that
	// orchestrateTransitionApply runs before the close, which says "active work-phase is wp-2". Driven at
	// the close itself the §35-6 fixed-target check answers instead, and it names the same active phase.
	if got.Code != 1 || !strings.Contains(got.Output, "does not match active work-phase wp-2") {
		t.Fatalf("refusal: %+v", got)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseC || after.DcloseRecovery != nil {
		t.Fatalf("state: %+v", after)
	}
	if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-2"); got != goalplan.WorkPhaseInProgress {
		t.Fatalf("wp-2 = %s", got)
	}
}

// TestOrchestrateDcloseRequiresTheCheckEpoch pins what a bound session without a check epoch actually gets:
// CHECK-BINDING-01 refuses first, because validateCheckReceipt requires the epoch the receipt binds to. The
// oracle's §35-8 "current C check epoch is required" branch is therefore unreachable for a non-recovering
// bound close (docs/port-cxc/known-defects/CRW-756.md), and the port keeps it as the oracle wrote it.
func TestOrchestrateDcloseRequiresTheCheckEpoch(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-no-epoch", "cycle-no-epoch-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	s := state.ReadState(cwd, id)
	s.CheckEpoch = nil
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath)

	got, err := orchestrateDclose(cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "CHECK-BINDING-01") || !strings.Contains(got.Output, "Nothing was written.") {
		t.Fatalf("refusal: %+v", got)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseRequiresATestReceipt is CHECK-BINDING-01 (orchestrate-cli.test.ts:960): a bound
// session's C>D must name a receipt this cycle produced, and the refusal runs before any write.
func TestOrchestrateDcloseRequiresATestReceipt(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-no-receipt", "cycle-no-receipt-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	before := orchestrateDcloseSnapshot(t, filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json"), state.StatePath(cwd, id))

	att := orchestrateDcloseAttest(id)
	att.TestReceiptPath = ""
	got, err := orchestrateDclose(cwd, id, "wp-1", state.ReadState(cwd, id), att, false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "CHECK-BINDING-01") || !strings.Contains(got.Output, "Nothing was written.") {
		t.Fatalf("refusal: %+v", got)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseWriteOrder is the §5/§35-8 order the issue's c2 names: the recovery marker lands
// before the plan commit, and both goalplan rows follow it. The seam stops the close at each step, so
// the order is observed rather than inferred.
func TestOrchestrateDcloseWriteOrder(t *testing.T) {
	t.Run("after the marker write the plan is untouched", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "order-marker", "order-marker-plan"
		orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		stop := errors.New("stop after the marker")
		_, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{afterRecoveryMarkerWrite: func() error { return stop }})
		if !errors.Is(err, stop) {
			t.Fatalf("err = %v, want the seam's error", err)
		}
		marker := state.ReadState(cwd, id).DcloseRecovery
		if marker == nil || marker.ClosedWorkPhaseID != "wp-1" || marker.NextWorkPhaseID == nil || *marker.NextWorkPhaseID != "wp-2" {
			t.Fatalf("marker: %+v", marker)
		}
		if state.ReadState(cwd, id).Phase != state.PhaseC {
			t.Fatal("the marker write moved the phase")
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if rows := orchestrateDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
			t.Fatalf("a goalplan row preceded the plan commit: %+v", rows)
		}
	})
	t.Run("after the plan commit the goalplan rows are absent", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "order-commit", "order-commit-plan"
		orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)

		stop := errors.New("stop after the plan commit")
		_, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{afterGoalplanCommit: func() error { return stop }})
		if !errors.Is(err, stop) {
			t.Fatalf("err = %v, want the seam's error", err)
		}
		if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-1"); got != goalplan.WorkPhaseDone {
			t.Fatalf("wp-1 = %s, want the committed close", got)
		}
		if rows := orchestrateDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
			t.Fatalf("a goalplan row preceded the marker's commit: %+v", rows)
		}
	})
}

// TestOrchestrateDcloseRetryWritesNoDuplicateRow covers "D-close retry after goalplan commit closes the
// fixed phase only once" (:1274), "D-close retry after the recovery marker write closes the fixed phase
// like a normal close" (:1312) and "D-close retry after state write appends only the missing PABCD row" (:2268).
func TestOrchestrateDcloseRetryWritesNoDuplicateRow(t *testing.T) {
	cases := []struct {
		name string
		seam orchestrateDcloseSeam
	}{
		{"after-goalplan-commit", orchestrateDcloseSeam{afterGoalplanCommit: func() error { return errors.New("fail after goalplan commit") }}},
		{"after-recovery-marker", orchestrateDcloseSeam{afterRecoveryMarkerWrite: func() error { return errors.New("fail right after the marker") }}},
		{"after-state-write", orchestrateDcloseSeam{afterStateWrite: func() error { return errors.New("fail after state write") }}},
		{"after-pabcd-append", orchestrateDcloseSeam{afterPabcdLedgerAppend: func() error { return errors.New("fail after PABCD append") }}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := orchestrateDcloseTestCwd(t)
			id, slug := "retry-"+c.name, "retry-"+c.name+"-plan"
			orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)

			if _, err := orchestrateDcloseRun(t, cwd, id, c.seam); err == nil {
				t.Fatal("the seam's error did not reach the caller")
			}
			if n := orchestrateDcloseDoneRows(t, cwd, id); n > 1 {
				t.Fatalf("the interrupted close wrote %d rows", n)
			}
			recovering := state.MatchesDcloseRecovery(state.ReadState(cwd, id), "wp-1")
			got, err := orchestrateDclose(cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), recovering, orchestrateDcloseSeam{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Code != 0 {
				t.Fatalf("retry: %+v", got)
			}
			if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
				t.Fatalf("done rows = %d, want 1", n)
			}
			if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-1"); got != goalplan.WorkPhaseDone {
				t.Fatalf("wp-1 = %s", got)
			}
			if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-2"); got != goalplan.WorkPhaseInProgress {
				t.Fatalf("wp-2 = %s", got)
			}
			after := state.ReadState(cwd, id)
			if after.Phase != state.PhaseIdle || after.DcloseRecovery != nil || after.CheckEpoch != nil {
				t.Fatalf("state: %+v", after)
			}
			var done, started int
			for _, row := range orchestrateDcloseGoalplanRows(t, cwd, slug) {
				switch row["event"] {
				case "workphase_done":
					done++
				case "workphase_started":
					started++
				}
			}
			if done != 1 || started != 1 {
				t.Fatalf("goalplan rows: done=%d started=%d", done, started)
			}
		})
	}
}

// TestOrchestrateDcloseFinalizeLockFailureAnswersCodeZero is §39 Y3: the state write already moved the
// FSM to IDLE outside the lock, so a failed finalization lock answers code 0 with the pending text and
// leaves the marker for the next request.
func TestOrchestrateDcloseFinalizeLockFailureAnswersCodeZero(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-finalize-pending", "cycle-finalize-pending-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)

	lockDir, err := goalplan.GoalplanWriteLockDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	// The first lock is already released by the time the state write runs, so a lock taken here fails
	// only the finalization pass. The hook answers nil, because the close must continue into it.
	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{afterStateWrite: func() error {
		return os.Mkdir(lockDir, 0o700)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 || !strings.Contains(got.Output, "finalization is pending") ||
		!strings.Contains(got.Output, "The recovery marker is still on the session") {
		t.Fatalf("pending finalization: %+v", got)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseIdle || after.DcloseRecovery == nil {
		t.Fatalf("state: %+v", after)
	}
	// The lock this test took is its own artefact; release it before the retry, as an operator does
	// after verifying no writer is active.
	if err := os.Remove(lockDir); err != nil {
		t.Fatal(err)
	}
	// The marker survives, so the next D request for the same tuple finishes the cleanup. It must
	// answer code 0 with the completion text and write the row it still owed.
	retry, err := orchestrateDclose(cwd, id, "wp-1", state.ReadState(cwd, id), orchestrateDcloseAttest(id), true, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Code != 0 || !strings.Contains(retry.Output, "close target wp-1 is complete") {
		t.Fatalf("retry: %+v", retry)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows = %d, want 1", n)
	}
	if cleared := state.ReadState(cwd, id); cleared.DcloseRecovery != nil || cleared.CheckEpoch != nil {
		t.Fatalf("marker survived the finalize: %+v", cleared)
	}
}

// TestOrchestrateDclosePublishedStateWriteWarns is the CRW-744/CRW-811 rule: a state write that reports
// state.Published (renamed into place, only the directory sync failed) counts as written, the close
// continues, and the answer carries the warning line. A failure before the rename stays an error.
func TestOrchestrateDclosePublishedStateWriteWarns(t *testing.T) {
	t.Run("published counts as written", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "published-write", "published-write-plan"
		orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
		seam := orchestrateDcloseSeam{writeState: func(c string, next state.State) error {
			if err := state.WriteState(c, next); err != nil {
				return err
			}
			return &state.PublishedError{Err: syscall.EIO}
		}}
		got, err := orchestrateDcloseRun(t, cwd, id, seam)
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 || !strings.Contains(got.Output, "close target wp-1 is complete") {
			t.Fatalf("close: %+v", got)
		}
		if !strings.Contains(got.Output, orchestrateDcloseStateWarning) || !strings.Contains(got.Output, "input/output error") {
			t.Fatalf("warning missing: %q", got.Output)
		}
		if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
			t.Fatalf("done rows = %d, want 1", n)
		}
		if state.ReadState(cwd, id).Phase != state.PhaseIdle {
			t.Fatal("the published write did not reach IDLE")
		}
	})
	t.Run("a pre-publication failure stays an error", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "unpublished-write", "unpublished-write-plan"
		orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
		seam := orchestrateDcloseSeam{writeState: func(string, state.State) error { return syscall.EIO }}
		if _, err := orchestrateDcloseRun(t, cwd, id, seam); err == nil {
			t.Fatal("a pre-publication failure was reported as success")
		}
		if state.ReadState(cwd, id).Phase != state.PhaseC {
			t.Fatal("a failed write moved the session")
		}
		if rows := orchestrateTransitionLedger(t, cwd); len(rows) != 0 {
			t.Fatalf("a failed write appended a row: %+v", rows)
		}
	})
}

// TestOrchestrateDcloseRefusesALossyStateRewrite is the port's data-loss rule for this writer: a session
// whose stored record the reader would rewrite is refused before the first write.
func TestOrchestrateDcloseRefusesALossyStateRewrite(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-lossy", "cycle-lossy-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	// The stored file carries a lone UTF-16 surrogate escape, which the reader normalizes to U+FFFD,
	// so publishing the rebuilt state would rewrite it. The document is edited as text because
	// json.Marshal would emit the replacement character instead of the escape the reader normalizes.
	raw, err := os.ReadFile(state.StatePath(cwd, id))
	if err != nil {
		t.Fatal(err)
	}
	claim := "\\ud800"
	rewritten := strings.Replace(string(raw), "\"unverifiedSubagents\": []",
		"\"unverifiedSubagents\": [{\"agentId\": \"a\", \"recordedAt\": \"2026-01-01T00:00:00.000Z\", \"receiptClaimed\": \""+claim+"\"}]", 1)
	if rewritten == string(raw) {
		t.Fatal("the seeded session file has no empty unverifiedSubagents list to replace")
	}
	if err := os.WriteFile(state.StatePath(cwd, id), []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath)

	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "refusing to overwrite it") {
		t.Fatalf("refusal: %+v", got)
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseTwoCyclesHaveDistinctCloseKeys is "one session closes two consecutive cycles with
// distinct close keys" (:2484).
func TestOrchestrateDcloseTwoCyclesHaveDistinctCloseKeys(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "two-cycles-one-session", "two-cycles-one-session-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)

	first, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if first.Code != 0 {
		t.Fatalf("first close: %+v", first)
	}
	if plan := orchestrateDcloseGoalplan(t, cwd, slug); plan.ActiveWorkPhaseID == nil || *plan.ActiveWorkPhaseID != "wp-2" {
		t.Fatalf("cursor after the first close: %v", plan.ActiveWorkPhaseID)
	}
	secondEpoch := "c-second-cycle"
	s := state.ReadState(cwd, id)
	s.Phase, s.CheckEpoch, s.DcloseRecovery = state.PhaseC, &secondEpoch, nil
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, secondEpoch)
	secondAtt := orchestrateDcloseAttest(id)
	secondAtt.WorkPhaseID = "wp-2"
	got, err := orchestrateDclose(cwd, id, "wp-2", state.ReadState(cwd, id), secondAtt, false, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("second close: %+v", got)
	}
	var keys [][2]any
	for _, row := range orchestrateTransitionLedger(t, cwd) {
		if row["sessionId"] == id && row["from"] == "C" && row["to"] == "IDLE" {
			keys = append(keys, [2]any{row["checkEpoch"], row["closedWorkPhaseId"]})
		}
	}
	if len(keys) != 2 || keys[0][0] != "c-test-epoch" || keys[0][1] != "wp-1" || keys[1][0] != secondEpoch || keys[1][1] != "wp-2" {
		t.Fatalf("close keys: %+v", keys)
	}
	if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-2"); got != goalplan.WorkPhaseDone {
		t.Fatalf("wp-2 = %s", got)
	}
}

// TestOrchestrateDcloseLeavesRealHomesAlone is the packet's real-state rule, applied to this writer: the
// temporary home roots are compared before and after, and a difference is reported rather than cleaned up.
func TestOrchestrateDcloseLeavesRealHomesAlone(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "homes-untouched", "homes-untouched-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	codexHome, crwHome := os.Getenv("CODEX_HOME"), os.Getenv("CRW_HOME")
	beforeCodex, beforeCrw := orchestrateDcloseTree(t, codexHome), orchestrateDcloseTree(t, crwHome)

	if _, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{}); err != nil {
		t.Fatal(err)
	}
	if got := orchestrateDcloseTree(t, codexHome); strings.Join(got, "\n") != strings.Join(beforeCodex, "\n") {
		t.Fatalf("CODEX_HOME changed: %v -> %v", beforeCodex, got)
	}
	if got := orchestrateDcloseTree(t, crwHome); strings.Join(got, "\n") != strings.Join(beforeCrw, "\n") {
		t.Fatalf("CRW_HOME changed: %v -> %v", beforeCrw, got)
	}
}

// TestOrchestrateDcloseHarnessPathRunsThePortedBody proves the verb's own call site reaches the port: the
// read half resolves the session and the transition half performs the close, with the unbound wording the
// oracle keeps. The bound path is covered by the cases above, which drive the same function the call site calls.
func TestOrchestrateDcloseHarnessPathRunsThePortedBody(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "d-close-unbound"
	orchestrateTransitionSession(t, cwd, id, "{\"phase\":\"C\",\"orchestrationActive\":true}")
	got := orchestrateTransitionRun(t, cwd, "D", "--session", id, "--attest",
		"{\"from\":\"C\",\"to\":\"D\",\"did\":\"verified\",\"checkOutput\":\"tests passed\",\"exitCode\":0}")
	if got.Code != 0 || !strings.Contains(got.Output, "orchestrate D: current=C -> IDLE (C \u2192 IDLE, cycle closed, session "+id+")") {
		t.Fatalf("close: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the close did not reach IDLE")
	}
}

// TestOrchestrateDcloseFailsClosedOnADamagedLedger is the Codex review finding on this pull request: a
// ledger line that is exactly `null` decodes without an error, and a scanner that read it as an empty
// object would answer "no row yet" and append a duplicate. The oracle throws there, so the port refuses
// too. The all-done path shows it: its row guard runs inside the first lock, before any write, so the
// refusal leaves the plan, the state and both ledgers exactly as they were.
func TestOrchestrateDcloseFailsClosedOnADamagedLedger(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "damaged-ledger", "damaged-ledger-plan"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "damaged ledger"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "closed", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch = state.PhaseC, &epoch
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)
	ledgerPath := filepath.Join(cwd, ".crw", "ledger.jsonl")
	if err := os.WriteFile(ledgerPath, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath, ledgerPath, state.StatePath(cwd, id))

	if _, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{}); err == nil {
		t.Fatal("a damaged ledger was read as an empty one")
	}
	orchestrateDcloseAssertUnchanged(t, before)
	if state.ReadState(cwd, id).Phase != state.PhaseC {
		t.Fatal("the refusal moved the session")
	}
}

// TestOrchestrateDcloseUnboundRowSurvivesAFailedAppend is CRW-1097's answer to the departure CRW-756
// kept: the oracle publishes IDLE before it appends the done row, so an append that failed left the
// session resting with no record of the close, and the FSM has no IDLE -> D edge for a retry to use. The
// row is now prepared in the session's ledger outbox before IDLE is published: the close answers success
// with the pending warning, the retry is still refused, and the next locked call of the session records
// the row exactly once.
func TestOrchestrateDcloseUnboundRowSurvivesAFailedAppend(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id := "cycle-hitl-order"
	s := state.DefaultState(id, "")
	s.Phase = state.PhaseC
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	// A ledger path that cannot be appended to, so the state write succeeds and the append fails.
	ledgerDir := filepath.Join(cwd, ".crw", "ledger.jsonl")
	if err := os.MkdirAll(ledgerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	zero := float64(0)
	att := &attest.Attestation{From: state.PhaseC, To: state.PhaseD, Did: "ran the suite", CheckOutput: "722 pass", ExitCode: &zero}

	got, err := orchestrateDclose(cwd, id, "", state.ReadState(cwd, id), att, false, orchestrateDcloseSeam{})
	if err != nil || got.Code != 0 || !strings.Contains(got.Output, "warning: ledger row for C -> IDLE could not be written: ") ||
		!strings.Contains(got.Output, "kept pending") {
		t.Fatalf("an applied close whose append failed answered (%+v, %v); want success with the pending warning", got, err)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the unbound close did not publish IDLE")
	}
	if err := os.Remove(ledgerDir); err != nil {
		t.Fatal(err)
	}
	// The FSM still refuses the retry; the call takes the session lock and records the pending row.
	retry := orchestrateTransitionRun(t, cwd, "D", "--session", id, "--attest",
		"{\"from\":\"C\",\"to\":\"D\",\"did\":\"verified\",\"checkOutput\":\"tests passed\",\"exitCode\":0}")
	if retry.Code == 0 {
		t.Fatalf("a retry of the unbound close was accepted: %+v", retry)
	}
	orchestrateTransitionRun(t, cwd, "reset", "--session", id)
	rows := orchestrateTransitionLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "C" || rows[0]["to"] != "IDLE" || rows[0]["reason"] != "done" || rows[0]["evidence"] != "ran the suite" {
		t.Fatalf("the ledger after the next calls: %+v, want exactly the close's done row", rows)
	}
}

// TestOrchestrateDcloseAllDoneStateWriteFailureReconciles covers the Devin review finding of CRW-756 as
// CRW-1097 answers it: the oracle appended the all-done close's C -> IDLE row inside the first lock, before
// the IDLE state write, so a write that failed before the publication left a done row beside a session
// still at C. The row is now prepared in the session's ledger outbox with the IDLE write, so the failed
// write leaves no row at all, and the same D request closes the cycle with exactly one.
func TestOrchestrateDcloseAllDoneStateWriteFailureReconciles(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "all-done-state-failure", "all-done-state-failure-plan"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "already finished"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	epoch := "c-all-done"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch, s.OrchestrationActive = state.PhaseC, &epoch, true
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	orchestrateDcloseSeedReceipt(t, cwd, id, epoch)

	// The state write fails before publication, so the close reports it and the session stays at C.
	failing := orchestrateDcloseSeam{writeState: func(string, state.State) error { return syscall.EIO }}
	if _, err := orchestrateDcloseRun(t, cwd, id, failing); err == nil {
		t.Fatal("a pre-publication failure was reported as success")
	}
	if state.ReadState(cwd, id).Phase != state.PhaseC {
		t.Fatal("the failed write moved the session")
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 0 {
		t.Fatalf("done rows = %d, want none: the close did not happen", n)
	}

	// The same request reconciles: no second row, and the state reaches IDLE.
	got, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("retry: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the retry did not reach IDLE")
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows = %d, want 1", n)
	}
}
