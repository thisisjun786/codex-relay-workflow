// prompt_dclose_test.go is the Go form of the chat D-close cases of CXC v0.2.40
// pabcd-state/test/hook.test.ts for the unit prompt_dclose.go owns: the normal path of a bound
// chat D-close (hook.ts:939-989, 1137-1286, 1302-1354, 1394-1396), with the recovery branch in
// prompt_dclose_recovery_test.go. Every case drives the handler through promptSubmitHandleWith,
// the way the leg runs it, so the whole path is exercised: the Stop-budget stamp, the turn guard,
// the command seam, the receipt gate, the goalplan lock, the state write and the ledgers.
package hook

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptDcloseGitEnv isolates git from the machine, the way the other hook tests do: no inherited
// routing or configuration, a fixed identity, discovery stopping at the returned directory.
func promptDcloseGitEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS"} {
		t.Setenv(name, "")
		_ = os.Unsetenv(name)
	}
	base := t.TempDir()
	for name, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CEILING_DIRECTORIES": base,
		"GIT_AUTHOR_NAME": "fixture", "GIT_AUTHOR_EMAIL": "fixture@example.invalid",
		"GIT_COMMITTER_NAME": "fixture", "GIT_COMMITTER_EMAIL": "fixture@example.invalid"} {
		t.Setenv(name, value)
	}
}

// promptDcloseGit runs one git command in dir, failing the test when it does not succeed.
func promptDcloseGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// promptDcloseRepo is gitRepoForHook: a repository whose only tracked file is src.txt, which the
// check receipt is bound to. The test's own temporary directory, so nothing touches a real home.
func promptDcloseRepo(t *testing.T) string {
	t.Helper()
	promptDcloseGitEnv(t)
	root := t.TempDir()
	promptDcloseWrite(t, root, "src.txt", "source\n")
	promptDcloseGit(t, root, "init", "-q", "-b", "main")
	promptDcloseGit(t, root, "add", "-A")
	promptDcloseGit(t, root, "commit", "-qm", "initial")
	return root
}

// promptDcloseWrite writes one file under root, creating its directory.
func promptDcloseWrite(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// promptDclosePlan writes a bound goalplan: one work-phase in progress holding the listed tasks,
// with the cursor on it.
func promptDclosePlan(t *testing.T, cwd, slug string, tasks []goalplan.GoalplanTask) *goalplan.Goalplan {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "chat dclose " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseInProgress, Tasks: tasks, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	return plan
}

// promptDcloseReadPlan reads a plan back from disk.
func promptDcloseReadPlan(t *testing.T, cwd, slug string) *goalplan.Goalplan {
	t.Helper()
	plan := goalplan.ReadGoalplan(cwd, slug)
	if plan == nil {
		t.Fatalf("goalplan %s is not readable", slug)
	}
	return plan
}

// promptDcloseStr boxes a string for a *string field.
func promptDcloseStr(s string) *string { return &s }

// promptDcloseSeedState writes the session state a bound close starts from: phase C with a check
// epoch, the slug bound and the gate flags the C>D edge unlocked.
func promptDcloseSeedState(t *testing.T, cwd, sessionID, slug, epoch string) {
	t.Helper()
	promptSubmitStateFile(t, cwd, sessionID, func(s *state.State) {
		s.Phase, s.Slug, s.OrchestrationActive = state.PhaseC, slug, true
		s.CheckEpoch = promptDcloseStr(epoch)
		s.Flags = state.Flags{Interview: false, AuditPassed: true, CheckPassed: true}
		s.StopBlockPhase, s.StopBlockCount = promptDclosePhase(state.PhaseC), 3
	})
}

// promptDclosePhase boxes a phase for a *Phase field.
func promptDclosePhase(p state.Phase) *state.Phase { return &p }

// promptDcloseReceipt writes the test receipt the C>D gate needs for this session and check cycle,
// captured over the repository's current source, and answers its cwd-relative path.
func promptDcloseReceipt(t *testing.T, cwd, sessionID, epoch string) string {
	t.Helper()
	rel := filepath.ToSlash(filepath.Join(crwdir.DirName, "evidence", sessionID, "test-receipt.json"))
	body := map[string]any{
		"kind": "test", "command": "go test ./...", "exitCode": 0,
		"createdAt":      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"sourceIdentity": source.Capture(cwd, source.Options{ExcludeStateArtifacts: true}),
		"ownerSessionId": sessionID, "checkEpoch": epoch,
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	promptDcloseWrite(t, cwd, rel, string(data))
	return rel
}

// promptDcloseAttest is the attestation a bound chat D-close carries, in the grammar's JSON form.
func promptDcloseAttest(workPhaseID, receiptPath string) string {
	body := map[string]any{"from": "C", "to": "D", "did": "ran the suite", "checkOutput": "ok", "exitCode": 0}
	if workPhaseID != "" {
		body["workPhaseId"] = workPhaseID
	}
	if receiptPath != "" {
		body["testReceiptPath"] = receiptPath
	}
	data, _ := json.Marshal(body)
	return string(data)
}

// promptDcloseClose drives one bound chat D-close the way the leg does, with the seams the caller
// names. It returns the injected context and the panic a seam reported, if any.
func promptDcloseRunWith(t *testing.T, cwd, sessionID, turn, attest string, seams *promptDcloseSeams) (string, any) {
	t.Helper()
	prompt := "orchestrate d --attest " + attest
	var answer string
	panicked := catchPanic(func() {
		answer = promptSubmitHandleWith(PromptSubmitPayload{Cwd: cwd, SessionID: sessionID, Prompt: prompt,
			TurnID: turn, PabcdEnabled: true}, "", promptSubmitHost(cwd), state.WithSessionLock, seams)
	})
	return answer, panicked
}

// promptDcloseClose is promptDcloseRunWith without a seam.
func promptDcloseRun(t *testing.T, cwd, sessionID, turn, attest string) string {
	t.Helper()
	answer, panicked := promptDcloseRunWith(t, cwd, sessionID, turn, attest, nil)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	return answer
}

// catchPanic runs fn and answers what it panicked with, or nil.
func catchPanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

// promptDcloseGoalplanRows reads the bound plan's ledger rows.
func promptDcloseGoalplanRows(t *testing.T, cwd, slug string) []map[string]any {
	t.Helper()
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	return promptDcloseRows(t, filepath.Join(dir, goalplan.GoalplanLedgerFile))
}

// promptDcloseRows reads one JSONL file into decoded rows; an absent file is no rows.
func promptDcloseRows(t *testing.T, path string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// promptDcloseDirs is what a refusal must leave alone: the bound plan and both ledgers byte for
// byte, and the phase and the recovery marker of the session. The session file's own Stop-budget
// stamp is the leading section's write (hook.ts:688-691), which the oracle performs before the
// command runs and which no refusal undoes, so it is judged by its fields and not by its bytes.
type promptDcloseDirs struct {
	plan, pabcd, goalplan string
	phase                 state.Phase
	marker                string
}

func promptDcloseSnapshot(t *testing.T, cwd, sessionID, slug string) promptDcloseDirs {
	t.Helper()
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	return promptDcloseDirs{
		plan:     promptDcloseFileText(filepath.Join(dir, goalplan.GoalplanFile)),
		pabcd:    promptDcloseFileText(filepath.Join(cwd, crwdir.DirName, state.LedgerFile)),
		goalplan: promptDcloseFileText(filepath.Join(dir, goalplan.GoalplanLedgerFile)),
		phase:    state.ReadState(cwd, sessionID).Phase,
		marker:   promptDcloseMarkerText(state.ReadState(cwd, sessionID)),
	}
}

// promptDcloseMarkerText is the session's recovery marker as text, so an unchanged absence and an
// unchanged presence both compare equal.
func promptDcloseMarkerText(s state.State) string {
	if s.DcloseRecovery == nil {
		return ""
	}
	return s.DcloseRecovery.SessionID + "|" + s.DcloseRecovery.CheckEpoch + "|" + s.DcloseRecovery.ClosedWorkPhaseID + "|" + promptDcloseString(s.DcloseRecovery.NextWorkPhaseID)
}

// promptDcloseUnchanged fails when any of the three moved.
func promptDcloseUnchanged(t *testing.T, before promptDcloseDirs, after promptDcloseDirs, what string) {
	t.Helper()
	if before != after {
		t.Errorf("%s wrote something:\n before %+v\n  after %+v", what, before, after)
	}
}

// promptDcloseFileText reads a file as text; an absent file is the empty text, so an unchanged
// absence compares equal to an unchanged absence.
func promptDcloseFileText(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(raw)
}

// errPromptDcloseSeam is the panic a seam reports when it wants the close to stop there, the Go form
// of the throw the oracle's own hook.test.ts seam callbacks raise.
var errPromptDcloseSeam = errors.New("promptDclose: the seam stopped the close")

// promptDcloseStop is a seam that stops the close and reports that it did.
func promptDcloseStop() func() { return func() { panic(errPromptDcloseSeam) } }

// TestPromptDcloseMissingReceiptIsRefused is hook.test.ts "chat D-close is refused while the
// work-phase has open tasks" in its receipt half and the CHECK-BINDING-01 rule: a bound C>D
// without the receipt it names is refused before the goalplan is touched, and writes nothing.
func TestPromptDcloseMissingReceiptIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-missing-receipt"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-test")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", ""))
	if !strings.Contains(answer, "refused") || !strings.Contains(answer, `requires "testReceiptPath"`) {
		t.Errorf("the receipt refusal: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC || s.DcloseRecovery != nil {
		t.Errorf("a refused close moved the state: %+v", s)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a receipt refusal")
}

// TestPromptDcloseAllDonePlanClosesWithoutATarget is the oracle's all-done case (hook.test.ts
// "all-done bound chat closes without a marker"): an all-done plan closes the cycle, writes no
// recovery marker and no goalplan row, and leaves the plan bytes untouched.
func TestPromptDcloseAllDonePlanClosesWithoutATarget(t *testing.T) {
	for _, workPhaseID := range []string{"", "wp-finished"} {
		t.Run("workPhaseId="+workPhaseID, func(t *testing.T) {
			cwd := promptDcloseRepo(t)
			slug := "chat-all-done"
			plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "close an all-done chat cycle"})
			plan.Slug = slug
			plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-finished", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
			if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
				t.Fatal(err)
			}
			promptDcloseSeedState(t, cwd, "s1", slug, "c-all-done")
			receipt := promptDcloseReceipt(t, cwd, "s1", "c-all-done")
			beforePlan := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug))
			answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest(workPhaseID, receipt))
			if strings.Contains(answer, "refused") {
				t.Errorf("an all-done close refused: %q", answer)
			}
			if !strings.Contains(answer, "[crw: DONE]") || !strings.Contains(answer, "IPABCD: IDLE") {
				t.Errorf("the DONE directive: %q", answer)
			}
			s := state.ReadState(cwd, "s1")
			if s.Phase != state.PhaseIdle || s.DcloseRecovery != nil || s.CheckEpoch != nil {
				t.Errorf("the resting state: %+v", s)
			}
			if after := promptDcloseFileText(promptDclosePlanPath(t, cwd, slug)); after != beforePlan {
				t.Errorf("an all-done close rewrote the plan:\n got %q\nwant %q", after, beforePlan)
			}
			if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
				t.Errorf("an all-done close wrote a goalplan row: %+v", rows)
			}
		})
	}
}

// TestPromptDcloseAllDoneRecordsTheNullCloseKey is hook.test.ts "all-done bound chat records
// closedWorkPhaseId null even when workPhaseId is provided": the close row carries the check
// epoch and a null closed phase, whatever the attest named.
func TestPromptDcloseAllDoneRecordsTheNullCloseKey(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-all-done-ledger"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "close an all-done cycle"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-finished", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-all-done-null")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-all-done-null")
	promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-finished", receipt))
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 {
		t.Fatalf("the close rows: %+v", rows)
	}
	if rows[0]["checkEpoch"] != "c-all-done-null" || rows[0]["closedWorkPhaseId"] != nil || rows[0]["reason"] != "done" {
		t.Errorf("the close row: %+v", rows[0])
	}
}

// TestPromptDcloseOpenTasksRefuseAndWriteNothing is hook.test.ts "chat D-close is refused while
// the work-phase has open tasks, and writes nothing": the refusal names the open task and the
// CYCLE-COMPLETION-01 rule, and neither the state, the plan nor a ledger row moves.
func TestPromptDcloseOpenTasksRefuseAndWriteNothing(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-cycle-pending"
	promptDclosePlan(t, cwd, slug, []goalplan.GoalplanTask{{ID: "t-1", Title: "the work", Status: goalplan.TaskPending}})
	promptDcloseSeedState(t, cwd, "s1", slug, "c-test")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-test")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	for _, want := range []string{"refused", "open task", "CYCLE-COMPLETION-01", "t-1 (the work)"} {
		if !strings.Contains(answer, want) {
			t.Errorf("the refusal %q does not mention %q", answer, want)
		}
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC {
		t.Errorf("a refused close moved the phase: %+v", s)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "an open-task refusal")
}

// TestPromptDcloseSucceedsOnceTheTasksAreDone is hook.test.ts "chat D-close succeeds once the
// tasks are done": the cycle closes, the work phase is done on disk, and the marker the write
// left names the target it closed.
func TestPromptDcloseSucceedsOnceTheTasksAreDone(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-cycle-done"
	promptDclosePlan(t, cwd, slug, []goalplan.GoalplanTask{{ID: "t-1", Title: "the work", Status: goalplan.TaskDone, Outcome: "focused tests passed"}})
	promptDcloseSeedState(t, cwd, "s1", slug, "c-test")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-test")
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if strings.Contains(answer, "refused") {
		t.Fatalf("the close refused: %q", answer)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseIdle {
		t.Errorf("the resting phase: %s", s.Phase)
	}
	if saved := promptDcloseReadPlan(t, cwd, slug); saved.WorkPhases[0].Status != goalplan.WorkPhaseDone {
		t.Errorf("the work phase was not closed: %+v", saved.WorkPhases[0])
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["from"] != "C" || rows[0]["to"] != "IDLE" || rows[0]["reason"] != "done" {
		t.Errorf("the close row: %+v", rows)
	}
	if rows[0]["checkEpoch"] != "c-test" || rows[0]["closedWorkPhaseId"] != "wp-1" {
		t.Errorf("the close key: %+v", rows[0])
	}
	goalplanRows := promptDcloseGoalplanRows(t, cwd, slug)
	if len(goalplanRows) != 1 || goalplanRows[0]["event"] != "workphase_done" || goalplanRows[0]["detail"] != "closed wp-1" {
		t.Errorf("the goalplan rows: %+v", goalplanRows)
	}
}

// promptDclosePlanPath is the bound plan's goalplan.json.
func promptDclosePlanPath(t *testing.T, cwd, slug string) string {
	t.Helper()
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, goalplan.GoalplanFile)
}

// TestPromptDcloseWithoutWorkPhaseIDIsRefused is hook.test.ts "bound chat D-close without
// workPhaseId is refused after empty and all-done checks": the target is required only after an
// all-done plan has been ruled out, and the refusal writes nothing.
func TestPromptDcloseWithoutWorkPhaseIDIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-missing-target"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-test")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-test")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("", receipt))
	if !strings.Contains(answer, "requires attest.workPhaseId") {
		t.Errorf("the refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a missing-target refusal")
}

// TestPromptDcloseTargetAbsentIsRefused is the oracle's target-not-found refusal: a work phase
// that is not in the bound plan refuses by name and writes nothing.
func TestPromptDcloseTargetAbsentIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-target-absent"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-test")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-test")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-ghost", receipt))
	if !strings.Contains(answer, "work-phase wp-ghost is not in the bound goalplan") {
		t.Errorf("the refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "an absent-target refusal")
}

// TestPromptDcloseTargetMismatchIsRefused is the oracle's fixed-close-target check: the attest
// names wp-1 but the cursor is on wp-2, so the close refuses rather than closing the wrong phase.
func TestPromptDcloseTargetMismatchIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-target-mismatch"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "mismatch"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-test")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-test")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "does not match active work-phase wp-2") {
		t.Errorf("the refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a target-mismatch refusal")
}

// TestPromptDcloseWithoutCheckEpochIsRefused is the oracle's C epoch requirement: a session that
// is not in C with an epoch cannot close, and the refusal writes nothing.
func TestPromptDcloseWithoutCheckEpochIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-no-epoch"
	promptDclosePlan(t, cwd, slug, nil)
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.Slug, s.OrchestrationActive = state.PhaseC, slug, true
		s.CheckEpoch = nil
		s.Flags = state.Flags{AuditPassed: true, CheckPassed: true}
	})
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-test")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "this check cycle predates CHECK-BINDING-01") {
		t.Errorf("the refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a no-epoch refusal")
}

// TestPromptDcloseEmptyPlanIsRefused is the oracle's empty-plan refusal: a bound plan with no
// work phase cannot close a cycle, and nothing is written.
func TestPromptDcloseEmptyPlanIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-empty-plan"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "empty"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-test")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-test")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "has no active work-phase to close") {
		t.Errorf("the refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "an empty-plan refusal")
}

// TestPromptDcloseInvalidDependencyPlanIsRefused is hook.test.ts "chat D-close rejects an invalid
// v3 dependency plan before every write": the integrity check runs inside the first goalplan lock
// and leaves all four files exactly as they were.
func TestPromptDcloseInvalidDependencyPlanIsRefused(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "invalid-v3-chat-close"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "invalid chat dependency close"})
	plan.Slug = slug
	plan.SchemaVersion = promptDcloseFloat(3)
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-1", Title: "broken", Status: goalplan.WorkPhaseInProgress, DependsOn: []string{"missing"}, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-invalid")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-invalid")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "invalid goalplan") {
		t.Errorf("the refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "an integrity refusal")
}

// promptDcloseFloat boxes a float64 for a *float64 field.
func promptDcloseFloat(v float64) *float64 { return &v }

// TestPromptDcloseCommitsMarkerPlanAndRowsInOrder is the oracle's write order for a fresh close
// (hook.ts:1229-1264): the recovery marker lands first, then the plan, then the workphase_done
// row and the workphase_started row the successor it chose names.
func TestPromptDcloseCommitsMarkerPlanAndRowsInOrder(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-order"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "order"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-order")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-order")
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if strings.Contains(answer, "refused") {
		t.Fatalf("the close refused: %q", answer)
	}
	rows := promptDcloseGoalplanRows(t, cwd, slug)
	if len(rows) != 2 {
		t.Fatalf("the goalplan rows: %+v", rows)
	}
	if rows[0]["event"] != "workphase_done" || rows[0]["detail"] != "closed wp-1" {
		t.Errorf("row 0: %+v", rows[0])
	}
	if rows[1]["event"] != "workphase_started" || rows[1]["detail"] != "started wp-2" {
		t.Errorf("row 1: %+v", rows[1])
	}
	saved := promptDcloseReadPlan(t, cwd, slug)
	if saved.WorkPhases[0].Status != goalplan.WorkPhaseDone || saved.WorkPhases[1].Status != goalplan.WorkPhaseInProgress {
		t.Errorf("the committed plan: %+v", saved.WorkPhases)
	}
	if saved.ActiveWorkPhaseID == nil || *saved.ActiveWorkPhaseID != "wp-2" {
		t.Errorf("the cursor: %v", saved.ActiveWorkPhaseID)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery != nil || s.CheckEpoch != nil {
		t.Errorf("the finalization did not clear the marker: %+v", s.DcloseRecovery)
	}
}

// TestPromptDcloseRepeatedRequestAddsNoRow is the oracle's idempotency: running the same request
// twice appends no second close row, and the second run is silent because the leading section's
// turn guard answers an already-injected turn.
func TestPromptDcloseRepeatedRequestAddsNoRow(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-repeat"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-repeat")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-repeat")
	attest := promptDcloseAttest("wp-1", receipt)
	promptDcloseRun(t, cwd, "s1", "t1", attest)
	if answer := promptDcloseRun(t, cwd, "s1", "t1", attest); answer != "" {
		t.Errorf("the same turn answered again: %q", answer)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 {
		t.Errorf("the close rows: %+v", rows)
	}
	// A later turn on the resting IDLE session is refused as an illegal adjacency and still
	// appends nothing.
	promptDcloseRun(t, cwd, "s1", "t2", attest)
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 {
		t.Errorf("a second turn appended a row: %+v", rows)
	}
}

// TestPromptDcloseKeepsTheExistingStateFields is hook.test.ts "chat D-close keeps same-turn dedup
// and clears the Stop guard": the replacement state write must not drop injectedTurns or the
// stopBlock reset.
func TestPromptDcloseKeepsTheExistingStateFields(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-state-fields"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-fields")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-fields")
	promptDcloseRun(t, cwd, "s1", "same-turn", promptDcloseAttest("wp-1", receipt))
	s := state.ReadState(cwd, "s1")
	if s.StopBlockPhase != nil || s.StopBlockWorkPhaseID != nil || s.StopBlockCount != 0 {
		t.Errorf("the Stop guard survived the close: %+v", s)
	}
	if len(s.InjectedTurns) != 1 || s.InjectedTurns[0] != "same-turn" {
		t.Errorf("the recorded turns: %+v", s.InjectedTurns)
	}
	if s.OrchestrationActive || s.LastInjectedPhase != nil || s.PhaseEntrySource != nil || s.PlanUnit != nil || s.PlanEpoch != nil {
		t.Errorf("the resting state: %+v", s)
	}
}

// TestPromptDcloseBusyGoalplanLockWritesNothing is hook.test.ts "chat D-close lock timeout keeps
// phase C, emits a warning, and writes no ledger": a held goalplan lock answers the
// D-close-was-not-applied text with the busy reason and changes nothing.
func TestPromptDcloseBusyGoalplanLockWritesNothing(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-lock-timeout"
	promptDclosePlan(t, cwd, slug, []goalplan.GoalplanTask{{ID: "t-1", Title: "the work", Status: goalplan.TaskDone}})
	promptDcloseSeedState(t, cwd, "s1", slug, "c-lock")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-lock")
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, ".goalplan.lock")
	if err := os.Mkdir(lock, 0o777); err != nil {
		t.Fatal(err)
	}
	promptDcloseWrite(t, cwd, filepath.Join(".crw", "goalplans", slug, ".goalplan.lock", "owner.json"), "{\"pid\":4242}\n")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "D-close was not applied") || !strings.Contains(answer, ".goalplan.lock") {
		t.Errorf("the busy refusal: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseC {
		t.Errorf("a busy lock moved the phase: %s", s.Phase)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a busy goalplan lock")
}

// TestPromptDcloseBusySessionLockWritesNothing is the port's own lock rule: a session lock the
// bound close cannot take answers the same D-close text with the busy reason and writes nothing,
// where the oracle holds no lock at all. The leading section's own Stop-budget stamp takes the lock
// first (hook.ts:688-691), so the failure is armed on the close's own acquisition.
func TestPromptDcloseBusySessionLockWritesNothing(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-session-lock"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-session-lock")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-session-lock")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer, panicked := promptDcloseRunLocked(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), promptDcloseLockFailingAfter(1))
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "D-close was not applied") || !strings.Contains(answer, "lock unavailable") {
		t.Errorf("the busy refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a busy session lock")
}

// promptDcloseLockFailingAfter is a session lock that grants the first n acquisitions and refuses
// the next one, so a test can arm the failure on the bound close's own acquisition rather than on
// the leading section's Stop-budget stamp.
func promptDcloseLockFailingAfter(n int) func(cwd, sessionID string, fn func() error) error {
	granted := 0
	return func(cwd, sessionID string, fn func() error) error {
		if granted >= n {
			return errors.New("lock unavailable")
		}
		granted++
		return state.WithSessionLock(cwd, sessionID, fn)
	}
}

// promptDcloseRunLocked drives one bound close with the caller's own session lock.
func promptDcloseRunLocked(t *testing.T, cwd, sessionID, turn, attest string, lock func(cwd, sessionID string, fn func() error) error) (string, any) {
	t.Helper()
	var answer string
	panicked := catchPanic(func() {
		answer = promptSubmitHandleWith(PromptSubmitPayload{Cwd: cwd, SessionID: sessionID,
			Prompt: "orchestrate d --attest " + attest, TurnID: turn, PabcdEnabled: true},
			"", promptSubmitHost(cwd), lock, nil)
	})
	return answer, panicked
}

// TestPromptDcloseSeamsStopAfterEachWrite is the oracle's four commit seams (hook.test.ts
// "chat D-close retry after the recovery marker write" and "retry after state write" and "retry
// after PABCD append"): each seam fires after its own write and before the next one, so a close
// stopped there leaves exactly the state the oracle's stopped close leaves.
func TestPromptDcloseSeamsStopAfterEachWrite(t *testing.T) {
	type want struct {
		name         string
		seams        *promptDcloseSeams
		phase        state.Phase
		marker       bool
		planClosed   bool
		pabcdRows    int
		goalplanRows int
	}
	cases := []want{
		{name: "afterRecoveryMarkerWrite", seams: &promptDcloseSeams{afterRecoveryMarkerWrite: promptDcloseStop()},
			phase: state.PhaseC, marker: true, planClosed: false, pabcdRows: 0, goalplanRows: 0},
		{name: "afterGoalplanCommit", seams: &promptDcloseSeams{afterGoalplanCommit: promptDcloseStop()},
			phase: state.PhaseC, marker: true, planClosed: true, pabcdRows: 0, goalplanRows: 0},
		{name: "afterStateWrite", seams: &promptDcloseSeams{afterStateWrite: promptDcloseStop()},
			phase: state.PhaseIdle, marker: true, planClosed: true, pabcdRows: 0, goalplanRows: 0},
		{name: "afterPabcdLedgerAppend", seams: &promptDcloseSeams{afterPabcdLedgerAppend: promptDcloseStop()},
			phase: state.PhaseIdle, marker: true, planClosed: true, pabcdRows: 1, goalplanRows: 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cwd := promptDcloseRepo(t)
			slug := "chat-seam-" + c.name
			plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "seam"})
			plan.Slug = slug
			plan.WorkPhases = []goalplan.GoalplanWorkPhase{
				{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
				{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
			}
			plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
			if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
				t.Fatal(err)
			}
			promptDcloseSeedState(t, cwd, "s1", slug, "c-seam")
			receipt := promptDcloseReceipt(t, cwd, "s1", "c-seam")
			_, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), c.seams)
			if panicked != errPromptDcloseSeam {
				t.Fatalf("the seam did not stop the close: %v", panicked)
			}
			s := state.ReadState(cwd, "s1")
			if s.Phase != c.phase {
				t.Errorf("the phase after %s: %s, want %s", c.name, s.Phase, c.phase)
			}
			if (s.DcloseRecovery != nil) != c.marker {
				t.Errorf("the marker after %s: %+v, want present=%v", c.name, s.DcloseRecovery, c.marker)
			}
			saved := promptDcloseReadPlan(t, cwd, slug)
			if closed := saved.WorkPhases[0].Status == goalplan.WorkPhaseDone; closed != c.planClosed {
				t.Errorf("the plan after %s: closed=%v, want %v", c.name, closed, c.planClosed)
			}
			if rows := promptOrchestrateLedger(t, cwd); len(rows) != c.pabcdRows {
				t.Errorf("the PABCD rows after %s: %+v", c.name, rows)
			}
			if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != c.goalplanRows {
				t.Errorf("the goalplan rows after %s: %+v", c.name, rows)
			}
		})
	}
}

// TestPromptDcloseStoppedCloseIsFinishedByTheSameRequest is hook.test.ts "chat D-close retry
// after the recovery marker write matches an uninterrupted close": the same request finishes the
// close the marker describes and leaves one close row.
func TestPromptDcloseStoppedCloseIsFinishedByTheSameRequest(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-retry-after-marker"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "retry"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-retry")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-retry")
	attest := promptDcloseAttest("wp-1", receipt)
	if _, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", attest, &promptDcloseSeams{afterRecoveryMarkerWrite: promptDcloseStop()}); panicked != errPromptDcloseSeam {
		t.Fatalf("the marker seam did not fire: %v", panicked)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseC || s.DcloseRecovery == nil {
		t.Fatalf("the stopped close: %+v", s)
	}
	if s.DcloseRecovery.ClosedWorkPhaseID != "wp-1" || promptDcloseString(s.DcloseRecovery.NextWorkPhaseID) != "wp-2" {
		t.Errorf("the marker: %+v", s.DcloseRecovery)
	}
	if saved := promptDcloseReadPlan(t, cwd, slug); saved.WorkPhases[0].Status != goalplan.WorkPhaseInProgress {
		t.Errorf("the plan moved before the commit: %+v", saved.WorkPhases[0])
	}
	answer := promptDcloseRun(t, cwd, "s1", "t2", attest)
	if strings.Contains(answer, "refused") {
		t.Fatalf("the retry refused: %q", answer)
	}
	rows := promptOrchestrateLedger(t, cwd)
	if len(rows) != 1 || rows[0]["closedWorkPhaseId"] != "wp-1" {
		t.Errorf("the close rows: %+v", rows)
	}
	if started := promptDcloseGoalplanRows(t, cwd, slug); len(started) != 2 || started[1]["detail"] != "started wp-2" {
		t.Errorf("the goalplan rows: %+v", started)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle || s.DcloseRecovery != nil {
		t.Errorf("the finished state: %+v", s)
	}
}

// TestPromptDcloseRetryAfterThePabcdAppendKeepsOneRow is hook.test.ts "chat D-close retry after
// PABCD append keeps one close row": a close stopped after its close row leaves the marker in
// place, and the retry sees the row already there.
func TestPromptDcloseRetryAfterThePabcdAppendKeepsOneRow(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-retry-append"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-retry-append")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-retry-append")
	attest := promptDcloseAttest("wp-1", receipt)
	if _, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", attest, &promptDcloseSeams{afterPabcdLedgerAppend: promptDcloseStop()}); panicked != errPromptDcloseSeam {
		t.Fatalf("the append seam did not fire: %v", panicked)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 {
		t.Fatalf("the stopped close rows: %+v", rows)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil {
		t.Fatalf("the marker was cleared before the finalization finished: %+v", s)
	}
	promptDcloseRun(t, cwd, "s1", "t2", attest)
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 1 {
		t.Errorf("the retry appended a second close row: %+v", rows)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery != nil || s.CheckEpoch != nil {
		t.Errorf("the retry did not finish the cleanup: %+v", s.DcloseRecovery)
	}
}

// TestPromptDcloseRefusesARewriteTheReaderWouldLose is the data-loss rule the parity revision of
// 2026-10-03 fixes during the port: the oracle writes the whole state back with no lock, so a
// stored record the reader cannot keep - here a legacy D-close marker, which the reader rebuilds
// without its distinction - is lost with the write. This writer refuses and writes nothing.
func TestPromptDcloseRefusesARewriteTheReaderWouldLose(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-lossy-state"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-lossy")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-lossy")
	promptDcloseWrite(t, cwd, ".crw/sessions/s1.json", promptDcloseLossyState(slug, "c-lossy"))
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Errorf("the data-loss refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a lossy-state refusal")
}

// promptDcloseLossyState is a bound C session whose stored unverified-subagent record carries a
// receiptClaimed longer than the reader keeps, so a write-back would publish it truncated. No
// recovery marker is present, so the refusal comes from the rewrite guard rather than from a gate
// that runs earlier.
func promptDcloseLossyState(slug, epoch string) string {
	claim := strings.Repeat("x", state.MaxReceiptClaimLen+1)
	return `{"phase":"C","sessionId":"s1","slug":"` + slug + `","orchestrationActive":true,"checkEpoch":"` + epoch +
		`","unverifiedSubagents":[{"agentId":"a1","turnId":"t1","agentType":"worker","attempts":1,"receiptClaimed":"` + claim +
		`","recordedAt":"2026-01-01T00:00:00.000Z","resolvable":false}]}`
}

// TestPromptDcloseKeepsAParticipatingWritersUpdate is the port's own data-loss rule on this path
// (found by the Codex review of this pull request): the close is judged on the state the leading
// section read, but the resting state must be built from the state the session lock found, or a
// field a participating writer landed in between is overwritten by the stale copy.
func TestPromptDcloseKeepsAParticipatingWritersUpdate(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-participant"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-participant")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-participant")
	// A participating writer (the memory gate) records a grant after the leading section's read and
	// before the close takes the session lock, which the lock seam stages exactly.
	answer, _ := promptDcloseRunLocked(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), promptDcloseGrantingLock(t, cwd, "s1"))
	if strings.Contains(answer, "refused") {
		t.Fatalf("the close refused: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); !s.MemoryWriteGrant {
		t.Errorf("the close dropped a participating writer's update: %+v", s)
	}
}

// TestPromptDcloseRefusesAReceiptOfAnEarlierCheckCycle is the CHECK-BINDING-01 rule the receipt
// gate exists for (found by the Codex review of this pull request): the gate runs before the
// session lock, so a session that rotated through a reset and re-entered C while the gate was
// running must not spend the earlier cycle's receipt on the new epoch.
func TestPromptDcloseRefusesAReceiptOfAnEarlierCheckCycle(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-epoch-rotation"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-old")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-old")
	// The session re-enters C with a fresh epoch before the close takes the lock, the way a
	// reset followed by B>C does. The receipt still names the old cycle.
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer, _ := promptDcloseRunLocked(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), promptDcloseRotatingLock(t, cwd, "s1"))
	if !strings.Contains(answer, "the session state changed") {
		t.Errorf("a rotated epoch accepted the earlier cycle's receipt: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a rotated check epoch")
}

// TestPromptDcloseDoesNotOverwriteAnOutstandingMarker is the data-loss rule (found by the Codex
// review of this pull request): a request that does not match the recovery marker the session
// carries must not take the fresh-close path, because that path overwrites the marker and the
// close the marker describes can then never be finalized.
func TestPromptDcloseDoesNotOverwriteAnOutstandingMarker(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-outstanding-marker"
	// The first attempt closed wp-1 and committed the plan; the close of wp-2 is requested next.
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "outstanding marker"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.Phase, s.Slug, s.OrchestrationActive = state.PhaseC, slug, true
		s.CheckEpoch = promptDcloseStr("c-outstanding")
		s.Flags = state.Flags{AuditPassed: true, CheckPassed: true}
		s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: "s1", CheckEpoch: "c-outstanding", ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: promptDcloseStr("wp-2")}
	})
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-outstanding")
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-2", receipt))
	if !strings.Contains(answer, "refused") || !strings.Contains(answer, "wp-1") {
		t.Errorf("a mismatched request was not refused by the outstanding marker: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a mismatched request with an outstanding marker")
}

// TestPromptDcloseAllDoneRowFollowsTheStateWrite is the commit order on the all-done path (found
// by the Codex review and the Devin review of this pull request): the row is written inside the
// first goalplan lock so a failed second lock cannot lose it, but a state the close cannot publish
// must not leave that row behind, because the session then stays in C with a C-to-IDLE row.
func TestPromptDcloseAllDoneRowFollowsTheStateWrite(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-all-done-lossy"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "all done lossy"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{{ID: "wp-finished", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	// An unverified-subagent record whose receiptClaimed is past the reader's cap: the write-back
	// would publish it truncated, so the rewrite guard refuses the resting state.
	promptDcloseWrite(t, cwd, ".crw/sessions/s1.json", promptDcloseLossyState(slug, "c-all-done-lossy"))
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-finished", promptDcloseReceipt(t, cwd, "s1", "c-all-done-lossy")))
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Errorf("the data-loss refusal: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a refused all-done close")
}

// TestPromptDcloseTreatsAPublishedWriteAsLanded pins the commit-order decision the reviews asked
// for: state.WriteState reports a *state.PublishedError when the file reached its final path but
// the directory could not be synced, which is a durability warning on a write that happened, not
// a write that did not.
func TestPromptDcloseTreatsAPublishedWriteAsLanded(t *testing.T) {
	if landed, _ := promptDcloseWriteLanded(nil); !landed {
		t.Error("a clean write did not land")
	}
	if landed, warning := promptDcloseWriteLanded(&state.PublishedError{Err: errors.New("sync")}); !landed || warning == "" {
		t.Errorf("a published write with a sync failure: landed=%v warning=%q", landed, warning)
	}
	if landed, _ := promptDcloseWriteLanded(errors.New("before the rename")); landed {
		t.Error("a write that never reached its final path landed")
	}
}

// promptDcloseGrantingLock grants the leading section's lock and, on the close's own acquisition,
// records a participating writer's update first: the memory gate's grant, landed between the
// leading section's read and the close's lock.
func promptDcloseGrantingLock(t *testing.T, cwd, sessionID string) func(cwd, sessionID string, fn func() error) error {
	t.Helper()
	granted := 0
	return func(lockCwd, lockSession string, fn func() error) error {
		granted++
		if granted == 2 {
			promptSubmitStateFile(t, cwd, sessionID, func(s *state.State) { s.MemoryWriteGrant = true })
		}
		return state.WithSessionLock(lockCwd, lockSession, fn)
	}
}

// promptDcloseRotatingLock grants the leading section's lock and, on the close's own acquisition,
// moves the session to a fresh check cycle first: a reset followed by a re-entered C.
func promptDcloseRotatingLock(t *testing.T, cwd, sessionID string) func(cwd, sessionID string, fn func() error) error {
	t.Helper()
	granted := 0
	return func(lockCwd, lockSession string, fn func() error) error {
		granted++
		if granted == 2 {
			promptSubmitStateFile(t, cwd, sessionID, func(s *state.State) { s.CheckEpoch = promptDcloseStr("c-new") })
		}
		return state.WithSessionLock(lockCwd, lockSession, fn)
	}
}

// promptDcloseGoalplanLedgerPath is the bound plan's ledger file.
func promptDcloseGoalplanLedgerPath(t *testing.T, cwd, slug string) string {
	t.Helper()
	dir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, goalplan.GoalplanLedgerFile)
}

// promptDclosePabcdLedgerPath is the session transition ledger.
func promptDclosePabcdLedgerPath(cwd string) string {
	return filepath.Join(cwd, crwdir.DirName, state.LedgerFile)
}

// promptDcloseBlockAppend replaces a JSONL path with a directory, so the next append to it fails
// the way an unwritable ledger does, and answers the undo. It is how these tests arm a write
// failure at one exact point of the close.
func promptDcloseBlockAppend(t *testing.T, path string) func() {
	t.Helper()
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return func() { _ = os.Remove(path) }
}

// promptDcloseHoldPlanLock takes the bound plan's write lock and holds it until release runs,
// failing the test when the lock could not be taken at all. A goroutine takes it because the caller
// must keep running while it is held, the way a second session bound to the same plan sits inside
// the critical section its own goalplan rows are appended in.
func promptDcloseHoldPlanLock(t *testing.T, cwd, slug string) (release func()) {
	t.Helper()
	got, stop, done, failure := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan string, 1)
	go func() {
		defer close(done)
		locked, err := goalplan.WithGoalplanWriteLock(cwd, slug, func(*goalplan.Goalplan) (struct{}, error) {
			close(got)
			<-stop
			return struct{}{}, nil
		}, &goalplan.GoalplanWriteLockOptions{RetryDelaysMs: []int{}})
		if err != nil || locked.Kind != "ok" {
			failure <- locked.Reason
			close(got)
		}
	}()
	<-got
	select {
	case reason := <-failure:
		t.Fatalf("the plan lock could not be held: %s", reason)
	default:
	}
	return func() { close(stop); <-done }
}

// TestPromptDcloseAppendsTheGoalplanRowsUnderThePlanLock is finding (a) of generation 2 of
// CRW-797: the two goalplan rows are appended inside the second goalplan write lock, as
// goalplan/write.go requires of an existing-plan caller. Two sessions bound to the same plan whose
// closes interleave between the state publication and the rows must end with each row exactly
// once, which holds only when the dedup read and the append are one critical section.
func TestPromptDcloseAppendsTheGoalplanRowsUnderThePlanLock(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-rows-under-lock"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "rows under the plan lock"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	// Two sessions bound to the same plan, each carrying the marker of the same half-finished close
	// of wp-1. Both closes owe the same two rows, which is what makes the dedup read and the append
	// one critical section's business.
	attests := map[string]string{}
	for _, sessionID := range []string{"s1", "s2"} {
		promptSubmitStateFile(t, cwd, sessionID, func(s *state.State) {
			s.Phase, s.Slug, s.OrchestrationActive = state.PhaseC, slug, true
			s.CheckEpoch = promptDcloseStr("c-rows")
			s.Flags = state.Flags{AuditPassed: true, CheckPassed: true}
			s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: sessionID, CheckEpoch: "c-rows",
				ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: promptDcloseStr("wp-2")}
		})
		attests[sessionID] = promptDcloseAttest("wp-1", "")
	}

	// A second writer holds the plan's write lock across the window in which s1 sits between its
	// state publication and its rows. The rows belong inside that lock, so s1 writes none of them
	// while it is held; the order this finding names appended them there, outside every plan lock.
	var release func()
	seams := &promptDcloseSeams{afterStateWrite: func() {
		release = promptDcloseHoldPlanLock(t, cwd, slug)
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", attests["s1"], seams)
	if panicked != nil {
		t.Fatalf("s1's close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "finalization is pending") {
		t.Errorf("s1's close while the plan lock was held: %q", answer)
	}
	if rows := promptDcloseGoalplanRows(t, cwd, slug); len(rows) != 0 {
		t.Errorf("s1 appended its goalplan rows while another writer held the plan lock: %+v", rows)
	}
	release()

	// The same request finishes the close now that the lock is free, each row exactly once.
	if retry := promptDcloseRun(t, cwd, "s1", "t2", attests["s1"]); strings.Contains(retry, "refused") || strings.Contains(retry, "pending") {
		t.Fatalf("the retry did not finish the close: %q", retry)
	}
	// The second session bound to the same plan runs the same close and adds no goalplan row.
	if answer2 := promptDcloseRun(t, cwd, "s2", "t1", attests["s2"]); strings.Contains(answer2, "refused") {
		t.Fatalf("s2's close refused: %q", answer2)
	}
	rows := promptDcloseGoalplanRows(t, cwd, slug)
	counts := map[string]int{}
	for _, row := range rows {
		counts[promptDcloseStringOf(row["detail"])]++
	}
	if counts["closed wp-1"] != 1 || counts["started wp-2"] != 1 {
		t.Errorf("the goalplan rows are not exactly once each: %+v", rows)
	}
}

// promptDcloseStringOf reads a decoded JSONL value as text, where an absent key and a null both
// read as the empty text.
func promptDcloseStringOf(value any) string {
	s, _ := value.(string)
	return s
}

// TestPromptDcloseGoalplanRowFailureAfterPublicationAnswersPending is finding (b) of generation 2
// of CRW-797: once the resting state is published, a goalplan row that could not be written is not
// a close that wrote nothing. The answer is the finalization-pending text, the marker stays, and
// the same D request finishes the rows once.
func TestPromptDcloseGoalplanRowFailureAfterPublicationAnswersPending(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-rows-fail-after-publication"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "rows fail after publication"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-1")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-rows-fail")
	attest := promptDcloseAttest("wp-1", promptDcloseReceipt(t, cwd, "s1", "c-rows-fail"))
	restore := promptDcloseBlockAppend(t, promptDcloseGoalplanLedgerPath(t, cwd, slug))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "finalization is pending") {
		t.Errorf("a goalplan row failure after the publication: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written") {
		t.Errorf("the answer claims nothing was written: %q", answer)
	}
	s := state.ReadState(cwd, "s1")
	if s.Phase != state.PhaseIdle || s.DcloseRecovery == nil {
		t.Fatalf("the state after the failed row: %+v", s)
	}
	if promptDcloseReadPlan(t, cwd, slug).WorkPhases[0].Status != goalplan.WorkPhaseDone {
		t.Errorf("the plan was not committed before the rows")
	}
	// The same D request, with the ledger writable again, finishes the rows, the close row and the
	// marker cleanup, each exactly once.
	restore()
	retry := promptDcloseRun(t, cwd, "s1", "t2", attest)
	if strings.Contains(retry, "refused") || strings.Contains(retry, "pending") {
		t.Fatalf("the retry did not finish the close: %q", retry)
	}
	rows := promptDcloseGoalplanRows(t, cwd, slug)
	if len(rows) != 2 || rows[0]["detail"] != "closed wp-1" || rows[1]["detail"] != "started wp-2" {
		t.Errorf("the goalplan rows: %+v", rows)
	}
	if closeRows := promptOrchestrateLedger(t, cwd); len(closeRows) != 1 || closeRows[0]["closedWorkPhaseId"] != "wp-1" {
		t.Errorf("the close rows: %+v", closeRows)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery != nil || s.CheckEpoch != nil {
		t.Errorf("the retry did not clear the marker: %+v", s)
	}
}

// TestPromptDcloseAllDoneRowFailureAnswersTheWarning is finding (c) of generation 2 of CRW-797: an
// all-done close publishes a resting state with no marker and an empty check epoch, so a PABCD
// close row that could not be written has no same-D retry to promise. The answer is the success
// text with one warning line, never the finalization-pending text a retry cannot honour.
func TestPromptDcloseAllDoneRowFailureAnswersTheWarning(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-all-done-row-fail"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "all done row failure"})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-finished", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseSeedState(t, cwd, "s1", slug, "c-all-done-row-fail")
	attest := promptDcloseAttest("", promptDcloseReceipt(t, cwd, "s1", "c-all-done-row-fail"))
	restore := promptDcloseBlockAppend(t, promptDclosePabcdLedgerPath(cwd))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	restore()
	if strings.Contains(answer, "finalization is pending") {
		t.Errorf("an all-done close with no marker promised a retry: %q", answer)
	}
	if !strings.Contains(answer, "[crw: DONE]") || !strings.Contains(answer, "IPABCD: IDLE") {
		t.Errorf("the DONE directive: %q", answer)
	}
	warning := "the close was applied but its ledger row could not be written: "
	if strings.Count(answer, warning) != 1 {
		t.Errorf("the close row warning: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.Phase != state.PhaseIdle || s.DcloseRecovery != nil || s.CheckEpoch != nil {
		t.Errorf("the resting state: %+v", s)
	}
	if rows := promptOrchestrateLedger(t, cwd); len(rows) != 0 {
		t.Errorf("the close rows: %+v", rows)
	}
}
