package goalplan

import (
	"fmt"
	"strings"

	jstext "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The E8 completion validator, CXC v0.2.40 goalplan.ts:1656-1712 (commit 3c1459ac), and its
// structural subset, goalplanStructuralReasons at :1744-1758. computeQaRequired (:1643) and
// supersededIntegrityReasons (:1714-1743) already sit in integrity.go; the definition and dependency
// reasons, the remaining/unmet/done queries and the v2 final-gate checks are called, not copied.
//
// A reason string is the oracle's after the repository's name substitution (decision 1,
// contract/schema/cxc/name-substitution.json): R32 turns "codexclaw" into "crw" and R33 turns
// "$cxc-loop" into "$crw-loop", the rule internal/role/spawn/classify.go:27 applies to
// RecurseDenyReason. Every other byte, and the order of the reasons, is the oracle's. The
// schemaVersion is interpolated with finalGateJSNumber, the formatter finalgate.go already uses for
// the same field.
//
// Neither function has a caller yet: the "crw loop validate" verb that prints these reasons and the
// IDLE release that consults the structural subset are later issues, as finalgate.go records for the
// gate it added.

// ValidateGoalplan is validateGoalplan: the quality gate a goalplan must clear before the goal may
// certify completion (GOAL-COMPLETE-GATE-01; not consulted during a D-close). A plan whose
// schemaVersion is newer than this build supports is refused before any other check, so a plan this
// binary cannot fully represent is never judged complete on a partial reading of it. The structural,
// repairable reasons then come before the progress ones, because the caller shows only the first four.
func ValidateGoalplan(plan *Goalplan, ctx *GoalplanValidationCtx) GoalplanValidation {
	reasons := []string{}
	if plan.SchemaVersion != nil && *plan.SchemaVersion > SupportedMaxSchemaVersion {
		return GoalplanValidation{OK: false, Reasons: []string{fmt.Sprintf(
			"schemaVersion %s is newer than this build supports (max %d) - upgrade crw before validating this plan",
			finalGateJSNumber(*plan.SchemaVersion), SupportedMaxSchemaVersion)}}
	}
	reasons = append(reasons, GoalplanDefinitionIntegrityReasons(plan)...)
	reasons = append(reasons, GoalplanDependencyCompletionReasons(plan)...)
	if len(plan.WorkPhases) == 0 && len(plan.Criteria) == 0 {
		reasons = append(reasons, "plan is empty: no workPhases[] and no criteria[] registered — fill the goalplan (schema in $crw-loop) before the E8 gate can certify completion")
	}
	for i := range plan.Criteria {
		c := &plan.Criteria[i]
		if c.Status == CriterionMet && (c.CapturedEvidence == nil || jstext.Trim(*c.CapturedEvidence) == "") {
			reasons = append(reasons, fmt.Sprintf("criterion %s marked met but has no captured evidence", c.ID))
		}
	}
	if remaining := RemainingWorkPhases(plan); len(remaining) > 0 {
		ids := make([]string, 0, len(remaining))
		for _, phase := range remaining {
			ids = append(ids, phase.ID)
		}
		reasons = append(reasons, fmt.Sprintf("%d work phase(s) not done: %s", len(remaining), strings.Join(ids, ", ")))
	}
	// CYCLE-COMPLETION-01: a phase closed over open tasks cannot certify completion.
	for _, phase := range DoneWorkPhasesWithPendingTasks(plan) {
		open := []string{}
		for i := range phase.Tasks {
			if phase.Tasks[i].Status != TaskDone {
				open = append(open, phase.Tasks[i].ID)
			}
		}
		reasons = append(reasons, fmt.Sprintf("work phase %s is marked done but still has open task(s): %s", phase.ID, strings.Join(open, ", ")))
	}
	if unmet := UnmetCriteria(plan); len(unmet) > 0 {
		ids := make([]string, 0, len(unmet))
		for _, criterion := range unmet {
			ids = append(ids, criterion.ID)
		}
		reasons = append(reasons, fmt.Sprintf("%d unmet criterion/criteria: %s", len(unmet), strings.Join(ids, ", ")))
	}
	reasons = append(reasons, supersededIntegrityReasons(plan)...)
	reasons = append(reasons, finalGateReasons(plan, ctx)...)
	return GoalplanValidation{OK: len(reasons) == 0, Reasons: reasons}
}

// ValidateGoalplanStructuralReasons is goalplanStructuralReasons: every E8 reason that means the plan
// itself is broken, as opposed to work that is simply not finished yet. The IDLE decision release
// refuses any of these so the agent keeps being prompted to repair the plan. It takes no validation
// context, so the v2 final-gate checks are not part of it. Its done-with-open-tasks reason names no
// task ids, where the full validator's does (:1751 against :1703).
func ValidateGoalplanStructuralReasons(plan *Goalplan) []string {
	reasons := []string{}
	if plan.SchemaVersion != nil && *plan.SchemaVersion > SupportedMaxSchemaVersion {
		reasons = append(reasons, fmt.Sprintf("schemaVersion %s is newer than this build supports", finalGateJSNumber(*plan.SchemaVersion)))
	}
	reasons = append(reasons, GoalplanDefinitionIntegrityReasons(plan)...)
	reasons = append(reasons, GoalplanDependencyCompletionReasons(plan)...)
	for i := range plan.Criteria {
		c := &plan.Criteria[i]
		if c.Status == CriterionMet && (c.CapturedEvidence == nil || jstext.Trim(*c.CapturedEvidence) == "") {
			reasons = append(reasons, fmt.Sprintf("criterion %s marked met but has no captured evidence", c.ID))
		}
	}
	for _, phase := range DoneWorkPhasesWithPendingTasks(plan) {
		reasons = append(reasons, fmt.Sprintf("work phase %s is marked done but still has open task(s)", phase.ID))
	}
	reasons = append(reasons, supersededIntegrityReasons(plan)...)
	return reasons
}
