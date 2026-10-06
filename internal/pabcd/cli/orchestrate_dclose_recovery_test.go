package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// These are the D close recovery cases of CXC v0.2.40 pabcd-state/test/orchestrate-cli.test.ts
// (:1189-2540), merged into CRW-756 from CRW-757. Every case seeds a session whose marker matches the
// request, which is what matchesDcloseRecovery makes of a retry, and drives the same orchestrateDclose
// the verb's call site calls with recovering true.
//
// The helpers come from orchestrate_dclose_test.go and orchestrate_transition_test.go; HOME, CODEX_HOME
// and CRW_HOME point into temporary directories before anything here runs.

// orchestrateDcloseRecoverySeed is the state seedBoundCycleAtC leaves behind after a crash: a bound
// session at C with a marker naming wp-1 and the successor wp-2 it picked.
func orchestrateDcloseRecoverySeed(t *testing.T, cwd, id, slug string) {
	t.Helper()
	orchestrateDcloseSeedPlan(t, cwd, slug, goalplan.TaskDone)
	next := "wp-2"
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch, s.OrchestrationActive = state.PhaseC, &epoch, true
	s.DcloseRecovery = &state.DcloseRecoveryMarker{
		SessionID: id, CheckEpoch: epoch, ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: &next,
	}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
}

// orchestrateDcloseRecoveryRun drives the retry the relay would send: the same D request, with the
// marker matching, so the close takes the recovery branch.
func orchestrateDcloseRecoveryRun(t *testing.T, cwd, id string, seam orchestrateDcloseSeam) (CliResult, error) {
	t.Helper()
	cur := state.ReadState(cwd, id)
	recovering := state.MatchesDcloseRecovery(cur, "wp-1")
	if !recovering {
		t.Fatalf("the seeded marker does not match the request: %+v", cur.DcloseRecovery)
	}
	return orchestrateDclose(cwd, id, "wp-1", cur, orchestrateDcloseAttest(id), true, seam)
}

// orchestrateDcloseRecoveryStatuses is the stored [id, status] pairs, for shape comparisons.
func orchestrateDcloseRecoveryStatuses(t *testing.T, cwd, slug string) [][2]string {
	t.Helper()
	out := [][2]string{}
	for _, wp := range orchestrateDcloseGoalplan(t, cwd, slug).WorkPhases {
		out = append(out, [2]string{wp.ID, string(wp.Status)})
	}
	return out
}

// orchestrateDcloseRecoveryStartedRows is the plan ledger's started rows.
func orchestrateDcloseRecoveryStartedRows(t *testing.T, cwd, slug string) []string {
	t.Helper()
	out := []string{}
	for _, row := range orchestrateDcloseGoalplanRows(t, cwd, slug) {
		if row["event"] == "workphase_started" {
			detail, _ := row["detail"].(string)
			out = append(out, detail)
		}
	}
	return out
}

// TestOrchestrateDcloseRecoveryAfterTheMarkerWriteMatchesANormalClose is "D-close retry after the
// recovery marker write closes the fixed phase like a normal close" (:1312): the recovered plan must
// equal what an uninterrupted close would have written, and each ledger row appears once.
func TestOrchestrateDcloseRecoveryAfterTheMarkerWriteMatchesANormalClose(t *testing.T) {
	reference := orchestrateDcloseTestCwd(t)
	orchestrateDcloseSeedAtC(t, reference, "reference-close", "reference-close-plan", goalplan.TaskDone)
	if got, err := orchestrateDcloseRun(t, reference, "reference-close", orchestrateDcloseSeam{}); err != nil || got.Code != 0 {
		t.Fatalf("reference close: %+v %v", got, err)
	}
	referencePlan := orchestrateDcloseGoalplan(t, reference, "reference-close-plan")

	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "retry-after-marker", "retry-after-marker-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	stop := errors.New("fail right after the marker")
	if _, err := orchestrateDcloseRun(t, cwd, id, orchestrateDcloseSeam{afterRecoveryMarkerWrite: func() error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the seam's error", err)
	}
	// The marker survives and the plan is untouched: this is step 1 of the oracle's §5 table.
	crashed := state.ReadState(cwd, id)
	if crashed.Phase != state.PhaseC || crashed.DcloseRecovery == nil {
		t.Fatalf("state after the crash: %+v", crashed)
	}
	if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-1"); got != goalplan.WorkPhaseInProgress {
		t.Fatalf("wp-1 = %s, want the untouched in_progress", got)
	}

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("retry: %+v", got)
	}
	if a, b := orchestrateDcloseRecoveryStatuses(t, cwd, slug), orchestrateDcloseRecoveryStatuses(t, reference, "reference-close-plan"); len(a) != len(b) || a[0] != b[0] || a[1] != b[1] {
		t.Fatalf("recovered plan %v, want the reference %v", a, b)
	}
	plan, refPlan := orchestrateDcloseGoalplan(t, cwd, slug), referencePlan
	if orchestrateDcloseDeref(plan.ActiveWorkPhaseID) != orchestrateDcloseDeref(refPlan.ActiveWorkPhaseID) {
		t.Fatalf("cursor %v, want %v", plan.ActiveWorkPhaseID, refPlan.ActiveWorkPhaseID)
	}
	if started := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(started) != 1 || started[0] != "started wp-2" {
		t.Fatalf("started rows: %v", started)
	}
	var done int
	for _, row := range orchestrateDcloseGoalplanRows(t, cwd, slug) {
		if row["event"] == "workphase_done" && row["detail"] == "closed wp-1" {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("done rows = %d, want 1", done)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseIdle || after.DcloseRecovery != nil || after.CheckEpoch != nil {
		t.Fatalf("state: %+v", after)
	}
}

// TestOrchestrateDcloseRecoveryRefusesALegacyMarker is "recovery is refused when the marker predates the
// successor field" (:1963): the file cannot say whether the plan commit landed, so the close refuses,
// keeps the marker and writes nothing.
func TestOrchestrateDcloseRecoveryRefusesALegacyMarker(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "recovery-legacy-marker", "recovery-legacy-marker-plan"
	orchestrateDcloseSeedPlan(t, cwd, slug, goalplan.TaskDone)
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch = state.PhaseC, &epoch
	s.DcloseRecovery = &state.DcloseRecoveryMarker{
		SessionID: id, CheckEpoch: epoch, ClosedWorkPhaseID: "wp-1", Legacy: true,
	}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath)

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "predates the successor field") || !strings.Contains(got.Output, "The marker was kept") {
		t.Fatalf("refusal: %+v", got)
	}
	if !strings.Contains(got.Output, "crw pabcd orchestrate reset --session "+id) {
		t.Fatalf("the refusal does not name the reset command: %q", got.Output)
	}
	orchestrateDcloseAssertUnchanged(t, before)
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseC || after.DcloseRecovery == nil || !after.DcloseRecovery.Legacy {
		t.Fatalf("state: %+v", after)
	}
	if rows := orchestrateDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
		t.Fatalf("the refusal wrote a goalplan row: %+v", rows)
	}
}

// TestOrchestrateDcloseRecoveryRefusesATargetThatChanged covers "recovery is refused when the fixed
// target gained an open task after its marker" (:1424), "became blocked" (:1473) and "lost a
// dependency" (:1507). Each refusal keeps the marker and writes nothing, so the operator can fix the
// plan and finish with the same request.
func TestOrchestrateDcloseRecoveryRefusesATargetThatChanged(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(plan *goalplan.Goalplan)
		expect string
	}{
		{
			"gained-a-task",
			func(plan *goalplan.Goalplan) {
				plan.WorkPhases[0].Tasks = append(plan.WorkPhases[0].Tasks,
					goalplan.GoalplanTask{ID: "t-late", Title: "added late", Status: goalplan.TaskPending})
			},
			"recovery target wp-1 gained 1 open task(s) after its marker was written",
		},
		{
			"became-blocked",
			func(plan *goalplan.Goalplan) { plan.WorkPhases[0].Status = goalplan.WorkPhaseBlocked },
			"recovery target wp-1 is now blocked",
		},
		{
			"lost-a-dependency",
			func(plan *goalplan.Goalplan) { plan.WorkPhases[0].DependsOn = []string{"wp-2"} },
			"recovery target wp-1 now waits for wp-2",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := orchestrateDcloseTestCwd(t)
			id, slug := "recovery-"+c.name, "recovery-"+c.name+"-plan"
			orchestrateDcloseRecoverySeed(t, cwd, id, slug)
			plan := orchestrateDcloseGoalplan(t, cwd, slug)
			c.edit(plan)
			if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
			before := orchestrateDcloseSnapshot(t, planPath)

			got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Code != 1 || !strings.Contains(got.Output, c.expect) || !strings.Contains(got.Output, "The recovery marker was kept") {
				t.Fatalf("refusal: %+v", got)
			}
			if state.ReadState(cwd, id).Phase != state.PhaseC || state.ReadState(cwd, id).DcloseRecovery == nil {
				t.Fatal("the refusal cleared the marker")
			}
			orchestrateDcloseAssertUnchanged(t, before)
			if rows := orchestrateDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
				t.Fatalf("the refusal wrote a goalplan row: %+v", rows)
			}
		})
	}
}

// TestOrchestrateDcloseRecoveryRefusesAPendingTaskUnderADoneTarget is "recovery is refused when a
// pending task is hidden under the already-closed target" (:1555): the plan commit really landed, so
// only closeFixedWorkPhase running unconditionally catches the smuggled task.
func TestOrchestrateDcloseRecoveryRefusesAPendingTaskUnderADoneTarget(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "recovery-done-hides-pending", "recovery-done-hides-pending-plan"
	orchestrateDcloseRecoverySeed(t, cwd, id, slug)
	// The crash state the marker-then-crash case leaves: the target is done and the cursor moved.
	plan := orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	active := "wp-2"
	plan.ActiveWorkPhaseID = &active
	plan.WorkPhases[0].Tasks = append(plan.WorkPhases[0].Tasks,
		goalplan.GoalplanTask{ID: "t-hidden", Title: "snuck in", Status: goalplan.TaskPending})
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath)

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 1 || !strings.Contains(got.Output, "recovery target wp-1 gained 1 open task(s) after its marker was written") {
		t.Fatalf("refusal: %+v", got)
	}
	if state.ReadState(cwd, id).DcloseRecovery == nil {
		t.Fatal("the refusal cleared the marker")
	}
	orchestrateDcloseAssertUnchanged(t, before)
}

// TestOrchestrateDcloseRecoveryRebuildsAForgeableCursor covers "recovery repairs a forged cursor that
// points at a phase nobody activated" (:2172), "recovery re-runs the close when only the target status
// was edited to done" (:2226), "closing an open target does not cancel progress the plan already made"
// (:1668) and "a preserved cursor is dropped when the phase it names is not ready" (:1712).
func TestOrchestrateDcloseRecoveryRebuildsAForgeableCursor(t *testing.T) {
	t.Run("forged-cursor-onto-an-unactivated-phase", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-forged-cursor", "recovery-forged-cursor-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
		active := "wp-2"
		plan.ActiveWorkPhaseID = &active
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 {
			t.Fatalf("retry: %+v", got)
		}
		// The successor is genuinely activated, so the started row tells the truth.
		if started := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(started) != 1 || started[0] != "started wp-2" {
			t.Fatalf("started rows: %v", started)
		}
		if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-2"); got != goalplan.WorkPhaseInProgress {
			t.Fatalf("wp-2 = %s", got)
		}
		if state.ReadState(cwd, id).DcloseRecovery != nil {
			t.Fatal("the retry left the marker behind")
		}
	})
	t.Run("status-only-edit-is-not-a-commit", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-status-only-done", "recovery-status-only-done-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		// Exactly the crash state, with the status hand-edited and the cursor untouched.
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 {
			t.Fatalf("retry: %+v", got)
		}
		// The started row must name the successor, never the closed target.
		if started := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(started) != 1 || started[0] != "started wp-2" {
			t.Fatalf("started rows: %v", started)
		}
		if plan := orchestrateDcloseGoalplan(t, cwd, slug); orchestrateDcloseDeref(plan.ActiveWorkPhaseID) != "wp-2" {
			t.Fatalf("cursor: %v", plan.ActiveWorkPhaseID)
		}
	})
	t.Run("preserves-progress-the-plan-already-made", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-preserve-progress", "recovery-preserve-progress-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		plan.WorkPhases = append(plan.WorkPhases,
			goalplan.GoalplanWorkPhase{ID: "wp-3", Title: "third", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
		// The target never closed, but wp-2 ran and finished, starting wp-3.
		plan.WorkPhases[1].Status = goalplan.WorkPhaseDone
		plan.WorkPhases[2].Status = goalplan.WorkPhaseInProgress
		active := "wp-3"
		plan.ActiveWorkPhaseID = &active
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 {
			t.Fatalf("retry: %+v", got)
		}
		repaired := orchestrateDcloseGoalplan(t, cwd, slug)
		if orchestrateDcloseDeref(repaired.ActiveWorkPhaseID) != "wp-3" {
			t.Fatalf("cursor: %v", repaired.ActiveWorkPhaseID)
		}
		want := [][2]string{{"wp-1", "done"}, {"wp-2", "done"}, {"wp-3", "in_progress"}}
		gotStatuses := orchestrateDcloseRecoveryStatuses(t, cwd, slug)
		for i := range want {
			if gotStatuses[i] != want[i] {
				t.Fatalf("statuses %v, want %v", gotStatuses, want)
			}
		}
	})
	t.Run("drops-an-unready-preserved-cursor", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-preserve-unready", "recovery-preserve-unready-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		plan.WorkPhases = append(plan.WorkPhases,
			goalplan.GoalplanWorkPhase{ID: "wp-3", Title: "third", Status: goalplan.WorkPhasePending, DependsOn: []string{"wp-4"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
			goalplan.GoalplanWorkPhase{ID: "wp-4", Title: "fourth", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
		// wp-2 finished and left the cursor on wp-3, which still waits for wp-4.
		plan.WorkPhases[1].Status = goalplan.WorkPhaseDone
		plan.WorkPhases[2].Status = goalplan.WorkPhaseInProgress
		active := "wp-3"
		plan.ActiveWorkPhaseID = &active
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 {
			t.Fatalf("retry: %+v", got)
		}
		repaired := orchestrateDcloseGoalplan(t, cwd, slug)
		if repaired.ActiveWorkPhaseID != nil {
			t.Fatalf("cursor %v, want null", *repaired.ActiveWorkPhaseID)
		}
		// The point of dropping it: the derived selection is the only answer left, and it names the
		// phase that can actually run. wp-3 keeps running: stopping it is not a resume's job.
		if effective := goalplan.EffectiveActiveWorkPhaseID(repaired); effective == nil || *effective != "wp-4" {
			t.Fatalf("effective cursor: %v", effective)
		}
		if got := orchestrateDclosePhaseStatus(t, cwd, slug, "wp-3"); got != goalplan.WorkPhaseInProgress {
			t.Fatalf("wp-3 = %s", got)
		}
	})
	t.Run("already-done-target-still-normalizes", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-done-target-unready-cursor", "recovery-done-target-unready-cursor-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		plan.WorkPhases = append(plan.WorkPhases,
			goalplan.GoalplanWorkPhase{ID: "wp-3", Title: "third", Status: goalplan.WorkPhasePending, DependsOn: []string{"wp-4"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
			goalplan.GoalplanWorkPhase{ID: "wp-4", Title: "fourth", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
		// The commit landed, so wp-1 is done; wp-2 then finished and left the cursor on wp-3.
		plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
		plan.WorkPhases[1].Status = goalplan.WorkPhaseDone
		plan.WorkPhases[2].Status = goalplan.WorkPhaseInProgress
		active := "wp-3"
		plan.ActiveWorkPhaseID = &active
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 {
			t.Fatalf("retry: %+v", got)
		}
		repaired := orchestrateDcloseGoalplan(t, cwd, slug)
		if repaired.ActiveWorkPhaseID != nil {
			t.Fatalf("cursor %v, want null", *repaired.ActiveWorkPhaseID)
		}
		if effective := goalplan.EffectiveActiveWorkPhaseID(repaired); effective == nil || *effective != "wp-4" {
			t.Fatalf("effective cursor: %v", effective)
		}
		want := [][2]string{{"wp-1", "done"}, {"wp-2", "done"}, {"wp-3", "in_progress"}, {"wp-4", "pending"}}
		if got := orchestrateDcloseRecoveryStatuses(t, cwd, slug); len(got) != len(want) || got[3] != want[3] {
			t.Fatalf("statuses %v, want %v", got, want)
		}
	})
}

// TestOrchestrateDcloseRecoveryLostSuccessor covers the successor_lost reasons of closeFixedWorkPhase:
// "a marker naming its own target as successor points at reset, not at a plan fix" (:1627, corrupt) and
// "recovery is refused when the recorded successor left the plan" (:2013, absent).
func TestOrchestrateDcloseRecoveryLostSuccessor(t *testing.T) {
	t.Run("corrupt-marker-names-its-own-successor", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-self-successor", "recovery-self-successor-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		s := state.ReadState(cwd, id)
		self := "wp-1"
		s.DcloseRecovery.NextWorkPhaseID = &self
		if err := state.WriteState(cwd, s); err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "names that same work-phase as its successor") {
			t.Fatalf("refusal: %+v", got)
		}
		if !strings.Contains(got.Output, "crw pabcd orchestrate reset --session "+id) || strings.Contains(got.Output, "restore that work-phase") {
			t.Fatalf("the refusal does not point at reset: %q", got.Output)
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if after := state.ReadState(cwd, id); after.Phase != state.PhaseC || after.DcloseRecovery == nil {
			t.Fatalf("state: %+v", after)
		}
	})
	t.Run("successor-left-the-plan", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-successor-lost", "recovery-successor-lost-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		plan.WorkPhases = append(plan.WorkPhases,
			goalplan.GoalplanWorkPhase{ID: "wp-3", Title: "third", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
		kept := plan.WorkPhases[:0]
		for _, wp := range plan.WorkPhases {
			if wp.ID != "wp-2" {
				kept = append(kept, wp)
			}
		}
		plan.WorkPhases = kept
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "successor wp-2, which is no longer in the plan") {
			t.Fatalf("refusal: %+v", got)
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if rows := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(rows) != 0 {
			t.Fatalf("the refusal started a phase: %v", rows)
		}
	})
	t.Run("successor-can-no-longer-be-started", func(t *testing.T) {
		// §50: the recorded successor is binding. It is still in the plan but is blocked now, so
		// closeFixedWorkPhase answers successor_lost/not_runnable and the close fails closed.
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-successor-not-runnable", "recovery-successor-not-runnable-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		for i := range plan.WorkPhases {
			if plan.WorkPhases[i].ID == "wp-2" {
				plan.WorkPhases[i].Status = goalplan.WorkPhaseBlocked
			}
		}
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "was closed with successor wp-2, which can no longer be started") ||
			!strings.Contains(got.Output, "The recovery marker was kept") {
			t.Fatalf("refusal: %+v", got)
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if after := state.ReadState(cwd, id); after.Phase != state.PhaseC || after.DcloseRecovery == nil {
			t.Fatalf("state: %+v", after)
		}
		if rows := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(rows) != 0 {
			t.Fatalf("the refusal started a phase: %v", rows)
		}
	})
	t.Run("successor-waits-for-another-work-phase", func(t *testing.T) {
		// §50/§55: the recorded successor is pending but its own dependency is unmet, so the close
		// refuses with successor_lost/dependencies_unmet rather than activating it.
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "recovery-successor-unmet", "recovery-successor-unmet-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := orchestrateDcloseGoalplan(t, cwd, slug)
		for i := range plan.WorkPhases {
			if plan.WorkPhases[i].ID == "wp-2" {
				plan.WorkPhases[i].DependsOn = []string{"wp-3"}
			}
		}
		plan.WorkPhases = append(plan.WorkPhases,
			goalplan.GoalplanWorkPhase{ID: "wp-3", Title: "blocker", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "was closed with successor wp-2, which now waits for another work-phase") ||
			!strings.Contains(got.Output, "The recovery marker was kept") {
			t.Fatalf("refusal: %+v", got)
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if after := state.ReadState(cwd, id); after.Phase != state.PhaseC || after.DcloseRecovery == nil {
			t.Fatalf("state: %+v", after)
		}
	})
}

// TestOrchestrateDcloseRecoveryAbsentTarget covers "an absent target restores the cursor onto a stranded
// running successor" (:1817), "an absent target still activates the successor the marker recorded"
// (:2325), "an absent target refuses a running successor whose dependency is unmet" (:1784) and
// "recovery refuses when both the target and its recorded successor are gone" (:2373).
func TestOrchestrateDcloseRecoveryAbsentTarget(t *testing.T) {
	t.Run("activates-a-pending-successor", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "cli-recovery-absent-pending", "cli-recovery-absent-pending-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "absent target, pending successor"})
		plan.Slug = slug
		plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-2", Title: "next", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
		plan.ActiveWorkPhaseID = nil
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 {
			t.Fatalf("retry: %+v", got)
		}
		repaired := orchestrateDcloseGoalplan(t, cwd, slug)
		if orchestrateDcloseDeref(repaired.ActiveWorkPhaseID) != "wp-2" || repaired.WorkPhases[0].Status != goalplan.WorkPhaseInProgress {
			t.Fatalf("plan: %+v", repaired)
		}
		if started := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(started) != 1 || started[0] != "started wp-2" {
			t.Fatalf("started rows: %v", started)
		}
		if after := state.ReadState(cwd, id); after.Phase != state.PhaseIdle || after.DcloseRecovery != nil {
			t.Fatalf("state: %+v", after)
		}
	})
	t.Run("restores-a-stranded-running-successor", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "cli-recovery-stranded", "cli-recovery-stranded-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "absent target, stranded successor"})
		plan.Slug = slug
		plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-2", Title: "next", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
		plan.ActiveWorkPhaseID = nil
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 0 {
			t.Fatalf("retry: %+v", got)
		}
		repaired := orchestrateDcloseGoalplan(t, cwd, slug)
		if orchestrateDcloseDeref(repaired.ActiveWorkPhaseID) != "wp-2" {
			t.Fatalf("cursor: %v", repaired.ActiveWorkPhaseID)
		}
		// Status untouched: it was already running, only the cursor was missing.
		if repaired.WorkPhases[0].Status != goalplan.WorkPhaseInProgress {
			t.Fatalf("wp-2 = %s", repaired.WorkPhases[0].Status)
		}
	})
	t.Run("refuses-an-unready-successor", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "cli-recovery-unready", "cli-recovery-unready-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "absent target, unready successor"})
		plan.Slug = slug
		plan.WorkPhases = []goalplan.GoalplanWorkPhase{
			{ID: "wp-2", Title: "next", Status: goalplan.WorkPhaseInProgress, DependsOn: []string{"wp-9"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
			{ID: "wp-9", Title: "blocker", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		}
		plan.ActiveWorkPhaseID = nil
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "now waits for a prerequisite or decision") {
			t.Fatalf("refusal: %+v", got)
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if after := state.ReadState(cwd, id); after.DcloseRecovery == nil {
			t.Fatal("the refusal cleared the marker")
		}
	})
	t.Run("both-target-and-successor-gone", func(t *testing.T) {
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "cli-recovery-both-gone", "cli-recovery-both-gone-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "target and successor both gone"})
		plan.Slug = slug
		plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-9", Title: "unrelated", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
		plan.ActiveWorkPhaseID = nil
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "the successor wp-2 it recorded is gone too") {
			t.Fatalf("refusal: %+v", got)
		}
		if !strings.Contains(got.Output, "crw pabcd orchestrate reset --session "+id) {
			t.Fatalf("the refusal does not name reset: %q", got.Output)
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if after := state.ReadState(cwd, id); after.DcloseRecovery == nil || after.DcloseRecovery.ClosedWorkPhaseID != "wp-1" {
			t.Fatalf("state: %+v", after)
		}
		if rows := orchestrateDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
			t.Fatalf("the refusal wrote a goalplan row: %+v", rows)
		}
	})
	t.Run("refuses-a-successor-that-is-not-runnable", func(t *testing.T) {
		// resumeAbsentTarget answers successor_lost with one of three reasons. absent and
		// dependencies_unmet are covered by the subtests around this one; this is not_runnable: the
		// target is gone and the recorded successor is blocked, so the close refuses rather than
		// activating a phase that cannot run.
		cwd := orchestrateDcloseTestCwd(t)
		id, slug := "cli-recovery-successor-blocked", "cli-recovery-successor-blocked-plan"
		orchestrateDcloseRecoverySeed(t, cwd, id, slug)
		plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "absent target, blocked successor"})
		plan.Slug = slug
		plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-2", Title: "next", Status: goalplan.WorkPhaseBlocked, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
		plan.ActiveWorkPhaseID = nil
		if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
			t.Fatal(err)
		}
		planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
		before := orchestrateDcloseSnapshot(t, planPath)

		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "the successor wp-2 it recorded can no longer be started") ||
			!strings.Contains(got.Output, "The marker was kept") {
			t.Fatalf("refusal: %+v", got)
		}
		orchestrateDcloseAssertUnchanged(t, before)
		if after := state.ReadState(cwd, id); after.DcloseRecovery == nil {
			t.Fatalf("state: %+v", after)
		}
	})
}

// TestOrchestrateDcloseRecoveryResumesAnAbsentTarget is "CLI D-close recovery resumes when the marker
// target is absent from the plan" (:2410): the plan file is untouched, one row is appended, and the
// marker is cleared.
func TestOrchestrateDcloseRecoveryResumesAnAbsentTarget(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cli-recovery-target-absent", "cli-recovery-target-absent-plan"
	orchestrateDcloseRecoverySeed(t, cwd, id, slug)
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "resume a partially committed CLI close"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-2", Title: "next", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	active := "wp-2"
	plan.ActiveWorkPhaseID = &active
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath)

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	want := "orchestrate D: close target wp-1 is complete (cycle closed, session " + id + ")"
	if got.Code != 0 || got.Output != want {
		t.Fatalf("output = %q, want %q", got.Output, want)
	}
	if strings.Contains(got.Output, "not in the bound goalplan") {
		t.Fatalf("the resume refused the absent target: %q", got.Output)
	}
	orchestrateDcloseAssertUnchanged(t, before)
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("done rows = %d, want 1", n)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseIdle || after.CheckEpoch != nil || after.DcloseRecovery != nil {
		t.Fatalf("state: %+v", after)
	}
}

// TestOrchestrateDcloseRecoveryNullSuccessorStartsNothing is "recovery with an explicit no-successor
// marker does not start a phase added later" (:2090): nextWorkPhaseId null is a durable decision, so a
// phase registered after the crash must stay pending.
func TestOrchestrateDcloseRecoveryNullSuccessorStartsNothing(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "recovery-null-successor", "recovery-null-successor-plan"
	orchestrateDcloseRecoverySeed(t, cwd, id, slug)
	s := state.ReadState(cwd, id)
	s.DcloseRecovery.NextWorkPhaseID = nil
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	plan := orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases = plan.WorkPhases[:1]
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	// A new phase appears between the crash and the retry.
	plan = orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases = append(plan.WorkPhases,
		goalplan.GoalplanWorkPhase{ID: "wp-9", Title: "registered later", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("retry: %+v", got)
	}
	repaired := orchestrateDcloseGoalplan(t, cwd, slug)
	if repaired.ActiveWorkPhaseID != nil {
		t.Fatalf("cursor: %v", repaired.ActiveWorkPhaseID)
	}
	want := [][2]string{{"wp-1", "done"}, {"wp-9", "pending"}}
	got2 := orchestrateDcloseRecoveryStatuses(t, cwd, slug)
	if len(got2) != 2 || got2[0] != want[0] || got2[1] != want[1] {
		t.Fatalf("statuses %v, want %v", got2, want)
	}
	if rows := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(rows) != 0 {
		t.Fatalf("a phase was started: %v", rows)
	}
}

// TestOrchestrateDcloseRecoverySettlesWhenTheSuccessorFinished is "recovery settles when the recorded
// successor already finished its own cycle" (:1878): no plan write is owed, but the rows this
// interrupted close still owed are written once.
func TestOrchestrateDcloseRecoverySettlesWhenTheSuccessorFinished(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "recovery-successor-finished", "recovery-successor-finished-plan"
	orchestrateDcloseRecoverySeed(t, cwd, id, slug)
	plan := orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	plan.WorkPhases[1].Status = goalplan.WorkPhaseDone
	plan.ActiveWorkPhaseID = nil
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(cwd, ".crw", "goalplans", slug, "goalplan.json")
	before := orchestrateDcloseSnapshot(t, planPath)

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("retry: %+v", got)
	}
	orchestrateDcloseAssertUnchanged(t, before)
	if after := state.ReadState(cwd, id); after.Phase != state.PhaseIdle || after.DcloseRecovery != nil {
		t.Fatalf("state: %+v", after)
	}
	// The point of resuming at all: the rows this interrupted close still owed are written even though
	// no plan write was needed.
	if started := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(started) != 1 || started[0] != "started wp-2" {
		t.Fatalf("started rows: %v", started)
	}
	var done int
	for _, row := range orchestrateDcloseGoalplanRows(t, cwd, slug) {
		if row["event"] == "workphase_done" && row["detail"] == "closed wp-1" {
			done++
		}
	}
	if done != 1 {
		t.Fatalf("done rows = %d, want 1", done)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 1 {
		t.Fatalf("PABCD rows = %d, want 1", n)
	}
}

// TestOrchestrateDcloseRecoveryMarkerBeatsTheFileCursor is "recovery finishes the successor the marker
// recorded, not the one the file names" (:2065): the plan alone cannot separate a finished close that
// chose wp-2 from a hand edit that moved the cursor onto a phase that was already running.
func TestOrchestrateDcloseRecoveryMarkerBeatsTheFileCursor(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "recovery-marker-beats-cursor", "recovery-marker-beats-cursor-plan"
	orchestrateDcloseSeedPlan(t, cwd, slug, goalplan.TaskDone)
	plan := orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases = append(plan.WorkPhases,
		goalplan.GoalplanWorkPhase{ID: "wp-3", Title: "third", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
	// wp-2 was already running before the close, so the close picked wp-3 as its successor.
	plan.WorkPhases[1].Status = goalplan.WorkPhaseInProgress
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	next := "wp-3"
	epoch := "c-test-epoch"
	s := state.DefaultState(id, slug)
	s.Phase, s.CheckEpoch = state.PhaseC, &epoch
	s.DcloseRecovery = &state.DcloseRecoveryMarker{
		SessionID: id, CheckEpoch: epoch, ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: &next,
	}
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	// The forgery: the target is done and the cursor moved onto the phase that was already running.
	plan = orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	forged := "wp-2"
	plan.ActiveWorkPhaseID = &forged
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("retry: %+v", got)
	}
	repaired := orchestrateDcloseGoalplan(t, cwd, slug)
	if orchestrateDcloseDeref(repaired.ActiveWorkPhaseID) != "wp-3" {
		t.Fatalf("cursor: %v", repaired.ActiveWorkPhaseID)
	}
	// The started row names the recorded successor, and wp-2 never gets a second one.
	if started := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(started) != 1 || started[0] != "started wp-3" {
		t.Fatalf("started rows: %v", started)
	}
}

// TestOrchestrateDcloseRecoveryReadsTheMarkerSuccessorForTheStartedRow is the §52 rule: on a resume the
// started row names the marker's successor, never the persisted cursor, which a retry may leave stale.
func TestOrchestrateDcloseRecoveryReadsTheMarkerSuccessorForTheStartedRow(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "recovery-started-from-marker", "recovery-started-from-marker-plan"
	orchestrateDcloseRecoverySeed(t, cwd, id, slug)
	plan := orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases = append(plan.WorkPhases,
		goalplan.GoalplanWorkPhase{ID: "wp-3", Title: "third", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
	// The file's cursor already points past the recorded successor.
	past := "wp-3"
	plan.ActiveWorkPhaseID = &past
	plan.WorkPhases[0].Status = goalplan.WorkPhaseDone
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}

	got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Code != 0 {
		t.Fatalf("retry: %+v", got)
	}
	if started := orchestrateDcloseRecoveryStartedRows(t, cwd, slug); len(started) != 1 || started[0] != "started wp-2" {
		t.Fatalf("started rows = %v, want the marker's successor wp-2", started)
	}
}

// TestOrchestrateDcloseRecoveryKeepsTheMarkerOnAPlanItCannotRepair is the §50/§41 invariant shared by
// every refusal above: a refusal never wipes the marker, because that is the only route back.
func TestOrchestrateDcloseRecoveryKeepsTheMarkerOnAPlanItCannotRepair(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "recovery-keeps-marker", "recovery-keeps-marker-plan"
	orchestrateDcloseRecoverySeed(t, cwd, id, slug)
	plan := orchestrateDcloseGoalplan(t, cwd, slug)
	plan.WorkPhases[0].Status = goalplan.WorkPhaseBlocked
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}

	for attempt := 0; attempt < 2; attempt++ {
		got, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Code != 1 || !strings.Contains(got.Output, "The recovery marker was kept") {
			t.Fatalf("attempt %d: %+v", attempt, got)
		}
	}
	after := state.ReadState(cwd, id)
	if after.DcloseRecovery == nil || after.DcloseRecovery.ClosedWorkPhaseID != "wp-1" || after.DcloseRecovery.NextWorkPhaseID == nil {
		t.Fatalf("state: %+v", after)
	}
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 0 {
		t.Fatalf("a refused retry wrote %d rows", n)
	}
}

// TestOrchestrateDcloseRecoveryIsNotEnteredWithoutAMatchingMarker is "IDLE D attest without a matching
// marker is refused" (:1230) and "a marker from another session cannot authorize recovery" (:1246): the
// transition half refuses before the close, and the close itself never guesses.
func TestOrchestrateDcloseRecoveryIsNotEnteredWithoutAMatchingMarker(t *testing.T) {
	cwd := orchestrateTransitionRoot(t)
	id := "idle-no-marker"
	orchestrateTransitionSession(t, cwd, id, "{\"phase\":\"IDLE\"}")
	got := orchestrateTransitionRun(t, cwd, "D", "--session", id, "--attest",
		"{\"from\":\"C\",\"to\":\"D\",\"did\":\"verified\",\"checkOutput\":\"tests passed\",\"exitCode\":0,\"workPhaseId\":\"wp-1\"}")
	if got.Code != 1 {
		t.Fatalf("an IDLE D attest advanced: %+v", got)
	}
	if state.ReadState(cwd, id).Phase != state.PhaseIdle {
		t.Fatal("the refusal moved the session")
	}
	if state.MatchesDcloseRecovery(state.ReadState(cwd, id), "wp-1") {
		t.Fatal("an IDLE session with no marker reported recovery")
	}

	foreign := orchestrateDcloseTestCwd(t)
	id2, slug := "marker-owner", "marker-owner-plan"
	orchestrateDcloseSeedPlan(t, foreign, slug, goalplan.TaskDone)
	epoch := "c-owner"
	s := state.DefaultState(id2, slug)
	s.Phase, s.CheckEpoch = state.PhaseC, &epoch
	next := "wp-2"
	s.DcloseRecovery = &state.DcloseRecoveryMarker{
		SessionID: "different-session", CheckEpoch: epoch, ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: &next,
	}
	if err := state.WriteState(foreign, s); err != nil {
		t.Fatal(err)
	}
	// The reader drops a marker of another session, so the request is not a recovery and the marker is
	// not carried into the state this close writes.
	loaded := state.ReadState(foreign, id2)
	if loaded.DcloseRecovery != nil {
		t.Fatalf("a foreign marker survived the read: %+v", loaded.DcloseRecovery)
	}
	if state.MatchesDcloseRecovery(loaded, "wp-1") {
		t.Fatal("a foreign marker authorized recovery")
	}
}

// TestOrchestrateDcloseFinalizePendingTextAndRetry is §39 Y3's other half: when the finalization lock
// cannot be taken the close still answers code 0 with the pending text, the marker stays, and the next
// request finishes the cleanup and appends the row it still owed.
func TestOrchestrateDcloseFinalizePendingTextAndRetry(t *testing.T) {
	cwd := orchestrateDcloseTestCwd(t)
	id, slug := "cycle-finalize-pending", "cycle-finalize-pending-plan"
	orchestrateDcloseSeedAtC(t, cwd, id, slug, goalplan.TaskDone)
	lockDir, err := goalplan.GoalplanWriteLockDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}

	// The first lock is released before the state write runs, so a lock taken here breaks only the
	// finalization pass. The hook answers nil, because the close must continue into it.
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
	if n := orchestrateDcloseDoneRows(t, cwd, id); n != 0 {
		t.Fatalf("the pending finalization wrote %d rows", n)
	}
	after := state.ReadState(cwd, id)
	if after.Phase != state.PhaseIdle || after.DcloseRecovery == nil {
		t.Fatalf("state: %+v", after)
	}

	// Release the lock this test took, as an operator does after verifying no writer is active.
	if err := os.Remove(lockDir); err != nil {
		t.Fatal(err)
	}
	retry, err := orchestrateDcloseRecoveryRun(t, cwd, id, orchestrateDcloseSeam{})
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
		t.Fatalf("the marker survived the finalize: %+v", cleared)
	}
}
