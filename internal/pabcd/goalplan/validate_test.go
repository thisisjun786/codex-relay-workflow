package goalplan

import (
	"strings"
	"testing"
)

// The validateGoalplan and goalplanStructuralReasons cases of CXC v0.2.40 (commit 3c1459ac): the
// recorded scenarios of goalplan-integrity.test.ts:175, goalplan-regression.test.ts:203,
// goalplan.test.ts:538 and :553, and work-phase-states.test.ts:193, plus the schemaVersion refusal,
// which the oracle's suite leaves to the source (goalplan.ts:1660-1666). A reason string is the
// oracle's after the repository's name substitution: R32 makes "codexclaw" "crw" and R33 makes
// "$cxc-loop" "$crw-loop", as internal/role/spawn/classify.go:27 and loop_render.go:82-86 do it.

func validateVersion(v float64) *float64 { return &v }

func validatePlan(schemaVersion *float64, phases ...GoalplanWorkPhase) *Goalplan {
	return &Goalplan{Objective: "validate fixture", Slug: "validate", SchemaVersion: schemaVersion,
		WorkPhases: phases, Criteria: []GoalplanCriterion{}, Host: GoalplanHostLink{Source: HostSourceNone}}
}

func validatePhase(id string, status WorkPhaseStatus, dependsOn ...string) GoalplanWorkPhase {
	return GoalplanWorkPhase{ID: id, Title: id, Status: status, Tasks: []GoalplanTask{}, CriteriaIDs: []string{}, DependsOn: dependsOn}
}

func validateTask(id string, status TaskStatus) GoalplanTask {
	return GoalplanTask{ID: id, Title: id, Status: status}
}

func validateReasonsEqual(t *testing.T, got []string, want ...string) {
	t.Helper()
	if got == nil {
		t.Fatalf("reasons = nil, want non-nil %#v", want)
	}
	if len(got) != len(want) {
		t.Fatalf("reasons = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reasons = %#v, want %#v", got, want)
		}
	}
}

func validateContaining(reasons []string, needle string) []string {
	out := []string{}
	for _, reason := range reasons {
		if strings.Contains(reason, needle) {
			out = append(out, reason)
		}
	}
	return out
}

func validateHasReason(reasons []string, needle string) bool {
	return len(validateContaining(reasons, needle)) > 0
}

// goalplan-integrity.test.ts:175 "validateGoalplan places definition and completion reasons first".
func TestValidateGoalplanPlacesDefinitionAndCompletionReasonsFirst(t *testing.T) {
	base := validatePhase("base", WorkPhasePending)
	leaf := validatePhase("leaf", WorkPhaseDone, "base")
	leaf.Tasks = []GoalplanTask{validateTask("done-missing", TaskDone)}

	verdict := ValidateGoalplan(validatePlan(validateVersion(3), base, leaf), nil)
	if verdict.OK {
		t.Fatal("ok = true, want false")
	}
	if len(verdict.Reasons) < 2 {
		t.Fatalf("reasons = %#v, want the two leading reasons", verdict.Reasons)
	}
	validateReasonsEqual(t, verdict.Reasons[:2],
		"task leaf/done-missing is done but has no non-empty outcome",
		"work phase leaf is done while dependency work phase(s) are not done: base")
}

// goalplan-regression.test.ts:203 "wp7 outcome validation starts at schema v3 and is not a selector
// version branch": the four validateGoalplan assertions at :211, :219-222, :232-235 and :239.
func TestValidateGoalplanOutcomeReasonsStartAtSchemaV3(t *testing.T) {
	outcomePlan := func(schemaVersion *float64, status WorkPhaseStatus, task GoalplanTask) *Goalplan {
		phase := validatePhase("wp1", status)
		phase.Tasks = []GoalplanTask{task}
		plan := validatePlan(schemaVersion, phase)
		plan.ActiveWorkPhaseID = nil
		return plan
	}
	legacy := []struct {
		name    string
		version *float64
	}{{"absent", nil}, {"2", validateVersion(2)}}
	for _, c := range legacy {
		plan := outcomePlan(c.version, WorkPhaseDone, validateTask("t1", TaskDone))
		validateReasonsEqual(t, validateContaining(GoalplanDefinitionIntegrityReasons(plan), "outcome"))
		if got := ValidateGoalplan(plan, nil); validateHasReason(got.Reasons, "outcome") {
			t.Fatalf("schemaVersion %s: a legacy done task without outcome was reported: %#v", c.name, got.Reasons)
		}
	}

	missing := outcomePlan(validateVersion(3), WorkPhaseDone, validateTask("t1", TaskDone))
	validateReasonsEqual(t, validateContaining(GoalplanDefinitionIntegrityReasons(missing), "outcome"),
		"task wp1/t1 is done but has no non-empty outcome")
	if got := ValidateGoalplan(missing, nil); !validateHasReason(got.Reasons, "task wp1/t1 is done but has no non-empty outcome") {
		t.Fatalf("a done task without outcome: reasons = %#v", got.Reasons)
	}

	prematureTask := validateTask("t1", TaskPending)
	prematureTask.Outcome = "must not exist yet"
	premature := outcomePlan(validateVersion(3), WorkPhaseInProgress, prematureTask)
	validateReasonsEqual(t, validateContaining(GoalplanDefinitionIntegrityReasons(premature), "outcome"),
		"task wp1/t1 is pending but has outcome")
	if got := ValidateGoalplan(premature, nil); !validateHasReason(got.Reasons, "task wp1/t1 is pending but has outcome") {
		t.Fatalf("a pending task with outcome: reasons = %#v", got.Reasons)
	}

	validTask := validateTask("t1", TaskDone)
	validTask.Outcome = "tests: 12 passed"
	valid := outcomePlan(validateVersion(3), WorkPhaseDone, validTask)
	validateReasonsEqual(t, validateContaining(GoalplanDefinitionIntegrityReasons(valid), "outcome"))
	if got := ValidateGoalplan(valid, nil); validateHasReason(got.Reasons, "outcome") {
		t.Fatalf("a valid done task was reported: %#v", got.Reasons)
	}
}

// goalplan.test.ts:538 "030: validateGoalplan rejects met-without-evidence and incomplete plans".
func TestValidateGoalplanRejectsMetWithoutEvidenceAndIncomplete(t *testing.T) {
	plan := BuildGoalplan(NewGoalplanInput{Objective: "v", Criteria: []NewGoalplanCriterion{{Scenario: "c"}}, SchemaVersion: validateVersion(1)})
	if ValidateGoalplan(plan, nil).OK {
		t.Fatal("an unmet criterion: ok = true, want false")
	}
	plan.Criteria[0].Status = CriterionMet
	stamped := ValidateGoalplan(plan, nil)
	if stamped.OK {
		t.Fatal("met without evidence: ok = true, want false")
	}
	if !validateHasReason(stamped.Reasons, "no captured evidence") {
		t.Fatalf("reasons = %#v, want a captured-evidence reason", stamped.Reasons)
	}
	plan.Criteria[0].CapturedEvidence = ptr("proof")
	if got := ValidateGoalplan(plan, nil); !got.OK {
		t.Fatalf("with evidence and no work phases: ok = false, reasons = %#v", got.Reasons)
	}
}

// goalplan.test.ts:553 "260709: validateGoalplan FAILS an EMPTY plan (no workPhases, no criteria)".
func TestValidateGoalplanFailsAnEmptyPlan(t *testing.T) {
	empty := BuildGoalplan(NewGoalplanInput{Objective: "shell only", SchemaVersion: validateVersion(1)})
	verdict := ValidateGoalplan(empty, nil)
	if verdict.OK {
		t.Fatal("an empty plan: ok = true, want false")
	}
	validateReasonsEqual(t, verdict.Reasons,
		"plan is empty: no workPhases[] and no criteria[] registered — fill the goalplan (schema in $crw-loop) before the E8 gate can certify completion")

	// Registering EITHER a criterion or a work phase lifts the empty-plan failure.
	withCriterion := BuildGoalplan(NewGoalplanInput{Objective: "with criterion",
		Criteria: []NewGoalplanCriterion{{Scenario: "c", ExpectedEvidence: "e"}}, SchemaVersion: validateVersion(1)})
	withCriterion.Criteria[0].Status = CriterionMet
	withCriterion.Criteria[0].CapturedEvidence = ptr("proof")
	if got := ValidateGoalplan(withCriterion, nil); !got.OK {
		t.Fatalf("a registered criterion: ok = false, reasons = %#v", got.Reasons)
	}
}

// work-phase-states.test.ts:193 "a hand-edited superseded phase cannot buy completion". The oracle
// edits the plan file and reads it back; the same record is built directly here.
func TestValidateGoalplanSupersededPhaseCannotBuyCompletion(t *testing.T) {
	// schemaVersion 1, as the oracle's plan() helper pins it: a v2+ plan adds a final-gate reason
	// to every assertion here.
	plan := validatePlan(validateVersion(1), validatePhase("a", WorkPhaseDone), validatePhase("b", WorkPhaseSuperseded))
	if got := len(RemainingWorkPhases(plan)); got != 0 {
		t.Fatalf("remaining work phases = %d, want 0: superseded does leave the count", got)
	}
	verdict := ValidateGoalplan(plan, nil)
	if verdict.OK {
		t.Fatal("a superseded phase naming no replacement: ok = true, want false")
	}
	if !validateHasReason(verdict.Reasons, "does not name what replaced it") {
		t.Fatalf("reasons = %#v, want the supersededBy reason", verdict.Reasons)
	}
}

// goalplan.ts:1660-1666: the refusal precedes every other check, and the structural form uses the
// short wording. The oracle's suite records no case for either.
func TestValidateGoalplanRefusesANewerSchemaVersionFirst(t *testing.T) {
	plan := validatePlan(validateVersion(4), validatePhase("wp1", WorkPhasePending))
	validateReasonsEqual(t, ValidateGoalplan(plan, nil).Reasons,
		"schemaVersion 4 is newer than this build supports (max 3) - upgrade crw before validating this plan")
	if ValidateGoalplan(plan, nil).OK {
		t.Fatal("ok = true, want false")
	}
	validateReasonsEqual(t, ValidateGoalplanStructuralReasons(plan),
		"schemaVersion 4 is newer than this build supports")
}

// goalplanStructuralReasons (goalplan.ts:1744-1758): the structural subset only, with the short
// done-with-open-tasks wording the full validator lengthens with the task ids (:1703 against :1751).
func TestValidateGoalplanStructuralReasonsKeepOnlyStructuralProblems(t *testing.T) {
	progress := validatePlan(validateVersion(1), validatePhase("wp1", WorkPhasePending))
	progress.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "c", Status: CriterionOpen}}
	validateReasonsEqual(t, ValidateGoalplanStructuralReasons(progress))
	if !validateHasReason(ValidateGoalplan(progress, nil).Reasons, "unmet criterion/criteria") {
		t.Fatal("the full validator must still report the unmet criterion")
	}

	rubber := validatePlan(validateVersion(1))
	rubber.Criteria = []GoalplanCriterion{{ID: "c-1", Scenario: "c", Status: CriterionMet}}
	validateReasonsEqual(t, ValidateGoalplanStructuralReasons(rubber),
		"criterion c-1 marked met but has no captured evidence")

	done := validatePhase("wp1", WorkPhaseDone)
	done.Tasks = []GoalplanTask{validateTask("t1", TaskPending)}
	broken := validatePlan(validateVersion(1), done)
	validateReasonsEqual(t, ValidateGoalplanStructuralReasons(broken),
		"work phase wp1 is marked done but still has open task(s)")
	if got := ValidateGoalplan(broken, nil).Reasons; !validateHasReason(got, "work phase wp1 is marked done but still has open task(s): t1") {
		t.Fatalf("reasons = %#v, want the full validator to name the open task", got)
	}
}
