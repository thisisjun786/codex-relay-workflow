// prompt_dclose_published_test.go is the CRW-869 suite for the unit prompt_dclose.go owns: the
// bound chat D-close's ledger reads and the write results of its marker and plan writes. It is the
// Go form of the three findings of CRW-869, each red on the baseline:
//
//   - an unreadable ledger row lookup (anything but ENOENT) was read as "no row", so the second
//     goalplan lock re-appended the rows and cleared the marker;
//   - a marker or plan write that published the state and then failed a step after the rename
//     answered "Nothing was written" and stopped the close;
//   - a Stop-budget stamp that could not take a busy session lock answered "" instead of the
//     bound D-close's busy refusal.
//
// Every case drives the handler through promptSubmitHandleWith, the way the leg runs it, and the
// write seams are function arguments (a field of promptDcloseSeams the caller passes), never a
// package-level variable. The session lock is swapped the same way.
package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptDcloseTwoPhases writes a bound plan with wp-1 in progress and wp-2 pending, the cursor on
// wp-1, so a close of wp-1 records wp-2 as its successor.
func promptDcloseTwoPhases(t *testing.T, cwd, slug string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "two phases " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// promptDcloseStopAfterPabcdAppend stops a close right after its PABCD close row, which is where
// the retry's goalplan row reads run: the goalplan rows are already on disk and the recovery marker
// is still on the session.
func promptDcloseStopAfterPabcdAppend(t *testing.T, cwd, sessionID, turn, attest string) {
	t.Helper()
	if _, panicked := promptDcloseRunWith(t, cwd, sessionID, turn, attest,
		&promptDcloseSeams{afterPabcdLedgerAppend: promptDcloseStop()}); panicked != errPromptDcloseSeam {
		t.Fatalf("the append seam did not fire: %v", panicked)
	}
}

// promptDcloseMakeUnreadable makes one ledger readable only by its writer, so an ordinary read gets
// EACCES while an O_WRONLY|O_APPEND write still succeeds, and answers the restore the test calls
// before it reads the ledger again. Root ignores the mode, so the case skips there.
func promptDcloseMakeUnreadable(t *testing.T, path string) func() {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("the ledger to make unreadable: %v", err)
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	restore := func() { _ = os.Chmod(path, info.Mode().Perm()) }
	t.Cleanup(restore)
	return restore
}

// TestPromptDcloseUnreadableLedgerKeepsTheMarker is finding 1: a ledger read that fails with
// anything but ENOENT is unreadable, not absent, so the retry must not append the row again. The
// cycle close keeps its recovery marker and answers the finalization-pending text, where the
// baseline re-appends both goalplan rows and clears the marker.
func TestPromptDcloseUnreadableLedgerKeepsTheMarker(t *testing.T) {
	t.Run("goalplan ledger", func(t *testing.T) {
		cwd := promptDcloseRepo(t)
		slug := "chat-unreadable-goalplan-ledger"
		promptDcloseTwoPhases(t, cwd, slug)
		promptDcloseSeedState(t, cwd, "s1", slug, "c-unreadable")
		receipt := promptDcloseReceipt(t, cwd, "s1", "c-unreadable")
		attest := promptDcloseAttest("wp-1", receipt)
		promptDcloseStopAfterPabcdAppend(t, cwd, "s1", "t1", attest)
		if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 2 {
			t.Fatalf("the stopped close's goalplan rows: %+v", rows)
		}
		ledger := promptDcloseGoalplanLedgerPath(t, cwd, slug)
		restore := promptDcloseMakeUnreadable(t, ledger)
		answer := promptDcloseRun(t, cwd, "s1", "t2", attest)
		restore()
		if !strings.Contains(answer, "finalization is pending") {
			t.Errorf("the unreadable goalplan ledger answered %q, want the finalization-pending text", answer)
		}
		if strings.Contains(answer, "[crw: DONE]") {
			t.Errorf("the unreadable goalplan ledger answered the success directive: %q", answer)
		}
		if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 2 {
			t.Errorf("the retry re-appended the goalplan rows: %+v", rows)
		}
		if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil {
			t.Errorf("the retry cleared the recovery marker: %+v", s)
		}
	})

	t.Run("pabcd ledger", func(t *testing.T) {
		cwd := promptDcloseRepo(t)
		slug := "chat-unreadable-pabcd-ledger"
		promptDcloseTwoPhases(t, cwd, slug)
		promptDcloseSeedState(t, cwd, "s1", slug, "c-unreadable-pabcd")
		receipt := promptDcloseReceipt(t, cwd, "s1", "c-unreadable-pabcd")
		attest := promptDcloseAttest("wp-1", receipt)
		promptDcloseStopAfterPabcdAppend(t, cwd, "s1", "t1", attest)
		restore := promptDcloseMakeUnreadable(t, filepath.Join(cwd, ".crw", state.LedgerFile))
		answer := promptDcloseRun(t, cwd, "s1", "t2", attest)
		restore()
		if !strings.Contains(answer, "finalization is pending") {
			t.Errorf("the unreadable PABCD ledger answered %q, want the finalization-pending text", answer)
		}
		if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil {
			t.Errorf("the retry cleared the recovery marker: %+v", s)
		}
	})
}

// TestPromptDcloseUnreadablePabcdLedgerWarnsOnAllDoneClose is finding 1 on the all-done close: it
// leaves no marker, so no retry can finish its close row; the answer is the success directive plus
// the ledger warning (the CRW-797 generation-2 rule), where the baseline answers the success
// directive alone and appends a second row.
func TestPromptDcloseUnreadablePabcdLedgerWarnsOnAllDoneClose(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-unreadable-all-done"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "all done " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-all-done-unreadable")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-all-done-unreadable")
	// The PABCD ledger holds no close row yet, so give it one unrelated row: the file must exist for
	// the read to fail with EACCES rather than ENOENT, which is the absent case the control covers.
	ledger := filepath.Join(cwd, ".crw", state.LedgerFile)
	promptDcloseWrite(t, cwd, filepath.Join(".crw", state.LedgerFile),
		"{\"ts\":\"2026-01-01T00:00:00.000Z\",\"sessionId\":\"s1\",\"from\":\"IDLE\",\"to\":\"P\",\"reason\":\"chat\"}\n")
	restore := promptDcloseMakeUnreadable(t, ledger)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("", receipt))
	restore()
	if !strings.Contains(answer, "[crw: DONE]") {
		t.Errorf("the all-done close did not answer the success directive: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseLedgerWarning("")) {
		t.Errorf("the all-done close did not carry the ledger warning: %q", answer)
	}
}

// TestPromptDclosePublishedMarkerWriteCompletesTheClose is finding 2a: a marker write that reports
// a *state.PublishedError published the state and only failed a step after the rename, so the close
// counts it as done, goes on, and carries the directory-sync warning. The baseline answers
// "Nothing was written" and stops.
func TestPromptDclosePublishedMarkerWriteCompletesTheClose(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-published-marker"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-published-marker")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-published-marker")
	seams := &promptDcloseSeams{writeMarker: func(string, state.State, string, *string) error {
		return &state.PublishedError{Err: syscall.EIO}
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if strings.Contains(answer, "Nothing was written") {
		t.Errorf("a published marker write answered %q", answer)
	}
	if !strings.Contains(answer, "[crw: DONE]") {
		t.Errorf("a published marker write did not complete the close: %q", answer)
	}
	if !strings.Contains(answer, "published but its directory could not be synced") {
		t.Errorf("a published marker write carried no durability warning: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle {
		t.Errorf("a published marker write did not rest the session at IDLE: %+v", s)
	}
}

// TestPromptDclosePublishedPlanWriteCompletesTheClose is finding 2b: the same failure on the plan
// write. The plan commit counts as done, the close goes on, and the answer carries the warning.
func TestPromptDclosePublishedPlanWriteCompletesTheClose(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-published-plan"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-published-plan")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-published-plan")
	seams := &promptDcloseSeams{writePlan: func(string, *goalplan.Goalplan) error {
		return &state.PublishedError{Err: syscall.EIO}
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if strings.Contains(answer, "Nothing was written") {
		t.Errorf("a published plan write answered %q", answer)
	}
	if !strings.Contains(answer, "[crw: DONE]") {
		t.Errorf("a published plan write did not complete the close: %q", answer)
	}
	if !strings.Contains(answer, "published but its directory could not be synced") {
		t.Errorf("a published plan write carried no durability warning: %q", answer)
	}
}

// TestPromptDcloseBusyStopBudgetStampAnswersTheBoundRefusal is finding 3: the leading section's
// Stop-budget stamp takes the session lock before the bound D-close handler, so a lock that is
// already busy fails there. On a goalplan-bound "orchestrate D" the answer must be the bound
// D-close's busy refusal, not the empty string the baseline returns.
func TestPromptDcloseBusyStopBudgetStampAnswersTheBoundRefusal(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-busy-stamp"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-busy-stamp")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-busy-stamp")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	// A new turn id, so the stamp's own guard (a turn already stamped is skipped) does not fire and
	// the stamp really takes the lock.
	answer, panicked := promptDcloseRunLocked(t, cwd, "s1", "t-busy", promptDcloseAttest("wp-1", receipt), promptDcloseLockFailingAfter(0))
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if answer == "" {
		t.Fatalf("a busy Stop-budget stamp on a bound D answered nothing")
	}
	if want := promptDcloseNotApplied("lock unavailable"); answer != want {
		t.Errorf("the busy Stop-budget stamp\n got %q\nwant %q", answer, want)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a busy Stop-budget stamp")
}

// TestPromptDcloseAbsentGoalplanLedgerStillGetsItsRows is the first control of finding 1: ENOENT
// really is absence, so a close whose plan ledger was never written still appends its rows.
func TestPromptDcloseAbsentGoalplanLedgerStillGetsItsRows(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-absent-goalplan-ledger"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-absent")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-absent")
	if _, err := os.Stat(promptDcloseGoalplanLedgerPath(t, cwd, slug)); !os.IsNotExist(err) {
		t.Fatalf("the plan ledger exists before the close: %v", err)
	}
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if strings.Contains(answer, "finalization is pending") {
		t.Errorf("an absent plan ledger was read as unreadable: %q", answer)
	}
	if !strings.Contains(answer, "[crw: DONE]") {
		t.Errorf("the close did not complete: %q", answer)
	}
	rows := promptDcloseGoalplanRows(t, cwd, slug)
	if len(rows) != 2 || rows[0]["detail"] != "closed wp-1" || rows[1]["detail"] != "started wp-2" {
		t.Errorf("the goalplan rows of an absent-ledger close: %+v", rows)
	}
}

// TestPromptDclosePrePublicationMarkerFailureStillRefuses is the second control of finding 2: an
// error that is not a *state.PublishedError means the write never reached its final path, so the
// close still refuses with nothing written.
func TestPromptDclosePrePublicationMarkerFailureStillRefuses(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-prepublication-marker"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-prepublication")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-prepublication")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	seams := &promptDcloseSeams{writeMarker: func(string, state.State, string, *string) error {
		return errors.New("the marker could not be written before the rename")
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "Nothing was written") {
		t.Errorf("a pre-publication marker failure answered %q", answer)
	}
	if strings.Contains(answer, "[crw: DONE]") {
		t.Errorf("a pre-publication marker failure completed the close: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a pre-publication marker failure")
}

// TestPromptDcloseUnbusyCloseIsUnchanged is the third control: with no seam and a free lock the
// close completes exactly as before, resting at IDLE with the marker cleared.
func TestPromptDclosePrePublicationPlanFailureStillRefuses(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-prepublication-plan"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-prepublication-plan")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-prepublication-plan")
	beforePlan := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug))
	seams := &promptDcloseSeams{writePlan: func(string, *goalplan.Goalplan) error {
		return errors.New("the goalplan could not be written before the rename")
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "Nothing was written") {
		t.Errorf("a pre-publication plan failure answered %q", answer)
	}
	if strings.Contains(answer, "[crw: DONE]") {
		t.Errorf("a pre-publication plan failure completed the close: %q", answer)
	}
	// The recovery marker was written before the plan write (the oracle's own order), so only the
	// plan and the ledgers are asserted unchanged here; the marker stays for a retry.
	if after := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug)); after != beforePlan {
		t.Errorf("a pre-publication plan failure rewrote the plan:\n got %q\nwant %q", after, beforePlan)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("a pre-publication plan failure wrote a PABCD row: %+v", rows)
	}
	if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
		t.Errorf("a pre-publication plan failure wrote a goalplan row: %+v", rows)
	}
}

// TestPromptDcloseUnbusyCloseIsUnchanged is the third control: with no seam and a free lock the
// close completes exactly as before, resting at IDLE with the marker cleared.
func TestPromptDcloseUnbusyCloseIsUnchanged(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-unbusy-unchanged"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-unbusy")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-unbusy")
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "[crw: DONE]") || !strings.Contains(answer, "IPABCD: IDLE") {
		t.Errorf("the unbusy close: %q", answer)
	}
	if strings.Contains(answer, "warning") {
		t.Errorf("the unbusy close carried a durability warning: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle || s.DcloseRecovery != nil || s.CheckEpoch != nil {
		t.Errorf("the resting state: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 {
		t.Errorf("the close rows: %+v", rows)
	}
}
