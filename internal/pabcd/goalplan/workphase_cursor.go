package goalplan

// CXC v0.2.40 goalplan.ts:2134-2142,2249-2267 (commit 3c1459ac). Pure reads over a
// plan: the work-phase a bound loop is FOR, and the wording a resume gives when the
// successor a marker recorded is gone. No IO, no ledger and no writes; callers that
// change a plan publish it through WriteGoalplan under the write lock.

// EffectiveActiveWorkPhaseID ports effectiveActiveWorkPhaseId: the binding target of
// this cycle (LOOP-UNIT-CHAIN-01). An explicit cursor wins ONLY when it names a live,
// runnable work-phase — a stale ghost, an already-done cursor, or one whose
// dependencies are unmet falls through. Otherwise the first runnable in-progress
// phase, then the first runnable pending one. nil only when no open work-phase
// exists, so a bound, registered plan always yields a binding target and "bound but
// cursorless" cannot dodge the workPhaseId gate.
func EffectiveActiveWorkPhaseID(plan *Goalplan) *string {
	// The oracle's `if (plan.activeWorkPhaseId)` is a truthiness test, so an
	// empty-string cursor is unset there; a bare nil check would read it as set.
	if plan.ActiveWorkPhaseID != nil && *plan.ActiveWorkPhaseID != "" {
		// find(): the FIRST phase with the cursor id decides, and only it; a second
		// phase that happens to share the id is never consulted.
		for i := range plan.WorkPhases {
			if plan.WorkPhases[i].ID == *plan.ActiveWorkPhaseID {
				if IsRunnablePhase(plan, &plan.WorkPhases[i]) {
					return workphaseCursorText(plan.WorkPhases[i].ID)
				}
				break
			}
		}
	}
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].Status == WorkPhaseInProgress && IsRunnablePhase(plan, &plan.WorkPhases[i]) {
			return workphaseCursorText(plan.WorkPhases[i].ID)
		}
	}
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].Status == WorkPhasePending && IsRunnablePhase(plan, &plan.WorkPhases[i]) {
			return workphaseCursorText(plan.WorkPhases[i].ID)
		}
	}
	return nil
}

// AbsentSuccessorDetail ports absentSuccessorDetail: one wording source so the two
// surfaces cannot describe the same state differently. "dependencies_unmet" and any
// other reason take the third wording, as the oracle's final branch does.
func AbsentSuccessorDetail(reason string) string {
	switch reason {
	case "absent":
		return "is gone too"
	case "not_runnable":
		return "can no longer be started"
	default:
		return "now waits for a prerequisite or decision"
	}
}

// workphaseCursorText returns a copy of an id, so the answer never aliases the plan.
func workphaseCursorText(id string) *string { return &id }
