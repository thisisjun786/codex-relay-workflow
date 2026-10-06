package goalplan

// CXC v0.2.40 goalplan.ts:1989-2133,2149-2206,2207-2248 (commit 3c1459ac). These are pure
// plan transforms: they read a plan and answer a new plan, a refusal, or the settled answer
// that says the commit already landed. They do no IO and append no ledger; a caller publishes
// a changed plan through WriteGoalplan under the write lock. Behaviour is ported as-is, the
// oracle's variant set and reason strings included, and the reused predicates live in
// query.go (WorkPhaseReadyConditionsMet, IsRunnablePhase, OpenDecisionIDsForPhase,
// queryUnmetPhaseDependencyIDs) and workphase_cursor.go (EffectiveActiveWorkPhaseID).
//
// Every name here carries the workPhase prefix the issue asks for. The result unions are
// flattened into one struct each with the optional fields omitted when absent, the shape
// the steering-op port beside this file uses for its own oracle union.
import "strings"

// WorkPhaseRecordedNext is the oracle's `recordedNext?: string | null` argument, whose three
// states must stay apart (goalplan.ts:2064-2070). Known false is undefined: a FIRST close,
// with no decision yet, so the successor is computed. Known true with a nil ID is null: an
// earlier attempt found no successor, and a retry must not start a phase that was added
// afterwards. Known true with a pointer to the empty string is a corrupt marker. A plain
// *string would fold undefined and null together and lose the retry distinction.
type WorkPhaseRecordedNext struct {
	Known bool
	ID    *string
}

// WorkPhaseAdvanceKind is what advancing the cursor answered.
type WorkPhaseAdvanceKind string

// The kinds of an advance answer. ok carries the settled plan and the id it closed (nil when
// the plan was already fully done and this cycle closed without moving a cursor); tasks_pending
// refuses; no_active says there was nothing to close.
const (
	WorkPhaseAdvanceOK           WorkPhaseAdvanceKind = "ok"
	WorkPhaseAdvanceTasksPending WorkPhaseAdvanceKind = "tasks_pending"
	WorkPhaseAdvanceNoActive     WorkPhaseAdvanceKind = "no_active"
)

// WorkPhaseAdvanceResult is the oracle's AdvanceResult union.
type WorkPhaseAdvanceResult struct {
	Kind        WorkPhaseAdvanceKind `json:"kind"`
	ClosedID    *string              `json:"closedId,omitempty"`
	Plan        *Goalplan            `json:"plan,omitempty"`
	WorkPhaseID *string              `json:"workPhaseId,omitempty"`
	Pending     []GoalplanTask       `json:"pending,omitempty"`
}

// WorkPhaseCloseFixedKind is what closing a fixed work phase answered.
type WorkPhaseCloseFixedKind string

// The kinds of a fixed close. already_done means the commit already landed and every gate
// still holds, so no plan write is owed; successor_lost means the marker named a successor
// this retry cannot use, and the close fails closed rather than picking another phase.
const (
	WorkPhaseCloseFixedOK                WorkPhaseCloseFixedKind = "ok"
	WorkPhaseCloseFixedAlreadyDone       WorkPhaseCloseFixedKind = "already_done"
	WorkPhaseCloseFixedAbsent            WorkPhaseCloseFixedKind = "absent"
	WorkPhaseCloseFixedNotRunnable       WorkPhaseCloseFixedKind = "not_runnable"
	WorkPhaseCloseFixedDependenciesUnmet WorkPhaseCloseFixedKind = "dependencies_unmet"
	WorkPhaseCloseFixedTasksPending      WorkPhaseCloseFixedKind = "tasks_pending"
	WorkPhaseCloseFixedSuccessorLost     WorkPhaseCloseFixedKind = "successor_lost"
)

// WorkPhaseCloseFixedResult is the oracle's CloseFixedResult union. Reason is one of absent,
// not_runnable, dependencies_unmet or corrupt on the successor_lost variant; a corrupt marker
// cannot be fixed by editing the plan, which is why it is its own reason.
//
// WorkPhaseID and SuccessorID are pointers because the oracle's variant always carries the key
// and its value may be the empty string: a plan can hold a work phase with an empty id, and the
// corrupt marker this type reports is exactly the empty string. A plain string with omitempty
// would drop the key and lose the marker the refusal exists to report.
type WorkPhaseCloseFixedResult struct {
	Kind        WorkPhaseCloseFixedKind `json:"kind"`
	ClosedID    *string                 `json:"closedId,omitempty"`
	Plan        *Goalplan               `json:"plan,omitempty"`
	WorkPhaseID *string                 `json:"workPhaseId,omitempty"`
	Pending     []GoalplanTask          `json:"pending,omitempty"`
	Status      WorkPhaseStatus         `json:"status,omitempty"`
	SuccessorID *string                 `json:"successorId,omitempty"`
	Reason      string                  `json:"reason,omitempty"`
	Unmet       []string                `json:"unmet,omitempty"`
}

// WorkPhaseResumeAbsentTargetKind is what a resume answered when the fixed target is gone.
type WorkPhaseResumeAbsentTargetKind string

// The kinds of a resume answer. cleanup means nothing is owed but the idempotent state and
// ledger rows; activate carries the plan with the recorded successor running.
const (
	WorkPhaseResumeActivate      WorkPhaseResumeAbsentTargetKind = "activate"
	WorkPhaseResumeCleanup       WorkPhaseResumeAbsentTargetKind = "cleanup"
	WorkPhaseResumeSuccessorLost WorkPhaseResumeAbsentTargetKind = "successor_lost"
)

// WorkPhaseResumeAbsentTargetResult is the oracle's ResumeAbsentTargetResult union.
//
// SuccessorID is a pointer for the same reason as in WorkPhaseCloseFixedResult: the oracle's
// variant always carries the key, and an empty string is a value it can hold.
type WorkPhaseResumeAbsentTargetResult struct {
	Kind        WorkPhaseResumeAbsentTargetKind `json:"kind"`
	Plan        *Goalplan                       `json:"plan,omitempty"`
	SuccessorID *string                         `json:"successorId,omitempty"`
	Reason      string                          `json:"reason,omitempty"`
}

// workPhaseText boxes an id so the variant field it belongs to keeps the key even when the id
// is the empty string.
func workPhaseText(id string) *string { return &id }

// workPhaseDependsOnSeparator is the oracle's `join("\u0000")`: a NUL that no id can hold, so
// two dependency lists compare equal only when they hold the same ids in the same order.
const workPhaseDependsOnSeparator = "\x00"

// workPhaseCloseFixed is closeFixedWorkPhase (:1989-2133): close exactly workPhaseID and move
// the cursor the way a normal advance does, after-then-wrap over pending phases whose
// dependencies are met. The gates live here, not in the callers, so a D-close recovery retry
// and a first close cannot disagree about the resulting plan.
func workPhaseCloseFixed(plan *Goalplan, workPhaseID string, recordedNext WorkPhaseRecordedNext) WorkPhaseCloseFixedResult {
	currentIdx := -1
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].ID == workPhaseID {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedAbsent}
	}
	current := &plan.WorkPhases[currentIdx]

	// A blocked or superseded phase is never closable. An advance never picks one, so this
	// only fires when a recovery target changed state after its marker was written.
	if current.Status != WorkPhasePending && current.Status != WorkPhaseInProgress && current.Status != WorkPhaseDone {
		return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedNotRunnable, Status: current.Status}
	}

	// Runnable means dependencies met, not just a workable status: checking the status alone
	// would let a target through whose dependency turned blocked after the marker was written.
	if !WorkPhaseReadyConditionsMet(plan, current) {
		unmet := append([]string{}, queryUnmetPhaseDependencyIDs(plan, current)...)
		for _, id := range OpenDecisionIDsForPhase(plan, current) {
			unmet = append(unmet, "decision:"+id)
		}
		return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedDependenciesUnmet, Unmet: unmet}
	}

	// An open task keeps the phase open on the normal path and on a recovery retry alike.
	pending := []GoalplanTask{}
	for i := range current.Tasks {
		if current.Tasks[i].Status != TaskDone {
			pending = append(pending, current.Tasks[i])
		}
	}
	if len(pending) > 0 {
		return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedTasksPending, WorkPhaseID: workPhaseText(workPhaseID), Pending: pending}
	}

	// The oracle builds a new array and a new phase object for the target, and shares the
	// task array; a caller's plan is never edited in place.
	closedWorkPhases := make([]GoalplanWorkPhase, len(plan.WorkPhases))
	copy(closedWorkPhases, plan.WorkPhases)
	closedWorkPhases[currentIdx].Status = WorkPhaseDone
	closedPlan := *plan
	closedPlan.ActiveWorkPhaseID = nil
	closedPlan.WorkPhases = closedWorkPhases

	var next *GoalplanWorkPhase
	switch {
	case !recordedNext.Known:
		// undefined: a first close computes the successor, after-then-wrap over pending phases
		// whose dependencies are met.
		next = workPhaseFirstRunnable(&closedPlan, currentIdx+1, len(closedPlan.WorkPhases))
		if next == nil {
			next = workPhaseFirstRunnable(&closedPlan, 0, currentIdx)
		}
	case recordedNext.ID == nil:
		// null: an earlier attempt found none, and a retry must not start a phase that was
		// added afterwards, so the cursor stays empty.
		next = nil
	default:
		recorded := *recordedNext.ID
		// A marker naming the target as its own successor is corrupt, and so is an empty id:
		// a close never activates the phase it just finished, and an empty id is a damaged
		// marker rather than the decision that there is no successor.
		if len(recorded) == 0 || recorded == workPhaseID {
			return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedSuccessorLost, SuccessorID: workPhaseText(recorded), Reason: "corrupt"}
		}
		named := queryFindWorkPhase(&closedPlan, recorded)
		if named == nil {
			return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedSuccessorLost, SuccessorID: workPhaseText(recorded), Reason: "absent"}
		}
		if named.Status == WorkPhaseDone {
			// A finished successor is not a lost one: the recorded phase was started and then
			// closed by its own cycle, so this retry has nothing left to activate. The cursor
			// is normalized here, whether or not the target is already done: keep it only when
			// it names a DIFFERENT phase that is really running and whose readiness holds, since
			// a cursor on the target or on a phase whose dependencies are unmet is not progress.
			next = nil
			if plan.ActiveWorkPhaseID != nil {
				if cursor := queryFindWorkPhase(&closedPlan, *plan.ActiveWorkPhaseID); cursor != nil &&
					cursor.ID != workPhaseID && cursor.Status == WorkPhaseInProgress &&
					WorkPhaseReadyConditionsMet(&closedPlan, cursor) {
					next = cursor
				}
			}
		} else if named.Status != WorkPhasePending && named.Status != WorkPhaseInProgress {
			return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedSuccessorLost, SuccessorID: workPhaseText(recorded), Reason: "not_runnable"}
		} else if !WorkPhaseReadyConditionsMet(&closedPlan, named) {
			return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedSuccessorLost, SuccessorID: workPhaseText(recorded), Reason: "dependencies_unmet"}
		} else {
			next = named
		}
	}

	settledPlan := closedPlan
	settledPlan.ActiveWorkPhaseID = nil
	if next != nil {
		cursor := next.ID
		settledPlan.ActiveWorkPhaseID = &cursor
		activated := make([]GoalplanWorkPhase, len(closedWorkPhases))
		copy(activated, closedWorkPhases)
		for i := range activated {
			if activated[i].ID == next.ID {
				activated[i].Status = WorkPhaseInProgress
			}
		}
		settledPlan.WorkPhases = activated
	}

	// Identity is the last step, not the whole judgement: the settled shape is compared
	// against the input so a retry whose commit already landed answers already_done instead
	// of writing the close rows again.
	if workPhaseSamePlanShape(plan, &settledPlan) {
		return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedAlreadyDone}
	}
	closedID := workPhaseID
	return WorkPhaseCloseFixedResult{Kind: WorkPhaseCloseFixedOK, ClosedID: &closedID, Plan: &settledPlan}
}

// workPhaseFirstRunnable is the oracle's slice(...).find() over the half-open phase range:
// the first pending phase whose readiness holds, or nil.
func workPhaseFirstRunnable(plan *Goalplan, from, to int) *GoalplanWorkPhase {
	for i := from; i < to; i++ {
		if plan.WorkPhases[i].Status == WorkPhasePending && IsRunnablePhase(plan, &plan.WorkPhases[i]) {
			return &plan.WorkPhases[i]
		}
	}
	return nil
}

// workPhaseResumeAbsentTarget is resumeAbsentTarget (:2149-2192): what a resume should do when
// the fixed target is gone from the plan but the marker still names a successor. The gates
// mirror workPhaseCloseFixed, so a phase that is blocked, superseded or waiting on a
// dependency fails closed instead of logging started for work nobody scheduled.
func workPhaseResumeAbsentTarget(plan *Goalplan, recordedNext string) WorkPhaseResumeAbsentTargetResult {
	// A marker that recorded no successor has nothing to activate: the close it describes
	// ended the plan, so cleanup is the whole remaining job. The oracle's `!recordedNext` is
	// a falsy test, so null and the empty string answer the same.
	if recordedNext == "" {
		return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeCleanup}
	}
	named := queryFindWorkPhase(plan, recordedNext)
	if named == nil {
		return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeSuccessorLost, SuccessorID: workPhaseText(recordedNext), Reason: "absent"}
	}
	// Finished on its own: the activation happened and only the ledger and state rows are owed.
	if named.Status == WorkPhaseDone {
		return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeCleanup}
	}
	if named.Status != WorkPhasePending && named.Status != WorkPhaseInProgress {
		return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeSuccessorLost, SuccessorID: workPhaseText(recordedNext), Reason: "not_runnable"}
	}
	// Readiness is checked before either branch below, so deleting the target does not decide
	// the verdict: the same successor waiting on the same unmet dependency is refused either way.
	if !WorkPhaseReadyConditionsMet(plan, named) {
		return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeSuccessorLost, SuccessorID: workPhaseText(recordedNext), Reason: "dependencies_unmet"}
	}
	// Running: the activation happened too, but only if the cursor agrees. A null or moved
	// cursor stranding an in_progress phase is exactly the corruption a resume must repair.
	if named.Status == WorkPhaseInProgress {
		if plan.ActiveWorkPhaseID != nil && *plan.ActiveWorkPhaseID == named.ID {
			return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeCleanup}
		}
		activated := *plan
		cursor := named.ID
		activated.ActiveWorkPhaseID = &cursor
		return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeActivate, Plan: &activated}
	}
	activated := *plan
	cursor := named.ID
	activated.ActiveWorkPhaseID = &cursor
	activatedPhases := make([]GoalplanWorkPhase, len(plan.WorkPhases))
	copy(activatedPhases, plan.WorkPhases)
	for i := range activatedPhases {
		if activatedPhases[i].ID == named.ID {
			activatedPhases[i].Status = WorkPhaseInProgress
		}
	}
	activated.WorkPhases = activatedPhases
	return WorkPhaseResumeAbsentTargetResult{Kind: WorkPhaseResumeActivate, Plan: &activated}
}

// workPhaseSamePlanShape is samePlanShape (:2193-2206): structural equality over the fields a
// close writes, so timestamps and prose cannot make the check brittle. A nil cursor and the
// oracle's null cursor are the same state; the oracle's undefined cursor is not representable
// in Go, and a plan that was read from disk always carries null rather than undefined.
func workPhaseSamePlanShape(left, right *Goalplan) bool {
	if !workPhaseSameCursor(left.ActiveWorkPhaseID, right.ActiveWorkPhaseID) {
		return false
	}
	if len(left.WorkPhases) != len(right.WorkPhases) {
		return false
	}
	for i := range left.WorkPhases {
		a, b := &left.WorkPhases[i], &right.WorkPhases[i]
		if a.ID != b.ID || a.Status != b.Status {
			return false
		}
		if strings.Join(a.DependsOn, workPhaseDependsOnSeparator) != strings.Join(b.DependsOn, workPhaseDependsOnSeparator) {
			return false
		}
		if len(a.Tasks) != len(b.Tasks) {
			return false
		}
		for j := range a.Tasks {
			if a.Tasks[j].ID != b.Tasks[j].ID || a.Tasks[j].Status != b.Tasks[j].Status {
				return false
			}
		}
	}
	return true
}

func workPhaseSameCursor(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

// workPhaseAdvance is advanceWorkPhase (:2207-2248): mark the effective active work phase
// done and set the next pending one active. The pending-task refusal moves into the shared
// helper and is forwarded unchanged, so the CLI and the chat wording stay identical. Closing
// succeeds even when nothing else can start: a verified completion is not rolled back because
// a successor is blocked, and the cursor goes null.
func workPhaseAdvance(plan *Goalplan) WorkPhaseAdvanceResult {
	// A null or stale cursor adopts the effective active work phase instead of no-opping, so
	// the standard loop-init flow (cursor seeded null) still books work-phase closes. No
	// explicit guard against blocked or superseded is needed: the cursor reader already skips
	// them, so neither can be the phase this marks done.
	effectiveID := EffectiveActiveWorkPhaseID(plan)
	if effectiveID == nil {
		return WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceNoActive}
	}
	currentIdx := -1
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].ID == *effectiveID {
			currentIdx = i
			break
		}
	}
	if currentIdx < 0 {
		return WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceNoActive}
	}
	closed := workPhaseCloseFixed(plan, plan.WorkPhases[currentIdx].ID, WorkPhaseRecordedNext{})
	if closed.Kind == WorkPhaseCloseFixedTasksPending {
		return WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceTasksPending, WorkPhaseID: closed.WorkPhaseID, Pending: closed.Pending}
	}
	// absent, not_runnable and dependencies_unmet all fold into no_active: the effective
	// cursor never picks such a phase, so the normal path cannot reach them. successor_lost
	// cannot occur here at all, because this call records no successor.
	if closed.Kind != WorkPhaseCloseFixedOK {
		return WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceNoActive}
	}
	return WorkPhaseAdvanceResult{Kind: WorkPhaseAdvanceOK, ClosedID: closed.ClosedID, Plan: closed.Plan}
}

// AdvanceWorkPhase, CloseFixedWorkPhase and ResumeAbsentTarget are the exported wrappers of the
// three unexported transforms above. The chat D-close (internal/pabcd/hook/prompt_dclose.go) and
// the CLI D-close are separate surfaces in the oracle and share these helpers there, so the port
// shares them here through one exported name each. Nothing else in this file changes.
func AdvanceWorkPhase(plan *Goalplan) WorkPhaseAdvanceResult { return workPhaseAdvance(plan) }

func CloseFixedWorkPhase(plan *Goalplan, workPhaseID string, recordedNext WorkPhaseRecordedNext) WorkPhaseCloseFixedResult {
	return workPhaseCloseFixed(plan, workPhaseID, recordedNext)
}

func ResumeAbsentTarget(plan *Goalplan, recordedNext string) WorkPhaseResumeAbsentTargetResult {
	return workPhaseResumeAbsentTarget(plan, recordedNext)
}
