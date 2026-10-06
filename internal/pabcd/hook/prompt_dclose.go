// prompt_dclose.go holds the bound chat D-close, CXC v0.2.40 pabcd-state/src/hook.ts:875-901
// (the close target, the recovery match and the transition), 939-989 and 1137-1286 (the receipt
// gate and the first goalplan lock), 990-1136 (the recovery arms), 1256-1258 (the successor the
// started row names), 1302-1354 (the IDLE state write and the second lock) and 1394-1396 (the
// DONE directive), plus hasGoalplanRow / hasPabcdCloseRow / HookDcloseCommitHooks (628-654). It
// fills the seam CRW-385 left in prompt_orchestrate.go, which hands a bound D here instead of
// letting it fall through to the loose detector.
//
// What the range decides, in the oracle's order: whether this D request is finishing a close a
// previous attempt started (a marker that matches the session, the check epoch and the fixed
// target); the transition, which for a fresh close is the human free pass C>D and for a retry is
// the close the marker already describes; the C>D receipt gate, which a retry skips because the
// first attempt spent that binding; the goalplan integrity check, the empty plan, the all-done
// close, the required target, the advance or the fixed close, and the target and epoch checks; the
// recovery marker, the plan commit and the two goalplan ledger rows; the IDLE state write; and the
// PABCD close row with the marker cleanup. Texts are frozen byte for byte after
// contract/schema/cxc/name-substitution.json, and a backticked command is resolved at emission.
//
// Three departures from the oracle. The first is the class the parity rule revision of 2026-10-03
// fixes during the port, the second is the same class in the row readers, and the third is the
// lock the oracle does not have:
//
//   - The oracle reads the session state, changes fields and writes the whole state back with no
//     lock, twice. Here the whole close runs under one state.WithSessionLock: the state is re-read
//     strictly inside it, the close is applied to that read, and the write is refused when the
//     reader would not keep a stored record whole. The lock is taken once; the state write does not
//     go through promptSubmitWriteState, which would take it a second time. A close whose state
//     moved between the leading section's read and the lock refuses with nothing written.
//   - The oracle's hasPabcdCloseRow and hasGoalplanRow parse every line with JSON.parse, so one
//     damaged line throws out of the handler and the same D request can never finish. Here a line
//     that is not a JSON object matches nothing; the worst outcome is one duplicate ledger row.
//   - The commit seams the oracle's test passes as callbacks cannot throw here: they are struct
//     fields, and the tests drive the close through the same unexported entry points the harness
//     uses, so a stopped close is observed by the state on disk rather than by an exception.
//
// This file has no package-level initializer and needs no Node at run time.
package hook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source/session"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// promptDcloseTick is one backtick. The oracle's recovery refusals quote a command between two of
// them; building the text from this constant keeps the Go source free of a raw string.
const promptDcloseTick = "\u0060"

// promptDcloseTimestampLayout is Date.prototype.toISOString, as a ledger row prints a time.
const promptDcloseTimestampLayout = "2006-01-02T15:04:05.000Z"

// promptDcloseSeams is HookDcloseCommitHooks (:628-634): the four points the oracle's test stops a
// close at. They are fields of a value the caller passes, never a package-level variable, so a
// production run holds none of them and no package-level initializer does work.
type promptDcloseSeams struct {
	afterRecoveryMarkerWrite func()
	afterGoalplanCommit      func()
	afterStateWrite          func()
	afterPabcdLedgerAppend   func()
}

// promptDcloseOutcome is what one bound close did while it held the session lock: the refusal or
// the finalization-pending text to inject, or neither when the cycle closed.
type promptDcloseOutcome struct {
	refusal string
	pending string
}

// promptDclosePlanOutcome is what the first goalplan lock answered: the text to inject when it
// refused, the all-done discriminant, and the successor a fresh close recorded in the marker.
type promptDclosePlanOutcome struct {
	output     string
	allDone    bool
	markerNext *string
}

// promptDcloseCloseResult is the oracle's AdvanceResult as the shared tail reads it.
type promptDcloseCloseResult struct {
	kind     string
	closedID string
	plan     *goalplan.Goalplan
}

// promptOrchestrateBoundDclose is the bound D-close seam (hook.ts:939-1429). current is the state
// the leading section read and command is the parsed chat command. handled is always true: unlike
// the forward edges, a bound D is owned here even when it refuses.
func promptOrchestrateBoundDclose(p PromptSubmitPayload, current state.State, turn string, _ host.LookupEnv, lock func(cwd, sessionID string, fn func() error) error, command *fsm.OrchestrateCommand, seams *promptDcloseSeams) (string, bool) {
	closePhaseID := ""
	if command.Verb == fsm.VerbD && command.Attest != nil {
		closePhaseID = text.Trim(command.Attest.WorkPhaseID)
	}
	recovering := command.Verb == fsm.VerbD && state.MatchesDcloseRecovery(current, closePhaseID)

	// The transition: a retry replays the close the marker already describes, a fresh close is the
	// human free pass C>D. Both build the row the finalization appends.
	var result fsm.ApplyResult
	if recovering {
		result = promptDcloseRecoveredTransition(current, command)
	} else {
		result = fsm.ApplyHumanTransition(current, fsm.VerbD, command.Attest)
	}
	if !result.OK {
		return promptOrchestrateRefusal(result.Reason), true
	}

	// CHECK-BINDING-01 (075): the same receipt requirement as the CLI, checked here so a chat
	// D-close cannot be the way around it. A marker-matched retry already spent its receipt in the
	// first attempt, and re-requiring it would refuse a repair for a gate it cannot satisfy twice.
	if !recovering {
		receiptCheck := gate.ValidateCheckReceipt(current, p.SessionID, promptDcloseTestReceipt(command), p.Cwd)
		if !receiptCheck.OK {
			return promptOrchestrateRefusal(receiptCheck.Reason + " Nothing was written."), true
		}
	}

	outcome := promptDcloseOutcome{}
	err := lock(p.Cwd, p.SessionID, func() error {
		held, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
		if unreadable {
			outcome.refusal = promptDcloseStateRefusal()
			return nil
		}
		// The recovery decision and the receipt gate were judged on the state the leading section
		// read, before this lock. The close runs on the state the lock found, so the two must agree:
		// a close that would be judged twice by different rules writes nothing (the oracle holds no
		// lock and would have overwritten the participating writer outright).
		if held.Phase != current.Phase || held.Slug != current.Slug || state.MatchesDcloseRecovery(held, closePhaseID) != recovering {
			outcome.refusal = promptDcloseStateMovedRefusal()
			return nil
		}
		// The rewrite guard runs where a write is about to happen rather than here, so a refusal the
		// oracle answers earlier - the legacy marker, the integrity check, an empty plan - keeps the
		// oracle's own text. A close that writes nothing never consults it.
		guard := func() string {
			if !promptSubmitRewritable(p.Cwd, p.SessionID, held) {
				return promptDcloseStateRefusal()
			}
			return ""
		}
		outcome = promptDcloseClose(p, held, turn, closePhaseID, recovering, result, command, seams, guard)
		return nil
	})
	if err != nil {
		// The oracle holds no session lock; a lock that stays busy answers the D-close text with the
		// busy reason and writes nothing.
		return promptDcloseNotApplied(err.Error()), true
	}
	if outcome.refusal != "" {
		return outcome.refusal, true
	}
	if outcome.pending != "" {
		return outcome.pending, true
	}
	// done: the chat D-close. Inject the DONE summary directive this turn; the resting state is
	// already IDLE, so the footer surfaces IDLE.
	return WithFooter(PhaseDirective(state.PhaseD, nil), state.PhaseIdle), true
}

// promptDcloseRecoveredTransition is the recoveringDclose ApplyResult (:881-896): the resting
// state, with the check epoch and the marker the close still has to clear, and the close row the
// first attempt would have written.
func promptDcloseRecoveredTransition(current state.State, command *fsm.OrchestrateCommand) fsm.ApplyResult {
	next := fsm.ClearedIdle(current)
	next.CheckEpoch, next.DcloseRecovery = current.CheckEpoch, current.DcloseRecovery
	from := state.PhaseC
	row := &state.LedgerEntry{TS: promptDcloseTimestamp(), SessionID: current.SessionID, From: &from, To: state.PhaseIdle, Reason: "done", EvidenceAfterReason: true}
	if att := command.Attest; att != nil && att.Did != "" {
		did := att.Did
		row.Evidence = &did
	}
	return fsm.ApplyResult{OK: true, Control: fsm.ControlDone, State: &next, Ledger: row}
}

// promptDcloseClose runs the bound close on the state the session lock found. It is the body of
// the oracle's handler from the first goalplan lock to the finalization.
func promptDcloseClose(p PromptSubmitPayload, held state.State, turn, closePhaseID string, recovering bool, result fsm.ApplyResult, command *fsm.OrchestrateCommand, seams *promptDcloseSeams, guard func() string) promptDcloseOutcome {
	slug := held.Slug

	// The marker and the plan commit are one critical section, and the integrity check runs inside
	// it, before either write.
	locked, err := goalplan.WithGoalplanWriteLock(p.Cwd, slug, func(plan *goalplan.Goalplan) (promptDclosePlanOutcome, error) {
		return promptDclosePlanWork(p, held, plan, closePhaseID, recovering, result, seams, guard), nil
	}, nil)
	if err != nil {
		return promptDcloseOutcome{refusal: promptDcloseGoalplanUnreadable(err.Error())}
	}
	switch locked.Kind {
	case "locked":
		return promptDcloseOutcome{refusal: promptDcloseNotApplied(locked.Reason)}
	case "unreadable":
		return promptDcloseOutcome{refusal: promptDcloseGoalplanUnreadable(locked.Reason)}
	}
	if locked.Value == nil {
		return promptDcloseOutcome{refusal: promptDcloseGoalplanUnreadable("the bound goalplan could not be read")}
	}
	if locked.Value.output != "" {
		return promptDcloseOutcome{refusal: locked.Value.output}
	}
	allDoneClose := locked.Value.allDone

	// §40 Z3: the bound D-close owns its own state write. The write keeps every field the ordinary
	// D-close write sets - dropping injectedTurns would break same-turn dedup and dropping the
	// stopBlock reset would leave C stagnation state on an IDLE session - and layers the recovery
	// fields on top. It is written over the state the lock found, so a participating writer's
	// update survives.
	entrySource, planBinding, keepBinding := promptDcloseBindings(p, held, result, command)
	next := *result.State
	next.PhaseEntrySource = entrySource
	next.PlanUnit, next.PlanEpoch = promptDclosePlanBindingOf(planBinding, held, keepBinding)
	next.OrchestrationActive, next.LastInjectedPhase = false, nil
	next.InjectedTurns = promptDcloseInjectedTurns(turn, held.InjectedTurns)
	next.StopBlockPhase, next.StopBlockWorkPhaseID, next.StopBlockCount = nil, nil, 0
	switch {
	case allDoneClose:
		// §40 Z2: all-done wrote its close row inside the first lock and has no marker to clear.
		next.CheckEpoch, next.DcloseRecovery = nil, nil
	case recovering:
		next.CheckEpoch, next.DcloseRecovery = held.CheckEpoch, held.DcloseRecovery
	default:
		next.CheckEpoch = held.CheckEpoch
		next.DcloseRecovery = promptDcloseMarker(held, closePhaseID, locked.Value.markerNext)
	}
	if refusal := guard(); refusal != "" {
		return promptDcloseOutcome{refusal: refusal}
	}
	if writeErr := state.WriteState(p.Cwd, next); writeErr != nil {
		return promptDcloseOutcome{refusal: promptDcloseStateRefusal()}
	}
	promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterStateWrite })

	// §40 Z2: all-done wrote its close row inside the first lock, so it skips this second critical
	// section entirely. For everything else the row and the marker cleanup are one section, so two
	// recoveries cannot both observe an absent row.
	if allDoneClose {
		return promptDcloseOutcome{}
	}
	closeCheckEpoch, closedWorkPhaseID := held.CheckEpoch, closePhaseID
	finalize, finalizeErr := goalplan.WithGoalplanWriteLock(p.Cwd, slug, func(*goalplan.Goalplan) (struct{}, error) {
		if result.Ledger != nil && !promptDcloseHasPabcdCloseRow(p.Cwd, p.SessionID, closeCheckEpoch, closedWorkPhaseID) {
			row := *result.Ledger
			row.Close = &state.CloseKey{CheckEpoch: closeCheckEpoch, ClosedWorkPhaseID: &closedWorkPhaseID}
			// The hook's close rows spread the transition row, evidence included, before the close
			// key (hook.ts:1335 through orchestrate-apply.ts:111-118).
			row.EvidenceAfterReason = true
			if rowErr := state.AppendLedger(p.Cwd, row); rowErr != nil {
				return struct{}{}, rowErr
			}
			promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterPabcdLedgerAppend })
		}
		current, _ := state.ReadStateStrict(p.Cwd, p.SessionID)
		if state.MatchesDcloseRecovery(current, closePhaseID) {
			// The marker cleanup is a state write too, so it passes the same guard. A cleanup the
			// reader would not keep whole is left for the operator: the cycle is already closed and
			// the marker still describes it, which is what the pending text reports.
			if refusal := guard(); refusal != "" {
				return struct{}{}, errors.New("the recovery marker cannot be cleared without losing a stored record")
			}
			current.CheckEpoch, current.DcloseRecovery = nil, nil
			if writeErr := state.WriteState(p.Cwd, current); writeErr != nil {
				return struct{}{}, writeErr
			}
		}
		return struct{}{}, nil
	}, nil)
	if finalizeErr != nil || finalize.Kind != "ok" {
		reason := finalize.Reason
		if finalizeErr != nil {
			reason = finalizeErr.Error()
		}
		return promptDcloseOutcome{pending: promptDcloseFinalizePending(reason)}
	}
	return promptDcloseOutcome{}
}

// promptDclosePlanWork is the body of the first goalplan lock (:956-1266). It answers the text to
// inject when it refuses, or the all-done discriminant when it does not.
func promptDclosePlanWork(p PromptSubmitPayload, held state.State, plan *goalplan.Goalplan, closePhaseID string, recovering bool, result fsm.ApplyResult, seams *promptDcloseSeams, guard func() string) promptDclosePlanOutcome {
	slug := held.Slug

	// §5: integrity is checked inside the lock, before marker or any write.
	integrityReasons := goalplan.GoalplanDefinitionIntegrityReasons(plan)
	integrityReasons = append(integrityReasons, goalplan.GoalplanDependencyCompletionReasons(plan)...)
	if len(integrityReasons) > 0 {
		return promptDclosePlanOutcome{output: promptOrchestrateRefusal("invalid goalplan: " + strings.Join(integrityReasons, "; ") + ". Nothing was written.")}
	}
	if len(plan.WorkPhases) == 0 {
		return promptDclosePlanOutcome{output: promptOrchestrateRefusal("the bound goalplan " + promptDcloseQuote(slug) + " has no active work-phase to close (CYCLE-COMPLETION-01). Nothing was written.")}
	}

	// §39 Y2: recovery is checked BEFORE all-done, in the same order as the CLI path. Crashing
	// right after the final work-phase commit leaves an all-done plan; checking all-done first
	// would consume that retry as a plain cycle close and record closedWorkPhaseId: null, dropping
	// the marker's target.
	closeResult := promptDcloseCloseResult{}
	writeClosedPlan := false

	if recovering {
		out, refusal, refused := promptDcloseRecoveryClose(p, held, plan, closePhaseID)
		if refused {
			return promptDclosePlanOutcome{output: refusal}
		}
		closeResult, writeClosedPlan = out.result, out.writePlan
	} else {
		// §35-3: a non-empty all-done plan closes only the cycle. It needs no target and writes no
		// recovery marker or goalplan row. This sits inside the non-recovery branch so a matching
		// marker always wins.
		if promptDcloseAllDone(plan) {
			// §40 Z2: the close row lands inside this first lock, because all-done leaves no marker
			// for a failed second lock to resume.
			if result.Ledger != nil && held.Phase == state.PhaseC && !promptDcloseHasPabcdCloseRow(p.Cwd, p.SessionID, held.CheckEpoch, "") {
				row := *result.Ledger
				row.Close = &state.CloseKey{CheckEpoch: held.CheckEpoch}
				if rowErr := state.AppendLedger(p.Cwd, row); rowErr != nil {
					return promptDclosePlanOutcome{output: promptOrchestrateRefusal("the PABCD close row could not be written: " + rowErr.Error() + " Nothing was written.")}
				}
				promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterPabcdLedgerAppend })
			}
			return promptDclosePlanOutcome{allDone: true}
		}
		// §35-5: target validation follows empty-plan, all-done and recovery.
		if closePhaseID == "" {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("bound chat D-close requires attest.workPhaseId. Nothing was written.")}
		}
		if promptDcloseFindWorkPhase(plan, closePhaseID) == nil {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("work-phase " + closePhaseID + " is not in the bound goalplan. Nothing was written.")}
		}
		advanced := goalplan.AdvanceWorkPhase(plan)
		switch advanced.Kind {
		case goalplan.WorkPhaseAdvanceTasksPending:
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("work-phase " + promptDcloseString(advanced.WorkPhaseID) + " still has " + promptDcloseCount(len(advanced.Pending)) + " open task(s), so this cycle cannot close (CYCLE-COMPLETION-01): " + promptDclosePendingText(advanced.Pending) + ". Nothing was written.")}
		case goalplan.WorkPhaseAdvanceNoActive:
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal(promptDcloseNoActive(slug, plan))}
		}
		closeResult = promptDcloseCloseResult{kind: "ok", closedID: promptDcloseString(advanced.ClosedID), plan: advanced.Plan}
		writeClosedPlan = true
	}

	// The shared tail of both branches: the target check, the C epoch requirement, the marker, the
	// plan commit and the two goalplan ledger rows.
	if !recovering && closeResult.closedID != closePhaseID {
		return promptDclosePlanOutcome{output: promptOrchestrateRefusal("fixed close target " + closePhaseID + " does not match active work-phase " + closeResult.closedID + ". Nothing was written.")}
	}
	markerNext := (*string)(nil)
	if !recovering {
		if held.Phase != state.PhaseC || held.CheckEpoch == nil {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("current C check epoch is required. Nothing was written.")}
		}
		// §48: the successor this close chose is recorded before the plan commit, so a retry never
		// has to infer it from the file.
		markerNext = closeResult.plan.ActiveWorkPhaseID
		if refusal := guard(); refusal != "" {
			return promptDclosePlanOutcome{output: refusal}
		}
		if markerErr := promptDcloseWriteMarker(p.Cwd, held, closePhaseID, markerNext); markerErr != nil {
			return promptDclosePlanOutcome{output: promptDcloseStateRefusal()}
		}
		promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterRecoveryMarkerWrite })
	}
	if writeClosedPlan {
		if planErr := goalplan.WriteGoalplan(p.Cwd, closeResult.plan); planErr != nil {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("the goalplan could not be written: " + planErr.Error() + " Nothing was written.")}
		}
		promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterGoalplanCommit })
	}
	if !promptDcloseHasGoalplanRow(p.Cwd, slug, "workphase_done", "closed "+closePhaseID) {
		if rowErr := goalplan.AppendGoalplanLedger(p.Cwd, slug, goalplan.GoalplanLedgerEntry{
			Ts: promptDcloseTimestamp(), Slug: slug, Event: goalplan.EventWorkphaseDone, Detail: "closed " + closePhaseID,
		}); rowErr != nil {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("the goalplan ledger row could not be written: " + rowErr.Error() + " Nothing was written.")}
		}
	}
	// §52: a resume names the marker successor, a fresh close names the cursor it just computed.
	startedID := ""
	if recovering {
		startedID = promptDcloseString(held.DcloseRecovery.NextWorkPhaseID)
	} else {
		startedID = promptDcloseString(closeResult.plan.ActiveWorkPhaseID)
	}
	if startedID != "" && !promptDcloseHasGoalplanRow(p.Cwd, slug, "workphase_started", "started "+startedID) {
		if rowErr := goalplan.AppendGoalplanLedger(p.Cwd, slug, goalplan.GoalplanLedgerEntry{
			Ts: promptDcloseTimestamp(), Slug: slug, Event: goalplan.EventWorkphaseStarted, Detail: "started " + startedID,
		}); rowErr != nil {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("the goalplan ledger row could not be written: " + rowErr.Error() + " Nothing was written.")}
		}
	}
	return promptDclosePlanOutcome{markerNext: markerNext}
}

// promptDcloseRecoveryOutcome is what the recovery arm answered when it did not refuse.
type promptDcloseRecoveryOutcome struct {
	result    promptDcloseCloseResult
	writePlan bool
}

// promptDcloseRecoveryClose is the recoveringDclose arm (:990-1136): the legacy marker refusal, the
// absent target through ResumeAbsentTarget, and a present target through CloseFixedWorkPhase with
// its tasks_pending, not_runnable, dependencies_unmet and successor_lost answers. Every refusal
// keeps the marker so the operator can repair the plan and finish with the same request. refused
// tells the caller which field it holds: true means the first return is the text to inject.
func promptDcloseRecoveryClose(p PromptSubmitPayload, held state.State, plan *goalplan.Goalplan, closePhaseID string) (promptDcloseRecoveryOutcome, string, bool) {
	marker := held.DcloseRecovery
	// §50: a pre-§48 marker has no safe reading, so this stops instead of nulling the cursor on a
	// plan whose commit may have landed.
	if marker.Legacy {
		return promptDcloseRecoveryOutcome{}, promptOrchestrateRefusal("the recovery marker for " + closePhaseID +
			" predates the successor field, so this retry cannot tell whether the plan commit landed. " +
			"The marker was kept; inspect the goalplan, set the work-phase statuses and " +
			"activeWorkPhaseId by hand, then run " + promptDcloseResetCommand(p.SessionID) +
			" to clear the marker. Nothing was written."), true
	}
	// §39 Y1: the marker is written before the plan commit, so a matching marker does not prove
	// the plan was closed. An absent target means a later edit removed it and the commit is not
	// ours to redo; present-but-open is the marker-then-crash case and we close exactly that phase.
	fixed := promptDcloseFindWorkPhase(plan, closePhaseID)
	resumedAbsent := (*goalplan.Goalplan)(nil)
	if fixed == nil {
		// §53: the absent-target decision is shared with the CLI. Leaving it out here let the same
		// marker activate a pending successor on one surface and silently log started without a plan
		// write on the other.
		orphan := goalplan.ResumeAbsentTarget(plan, promptDcloseString(marker.NextWorkPhaseID))
		if orphan.Kind == goalplan.WorkPhaseResumeSuccessorLost {
			return promptDcloseRecoveryOutcome{}, promptOrchestrateRefusal("recovery target " + closePhaseID + " is gone from the plan and the successor " +
				promptDcloseString(orphan.SuccessorID) + " it recorded " + goalplan.AbsentSuccessorDetail(orphan.Reason) +
				", so this retry cannot tell what to finish. The marker was kept; inspect the goalplan, " +
				"set the work-phase statuses and activeWorkPhaseId by hand, then run " + promptDcloseResetCommand(p.SessionID) +
				" to clear the marker. Nothing was written."), true
		}
		if orphan.Kind == goalplan.WorkPhaseResumeActivate {
			resumedAbsent = orphan.Plan
		}
	}
	// §40 Z1: both surfaces go through CloseFixedWorkPhase, so the recovered plan matches what a
	// normal close would have written - cursor moved, successor in_progress, a truthful started row.
	closed := goalplan.WorkPhaseCloseFixedResult{Kind: goalplan.WorkPhaseCloseFixedAbsent}
	if fixed != nil {
		closed = goalplan.CloseFixedWorkPhase(plan, closePhaseID, goalplan.WorkPhaseRecordedNext{Known: true, ID: marker.NextWorkPhaseID})
	}
	switch closed.Kind {
	case goalplan.WorkPhaseCloseFixedTasksPending:
		// §41 W1: the marker stays so the operator can repair the plan and finish with the same request.
		return promptDcloseRecoveryOutcome{}, promptOrchestrateRefusal("recovery target " + closePhaseID + " gained " + promptDcloseCount(len(closed.Pending)) +
			" open task(s) after its marker was written (CYCLE-COMPLETION-01): " + promptDclosePendingText(closed.Pending) +
			". The recovery marker was kept; close those tasks and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedNotRunnable:
		return promptDcloseRecoveryOutcome{}, promptOrchestrateRefusal("recovery target " + closePhaseID + " is now " + string(closed.Status) +
			" (CYCLE-COMPLETION-01). The recovery marker was kept; restore that work-phase and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedDependenciesUnmet:
		return promptDcloseRecoveryOutcome{}, promptOrchestrateRefusal("recovery target " + closePhaseID + " now waits for " + strings.Join(closed.Unmet, ", ") +
			" (CYCLE-COMPLETION-01). The recovery marker was kept; satisfy those work-phases and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedSuccessorLost:
		// §51: a corrupt marker points at reset, not at a fix.
		if closed.Reason == "corrupt" {
			return promptDcloseRecoveryOutcome{}, promptOrchestrateRefusal("the recovery marker for " + closePhaseID + " names that same work-phase as its successor, " +
				"which no close can produce, so this retry cannot tell what to finish. The marker was kept; inspect the goalplan, " +
				"set the work-phase statuses and activeWorkPhaseId by hand, then run " + promptDcloseResetCommand(p.SessionID) +
				" to clear the marker. Nothing was written."), true
		}
		detail := "now waits for another work-phase"
		switch closed.Reason {
		case "absent":
			detail = "is no longer in the plan"
		case "not_runnable":
			detail = "can no longer be started"
		}
		return promptDcloseRecoveryOutcome{}, promptOrchestrateRefusal("recovery target " + closePhaseID + " was closed with successor " +
			promptDcloseString(closed.SuccessorID) + ", which " + detail + " (CYCLE-COMPLETION-01). The recovery marker was kept; " +
			"restore that work-phase and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedOK:
		return promptDcloseRecoveryOutcome{result: promptDcloseCloseResult{kind: "ok", closedID: promptDcloseString(closed.ClosedID), plan: closed.Plan}, writePlan: true}, "", false
	}
	// already_done and any other answer: the close settled on the fixed target, and the plan is
	// written only when an absent-target resume activated the recorded successor (§53).
	if resumedAbsent != nil {
		return promptDcloseRecoveryOutcome{result: promptDcloseCloseResult{kind: "ok", closedID: closePhaseID, plan: resumedAbsent}, writePlan: true}, "", false
	}
	return promptDcloseRecoveryOutcome{result: promptDcloseCloseResult{kind: "ok", closedID: closePhaseID, plan: plan}}, "", false
}

// promptDcloseAllDone is the oracle's plan.workPhases.every((wp) => wp.status === "done").
func promptDcloseAllDone(plan *goalplan.Goalplan) bool {
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].Status != goalplan.WorkPhaseDone {
			return false
		}
	}
	return true
}

// promptDcloseFindWorkPhase is the oracle's plan.workPhases.find((wp) => wp.id === id): the first
// phase with that id, or nil.
func promptDcloseFindWorkPhase(plan *goalplan.Goalplan, id string) *goalplan.GoalplanWorkPhase {
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].ID == id {
			return &plan.WorkPhases[i]
		}
	}
	return nil
}

// promptDcloseMarker is the recovery marker the state write leaves behind for a fresh close: the
// fixed target and the successor the close chose, recorded before the plan commit so a retry never
// has to infer them from the file (§48).
func promptDcloseMarker(held state.State, closePhaseID string, nextWorkPhaseID *string) *state.DcloseRecoveryMarker {
	if held.CheckEpoch == nil {
		return nil
	}
	return &state.DcloseRecoveryMarker{SessionID: held.SessionID, CheckEpoch: *held.CheckEpoch, ClosedWorkPhaseID: closePhaseID, NextWorkPhaseID: nextWorkPhaseID}
}

// promptDcloseWriteMarker is the §48 marker write: the session state with the close's target and
// the successor it chose recorded, so a retry replays the same close. It is written over the state
// the session lock found, which is the state the caller passed.
func promptDcloseWriteMarker(cwd string, held state.State, closePhaseID string, nextWorkPhaseID *string) error {
	if held.CheckEpoch == nil {
		return errors.New("the close has no check epoch to record")
	}
	next := held
	next.DcloseRecovery = promptDcloseMarker(held, closePhaseID, nextWorkPhaseID)
	return state.WriteState(cwd, next)
}

// promptDcloseHasGoalplanRow is hasGoalplanRow (:628-632): a row of the bound plan's ledger with
// this event and detail. An absent file has no row; a line that is not a JSON object matches
// nothing, where the oracle's JSON.parse throws and the same D request can never finish.
func promptDcloseHasGoalplanRow(cwd, slug, event, detail string) bool {
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		return false
	}
	return promptDcloseAnyRow(filepath.Join(dir, goalplan.GoalplanLedgerFile), func(row map[string]any) bool {
		gotEvent, _ := row["event"].(string)
		gotDetail, _ := row["detail"].(string)
		return gotEvent == event && gotDetail == detail
	})
}

// promptDcloseHasPabcdCloseRow is hasPabcdCloseRow (:634-645): the PABCD close row of this
// session, this check cycle and this closed work phase, where the closed phase is JSON null when
// the id is empty. A row of a damaged line matches nothing, as in promptDcloseHasGoalplanRow.
func promptDcloseHasPabcdCloseRow(cwd, sessionID string, checkEpoch *string, closedWorkPhaseID string) bool {
	return promptDcloseAnyRow(filepath.Join(cwd, crwdir.DirName, state.LedgerFile), func(row map[string]any) bool {
		gotSession, _ := row["sessionId"].(string)
		gotFrom, _ := row["from"].(string)
		gotTo, _ := row["to"].(string)
		gotReason, _ := row["reason"].(string)
		if gotSession != sessionID || gotFrom != "C" || gotTo != "IDLE" || gotReason != "done" {
			return false
		}
		if !promptDcloseSameJSONString(row["checkEpoch"], checkEpoch) {
			return false
		}
		if closedWorkPhaseID == "" {
			return row["closedWorkPhaseId"] == nil
		}
		gotClosed, _ := row["closedWorkPhaseId"].(string)
		return gotClosed == closedWorkPhaseID
	})
}

// promptDcloseAnyRow reads the JSON-object lines of a JSONL file and reports whether any of them
// satisfies match. A file that is not there has no row; a line that is not a JSON object, a blank
// line and an unreadable file match nothing.
func promptDcloseAnyRow(path string, match func(map[string]any) bool) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range text.SplitLines(string(data)) {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) != nil || row == nil {
			continue
		}
		if match(row) {
			return true
		}
	}
	return false
}

// promptDcloseSameJSONString is the oracle's row.checkEpoch === checkEpoch: JSON null and an
// absent key are the same null, and a stored value must be the same string.
func promptDcloseSameJSONString(stored any, want *string) bool {
	if stored == nil {
		return want == nil
	}
	got, ok := stored.(string)
	return ok && want != nil && got == *want
}

// promptDclosePendingText is the oracle's pending.map((t) => t.id (t.title)).join("; ").
func promptDclosePendingText(pending []goalplan.GoalplanTask) string {
	open := make([]string, 0, len(pending))
	for _, task := range pending {
		open = append(open, task.ID+" ("+task.Title+")")
	}
	return strings.Join(open, "; ")
}

// promptDcloseNoActive is the no_active refusal (:1193-1206): the dependency deadlock diagnosis
// when there is one, the plain no-active-work-phase text otherwise.
func promptDcloseNoActive(slug string, plan *goalplan.Goalplan) string {
	detail := "the bound goalplan " + promptDcloseQuote(slug) + " has no active work-phase to close"
	if deadlock := goalplan.DetectDependencyDeadlock(plan); deadlock != nil {
		detail = "Dependency deadlock: " + strings.Join(deadlock.Reasons, "; ")
	}
	return detail + " (CYCLE-COMPLETION-01). Nothing was written."
}

// promptDcloseTestReceipt is the oracle's command.attest?.testReceiptPath, empty when there is no
// attestation.
func promptDcloseTestReceipt(command *fsm.OrchestrateCommand) string {
	if command == nil || command.Attest == nil {
		return ""
	}
	return command.Attest.TestReceiptPath
}

// promptDcloseBindings is the entrySource / planBinding / keepBinding triple the oracle computes
// once for both the bound D-close write and the ordinary edge (:1290-1296). A D-close never enters
// B and never leaves P, so all three are empty here; they are ported because the write they feed is
// shared with the forward edges, and leaving them out would make this write a different one.
func promptDcloseBindings(p PromptSubmitPayload, held state.State, result fsm.ApplyResult, command *fsm.OrchestrateCommand) (*state.SourceIdentity, *promptOrchestrateBinding, bool) {
	var entrySource *state.SourceIdentity
	if result.State != nil && result.State.Phase == state.PhaseB {
		if id, err := session.Capture(p.Cwd, p.SessionID, session.CaptureOptions{ExcludeStateArtifacts: promptOrchestrateBoolPtr(true)}); err == nil {
			stored := promptOrchestrateStoredIdentity(id)
			entrySource = &stored
		}
	}
	var binding *promptOrchestrateBinding
	if held.Phase == state.PhaseP && result.State != nil && result.State.Phase == state.PhaseA {
		binding = promptOrchestrateChatPlanBinding(p.Cwd, held.Slug, command.Attest)
	}
	return entrySource, binding, result.State != nil && result.State.Phase == state.PhaseA && held.Phase == state.PhaseA
}

// promptDclosePlanBindingOf is the binding half of the write: the binding this edge minted, or the
// session's own when the write stays in A, or neither.
func promptDclosePlanBindingOf(binding *promptOrchestrateBinding, held state.State, keepBinding bool) (*string, *string) {
	if binding != nil {
		unit, epoch := binding.unit, binding.epoch
		return &unit, &epoch
	}
	if keepBinding {
		return held.PlanUnit, held.PlanEpoch
	}
	return nil, nil
}

// promptDcloseInjectedTurns is the oracle's turn ? appendTurn(state.injectedTurns, turn) :
// state.injectedTurns: a turnless payload keeps the list it found.
func promptDcloseInjectedTurns(turn string, turns []string) []string {
	if turn == "" {
		return turns
	}
	return promptSubmitAppendTurn(turns, turn)
}

// promptDcloseSeam runs one commit seam when the caller supplied one.
func promptDcloseSeam(seams *promptDcloseSeams, pick func(*promptDcloseSeams) func()) {
	if seams == nil {
		return
	}
	if fn := pick(seams); fn != nil {
		fn()
	}
}

// promptDcloseTimestamp is new Date().toISOString() as the rows print it.
func promptDcloseTimestamp() string { return time.Now().UTC().Format(promptDcloseTimestampLayout) }

// promptDcloseString is a *string as the oracle's template literal prints it: null and undefined
// both read as the empty text.
func promptDcloseString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// promptDcloseCount is a count as the oracle's template literal prints it.
func promptDcloseCount(n int) string { return strconv.Itoa(n) }

// promptDcloseQuote is a value between the double quotes the oracle's refusals use.
func promptDcloseQuote(value string) string { return "\"" + value + "\"" }

// promptDcloseResetCommand is the oracle's backticked reset instruction, resolved at emission so no
// constant carries a command: `cxc orchestrate reset --session <id>` after the name substitution.
func promptDcloseResetCommand(sessionID string) string {
	return promptDcloseTick + "crw pabcd orchestrate reset --session " + sessionID + promptDcloseTick
}

// promptDcloseStateRefusal is the text for a session state the close will not rewrite: the issue's
// own wording for the data-loss guard, which also covers a rewrite the reader would not keep whole.
func promptDcloseStateRefusal() string {
	return "[crw \u2014 D-close was not applied: the session state cannot be rewritten without losing a stored record. Nothing was written.]"
}

// promptDcloseStateMovedRefusal is the same refusal for the other half of the guard: a
// participating writer changed the session between the leading section's read and this lock, so the
// close cannot be applied to the state it judged.
func promptDcloseStateMovedRefusal() string {
	return "[crw \u2014 D-close was not applied: the session state changed while this close was being applied. Nothing was written.]"
}

// promptDcloseNotApplied is the lock-busy text (:1268-1274).
func promptDcloseNotApplied(reason string) string {
	return "[crw \u2014 D-close was not applied: " + reason + " The phase and goalplan ledger were not changed.]"
}

// promptDcloseGoalplanUnreadable is the unreadable-goalplan text (:1275-1281).
func promptDcloseGoalplanUnreadable(reason string) string {
	return "[crw \u2014 D-close was not applied: the bound goalplan could not be read (" + reason + "). Nothing was written.]"
}

// promptDcloseFinalizePending is the finalization-pending text (:1347-1354).
func promptDcloseFinalizePending(reason string) string {
	return "[crw \u2014 D-close was committed and the cycle is closed, but ledger/marker finalization is pending: " + reason + " The recovery marker is still on the session, so running the same D request again finishes the cleanup.]"
}
