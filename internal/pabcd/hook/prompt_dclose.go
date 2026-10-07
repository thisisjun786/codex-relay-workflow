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
	"io/fs"
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
	// writeMarker and writePlan are the two plan-stage writes the close makes before its rows.
	// nil means the real function, so a production run holds neither and a test can make one
	// report a post-rename failure without a package-level variable (CRW-869, finding 2).
	writeMarker func(cwd string, held state.State, closePhaseID string, nextWorkPhaseID *string) error
	writePlan   func(cwd string, plan *goalplan.Goalplan) error
}

// promptDcloseOutcome is what one bound close did while it held the session lock: the refusal or
// the finalization-pending text to inject, or neither when the cycle closed.
type promptDcloseOutcome struct {
	refusal string
	pending string
	// warning is a durability warning on a close that did happen: the state reached its final path
	// but the directory could not be synced. It is reported on the success answer, as the CLI
	// writers report one (CRW-744/793).
	warning string
}

// promptDclosePlanOutcome is what the first goalplan lock answered: the text to inject when it
// refused, the all-done discriminant, the successor a fresh close recorded in the marker, and the
// goalplan ledger rows the close still owes. The rows travel back rather than being appended inside
// the lock, because the session state is published before them: a state the close cannot write must
// not leave a ledger row describing a transition that did not happen.
type promptDclosePlanOutcome struct {
	output     string
	allDone    bool
	markerNext *string
	rows       []promptDcloseGoalplanRow
	// published records which artifacts this close actually wrote - the recovery marker and the
	// goalplan - beside their durability warnings, so a later refusal names each of them whether or
	// not a directory-sync warning was collected for it (CRW-930, c6: the promise CRW-869 left
	// open). The close goes on; the caller reports the warnings on the answer (CRW-869, finding 2).
	published promptDclosePublishedArtifacts
}

// promptDclosePublication is one artifact a close wrote: whether it reached its final path, and the
// durability warning when the step after its rename failed. A write that never reached its final
// path leaves landed false and wrote nothing.
type promptDclosePublication struct {
	landed  bool
	warning string
}

// promptDclosePublishedArtifacts is what one bound close published: its recovery marker and its
// goalplan. A refusal after either one names it instead of claiming nothing was written, because an
// artifact that published cleanly carries no warning to be recovered from (CRW-930, c6).
// "Nothing was written." stays only when neither landed and no other durability warning is
// outstanding.
type promptDclosePublishedArtifacts struct {
	marker promptDclosePublication
	plan   promptDclosePublication
}

// sentences name every artifact this close published, one sentence per artifact and in write order:
// the fixed sentence when it published cleanly, its own durability line when the step after its
// rename failed (that line already says which file it is). Either way the artifact is named, so the
// refusal never claims nothing was written. An artifact that never landed contributes nothing, so an
// empty answer means this close published nothing and the bare refusal is exact (CRW-930, c6).
func (p promptDclosePublishedArtifacts) sentences() []string {
	out := []string{}
	if p.marker.landed {
		out = append(out, p.marker.sentence(promptDcloseMarkerPublishedSentence()))
	}
	if p.plan.landed {
		out = append(out, p.plan.sentence(promptDcloseGoalplanPublishedSentence()))
	}
	return out
}

// sentence is one publication's naming sentence: its durability line when the write published and
// then failed a step after the rename, the fixed sentence when the write was clean.
func (p promptDclosePublication) sentence(clean string) string {
	if p.warning != "" {
		return p.warning
	}
	return clean
}

// names reports whether warning is already the sentence sentences() emits for one of the artifacts
// this close published, so a refusal that carries both names each artifact exactly once (CRW-930,
// c6: the warning is kept, not repeated).
func (p promptDclosePublishedArtifacts) names(warning string) bool {
	if warning == "" {
		return false
	}
	return (p.marker.landed && p.marker.warning == warning) || (p.plan.landed && p.plan.warning == warning)
}

// warningLines are the durability lines of the artifacts that published and then failed a step
// after the rename, in write order. A clean publication contributes nothing.
func (p promptDclosePublishedArtifacts) warningLines() []string {
	lines := []string{}
	if p.marker.warning != "" {
		lines = append(lines, p.marker.warning)
	}
	if p.plan.warning != "" {
		lines = append(lines, p.plan.warning)
	}
	return lines
}

// promptDcloseMarkerPublishedSentence is the fixed sentence naming a recovery marker this close
// published cleanly, so a later refusal of the same close names it instead of denying it
// (CRW-930, c6).
func promptDcloseMarkerPublishedSentence() string {
	return "the recovery marker was published."
}

// promptDcloseGoalplanPublishedSentence is the fixed sentence naming a goalplan this close
// published cleanly, so a later refusal of the same close names it instead of denying it
// (CRW-930, c6).
func promptDcloseGoalplanPublishedSentence() string {
	return "the goalplan was published."
}

// promptDcloseGoalplanRow is one goalplan ledger row the close owes, in the oracle's order.
type promptDcloseGoalplanRow struct {
	event  goalplan.GoalplanLedgerEvent
	detail string
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

	// The transition the request asks for, judged on the state the leading section read. This copy
	// fixes the oracle's refusal order - an illegal adjacency first, the receipt gate second - and is
	// not the state that gets written: the close applies the same transition again to the state the
	// session lock finds, so a participating writer's update survives the write.
	pre := promptDcloseTransition(current, command, recovering)
	if !pre.OK {
		return promptOrchestrateRefusal(pre.Reason), true
	}

	outcome := promptDcloseOutcome{}
	err := lock(p.Cwd, p.SessionID, func() error {
		held, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
		if unreadable {
			// The leading snapshot already matched this close's marker, so a matching retry may
			// already have published its marker and committed its goalplan before this stricter
			// reread failed. The refusal names them instead of denying them (CRW-930, d1).
			outcome.refusal = promptDclosePartialRefusal(promptDcloseStateRefusal(),
				promptDcloseRecoveryPublishedAt(p.Cwd, current.Slug, closePhaseID, current, recovering), nil)
			return nil
		}
		// The close is judged on the state the lock found, not on the one the leading section read: a
		// session that moved - a phase change, a rebind, another close's marker - writes nothing. The
		// check epoch is part of that judgement (found by the Codex review of this pull request): a
		// session that rotated through a reset and re-entered C carries a new epoch, and a receipt
		// validated against the old one must not close the new cycle.
		if held.Phase != current.Phase || held.Slug != current.Slug ||
			!promptDcloseSameOptionalText(held.CheckEpoch, current.CheckEpoch) ||
			state.MatchesDcloseRecovery(held, closePhaseID) != recovering {
			outcome.refusal = promptDcloseStateMovedRefusal()
			return nil
		}
		// An outstanding marker this request does not match belongs to a close that is still
		// half-finished. A fresh close would overwrite that marker, and the rows the close it
		// describes still owes could then never be written, which loses the session's own record of
		// the half-finished close (found by the Codex review of this pull request). The oracle
		// overwrites it; the port fixes it as the data-loss class the parity revision of 2026-10-03
		// fixes during the port.
		if !recovering && held.DcloseRecovery != nil {
			outcome.refusal = promptDcloseOutstandingMarkerRefusal(p.SessionID, held.DcloseRecovery.ClosedWorkPhaseID)
			return nil
		}
		// CHECK-BINDING-01 (075): the same receipt requirement as the CLI, checked on the state the
		// lock found so a chat D-close cannot be the way around it. A marker-matched retry already
		// spent its receipt in the first attempt, and re-requiring it would refuse a repair for a
		// gate it cannot satisfy twice.
		if !recovering {
			receiptCheck := gate.ValidateCheckReceipt(held, p.SessionID, promptDcloseTestReceipt(command), p.Cwd)
			if !receiptCheck.OK {
				outcome.refusal = promptOrchestrateRefusal(receiptCheck.Reason + " Nothing was written.")
				return nil
			}
		}
		// The transition is re-applied to the locked state, so the write carries every field that
		// state holds. The leading section already accepted it on a state of the same phase, so a
		// refusal here means the session moved between the two reads.
		fresh := promptDcloseTransition(held, command, recovering)
		if !fresh.OK || fresh.State == nil {
			outcome.refusal = promptDcloseStateMovedRefusal()
			return nil
		}
		// The rewrite guard runs where a write is about to happen, so a refusal the oracle answers
		// earlier - the legacy marker, the integrity check, an empty plan, an open task - keeps the
		// oracle's own text, and a close that writes nothing never consults the guard.
		guard := func() string {
			if !promptSubmitRewritable(p.Cwd, p.SessionID, held) {
				return promptDcloseStateRefusal()
			}
			return ""
		}
		outcome = promptDcloseClose(p, held, turn, closePhaseID, recovering, fresh, command, seams, guard)
		return nil
	})
	if err != nil {
		// The oracle holds no session lock; a lock that stays busy answers the D-close text with the
		// busy reason and writes nothing. A matching retry's first attempt may already have published
		// its marker and committed its goalplan, so the answer names them too (CRW-930, d3).
		published := promptDclosePublishedArtifacts{}
		if recovering {
			published.marker = promptDclosePublication{landed: true}
			published.plan = promptDcloseRecoveryCommittedPublication(goalplan.ReadGoalplan(p.Cwd, current.Slug), closePhaseID, current, recovering)
		}
		return promptDcloseRefusalNaming(promptDcloseNotApplied(err.Error()), published), true
	}
	if outcome.refusal != "" {
		return outcome.refusal, true
	}
	if outcome.pending != "" {
		// The close is closed but its finalization is pending, and a marker or plan write that
		// published and then failed the directory sync may also have a durability line to report
		// (CRW-869, finding 2). The pending text keeps its promise; the warning rides after it.
		if outcome.warning != "" {
			return outcome.pending + "\n" + outcome.warning, true
		}
		return outcome.pending, true
	}
	// done: the chat D-close. Inject the DONE summary directive this turn; the resting state is
	// already IDLE, so the footer surfaces IDLE. A durability warning on a close that did happen is
	// reported on the success answer, as the CLI writers report one (CRW-744/793).
	answer := WithFooter(PhaseDirective(state.PhaseD, nil), state.PhaseIdle)
	if outcome.warning != "" {
		answer += "\n" + outcome.warning
	}
	return answer, true
}

// promptDcloseTransition applies the D-close this request asks for to s: the close the marker
// already describes for a marker-matched retry, the human free pass C>D otherwise.
func promptDcloseTransition(s state.State, command *fsm.OrchestrateCommand, recovering bool) fsm.ApplyResult {
	if recovering {
		return promptDcloseRecoveredTransition(s, command)
	}
	return fsm.ApplyHumanTransition(s, fsm.VerbD, command.Attest)
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

// promptDcloseClose runs the bound close on the state the session lock found. It is the body of the
// oracle's handler from the first goalplan lock to the finalization.
//
// Commit order. The oracle appends its goalplan rows and its PABCD close row inside the goalplan
// lock, before the resting state is written. A state the close then refuses to write, or a write
// that never reaches the file, would leave a ledger row describing a close the session never made
// (found by the Devin review and the Codex review of this pull request). The port keeps the
// oracle's row order and text and appends them after the state publication, which is the order
// every other writer of this repository uses (CRW-744/793/811). The two rows sets are appended
// inside the second goalplan write lock, so the goalplan rows obey goalplan/write.go's rule that an
// existing-plan caller appends under that lock, and a session's own row set is one critical section
// with the marker cleanup. The session lock the caller holds serialises the whole sequence, so no
// second close can observe the rows half-written.
func promptDcloseClose(p PromptSubmitPayload, held state.State, turn, closePhaseID string, recovering bool, result fsm.ApplyResult, command *fsm.OrchestrateCommand, seams *promptDcloseSeams, guard func() string) promptDcloseOutcome {
	slug := held.Slug

	// The marker and the plan commit are one critical section, and the integrity check runs inside
	// it, before either write.
	locked, err := goalplan.WithGoalplanWriteLock(p.Cwd, slug, func(plan *goalplan.Goalplan) (promptDclosePlanOutcome, error) {
		return promptDclosePlanWork(p, held, plan, closePhaseID, recovering, command, seams, guard)
	}, nil)
	if err != nil {
		return promptDcloseOutcome{refusal: promptDclosePartialRefusal(
			promptDcloseGoalplanUnreadable(err.Error()),
			promptDcloseRecoveryPublishedAt(p.Cwd, slug, closePhaseID, held, recovering), nil)}
	}
	switch locked.Kind {
	case "locked":
		return promptDcloseOutcome{refusal: promptDcloseRefusalNaming(promptDcloseNotApplied(locked.Reason),
			promptDcloseRecoveryPublishedAt(p.Cwd, slug, closePhaseID, held, recovering))}
	case "unreadable":
		// "unreadable" also covers a plan that read cleanly and was refused for what a revival would
		// lose, so the plan is read here too: its commit is still on disk and the refusal must name it
		// (CRW-930, d2).
		return promptDcloseOutcome{refusal: promptDclosePartialRefusal(
			promptDcloseGoalplanUnreadable(locked.Reason),
			promptDcloseRecoveryPublishedAt(p.Cwd, slug, closePhaseID, held, recovering), nil)}
	}
	if locked.Value == nil {
		return promptDcloseOutcome{refusal: promptDclosePartialRefusal(
			promptDcloseGoalplanUnreadable("the bound goalplan could not be read"),
			promptDcloseRecoveryPublishedAt(p.Cwd, slug, closePhaseID, held, recovering), nil)}
	}
	if locked.Value.output != "" {
		return promptDcloseOutcome{refusal: locked.Value.output}
	}
	plan := locked.Value
	// published is what the first lock wrote - the recovery marker and the goalplan - so every
	// refusal below names each artifact this close actually published, whether or not it carried a
	// directory-sync warning (CRW-930, c6).
	published := plan.published
	// warnings collects every durability line of a write that published its artifact and then
	// failed a step after the rename: the marker and plan writes the first lock made (CRW-869,
	// finding 2), the resting state write below and the marker cleanup. A slice rather than one
	// string, so a state warning is not silently overwritten by the cleanup warning.
	warnings := append([]string{}, published.warningLines()...)

	// §40 Z3: the bound D-close owns its own state write. The write keeps every field the ordinary
	// D-close write sets - dropping injectedTurns would break same-turn dedup and dropping the
	// stopBlock reset would leave C stagnation state on an IDLE session - and layers the recovery
	// fields on top. ClearedIdle is applied to the state the session lock found, not to the one the
	// leading section read, so a field a participating writer landed in between survives the write
	// (found by the Codex review of this pull request).
	next := fsm.ClearedIdle(held)
	next.OrchestrationActive, next.LastInjectedPhase = false, nil
	next.InjectedTurns = promptDcloseInjectedTurns(turn, held.InjectedTurns)
	next.StopBlockPhase, next.StopBlockWorkPhaseID, next.StopBlockCount = nil, nil, 0
	switch {
	case plan.allDone:
		// §40 Z2: an all-done close leaves no marker to clear.
		next.CheckEpoch, next.DcloseRecovery = nil, nil
	case recovering:
		next.CheckEpoch, next.DcloseRecovery = held.CheckEpoch, held.DcloseRecovery
	default:
		next.CheckEpoch = held.CheckEpoch
		next.DcloseRecovery = promptDcloseMarker(held, closePhaseID, plan.markerNext)
	}
	if refusal := guard(); refusal != "" {
		// The guard re-reads the session file, so it can fail on a read error even though the
		// identical guard accepted before the marker write: the marker and the plan may already be on
		// disk. An answer that denied that would hide a partial commit the operator has to know about,
		// so the refusal names every artifact this close published and keeps their warnings
		// (CRW-930, c6). With nothing published and no warning the bare refusal is unchanged.
		return promptDcloseOutcome{refusal: promptDclosePartialRefusal(refusal, published, warnings)}
	}
	landed, stateWarning := promptDcloseWriteLanded(state.WriteState(p.Cwd, next))
	if !landed {
		// An earlier write of this close may already have published its artifact (the marker or the
		// plan), so the refusal must name it rather than deny that anything was written (CRW-869,
		// the review finding on the mixed-failure path; CRW-930, c6, extended it to a clean
		// publication).
		return promptDcloseOutcome{refusal: promptDclosePartialRefusal(promptDcloseStateRefusal(), published, warnings)}
	}
	if stateWarning != "" {
		warnings = append(warnings, stateWarning)
	}
	promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterStateWrite })

	// §40 Z2: an all-done close leaves no marker for a failed second lock to resume, so it writes no
	// recovery marker and has nothing to clean up; its close row carries a null closed work phase.
	closeCheckEpoch, closedWorkPhaseID := held.CheckEpoch, closePhaseID
	if plan.allDone {
		closedWorkPhaseID = ""
	}
	// Both row sets land inside the second goalplan lock, in the oracle's order: the two goalplan
	// rows first, then the PABCD close row, then the marker cleanup. The lock is what makes the
	// dedup reads sound - goalplan/write.go requires an existing-plan caller to append under it - so
	// two sessions bound to the same plan (a fresh close racing a recovery, or two recoveries) cannot
	// both observe a row absent and append it twice (finding (a) of generation 2 of CRW-797). The
	// session lock the caller holds is a different lock and serialises one session only.
	rowsErr := error(nil)
	finalize, finalizeErr := goalplan.WithGoalplanWriteLock(p.Cwd, slug, func(*goalplan.Goalplan) (struct{}, error) {
		for _, row := range plan.rows {
			// CRW-869 finding 1: a row read that failed with anything but ENOENT is unreadable, not
			// absent. Believing it would append the row again, so the close stops here with the
			// ledger read as the reason; the caller answers the pending text (or the all-done
			// warning) and the marker is kept, because the cleanup below never runs.
			present, readErr := promptDcloseHasGoalplanRow(p.Cwd, slug, string(row.event), row.detail)
			if readErr != nil {
				return struct{}{}, errors.New("the goalplan ledger could not be read: " + readErr.Error())
			}
			if present {
				continue
			}
			if rowErr := goalplan.AppendGoalplanLedger(p.Cwd, slug, goalplan.GoalplanLedgerEntry{
				Ts: promptDcloseTimestamp(), Slug: slug, Event: row.event, Detail: row.detail,
			}); rowErr != nil {
				// The resting state is already published, so this is not a refusal that wrote nothing.
				// Stop before the close row and the marker cleanup: the marker is the same-D retry's
				// only handle on this half-finished finalization, so leaving it in place is what makes
				// the pending text true (finding (b) of generation 2 of CRW-797).
				rowsErr = rowErr
				return struct{}{}, nil
			}
		}
		if result.Ledger != nil {
			present, readErr := promptDcloseHasPabcdCloseRow(p.Cwd, p.SessionID, closeCheckEpoch, closedWorkPhaseID)
			if readErr != nil {
				return struct{}{}, errors.New("the PABCD ledger could not be read: " + readErr.Error())
			}
			if !present {
				row := *result.Ledger
				row.Close = &state.CloseKey{CheckEpoch: closeCheckEpoch}
				if closedWorkPhaseID != "" {
					row.Close.ClosedWorkPhaseID = &closedWorkPhaseID
				}
				// The hook's close rows spread the transition row, evidence included, before the close key
				// (hook.ts:1147 and :1335 through orchestrate-apply.ts:111-118).
				row.EvidenceAfterReason = true
				if rowErr := state.AppendLedger(p.Cwd, row); rowErr != nil {
					return struct{}{}, rowErr
				}
				promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterPabcdLedgerAppend })
			}
		}
		if plan.allDone {
			return struct{}{}, nil
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
			// A rename that landed but whose directory sync failed is a committed cleanup with a
			// durability warning: the marker is gone from every reader's view, so a retry could no
			// longer match it (found by the Codex review of this pull request).
			cleanupLanded, cleanupWarning := promptDcloseWriteLanded(state.WriteState(p.Cwd, current))
			if !cleanupLanded {
				return struct{}{}, errors.New("the recovery marker could not be cleared")
			}
			if cleanupWarning != "" {
				warnings = append(warnings, cleanupWarning)
			}
		}
		return struct{}{}, nil
	}, nil)
	if finalizeErr != nil || finalize.Kind != "ok" {
		reason := finalize.Reason
		if finalizeErr != nil {
			reason = finalizeErr.Error()
		}
		if plan.allDone {
			// §40 Z2: an all-done close publishes a resting state with no marker, so there is no
			// same-D retry to promise and the pending text would be a lie - a retry is refused as
			// an illegal IDLE->D transition. The close did happen and only its ledger row is
			// missing, which is the durability warning the CLI writers use (CRW-744/793/811;
			// finding (c) of generation 2 of CRW-797).
			warnings = append(warnings, promptDcloseLedgerWarning(reason))
			return promptDcloseOutcome{warning: strings.Join(warnings, "\n")}
		}
		return promptDcloseOutcome{pending: promptDcloseFinalizePending(reason), warning: strings.Join(warnings, "\n")}
	}
	if rowsErr != nil {
		// The resting state and the recovery marker are already on disk when the goalplan rows are
		// appended, so a row that could not be written is not a close that wrote nothing (finding
		// (b) of generation 2 of CRW-797). The marker is still there, so the pending text is exact.
		return promptDcloseOutcome{pending: promptDcloseFinalizePending("the goalplan ledger row could not be written: " + rowsErr.Error()), warning: strings.Join(warnings, "\n")}
	}
	return promptDcloseOutcome{warning: strings.Join(warnings, "\n")}
}

// promptDclosePlanWork is the body of the first goalplan lock (:956-1266). It answers the text to
// inject when it refuses, or the all-done discriminant, the successor a fresh close recorded in the
// marker, and the goalplan ledger rows the close still owes. The rows travel back rather than being
// appended here, because the session state is published before them.
func promptDclosePlanWork(p PromptSubmitPayload, held state.State, plan *goalplan.Goalplan, closePhaseID string, recovering bool, command *fsm.OrchestrateCommand, seams *promptDcloseSeams, guard func() string) (promptDclosePlanOutcome, error) {
	slug := held.Slug
	// CRW-869 finding 2: the marker and the plan writes can each publish their artifact and then fail
	// a step after the rename. The seams default to the real functions, so a production run holds
	// neither; warnings collects the durability line of each such write.
	writeMarker := promptDcloseWriteMarker
	if seams != nil && seams.writeMarker != nil {
		writeMarker = seams.writeMarker
	}
	writePlan := goalplan.WriteGoalplan
	if seams != nil && seams.writePlan != nil {
		writePlan = seams.writePlan
	}
	// published records which artifacts this close wrote as it writes them, so a later refusal names
	// each of them whether or not a directory-sync warning was collected for it (CRW-930, c6). A
	// recovery retry continues a close whose marker is already on the session, so that marker counts
	// as published too.
	published := promptDclosePublishedArtifacts{}
	if recovering {
		published.marker = promptDclosePublication{landed: true}
	}

	// §5: integrity is checked inside the lock, before marker or any write.
	integrityReasons := goalplan.GoalplanDefinitionIntegrityReasons(plan)
	integrityReasons = append(integrityReasons, goalplan.GoalplanDependencyCompletionReasons(plan)...)
	if len(integrityReasons) > 0 {
		// The integrity check runs before the recovery accounting, so a retry reaches this refusal
		// without having looked at what its first attempt published. The plan is structurally readable
		// here (its definition is what failed), so the commit's own settled shape still decides whether
		// the goalplan counts as published (CRW-930, d2).
		published.plan = promptDcloseRecoveryCommittedPublication(plan, closePhaseID, held, recovering)
		return promptDclosePlanOutcome{output: promptDclosePartialRefusal(
			promptOrchestrateRefusal("invalid goalplan: "+strings.Join(integrityReasons, "; ")+". Nothing was written."), published, nil)}, nil
	}
	if len(plan.WorkPhases) == 0 {
		// A plan with no work-phase cannot carry a commit of this close, so the shape is false and
		// only the inherited marker is named; the call keeps the same accounting as above for the
		// reader (CRW-930, d2).
		published.plan = promptDcloseRecoveryCommittedPublication(plan, closePhaseID, held, recovering)
		return promptDclosePlanOutcome{output: promptDclosePartialRefusal(
			promptOrchestrateRefusal("the bound goalplan "+promptDcloseQuote(slug)+" has no active work-phase to close (CYCLE-COMPLETION-01). Nothing was written."), published, nil)}, nil
	}

	// §39 Y2: recovery is checked BEFORE all-done, in the same order as the CLI path. Crashing
	// right after the final work-phase commit leaves an all-done plan; checking all-done first
	// would consume that retry as a plain cycle close and record closedWorkPhaseId: null, dropping
	// the marker's target.
	closeResult := promptDcloseCloseResult{}
	writeClosedPlan := false

	if recovering {
		out, refusal, refused := promptDcloseRecoveryClose(p, held, plan, closePhaseID)
		// A retry continues the close the first attempt started, so the marker it inherits is an
		// artifact of this close, and so is the goalplan when that attempt's commit is on disk. Both
		// count as published for every refusal here and below (CRW-930, d1/d2).
		if out.planCommitted {
			published.plan = promptDclosePublication{landed: true}
		}
		if refused {
			return promptDclosePlanOutcome{output: promptDclosePartialRefusal(refusal, published, nil)}, nil
		}
		closeResult, writeClosedPlan = out.result, out.writePlan
	} else {
		// §35-3: a non-empty all-done plan closes only the cycle. It needs no target and writes no
		// recovery marker or goalplan row. This sits inside the non-recovery branch so a matching
		// marker always wins.
		if promptDcloseAllDone(plan) {
			return promptDclosePlanOutcome{allDone: true}, nil
		}
		// §35-5: target validation follows empty-plan, all-done and recovery.
		if closePhaseID == "" {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("bound chat D-close requires attest.workPhaseId. Nothing was written.")}, nil
		}
		if promptDcloseFindWorkPhase(plan, closePhaseID) == nil {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("work-phase " + closePhaseID + " is not in the bound goalplan. Nothing was written.")}, nil
		}
		advanced := goalplan.AdvanceWorkPhase(plan)
		switch advanced.Kind {
		case goalplan.WorkPhaseAdvanceTasksPending:
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("work-phase " + promptDcloseString(advanced.WorkPhaseID) + " still has " + promptDcloseCount(len(advanced.Pending)) + " open task(s), so this cycle cannot close (CYCLE-COMPLETION-01): " + promptDclosePendingText(advanced.Pending) + ". Nothing was written.")}, nil
		case goalplan.WorkPhaseAdvanceNoActive:
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal(promptDcloseNoActive(slug, plan))}, nil
		}
		closeResult = promptDcloseCloseResult{kind: "ok", closedID: promptDcloseString(advanced.ClosedID), plan: advanced.Plan}
		writeClosedPlan = true
	}

	// The shared tail of both branches: the target check, the C epoch requirement, the marker and
	// the plan commit. The ledger rows travel back to the caller.
	if !recovering && closeResult.closedID != closePhaseID {
		return promptDclosePlanOutcome{output: promptOrchestrateRefusal("fixed close target " + closePhaseID + " does not match active work-phase " + closeResult.closedID + ". Nothing was written.")}, nil
	}
	markerNext := (*string)(nil)
	if !recovering {
		if held.Phase != state.PhaseC || held.CheckEpoch == nil {
			return promptDclosePlanOutcome{output: promptOrchestrateRefusal("current C check epoch is required. Nothing was written.")}, nil
		}
		// §48: the successor this close chose is recorded before the plan commit, so a retry never
		// has to infer it from the file.
		markerNext = closeResult.plan.ActiveWorkPhaseID
		if refusal := guard(); refusal != "" {
			return promptDclosePlanOutcome{output: refusal}, nil
		}
		markerLanded, markerWarning := promptDcloseWriteLanded(writeMarker(p.Cwd, held, closePhaseID, markerNext))
		if !markerLanded {
			// The marker never reached its final path, so this close published nothing and the bare
			// refusal is exact.
			return promptDclosePlanOutcome{output: promptDcloseStateRefusal()}, nil
		}
		published.marker = promptDclosePublication{landed: true, warning: markerWarning}
		promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterRecoveryMarkerWrite })
	}
	if writeClosedPlan {
		planErr := writePlan(p.Cwd, closeResult.plan)
		switch {
		case planErr == nil:
			published.plan = promptDclosePublication{landed: true}
		case state.Published(planErr):
			// The artifact is the goalplan, not the session state, so the warning names the file whose
			// durability is in question while keeping the sentence shape of the state warning.
			published.plan = promptDclosePublication{landed: true, warning: promptDcloseGoalplanPublishedWarning(planErr)}
		default:
			// The plan write did not land, but the marker write before it did; the refusal names that
			// partial commit instead of denying it, whether or not the marker carried a warning
			// (CRW-869, the review finding on the mixed-failure path; CRW-930, c6, which extended it to
			// a clean publication).
			return promptDclosePlanOutcome{output: promptDclosePartialRefusal(
				promptOrchestrateRefusal("the goalplan could not be written: "+planErr.Error()+" Nothing was written."),
				published, published.warningLines())}, nil
		}
		promptDcloseSeam(seams, func(s *promptDcloseSeams) func() { return s.afterGoalplanCommit })
	}

	rows := []promptDcloseGoalplanRow{{event: goalplan.EventWorkphaseDone, detail: "closed " + closePhaseID}}
	// §52: a resume names the marker successor, a fresh close names the cursor it just computed.
	startedID := ""
	if recovering {
		startedID = promptDcloseString(held.DcloseRecovery.NextWorkPhaseID)
	} else {
		startedID = promptDcloseString(closeResult.plan.ActiveWorkPhaseID)
	}
	if startedID != "" {
		rows = append(rows, promptDcloseGoalplanRow{event: goalplan.EventWorkphaseStarted, detail: "started " + startedID})
	}
	return promptDclosePlanOutcome{markerNext: markerNext, rows: rows, published: published}, nil
}

// promptDcloseWriteLanded says whether a state write reached the file. state.WriteState reports a
// *state.PublishedError when the rename succeeded but the directory could not be synced: the new
// state is visible to every reader, so the write landed and only its durability is in question
// (the CRW-744/793 rule the CLI writers follow). The second answer is that warning, empty when
// there is none. A write that never reached its final path did not land.
func promptDcloseWriteLanded(err error) (bool, string) {
	switch {
	case err == nil:
		return true, ""
	case state.Published(err):
		return true, "the session state was published but its directory could not be synced: " + err.Error()
	}
	return false, ""
}

// promptDcloseGoalplanPublishedWarning is the durability line for a plan write that published the
// goalplan and then failed a step after the rename. It keeps promptDcloseWriteLanded's sentence
// shape but names the goalplan, which is a different file in a different directory from the session
// state, so the operator's verification target is right (CRW-869, finding 2).
func promptDcloseGoalplanPublishedWarning(err error) string {
	return "the goalplan was published but its directory could not be synced: " + err.Error()
}

// promptDclosePartialRefusal is a refusal whose trailing "Nothing was written." claim is replaced by
// what this close actually published: published names each artifact that landed (the recovery
// marker and the goalplan, one fixed sentence each) and warnings are the durability lines of the
// writes that published and then failed a step after the rename, plus the later steps' own. The
// close still did not apply, but an answer that denied a published artifact would hide a partial
// commit the operator has to know about, and an artifact that published cleanly carries no warning
// to recover it from (CRW-930, c6; the mixed-failure path was CRW-869). With nothing published and
// no warning the refusal is returned exactly as it was.
//
// Only the claim this close owns is rewritten: a refusal that embeds a writer's own error text may
// contain that phrase earlier in the sentence, so the last occurrence - the trailing claim - is the
// one replaced, never the first (CRW-930, d1).
func promptDclosePartialRefusal(refusal string, published promptDclosePublishedArtifacts, warnings []string) string {
	named := published.sentences()
	for _, warning := range warnings {
		if published.names(warning) {
			// The artifact sentences already name this one; adding its durability line again would
			// name the same file twice.
			continue
		}
		named = append(named, warning)
	}
	if len(named) == 0 {
		return refusal
	}
	const claim = "Nothing was written."
	at := strings.LastIndex(refusal, claim)
	if at < 0 {
		// The refusal carries no claim to correct; returning it unchanged is what the first-match form
		// did too, and every call site's own text ends with the claim.
		return refusal
	}
	return refusal[:at] + strings.Join(named, " ") + " Nothing else was written." + refusal[at+len(claim):]
}

// promptDcloseSameOptionalText compares two optional texts, where an absent one differs from any
// present one.
func promptDcloseSameOptionalText(a, b *string) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	}
	return *a == *b
}

// promptDcloseOutstandingMarkerRefusal is the refusal for a session that still carries a D-close
// marker this request does not match. The oracle overwrites that marker and the rows the close it
// describes still owes can then never be written, which loses the session's own record of the
// half-finished close (found by the Codex review of this pull request); the port asks for the exact
// retry or an explicit reset instead.
func promptDcloseOutstandingMarkerRefusal(sessionID, closedWorkPhaseID string) string {
	return promptOrchestrateRefusal("this session still carries the D-close recovery marker of work-phase " + closedWorkPhaseID +
		", which this request does not match. Finish that close by repeating its own D request, or clear the marker with " +
		promptDcloseResetCommand(sessionID) + ". Nothing was written.")
}

// promptDcloseRecoveryPlanCommitted reports whether the plan on disk already carries the effect of
// this close's commit, so a recovery refusal must name the goalplan as published. The marker is
// written before the commit, so a matching marker alone proves nothing; what proves it is the
// commit's own settled shape, the shape CloseFixedWorkPhase compares for its already_done answer
// (CRW-930, d1/d2/d3):
//   - a marker that recorded no successor: the commit closed the target and cleared the cursor, so
//     the target is done and no cursor is set;
//   - a marker that recorded a successor: the commit started it, so it is running on the cursor, or
//     it finished on its own and is done. The marker never names the target as its own successor -
//     no close produces that, so a self-successor marker is corrupt and proves nothing (CRW-930,
//     d1). The target itself may be gone, because a later operator edit removed it; that does not
//     undo a commit whose successor is still on the cursor.
//
// A hand edit that only marks the target done leaves the cursor where it was and matches neither
// shape, so it is never read as a commit.
//
// A legacy marker records no successor because its successor field was absent or malformed, not
// because the close had none, so the shapes below cannot read it: it is unknown in both directions
// and never evidence of a commit (CRW-930, d2). The arm that answers a legacy marker says so in its
// own text and asks the operator to inspect the plan.
func promptDcloseRecoveryPlanCommitted(plan *goalplan.Goalplan, closePhaseID string, marker *state.DcloseRecoveryMarker) bool {
	if marker == nil || marker.Legacy {
		return false
	}
	recordedNext := marker.NextWorkPhaseID
	if recordedNext == nil || *recordedNext == "" {
		target := promptDcloseFindWorkPhase(plan, closePhaseID)
		return target != nil && target.Status == goalplan.WorkPhaseDone && plan.ActiveWorkPhaseID == nil
	}
	if *recordedNext == closePhaseID {
		return false
	}
	// The commit closed the target, so a target still in the plan must be done. A plan whose target
	// is still open carries no commit of this close: an operator who started the successor by hand
	// moves the cursor without closing anything (CRW-930, d1). A target that is gone was removed by a
	// later edit, which does not undo a commit whose successor is settled below.
	if target := promptDcloseFindWorkPhase(plan, closePhaseID); target != nil && target.Status != goalplan.WorkPhaseDone {
		return false
	}
	next := promptDcloseFindWorkPhase(plan, *recordedNext)
	if next == nil {
		return false
	}
	if next.Status == goalplan.WorkPhaseDone {
		return true
	}
	return next.Status == goalplan.WorkPhaseInProgress && plan.ActiveWorkPhaseID != nil && *plan.ActiveWorkPhaseID == *recordedNext
}

// promptDcloseRecoveryCommittedPublication is what a refusal that runs before the recovery
// accounting says about the goalplan: a retry inherits the marker its first attempt wrote, and the
// goalplan too when that attempt's commit is on disk. An integrity refusal and an empty plan both
// sit before that accounting, so they read the same shape here rather than deny the publication
// (CRW-930, d2). A fresh close published nothing on those paths, and a plan that is absent or
// unreadable carries no commit to name.
func promptDcloseRecoveryCommittedPublication(plan *goalplan.Goalplan, closePhaseID string, held state.State, recovering bool) promptDclosePublication {
	if !recovering || held.DcloseRecovery == nil || plan == nil {
		return promptDclosePublication{}
	}
	if promptDcloseRecoveryPlanCommitted(plan, closePhaseID, held.DcloseRecovery) {
		return promptDclosePublication{landed: true}
	}
	return promptDclosePublication{}
}

// promptDcloseRecoveryPublishedAt is what a refusal that never reached the close's own accounting
// may name. A matching retry inherits the marker its first attempt wrote; the goalplan counts too
// when the plan on disk still carries that attempt's commit. The plan is read here rather than taken
// from the lock, because the paths that need this are exactly the ones that could not take the lock
// or were handed no plan: the busy lock, the lock's own error, an "unreadable" outcome (which also
// covers a plan that read cleanly and was refused for what a revival would lose) and a nil plan.
// That read decides the refusal's text, never the refusal, so it does not need the lock
// (CRW-930, d2/d3). A fresh close published nothing on those paths and names nothing.
func promptDcloseRecoveryPublishedAt(cwd, slug, closePhaseID string, held state.State, recovering bool) promptDclosePublishedArtifacts {
	if !recovering {
		return promptDclosePublishedArtifacts{}
	}
	return promptDclosePublishedArtifacts{
		marker: promptDclosePublication{landed: true},
		plan:   promptDcloseRecoveryCommittedPublication(goalplan.ReadGoalplan(cwd, slug), closePhaseID, held, recovering),
	}
}

// promptDcloseRecoveryOutcome is what the recovery arm answered when it did not refuse.
type promptDcloseRecoveryOutcome struct {
	result    promptDcloseCloseResult
	writePlan bool
	// planCommitted is true when the first attempt of this same close already committed the plan, so
	// a refusal of the retry must name it even though this invocation rewrites nothing: the settled
	// shape is on disk (CloseFixedWorkPhase's already_done) or the target is closed, which is the
	// commit's own effect, or the absent-target resume answered cleanup, whose comment records that
	// the activation happened and only the idempotent rows are owed (CRW-930, d1/d2).
	planCommitted bool
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
		return promptDcloseRecoveryOutcome{planCommitted: promptDcloseRecoveryPlanCommitted(plan, closePhaseID, marker)}, promptOrchestrateRefusal("the recovery marker for " + closePhaseID +
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
	orphan := goalplan.WorkPhaseResumeAbsentTargetResult{}
	if fixed == nil {
		// §53: the absent-target decision is shared with the CLI. Leaving it out here let the same
		// marker activate a pending successor on one surface and silently log started without a plan
		// write on the other.
		orphan = goalplan.ResumeAbsentTarget(plan, promptDcloseString(marker.NextWorkPhaseID))
		if orphan.Kind == goalplan.WorkPhaseResumeSuccessorLost {
			return promptDcloseRecoveryOutcome{planCommitted: promptDcloseRecoveryPlanCommitted(plan, closePhaseID, marker)}, promptOrchestrateRefusal("recovery target " + closePhaseID + " is gone from the plan and the successor " +
				promptDcloseString(orphan.SuccessorID) + " it recorded " + goalplan.AbsentSuccessorDetail(orphan.Reason) +
				", so this retry cannot tell what to finish. The marker was kept; inspect the goalplan, " +
				"set the work-phase statuses and activeWorkPhaseId by hand, then run " + promptDcloseResetCommand(p.SessionID) +
				" to clear the marker. Nothing was written."), true
		}
		if orphan.Kind == goalplan.WorkPhaseResumeActivate {
			resumedAbsent = orphan.Plan
		}
	}
	// planCommitted is what every refusal below must report: the first attempt of this close published
	// the goalplan when the plan on disk already carries the commit's own settled shape. A cleanup
	// answer is not by itself that proof - the resume also answers cleanup when the marker recorded no
	// successor at all - so only the shape decides (CRW-930, d1/d2/d3).
	planCommitted := promptDcloseRecoveryPlanCommitted(plan, closePhaseID, marker)
	// §40 Z1: both surfaces go through CloseFixedWorkPhase, so the recovered plan matches what a
	// normal close would have written - cursor moved, successor in_progress, a truthful started row.
	closed := goalplan.WorkPhaseCloseFixedResult{Kind: goalplan.WorkPhaseCloseFixedAbsent}
	if fixed != nil {
		closed = goalplan.CloseFixedWorkPhase(plan, closePhaseID, goalplan.WorkPhaseRecordedNext{Known: true, ID: marker.NextWorkPhaseID})
	}
	switch closed.Kind {
	case goalplan.WorkPhaseCloseFixedTasksPending:
		// §41 W1: the marker stays so the operator can repair the plan and finish with the same request.
		return promptDcloseRecoveryOutcome{planCommitted: planCommitted}, promptOrchestrateRefusal("recovery target " + closePhaseID + " gained " + promptDcloseCount(len(closed.Pending)) +
			" open task(s) after its marker was written (CYCLE-COMPLETION-01): " + promptDclosePendingText(closed.Pending) +
			". The recovery marker was kept; close those tasks and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedNotRunnable:
		return promptDcloseRecoveryOutcome{planCommitted: planCommitted}, promptOrchestrateRefusal("recovery target " + closePhaseID + " is now " + string(closed.Status) +
			" (CYCLE-COMPLETION-01). The recovery marker was kept; restore that work-phase and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedDependenciesUnmet:
		return promptDcloseRecoveryOutcome{planCommitted: planCommitted}, promptOrchestrateRefusal("recovery target " + closePhaseID + " now waits for " + strings.Join(closed.Unmet, ", ") +
			" (CYCLE-COMPLETION-01). The recovery marker was kept; satisfy those work-phases and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedSuccessorLost:
		// §51: a corrupt marker points at reset, not at a fix.
		if closed.Reason == "corrupt" {
			return promptDcloseRecoveryOutcome{planCommitted: planCommitted}, promptOrchestrateRefusal("the recovery marker for " + closePhaseID + " names that same work-phase as its successor, " +
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
		return promptDcloseRecoveryOutcome{planCommitted: planCommitted}, promptOrchestrateRefusal("recovery target " + closePhaseID + " was closed with successor " +
			promptDcloseString(closed.SuccessorID) + ", which " + detail + " (CYCLE-COMPLETION-01). The recovery marker was kept; " +
			"restore that work-phase and repeat the same D request. Nothing was written."), true
	case goalplan.WorkPhaseCloseFixedOK:
		return promptDcloseRecoveryOutcome{result: promptDcloseCloseResult{kind: "ok", closedID: promptDcloseString(closed.ClosedID), plan: closed.Plan}, writePlan: true}, "", false
	case goalplan.WorkPhaseCloseFixedAlreadyDone:
		// The settled shape is already on disk, so the first attempt of this close committed the plan:
		// this invocation rewrites nothing, but the plan is one of the artifacts the close published and
		// every later refusal must name it (CRW-930, d1).
		return promptDcloseRecoveryOutcome{result: promptDcloseCloseResult{kind: "ok", closedID: closePhaseID, plan: plan}, planCommitted: true}, "", false
	}
	// already_done and any other answer: the close settled on the fixed target, and the plan is
	// written only when an absent-target resume activated the recorded successor (§53).
	if resumedAbsent != nil {
		return promptDcloseRecoveryOutcome{result: promptDcloseCloseResult{kind: "ok", closedID: closePhaseID, plan: resumedAbsent}, writePlan: true}, "", false
	}
	return promptDcloseRecoveryOutcome{result: promptDcloseCloseResult{kind: "ok", closedID: closePhaseID, plan: plan}, planCommitted: planCommitted}, "", false
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
// nothing, where the oracle's JSON.parse throws and the same D request can never finish. A
// directory the slug cannot be resolved to is unreadable, not absent: the row's presence is unknown
// and a caller must not append (CRW-869, finding 1).
func promptDcloseHasGoalplanRow(cwd, slug, event, detail string) (bool, error) {
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		return false, err
	}
	return promptDcloseAnyRow(filepath.Join(dir, goalplan.GoalplanLedgerFile), func(row map[string]any) bool {
		gotEvent, _ := row["event"].(string)
		gotDetail, _ := row["detail"].(string)
		return gotEvent == event && gotDetail == detail
	})
}

// promptDcloseHasPabcdCloseRow is hasPabcdCloseRow (:634-645): the PABCD close row of this
// session, this check cycle and this closed work phase, where the closed phase is JSON null when
// the id is empty. A row of a damaged line matches nothing, as in promptDcloseHasGoalplanRow, and
// an unreadable ledger is unreadable, not absent (CRW-869, finding 1).
func promptDcloseHasPabcdCloseRow(cwd, sessionID string, checkEpoch *string, closedWorkPhaseID string) (bool, error) {
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
// satisfies match. The answer has three states, by construction: present (true, nil), absent
// (false, nil) for a file that is not there, and unreadable (false, err) for any other read error
// (CRW-869, finding 1). A line that is not a JSON object and a blank line match nothing.
func promptDcloseAnyRow(path string, match func(map[string]any) bool) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
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
			return true, nil
		}
	}
	return false, nil
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

// promptDcloseRefusalNaming is a refusal with what this close already published named before its
// closing bracket. It is for the texts that carry no "Nothing was written." claim of their own - the
// busy text says the phase and goalplan ledger were not changed, the entry gate names the broken
// source binding - so the publication sentences are added rather than substituted, and the text's own
// claim stays true: this retry changed nothing (CRW-930, d1/d3). The sentences follow the text's
// final period, separated from it by a space, and keep the lowercase sentence form the partial
// refusal uses. With nothing published the text is returned exactly as it was.
func promptDcloseRefusalNaming(refusal string, published promptDclosePublishedArtifacts) string {
	named := published.sentences()
	if len(named) == 0 {
		return refusal
	}
	const tail = "]"
	at := strings.LastIndex(refusal, tail)
	if at < 0 {
		return refusal
	}
	return refusal[:at] + " " + strings.Join(named, " ") + refusal[at:]
}

// promptDcloseGoalplanUnreadable is the unreadable-goalplan text (:1275-1281).
func promptDcloseGoalplanUnreadable(reason string) string {
	return "[crw \u2014 D-close was not applied: the bound goalplan could not be read (" + reason + "). Nothing was written.]"
}

// promptDcloseFinalizePending is the finalization-pending text (:1347-1354).
func promptDcloseFinalizePending(reason string) string {
	return "[crw \u2014 D-close was committed and the cycle is closed, but ledger/marker finalization is pending: " + reason + " The recovery marker is still on the session, so running the same D request again finishes the cleanup.]"
}

// promptDcloseLedgerWarning is the durability warning for a close that did happen whose PABCD
// close row could not be written. It follows the CLI writers' wording (CRW-744/793/811) and is
// carried as one extra line after the DONE directive. It is the all-done answer's counterpart of
// the pending text: an all-done close leaves no marker, so no retry can finish the row.
func promptDcloseLedgerWarning(reason string) string {
	return "the close was applied but its ledger row could not be written: " + reason
}
