// prompt_dclose_recovery_test.go is the Go form of the chat D-close recovery cases of CXC
// v0.2.40 pabcd-state/test/hook.test.ts for the unit prompt_dclose.go owns (hook.ts:990-1136 and
// 1256-1258), absorbed from CRW-798. A recovery is a second D request that matches the marker the
// first attempt left: it replays the close the marker describes instead of applying C>D again, and
// every refusal keeps the marker so the operator can repair the plan and repeat the request.
package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptDcloseRecoverable is seedRecoverableChatClose: a bound session at C whose marker records
// the close of wp-1, with the plan still holding wp-1 in progress and the recorded successor.
// The plan commit did not land, which is the state right after the marker write.
func promptDcloseRecoverable(t *testing.T, cwd, sessionID, slug string, next *string) string {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "recover " + sessionID})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptSubmitStateFile(t, cwd, sessionID, func(s *state.State) {
		s.Phase, s.Slug, s.OrchestrationActive = state.PhaseC, slug, true
		s.CheckEpoch = promptDcloseStr("c-recovery")
		s.Flags = state.Flags{AuditPassed: true, CheckPassed: true}
		s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: sessionID, CheckEpoch: "c-recovery", ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: next}
	})
	// A recovery spends no receipt: the first attempt already did, so the attest carries none.
	return promptDcloseAttest("wp-1", "")
}

// promptDcloseRecoveryPlan rewrites the bound plan to the listed phases, the way an operator's
// hand edit between the two attempts does.
func promptDcloseRecoveryPlan(t *testing.T, cwd, slug string, phases []goalplan.GoalplanWorkPhase, cursor *string) {
	t.Helper()
	plan := promptDcloseReadPlan(t, cwd, slug)
	plan.WorkPhases, plan.ActiveWorkPhaseID = phases, cursor
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// TestPromptDcloseRecoveryActivatesTheRecordedSuccessor is hook.test.ts "chat recovery activates
// the recorded successor when the target is absent": the target is gone and the recorded
// successor is pending, so the resume activates it and the close finishes.
func TestPromptDcloseRecoveryActivatesTheRecordedSuccessor(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-absent-pending"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
	promptDcloseRecoveryPlan(t, cwd, slug,
		[]goalplan.GoalplanWorkPhase{{ID: "wp-2", Title: "next", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}},
		nil)
	// The seed records no successor, so point the marker at wp-2 the way a real close would.
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.DcloseRecovery.NextWorkPhaseID = promptDcloseStr("wp-2")
	})
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if strings.Contains(answer, "refused") {
		t.Fatalf("the recovery refused: %q", answer)
	}
	repaired := promptDcloseReadPlan(t, cwd, slug)
	if repaired.ActiveWorkPhaseID == nil || *repaired.ActiveWorkPhaseID != "wp-2" {
		t.Errorf("the cursor: %v", repaired.ActiveWorkPhaseID)
	}
	if repaired.WorkPhases[0].Status != goalplan.WorkPhaseInProgress {
		t.Errorf("the activated successor: %+v", repaired.WorkPhases[0])
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseIdle || s.DcloseRecovery != nil {
		t.Errorf("the finished state: %+v", s)
	}
}

// TestPromptDcloseRecoveryResumesWhenTheTargetIsAbsent is hook.test.ts "chat D-close recovery
// resumes when the marker target is absent from the plan": the recorded successor is already
// running, so the resume owes only the ledger row and the cleanup, and the plan bytes stay.
func TestPromptDcloseRecoveryResumesWhenTheTargetIsAbsent(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-target-absent"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryPlan(t, cwd, slug,
		[]goalplan.GoalplanWorkPhase{{ID: "wp-2", Title: "next", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}},
		promptDcloseStr("wp-2"))
	beforePlan := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if strings.Contains(answer, "refused") {
		t.Fatalf("the recovery refused: %q", answer)
	}
	if !strings.Contains(answer, "[crw: DONE]") || !strings.Contains(answer, "IPABCD: IDLE") {
		t.Errorf("the DONE directive: %q", answer)
	}
	if after := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug)); after != beforePlan {
		t.Errorf("a resume with nothing to activate rewrote the plan:\n got %q\nwant %q", after, beforePlan)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["checkEpoch"] != "c-recovery" || rows[0]["closedWorkPhaseId"] != "wp-1" {
		t.Errorf("the close rows: %+v", rows)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseIdle || s.CheckEpoch != nil || s.DcloseRecovery != nil {
		t.Errorf("the finished state: %+v", s)
	}
}

// TestPromptDcloseRecoveryLegacyMarkerIsRefused is hook.test.ts's legacy-marker case (the §50
// rule): a marker that predates the successor field cannot be read safely, so the retry refuses,
// names the reset that clears it, and leaves the marker exactly as it was.
func TestPromptDcloseRecoveryLegacyMarkerIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-legacy"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
	// A stored marker with no successor field at all reads back as legacy.
	promptDcloseWrite(t, cwd, ".crw/sessions/s1.json", `{"phase":"C","sessionId":"s1","slug":"`+slug+`","orchestrationActive":true,"checkEpoch":"c-recovery","dcloseRecovery":{"sessionId":"s1","checkEpoch":"c-recovery","closedWorkPhaseId":"wp-1"}}`)
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "predates the successor field") || !strings.Contains(answer, "The marker was kept") {
		t.Errorf("the legacy refusal: %q", answer)
	}
	if !strings.Contains(answer, "crw pabcd orchestrate reset --session s1") {
		t.Errorf("the reset instruction: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil || !s.DcloseRecovery.Legacy {
		t.Errorf("the marker was not kept: %+v", s.DcloseRecovery)
	}
	if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
		t.Errorf("a refused recovery wrote a goalplan row: %+v", rows)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("a refused recovery wrote a PABCD row: %+v", rows)
	}
	_ = before
}

// TestPromptDcloseRecoverySuccessorLostReasons is hook.test.ts's four successor_lost reasons and
// the corrupt split: the target is gone and the recorded successor cannot be activated, so the
// retry refuses with the reason's own wording and keeps the marker.
func TestPromptDcloseRecoverySuccessorLostReasons(t *testing.T) {
	type scenario struct {
		name   string
		phases []goalplan.GoalplanWorkPhase
		want   string
	}
	blocked := goalplan.GoalplanWorkPhase{ID: "wp-2", Title: "next", Status: goalplan.WorkPhaseBlocked, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}, BlockedReason: promptDcloseStr("vendor")}
	cases := []scenario{
		{name: "absent", phases: []goalplan.GoalplanWorkPhase{
			{ID: "wp-9", Title: "other", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		}, want: "is gone too"},
		{name: "not_runnable", phases: []goalplan.GoalplanWorkPhase{blocked}, want: "can no longer be started"},
		{name: "dependencies_unmet", phases: []goalplan.GoalplanWorkPhase{
			{ID: "wp-2", Title: "next", Status: goalplan.WorkPhasePending, DependsOn: []string{"wp-3"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
			{ID: "wp-3", Title: "prereq", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		}, want: "now waits for a prerequisite or decision"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := promptDcloseRepo(t)
			slug := "chat-recovery-" + c.name
			attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
			promptDcloseRecoveryPlan(t, cwd, slug, c.phases, nil)
			beforePlan := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug))
			answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
			if !strings.Contains(answer, "is gone from the plan") || !strings.Contains(answer, c.want) {
				t.Errorf("the refusal: %q (want %q)", answer, c.want)
			}
			if !strings.Contains(answer, "The marker was kept") {
				t.Errorf("the refusal does not keep the marker: %q", answer)
			}
			if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC || s.DcloseRecovery == nil {
				t.Errorf("the refused state: %+v", s)
			}
			if after := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug)); after != beforePlan {
				t.Errorf("a refused recovery wrote the plan:\n got %q\nwant %q", after, beforePlan)
			}
			if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
				t.Errorf("a refused recovery wrote a goalplan row: %+v", rows)
			}
		})
	}
}

// TestPromptDcloseRecoveryTasksPendingRefuses is hook.test.ts "chat recovery is refused when a
// pending task is hidden under the closed target" and the "gained an open task" scenario: the
// commit landed but a pending task sits under the closed target, so the retry refuses, keeps the
// marker and writes nothing.
func TestPromptDcloseRecoveryTasksPendingRefuses(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-open-task"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
	// The first attempt committed the close; the operator then hid a pending task under it.
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{{ID: "t-late", Title: "added late", Status: goalplan.TaskPending}}, CriteriaIDs: []string{}},
	}, promptDcloseStr("wp-1"))
	beforePlan := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "gained 1 open task(s) after its marker was written") || !strings.Contains(answer, "The recovery marker was kept") {
		t.Errorf("the refusal: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC || s.DcloseRecovery == nil {
		t.Errorf("the refused state: %+v", s)
	}
	if after := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug)); after != beforePlan {
		t.Errorf("a refused recovery wrote the plan:\n got %q\nwant %q", after, beforePlan)
	}
	if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
		t.Errorf("a refused recovery wrote a goalplan row: %+v", rows)
	}
}

// TestPromptDcloseRecoveryFixedTargetStates is hook.test.ts's "became blocked" and "lost a
// dependency" scenarios: a fixed target that turned blocked, or that gained an unmet dependency,
// is refused by the same three gates the CLI enforces, and the marker stays.
func TestPromptDcloseRecoveryFixedTargetStates(t *testing.T) {
	type scenario struct {
		name  string
		phase goalplan.GoalplanWorkPhase
		extra []goalplan.GoalplanWorkPhase
		want  string
	}
	cases := []scenario{
		{name: "became-blocked", want: "is now blocked",
			phase: goalplan.GoalplanWorkPhase{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseBlocked, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}, BlockedReason: promptDcloseStr("vendor")}},
		{name: "lost-a-dependency", want: "now waits for wp-2",
			phase: goalplan.GoalplanWorkPhase{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseInProgress, DependsOn: []string{"wp-2"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
			extra: []goalplan.GoalplanWorkPhase{{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := promptDcloseRepo(t)
			slug := "chat-recovery-" + c.name
			attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
			phases := append([]goalplan.GoalplanWorkPhase{c.phase}, c.extra...)
			promptDcloseRecoveryPlan(t, cwd, slug, phases, promptDcloseStr("wp-1"))
			beforePlan := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug))
			answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
			if !strings.Contains(answer, c.want) || !strings.Contains(answer, "The recovery marker was kept") {
				t.Errorf("the refusal: %q (want %q)", answer, c.want)
			}
			if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC || s.DcloseRecovery == nil {
				t.Errorf("the refused state: %+v", s)
			}
			if after := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug)); after != beforePlan {
				t.Errorf("a refused recovery wrote the plan:\n got %q\nwant %q", after, beforePlan)
			}
		})
	}
}

// TestPromptDcloseRecoveryCorruptMarkerIsRefused is the §51 split: a marker that names the target
// as its own successor is corrupt, which no close can produce, so the retry points at reset.
func TestPromptDcloseRecoveryCorruptMarkerIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-corrupt"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-1"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "names that same work-phase as its successor") || !strings.Contains(answer, "The marker was kept") {
		t.Errorf("the corrupt refusal: %q", answer)
	}
	if !strings.Contains(answer, "crw pabcd orchestrate reset --session s1") {
		t.Errorf("the reset instruction: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC || s.DcloseRecovery == nil {
		t.Errorf("the refused state: %+v", s)
	}
}

// TestPromptDcloseRecoveryRerunsAStatusOnlyClose is hook.test.ts "chat recovery re-runs the close
// when only the target status was edited to done": a status-only edit keeps the cursor on wp-1,
// so treating done as committed would log a false started wp-1. The retry re-runs the close and
// names the successor the close computes.
func TestPromptDcloseRecoveryRerunsAStatusOnlyClose(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-status-only-done"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "status only"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.Slug, s.OrchestrationActive = state.PhaseC, slug, true
		s.CheckEpoch = promptDcloseStr("c-status")
		s.Flags = state.Flags{AuditPassed: true, CheckPassed: true}
		s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: "s1", CheckEpoch: "c-status", ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: promptDcloseStr("wp-2")}
	})
	attest := promptDcloseAttest("wp-1", "")
	// The first attempt stopped at the marker; the operator then edited only the target's status,
	// which leaves the cursor on wp-1 and is exactly the edit that must not be read as committed.
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}, promptDcloseStr("wp-1"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if strings.Contains(answer, "refused") {
		t.Fatalf("the recovery refused: %q", answer)
	}
	rows := promptDcloseGoalplanRows(t, cwd, slug)
	var started []string
	for _, row := range rows {
		if row["event"] == "workphase_started" {
			started = append(started, row["detail"].(string))
		}
	}
	if len(started) != 1 || started[0] != "started wp-2" {
		t.Errorf("the started rows: %+v", started)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle || s.DcloseRecovery != nil {
		t.Errorf("the finished state: %+v", s)
	}
}

// TestPromptDcloseRecoveryWritesEachRowOnce is the oracle's idempotent recovery: a marker-matched
// retry that runs to completion appends each of its ledger rows exactly once, and a further retry
// on the resting session appends nothing.
func TestPromptDcloseRecoveryWritesEachRowOnce(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-once"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
	promptDcloseRun(t, cwd, "s1", "t1", attest)
	pabcd := promptOrchestrateLedger(t, cwd)
	if len(pabcd) != 1 {
		t.Fatalf("the PABCD rows: %+v", pabcd)
	}
	goalplanRows := promptDcloseGoalplanRows(t, cwd, slug)
	if len(goalplanRows) != 1 || goalplanRows[0]["event"] != "workphase_done" || goalplanRows[0]["detail"] != "closed wp-1" {
		t.Errorf("the goalplan rows: %+v", goalplanRows)
	}
	// The resting session refuses a further request and appends nothing.
	promptDcloseRun(t, cwd, "s1", "t2", attest)
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 {
		t.Errorf("a further request appended a row: %+v", rows)
	}
}

// TestPromptDcloseRecoveryKeepsThePlanFields is hook.test.ts's wp7 preservation case: a recovery
// close keeps dependsOn and outcome of the tasks it leaves in place.
func TestPromptDcloseRecoveryKeepsThePlanFields(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "wp7-chat-d"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "wp7 chat D"})
	plan.Slug = slug
	plan.SchemaVersion = promptDcloseFloat(3)
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{
		ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseInProgress, CriteriaIDs: []string{},
		Tasks: []goalplan.GoalplanTask{
			{ID: "t-1", Title: "first", Status: goalplan.TaskDone, DependsOn: []string{}, Outcome: "first task verified"},
			{ID: "t-2", Title: "second", Status: goalplan.TaskDone, DependsOn: []string{"t-1"}, Outcome: "second task verified"},
		},
	}}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "wp7-check")
	receipt := promptDcloseReceipt(t, cwd, "s1", "wp7-check")
	answer := promptDcloseRun(t, cwd, "s1", "turn-1", promptDcloseAttest("wp-1", receipt))
	if strings.Contains(answer, "refused") {
		t.Fatalf("the close refused: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Errorf("the resting phase: %s", s.Phase)
	}
	saved := promptDcloseReadPlan(t, cwd, slug)
	if saved.WorkPhases[0].Status != goalplan.WorkPhaseDone {
		t.Errorf("the closed phase: %+v", saved.WorkPhases[0])
	}
	if len(saved.WorkPhases[0].Tasks) != 2 {
		t.Fatalf("the tasks: %+v", saved.WorkPhases[0].Tasks)
	}
	second := saved.WorkPhases[0].Tasks[1]
	if second.ID != "t-2" || len(second.DependsOn) != 1 || second.DependsOn[0] != "t-1" || second.Outcome != "second task verified" {
		t.Errorf("the second task lost a field: %+v", second)
	}
}
