package cli

// The D close of CXC v0.2.40 pabcd-state/src/orchestrate-cli.ts (commit 3c1459ac): the
// CHECK-BINDING-01 receipt check (:726-736), the unbound close (:737-750), the goalplan-locked
// bound close (:752-1015) and the finalize (:1016-1064). CRW-756 ports it together with the
// recovery branch (:776-885) that CRW-757 originally owned, because the two are one concept and
// neither half is independently verifiable. Behaviour is ported as-is, the oracle's texts with
// the authorized name substitution only (`cxc orchestrate reset` -> `crw pabcd orchestrate reset`).
//
// Two departures the port's own rules require, both stated in the oracle's comments' place:
//
//   - The data-loss refusal (orchestrateTransitionStateWritable) runs before the first write, as
//     orchestrateTransitionApply's other edges do it. The oracle publishes the state its reader
//     rebuilt, so a stored lone surrogate or a record past a cap is replaced or dropped; this
//     writer refuses instead and writes nothing.
//   - A state write whose error is state.Published (renamed into place, only the directory sync
//     failed) counts as written, and the answer carries the warning line the scope gives
//     (CRW-744 and CRW-811's rule). A failure before the rename and a failed ledger append stay
//     the oracle's: on the bound path the marker survives and a retry takes the recovery path,
//     and on the unbound path the failure is reported as the oracle reports it
//     (docs/port-cxc/known-defects/CRW-756.md records why that order is kept).
//
// The oracle's commit hooks (OrchestrateCommitHooks, :424-429) become orchestrateDcloseSeam, an
// unexported seam the tests pass in. Nothing here is a package-level variable and nothing runs at
// program start.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/fsm"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/gate"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// orchestrateDcloseSeam is the oracle's OrchestrateCommitHooks (:424-429), the seam its own tests use
// to stop the close after each durable write, plus the state-write override the CRW-744/CRW-811 rule
// needs a test for (the same shape EvidenceResolveArgs.writeState has). A non-nil hook answer unwinds
// exactly as the oracle's thrown hook does: the error reaches the caller and nothing after it runs.
type orchestrateDcloseSeam struct {
	afterRecoveryMarkerWrite func() error
	afterGoalplanCommit      func() error
	afterPabcdLedgerAppend   func() error
	afterStateWrite          func() error
	writeState               func(cwd string, next state.State) error
}

// orchestrateDcloseRunHook runs one hook, or nothing when the test left it nil.
func orchestrateDcloseRunHook(hook func() error) error {
	if hook == nil {
		return nil
	}
	return hook()
}

// orchestrateDcloseStateWarning is the line a published-but-unsynced state write adds to the answer
// (the CRW-744 and CRW-811 rule). A failure before the rename is not a publication and stays an error.
const orchestrateDcloseStateWarning = "session state was published but its directory could not be synced: "

// orchestrateDcloseWriteState publishes next and answers the warning line, if any. The state at the
// final path is visible to every reader once WriteState renames it, so a directory-sync failure after
// the rename is a success with a warning rather than a rollback (state.Published).
func orchestrateDcloseWriteState(seam orchestrateDcloseSeam, cwd string, next state.State) (string, error) {
	write := seam.writeState
	if write == nil {
		write = state.WriteState
	}
	err := write(cwd, next)
	if err == nil {
		return "", nil
	}
	if !state.Published(err) {
		return "", err
	}
	return orchestrateDcloseStateWarning + err.Error(), nil
}

// orchestrateDcloseAnswer joins the oracle's output with the warning lines the published state writes
// produced, in the order they happened. No warning leaves the oracle's bytes untouched.
func orchestrateDcloseAnswer(output string, warnings []string) string {
	for _, w := range warnings {
		if w != "" {
			output += "\n" + w
		}
	}
	return output
}

// orchestrateDcloseLockAnswer is the {code, allDone, output} object the locked callback returns
// (:753-1015). AllDone is what the caller reads after the lock to decide the finalize shape.
type orchestrateDcloseLockAnswer struct {
	Code    int
	AllDone bool
	Output  string
}

// orchestrateDcloseSurrogateOptions is the reading both guards take of a ledger line: the oracle's
// JSON.parse. Surrogates keeps a lone surrogate escape as the three WTF-8 bytes a Go string holds a
// lone surrogate in, where encoding/json folds it into U+FFFD - that folding is the defect CRW-850
// fixes, because a stored "closed wp-\\ud800" row then compared equal to a U+FFFD close and the close
// skipped the row it owed (data loss). Map reads an object into a map[string]any, as the oracle's
// Record is, and SpelledNumbers keeps a number as spelled, which is what JSON.parse's double holds
// for an integer the way the guard keys never read a number.
var orchestrateDcloseSurrogateOptions = pyjson.LoadOptions{Surrogates: true, Map: true, Numbers: pyjson.SpelledNumbers}

// orchestrateDcloseReadJSONLObjects is readJsonlObjects (:431-435): every non-empty line of the file
// as a JSON object. A missing file is no rows. The bytes are decoded as UTF-8 first (source.DecodeUTF8,
// what Node's readFileSync(path, "utf8") and goalplan read.go both do: an invalid byte becomes one
// U+FFFD), then each non-empty line is parsed the way the oracle's JSON.parse parses it.
//
// A line the oracle's readers would refuse is an error, so a damaged ledger fails the close loudly
// instead of silently answering "no row yet" and writing a duplicate: text JSON.parse cannot read at
// all, and a bare `null`, whose property access is the TypeError the oracle's .some() callback throws.
// A line that is another JSON value is not an error there - property access on a number, string,
// boolean or array answers undefined - so it is dropped as a row that matches nothing, and no reader
// is fooled into treating it as an empty object. A repeated key keeps its last value, as a JS object
// literal and a Python dict both do.
//
// The error text is the reader's own, not the oracle's V8 SyntaxError text. Two refusals differ from
// the encoding/json reading this replaces, both named in the pull request: a number past float64's
// range is now read rather than refused, and text after the value reads as "trailing data after the
// JSON value" rather than encoding/json's own wording.
func orchestrateDcloseReadJSONLObjects(path string) ([]map[string]any, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, line := range strings.Split(source.DecodeUTF8(raw), "\n") {
		if line == "" {
			continue
		}
		value, err := pyjson.Loads(line, orchestrateDcloseSurrogateOptions)
		if err != nil {
			return nil, err
		}
		if value == nil {
			return nil, errors.New("ledger line " + strconv.Quote(line) + " is null, which the oracle's readers refuse")
		}
		row, isObject := value.(map[string]any)
		if !isObject {
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// orchestrateDcloseHasGoalplanRow is hasGoalplanRow (:437-440): a row of the plan's own ledger with
// this event and detail. It is the idempotence guard of both goalplan rows.
func orchestrateDcloseHasGoalplanRow(cwd, slug string, event goalplan.GoalplanLedgerEvent, detail string) (bool, error) {
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		return false, err
	}
	rows, err := orchestrateDcloseReadJSONLObjects(filepath.Join(dir, goalplan.GoalplanLedgerFile))
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		gotEvent, _ := row["event"].(string)
		gotDetail, _ := row["detail"].(string)
		if gotEvent == string(event) && gotDetail == detail {
			return true, nil
		}
	}
	return false, nil
}

// orchestrateDcloseRowMatchesKey compares a stored JSON value against a *string key the way the
// oracle's === does: an absent key is undefined and never equals null, while a present JSON null
// does. Folding the two together would let a row that predates the close key suppress a real row.
func orchestrateDcloseRowMatchesKey(row map[string]any, key string, want *string) bool {
	got, present := row[key]
	if want == nil {
		return present && got == nil
	}
	text, ok := got.(string)
	return ok && text == *want
}

// orchestrateDcloseHasPabcdCloseRow is hasPabcdCloseRow (:442-453): the PABCD ledger already holds
// this close's C -> IDLE row for this session, check epoch and closed work phase. It is the guard
// that makes a retry append the row once.
func orchestrateDcloseHasPabcdCloseRow(cwd, sessionID string, checkEpoch, closedWorkPhaseID *string) (bool, error) {
	rows, err := orchestrateDcloseReadJSONLObjects(filepath.Join(cwd, crwdir.DirName, state.LedgerFile))
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		gotSession, _ := row["sessionId"].(string)
		gotFrom, _ := row["from"].(string)
		gotTo, _ := row["to"].(string)
		gotReason, _ := row["reason"].(string)
		if gotSession == sessionID && gotFrom == "C" && gotTo == "IDLE" && gotReason == "done" &&
			orchestrateDcloseRowMatchesKey(row, "checkEpoch", checkEpoch) &&
			orchestrateDcloseRowMatchesKey(row, "closedWorkPhaseId", closedWorkPhaseID) {
			return true, nil
		}
	}
	return false, nil
}

// orchestrateDcloseFindWorkPhase is the oracle's workPhases.find((wp) => wp.id === id).
func orchestrateDcloseFindWorkPhase(plan *goalplan.Goalplan, id string) *goalplan.GoalplanWorkPhase {
	for i := range plan.WorkPhases {
		if plan.WorkPhases[i].ID == id {
			return &plan.WorkPhases[i]
		}
	}
	return nil
}

// orchestrateDcloseDescribeTasks is the oracle's pending.map((task) => `${task.id} (${task.title})`).join("; ").
func orchestrateDcloseDescribeTasks(tasks []goalplan.GoalplanTask) string {
	parts := make([]string, 0, len(tasks))
	for i := range tasks {
		parts = append(parts, tasks[i].ID+" ("+tasks[i].Title+")")
	}
	return strings.Join(parts, "; ")
}

// orchestrateDcloseDeref prints an optional id the way a JavaScript template literal prints it: the
// empty string when the variant carries none, because the oracle's field is always present and may
// hold the empty string (a corrupt marker, a plan whose phase id is empty).
func orchestrateDcloseDeref(id *string) string {
	if id == nil {
		return ""
	}
	return *id
}

// orchestrateDcloseEvidence is ...(args.attest?.did ? { evidence: args.attest.did } : {}), the tail both
// PABCD close rows carry.
func orchestrateDcloseEvidence(att *attest.Attestation) *string {
	if att == nil || att.Did == "" {
		return nil
	}
	did := att.Did
	return &did
}

// orchestrateDcloseAppendPabcdRow appends the C -> IDLE done row this close owes. The oracle builds the
// object with the close key and then the evidence (orchestrate-cli.ts:899 and :1040), which is
// LedgerEntry's default key order, so the row's bytes match the recorded fixture's.
func orchestrateDcloseAppendPabcdRow(cwd string, cur state.State, checkEpoch, closedWorkPhaseID *string, att *attest.Attestation) error {
	from := state.PhaseC
	return state.AppendLedger(cwd, state.LedgerEntry{
		TS: orchestrateTransitionTimestamp(), SessionID: cur.SessionID, From: &from, To: state.PhaseIdle,
		Reason: "done", Evidence: orchestrateDcloseEvidence(att),
		Close: &state.CloseKey{CheckEpoch: checkEpoch, ClosedWorkPhaseID: closedWorkPhaseID},
	})
}

// orchestrateDclose is the D close, called from orchestrateTransitionApply while the session lock is
// already held. It takes only the goalplan write lock (never the session lock again: the repository
// lock order is session outside, goalplan inside), and the hooks are the oracle's commit seam.
func orchestrateDclose(cwd, sessionID, closePhaseID string, cur state.State, att *attest.Attestation, recovering bool, seam orchestrateDcloseSeam) (CliResult, error) {
	// The recovery branch reads the marker it is resuming. The caller decides recovering with
	// state.MatchesDcloseRecovery, which is false without one, so a nil marker here is a caller bug and
	// taking the normal path is the safe reading of it: no marker means no recorded close decision.
	if recovering && cur.DcloseRecovery == nil {
		recovering = false
	}
	// The data-loss refusal, before this edge's first write (see the file comment).
	if reason, ok := orchestrateTransitionStateWritable(cwd, sessionID, cur); !ok {
		return orchestrateTransitionStateRefusal(fsm.VerbD, reason), nil
	}
	warnings := []string{}

	// CHECK-BINDING-01 (075): a bound session must name a receipt this cycle produced. A
	// marker-matched retry already spent its receipt in the first attempt, and the epoch it ran
	// under is recorded on the marker.
	if cur.Slug != "" && !recovering {
		receiptPath := ""
		if att != nil {
			receiptPath = att.TestReceiptPath
		}
		receiptCheck := gate.ValidateCheckReceipt(cur, sessionID, receiptPath, cwd)
		if !receiptCheck.OK {
			return CliResult{Code: 1, Output: "orchestrate D: " + RenderPhaseContext(cur, sessionID) + "; " +
				receiptCheck.Reason + " Nothing was written."}, nil
		}
	}

	// §35-1: unbound HITL keeps the pre-wp5 path byte-for-byte. It never takes a goalplan lock and
	// never enters marker cleanup.
	if cur.Slug == "" {
		next := fsm.ClearedIdle(cur)
		next.StopBlockPhase, next.StopBlockCount = nil, 0
		warning, err := orchestrateDcloseWriteState(seam, cwd, next)
		if err != nil {
			return CliResult{}, err
		}
		warnings = append(warnings, warning)
		from := cur.Phase
		if err := state.AppendLedger(cwd, state.LedgerEntry{
			TS: orchestrateTransitionTimestamp(), SessionID: cur.SessionID, From: &from, To: state.PhaseIdle,
			Reason: "done", Evidence: orchestrateDcloseEvidence(att),
		}); err != nil {
			return CliResult{}, err
		}
		output := "orchestrate D: current=" + string(cur.Phase) + " -> IDLE (" + string(cur.Phase) +
			" \u2192 IDLE, cycle closed, session " + sessionID + ")"
		return CliResult{Code: 0, Output: orchestrateDcloseAnswer(output, warnings)}, nil
	}
	slug := cur.Slug
	allDoneClose := false
	locked, err := goalplan.WithGoalplanWriteLock(cwd, slug, func(plan *goalplan.Goalplan) (orchestrateDcloseLockAnswer, error) {
		refuse := func(output string) (orchestrateDcloseLockAnswer, error) {
			return orchestrateDcloseLockAnswer{Code: 1, AllDone: false, Output: output}, nil
		}
		// §5: integrity is checked inside the lock, before marker or any write.
		integrityReasons := append([]string{}, goalplan.GoalplanDefinitionIntegrityReasons(plan)...)
		integrityReasons = append(integrityReasons, goalplan.GoalplanDependencyCompletionReasons(plan)...)
		if len(integrityReasons) > 0 {
			return refuse("orchestrate D: invalid goalplan: " + strings.Join(integrityReasons, "; ") + ". Nothing was written.")
		}
		// §35-2: preserve the existing empty-plan refusal before target lookup.
		if len(plan.WorkPhases) == 0 {
			return refuse("orchestrate D: " + RenderPhaseContext(cur, sessionID) + "; the bound goalplan \"" + slug +
				"\" has no work-phase to close (CYCLE-COMPLETION-01): the plan is empty \u2014 register workPhases[] first. Nothing was written.")
		}

		// §39 Y2: recovery comes BEFORE the all-done special case. Crashing right after the final
		// work-phase was committed leaves an all-done plan, so checking all-done first would
		// re-enter it as a plain cycle close and record closedWorkPhaseId: null - losing the
		// target the marker names.
		closedPlan, writeClosedPlan := plan, false
		if recovering {
			// §50: a marker written before the successor field exists cannot say whether the plan
			// commit landed, and neither reading of it is safe. Hand it to a human instead of guessing.
			if cur.DcloseRecovery.Legacy {
				return refuse("orchestrate D: the recovery marker for " + closePhaseID + " predates the successor field, so this retry cannot tell whether the plan commit landed. " +
					"The marker was kept; inspect the goalplan, set the work-phase statuses and activeWorkPhaseId by hand, then run `crw pabcd orchestrate reset --session " +
					sessionID + "` to clear the marker. Nothing was written.")
			}
			// §39 Y1: the marker is written BEFORE the plan commit, so a matching marker does not
			// prove the plan was closed. Look the fixed target up - this is not the §38 X2 "target
			// validation" that refuses on absence.
			//
			// §52/§53: an absent target does NOT mean there is nothing to finish. The marker still
			// records the successor this close activated, and skipping to cleanup leaves that phase
			// unstarted while the ledger claims the cycle closed.
			fixed := orchestrateDcloseFindWorkPhase(plan, closePhaseID)
			if fixed == nil {
				orphan := goalplan.ResumeAbsentTarget(plan, orchestrateDcloseDeref(cur.DcloseRecovery.NextWorkPhaseID))
				if orphan.Kind == goalplan.WorkPhaseResumeSuccessorLost {
					return refuse("orchestrate D: recovery target " + closePhaseID + " is gone from the plan and the successor " +
						orchestrateDcloseDeref(orphan.SuccessorID) + " it recorded " + goalplan.AbsentSuccessorDetail(orphan.Reason) +
						", so this retry cannot tell what to finish. The marker was kept; inspect the goalplan, set the work-phase statuses and activeWorkPhaseId by hand, then run `crw pabcd orchestrate reset --session " +
						sessionID + "` to clear the marker. Nothing was written.")
				}
				if orphan.Kind == goalplan.WorkPhaseResumeActivate {
					closedPlan, writeClosedPlan = orphan.Plan, true
				}
			}
			if fixed != nil {
				// §40 Z1: closing it means closeFixedWorkPhase(), the same transformation the normal
				// path uses. §42/§43: the helper runs whenever the target exists and answers
				// already_done only after its three gates pass.
				closed := goalplan.CloseFixedWorkPhase(plan, closePhaseID,
					goalplan.WorkPhaseRecordedNext{Known: true, ID: cur.DcloseRecovery.NextWorkPhaseID})
				// §41 W1: a target that gained a pending task or became blocked after its marker is
				// FAIL-CLOSED, and the marker is deliberately left in place so the operator can fix
				// the plan and finish with the same request.
				switch closed.Kind {
				case goalplan.WorkPhaseCloseFixedTasksPending:
					return refuse("orchestrate D: recovery target " + closePhaseID + " gained " + strconv.Itoa(len(closed.Pending)) +
						" open task(s) after its marker was written, so this cycle cannot close (CYCLE-COMPLETION-01): " +
						orchestrateDcloseDescribeTasks(closed.Pending) +
						". The recovery marker was kept; close those tasks and run the same D request again. Nothing was written.")
				case goalplan.WorkPhaseCloseFixedNotRunnable:
					return refuse("orchestrate D: recovery target " + closePhaseID + " is now " + string(closed.Status) +
						", so this cycle cannot close (CYCLE-COMPLETION-01). The recovery marker was kept; restore that work-phase and run the same D request again. Nothing was written.")
				case goalplan.WorkPhaseCloseFixedDependenciesUnmet:
					return refuse("orchestrate D: recovery target " + closePhaseID + " now waits for " + strings.Join(closed.Unmet, ", ") +
						", so this cycle cannot close (CYCLE-COMPLETION-01). The recovery marker was kept; satisfy those work-phases and run the same D request again. Nothing was written.")
				case goalplan.WorkPhaseCloseFixedSuccessorLost:
					// §51: a corrupt marker names no repairable work-phase, so it gets its own route out.
					if closed.Reason == "corrupt" {
						return refuse("orchestrate D: the recovery marker for " + closePhaseID + " names that same work-phase as its successor, which no close can produce, so this retry cannot tell what to finish. " +
							"The marker was kept; inspect the goalplan, set the work-phase statuses and activeWorkPhaseId by hand, then run `crw pabcd orchestrate reset --session " +
							sessionID + "` to clear the marker. Nothing was written.")
					}
					detail := "now waits for another work-phase"
					switch closed.Reason {
					case "absent":
						detail = "is no longer in the plan"
					case "not_runnable":
						detail = "can no longer be started"
					}
					return refuse("orchestrate D: recovery target " + closePhaseID + " was closed with successor " +
						orchestrateDcloseDeref(closed.SuccessorID) + ", which " + detail +
						", so this cycle cannot close (CYCLE-COMPLETION-01). The recovery marker was kept; restore that work-phase and run the same D request again. Nothing was written.")
				case goalplan.WorkPhaseCloseFixedOK:
					closedPlan, writeClosedPlan = closed.Plan, true
				}
			}
		} else {
			// §35-3 / #49: an already-complete non-empty plan closes the cycle only. No marker, plan
			// write, or goalplan ledger row is needed. This is inside the non-recovery branch so a
			// matching marker always wins.
			everyDone := true
			for i := range plan.WorkPhases {
				everyDone = everyDone && plan.WorkPhases[i].Status == goalplan.WorkPhaseDone
			}
			if everyDone {
				// §40 Z2: finish the PABCD close row inside THIS lock. all-done mints no marker, so
				// if the row were left to a second lock and that lock failed, the retry would hit
				// `IDLE -> D` with nothing to recover from and the row would be lost for good.
				if cur.Phase == state.PhaseC {
					have, err := orchestrateDcloseHasPabcdCloseRow(cwd, sessionID, cur.CheckEpoch, nil)
					if err != nil {
						return orchestrateDcloseLockAnswer{}, err
					}
					if !have {
						if err := orchestrateDcloseAppendPabcdRow(cwd, cur, cur.CheckEpoch, nil, att); err != nil {
							return orchestrateDcloseLockAnswer{}, err
						}
						if err := orchestrateDcloseRunHook(seam.afterPabcdLedgerAppend); err != nil {
							return orchestrateDcloseLockAnswer{}, err
						}
					}
				}
				return orchestrateDcloseLockAnswer{Code: 0, AllDone: true}, nil
			}
			// §35-5: input and target membership checks follow all-done and recovery.
			if closePhaseID == "" {
				return refuse("orchestrate D: attest.workPhaseId is required. Nothing was written.")
			}
			if orchestrateDcloseFindWorkPhase(plan, closePhaseID) == nil {
				return refuse("orchestrate D: work-phase " + closePhaseID + " is not in the bound goalplan. Nothing was written.")
			}
			advanced := goalplan.AdvanceWorkPhase(plan)
			// §35-6: pending tasks are refused before no-active/deadlock handling.
			if advanced.Kind == goalplan.WorkPhaseAdvanceTasksPending {
				return refuse("orchestrate D: " + RenderPhaseContext(cur, sessionID) + "; work-phase " +
					orchestrateDcloseDeref(advanced.WorkPhaseID) + " still has " + strconv.Itoa(len(advanced.Pending)) +
					" open task(s), so this cycle cannot close (CYCLE-COMPLETION-01): " +
					orchestrateDcloseDescribeTasks(advanced.Pending) + ". Nothing was written.")
			}
			// §35-7: all-done was consumed above, so no_active now means a real
			// unavailable/deadlocked remainder. Prefer dependencyDeadlock() detail.
			if advanced.Kind == goalplan.WorkPhaseAdvanceNoActive {
				reason := "every remaining work-phase is blocked or superseded \u2014 unblock one"
				if deadlock := goalplan.DetectDependencyDeadlock(plan); deadlock != nil {
					reason = "Dependency deadlock: " + strings.Join(deadlock.Reasons, "; ")
				}
				return refuse("orchestrate D: " + RenderPhaseContext(cur, sessionID) + "; the bound goalplan \"" + slug +
					"\" has no work-phase to close (CYCLE-COMPLETION-01): " + reason + ". Nothing was written.")
			}
			if advanced.ClosedID == nil || *advanced.ClosedID != closePhaseID {
				return refuse("orchestrate D: fixed close target " + closePhaseID + " does not match active work-phase " +
					orchestrateDcloseDeref(advanced.ClosedID) + ". Nothing was written.")
			}
			closedPlan, writeClosedPlan = advanced.Plan, true

			// §35-8: only the normal bound close mints a marker and commits the plan.
			if cur.Phase != state.PhaseC || cur.CheckEpoch == nil {
				return refuse("orchestrate D: current C check epoch is required. Nothing was written.")
			}
			next := cur
			next.DcloseRecovery = &state.DcloseRecoveryMarker{
				SessionID: cur.SessionID, CheckEpoch: *cur.CheckEpoch, ClosedWorkPhaseID: closePhaseID,
				// §48: record the successor BEFORE the plan commit. A retry re-reads this decision,
				// so a later hand edit of the cursor cannot rewrite what this close meant to do.
				NextWorkPhaseID: closedPlan.ActiveWorkPhaseID,
			}
			warning, err := orchestrateDcloseWriteState(seam, cwd, next)
			if err != nil {
				return orchestrateDcloseLockAnswer{}, err
			}
			if warning != "" {
				warnings = append(warnings, warning)
			}
			if err := orchestrateDcloseRunHook(seam.afterRecoveryMarkerWrite); err != nil {
				return orchestrateDcloseLockAnswer{}, err
			}
		}
		if writeClosedPlan {
			if err := goalplan.WriteGoalplan(cwd, closedPlan); err != nil {
				return orchestrateDcloseLockAnswer{}, err
			}
			if err := orchestrateDcloseRunHook(seam.afterGoalplanCommit); err != nil {
				return orchestrateDcloseLockAnswer{}, err
			}
		}

		haveDone, err := orchestrateDcloseHasGoalplanRow(cwd, slug, goalplan.EventWorkphaseDone, "closed "+closePhaseID)
		if err != nil {
			return orchestrateDcloseLockAnswer{}, err
		}
		if !haveDone {
			if err := goalplan.AppendGoalplanLedger(cwd, slug, goalplan.GoalplanLedgerEntry{
				Ts: orchestrateTransitionTimestamp(), Slug: slug, Event: goalplan.EventWorkphaseDone,
				Detail: "closed " + closePhaseID,
			}); err != nil {
				return orchestrateDcloseLockAnswer{}, err
			}
		}
		// §52: the started row names the successor THIS close activated, which the marker records. The
		// persisted cursor is the wrong source on a resume: a retry that answers already_done leaves
		// `closedPlan` as the file, and that cursor may have moved past the successor - or been nulled
		// by an all-done plan - so the row this close still owed would never be written. The
		// hasGoalplanRow guard keeps it idempotent when the row already exists.
		startedID := closedPlan.ActiveWorkPhaseID
		if recovering {
			startedID = cur.DcloseRecovery.NextWorkPhaseID
		}
		haveStarted := false
		if startedID != nil && *startedID != "" {
			haveStarted, err = orchestrateDcloseHasGoalplanRow(cwd, slug, goalplan.EventWorkphaseStarted, "started "+*startedID)
			if err != nil {
				return orchestrateDcloseLockAnswer{}, err
			}
		}
		if startedID != nil && *startedID != "" && !haveStarted {
			if err := goalplan.AppendGoalplanLedger(cwd, slug, goalplan.GoalplanLedgerEntry{
				Ts: orchestrateTransitionTimestamp(), Slug: slug, Event: goalplan.EventWorkphaseStarted,
				Detail: "started " + *startedID,
			}); err != nil {
				return orchestrateDcloseLockAnswer{}, err
			}
		}
		return orchestrateDcloseLockAnswer{Code: 0, AllDone: false}, nil
	}, nil)
	if err != nil {
		return CliResult{}, err
	}
	switch locked.Kind {
	case "locked":
		return CliResult{Code: 1, Output: "orchestrate D: " + locked.Reason + " D-close was not applied. Nothing was written."}, nil
	case "unreadable":
		return CliResult{Code: 1, Output: "orchestrate D: the bound goalplan \"" + slug + "\" could not be read (CYCLE-COMPLETION-01): " +
			locked.Reason + ". Nothing was written."}, nil
	}
	if locked.Value == nil {
		return CliResult{}, errors.New("orchestrate D: the goalplan write lock answered without a value")
	}
	if locked.Value.Code != 0 {
		return CliResult{Code: locked.Value.Code, Output: locked.Value.Output}, nil
	}
	allDoneClose = locked.Value.AllDone

	recovery := state.ReadState(cwd, sessionID).DcloseRecovery
	closeCheckEpoch := cur.CheckEpoch
	if recovery != nil {
		epoch := recovery.CheckEpoch
		closeCheckEpoch = &epoch
	}
	var closedWorkPhaseID *string
	if !allDoneClose {
		closed := closePhaseID
		closedWorkPhaseID = &closed
	}
	if cur.Phase != state.PhaseIdle {
		next := fsm.ClearedIdle(cur)
		next.StopBlockPhase, next.StopBlockCount = nil, 0
		// clearedIdle already nulled both; only a live marker puts them back, so the state keeps the
		// epoch this close ran under until the finalize clears it (oracle :1023-1027).
		if !allDoneClose && recovery != nil {
			epoch := recovery.CheckEpoch
			next.CheckEpoch, next.DcloseRecovery = &epoch, recovery
		}
		warning, err := orchestrateDcloseWriteState(seam, cwd, next)
		if err != nil {
			return CliResult{}, err
		}
		warnings = append(warnings, warning)
		if err := orchestrateDcloseRunHook(seam.afterStateWrite); err != nil {
			return CliResult{}, err
		}
	}
	// check + append + marker cleanup is one critical section. Two recoveries cannot both observe an
	// absent 3-tuple. §40 Z2: all-done already wrote its row inside the first lock and has no marker
	// to clear, so it never enters this second critical section.
	if !allDoneClose {
		finalize, err := goalplan.WithGoalplanWriteLock(cwd, slug, func(plan *goalplan.Goalplan) (struct{}, error) {
			have, err := orchestrateDcloseHasPabcdCloseRow(cwd, sessionID, closeCheckEpoch, closedWorkPhaseID)
			if err != nil {
				return struct{}{}, err
			}
			if !have {
				if err := orchestrateDcloseAppendPabcdRow(cwd, cur, closeCheckEpoch, closedWorkPhaseID, att); err != nil {
					return struct{}{}, err
				}
				if err := orchestrateDcloseRunHook(seam.afterPabcdLedgerAppend); err != nil {
					return struct{}{}, err
				}
			}
			current := state.ReadState(cwd, sessionID)
			if state.MatchesDcloseRecovery(current, closePhaseID) {
				cleared := current
				cleared.CheckEpoch, cleared.DcloseRecovery = nil, nil
				warning, err := orchestrateDcloseWriteState(seam, cwd, cleared)
				if err != nil {
					return struct{}{}, err
				}
				if warning != "" {
					warnings = append(warnings, warning)
				}
				if err := orchestrateDcloseRunHook(seam.afterStateWrite); err != nil {
					return struct{}{}, err
				}
			}
			return struct{}{}, nil
		}, nil)
		if err != nil {
			return CliResult{}, err
		}
		if finalize.Kind != "ok" {
			// §39 Y3: not a refusal. The state write above already moved the FSM to IDLE outside the
			// lock, so returning code 1 here would report a failure for a cycle that is functionally
			// closed. The marker survives and the next D request for the same tuple finishes cleanup.
			output := "orchestrate D: close target " + closePhaseID + " is committed and the cycle is closed, but ledger/marker finalization is pending: " +
				finalize.Reason + " The recovery marker is still on the session, so running the same D request again finishes the cleanup."
			return CliResult{Code: 0, Output: orchestrateDcloseAnswer(output, warnings)}, nil
		}
	}
	output := "orchestrate D: close target " + closePhaseID + " is complete (cycle closed, session " + sessionID + ")"
	return CliResult{Code: 0, Output: orchestrateDcloseAnswer(output, warnings)}, nil
}
