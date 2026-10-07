// prompt_dclose_guard_partial_test.go is the CRW-930 suite for the bound D-close's IDLE-write guard.
// The guard that runs just before the resting state is written re-reads the session file, so it can
// refuse after an earlier step of the same close already published the recovery marker or the
// goalplan. The answer then has to name the published artifact and carry its directory-sync warning
// instead of claiming that nothing was written. With no earlier warning the bare refusal is
// unchanged.
//
// Every case drives the handler through promptSubmitHandleWith with function-argument seams, the way
// prompt_dclose_published_test.go does, and uses only temporary homes.
package hook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptDcloseSessionsDir is the directory the close writes its session file into.
func promptDcloseSessionsDir(cwd string) string {
	return filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir)
}

// TestPromptDcloseGuardRefusalNamesTheCleanlyPublishedMarkerAndPlan is the c6 case for the guard
// before the IDLE state write: the marker and the plan both publish with no error at all, so no
// directory-sync warning is collected, and the guard then refuses on an unreadable session file.
// The refusal must still name both artifacts; the generation-1 head answers "Nothing was written."
// because its partial refusal only ever named an artifact that carried a warning.
func TestPromptDcloseGuardRefusalNamesTheCleanlyPublishedMarkerAndPlan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	cwd := promptDcloseRepo(t)
	slug := "chat-guard-clean"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-guard-clean")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-guard-clean")
	sessionPath := filepath.Join(promptDcloseSessionsDir(cwd), "s1.json")
	seams := &promptDcloseSeams{
		afterGoalplanCommit: func() {
			// Both writes landed cleanly; the guard re-reads the session file, so make that read fail.
			if err := os.Chmod(sessionPath, 0o200); err != nil {
				t.Fatalf("chmod the session file: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(sessionPath, 0o644) })
		},
	}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if err := os.Chmod(sessionPath, 0o644); err != nil {
		t.Fatalf("restore the session file mode: %v", err)
	}
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Errorf("the guard did not refuse: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the guard refusal did not name the cleanly published marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the guard refusal did not name the cleanly published plan: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the guard refusal denied the cleanly published artifacts: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil {
		t.Errorf("the guard refusal lost the recovery marker: %+v", s)
	}
}

// TestPromptDclosePlanFailureAfterACleanMarkerNamesTheMarker is the c6 case for the plan write: the
// marker publishes with no error, then the plan write fails before its rename. The refusal must name
// the marker the close published, not deny it.
func TestPromptDclosePlanFailureAfterACleanMarkerNamesTheMarker(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-plan-after-clean-marker"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-plan-after-clean-marker")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-plan-after-clean-marker")
	seams := &promptDcloseSeams{writePlan: func(string, *goalplan.Goalplan) error {
		return errors.New("the goalplan could not be written before the rename")
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "refused") {
		t.Errorf("the failed plan write did not refuse: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the cleanly published marker: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the refusal denied the cleanly published marker: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil {
		t.Errorf("the failed plan write lost the published marker: %+v", s)
	}
}

// TestPromptDcloseStateWriteFailureNamesTheCleanMarkerAndPlan is the c6 case for the IDLE state
// write: the marker and the plan publish with no error, then the resting state write itself fails
// because the sessions directory cannot be written. The refusal must name both published artifacts.
func TestPromptDcloseStateWriteFailureNamesTheCleanMarkerAndPlan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	cwd := promptDcloseRepo(t)
	slug := "chat-state-after-clean-writes"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-state-after-clean-writes")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-state-after-clean-writes")
	sessions := promptDcloseSessionsDir(cwd)
	seams := &promptDcloseSeams{
		afterGoalplanCommit: func() {
			// Both writes landed cleanly; the session file stays readable so the guard passes and the
			// resting state write is the step that fails.
			if err := os.Chmod(sessions, 0o555); err != nil {
				t.Fatalf("chmod the sessions directory: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(sessions, 0o777) })
		},
	}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if err := os.Chmod(sessions, 0o777); err != nil {
		t.Fatalf("restore the sessions directory mode: %v", err)
	}
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Errorf("the failed state write did not answer the state refusal: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the cleanly published marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the refusal did not name the cleanly published plan: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the refusal denied the cleanly published artifacts: %q", answer)
	}
}

// TestPromptDcloseRecoveryEarlyRefusalNamesTheInheritedMarker is the d1 case: a retry whose plan
// cannot be read or fails its integrity check refuses before the recovery arm runs, but the marker
// the first attempt published is still on the session, so the refusal must name it.
func TestPromptDcloseRecoveryEarlyRefusalNamesTheInheritedMarker(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-early-refusal"
	// A marker-matched retry whose bound plan is empty: the integrity/empty-plan refusal fires before
	// the recovery arm, and the inherited marker is the artifact this close already published.
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{}, nil)
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "has no active work-phase to close") {
		t.Fatalf("the recovery did not refuse at the empty-plan check: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the early refusal did not name the inherited marker: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the early refusal denied the marker on the session: %q", answer)
	}
}

// TestPromptDcloseRecoveryRefusalWithoutASuccessorNamesOnlyTheMarker is the d3 case: a retry whose
// marker recorded no successor answers the absent-target cleanup, but that answer is not proof the
// first attempt's plan commit landed. A later refusal must name the marker alone, never claim the
// goalplan was published.
func TestPromptDcloseRecoveryRefusalWithoutASuccessorNamesOnlyTheMarker(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-no-successor"
	// wp-1 is gone and the marker recorded no successor, so the resume answers cleanup; a plan left
	// looking finished is not evidence of this close's commit.
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "no successor " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseWrite(t, cwd, filepath.Join(".crw", "sessions", "s1.json"),
		promptDcloseCommittedRecoveryStateNext(slug, "c-recovery", "null"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", ""))
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Fatalf("the retry did not refuse at the IDLE-write guard: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the refusal claimed a goalplan this close never proved it published: %q", answer)
	}
}

// TestPromptDcloseRecoveryRefusalNamesTheCommittedMarkerAndPlan is the recovery-refusal case: the
// first attempt of this close wrote the marker and committed the plan (the target is closed on
// disk), and the operator then left an open task under it, so the retry refuses. The refusal must
// name the artifacts this close published - the inherited marker and the committed plan - instead of
// ending in "Nothing was written." (CRW-930, d1).
func TestPromptDcloseRecoveryRefusalNamesTheCommittedMarkerAndPlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-refusal-names"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	// The first attempt committed the close: the target is done and its successor started. The
	// operator then hid a pending task under the closed target.
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "first", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{{ID: "t-late", Title: "added late", Status: goalplan.TaskPending}}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}, promptDcloseStr("wp-2"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "gained 1 open task(s) after its marker was written") {
		t.Fatalf("the recovery did not refuse: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the recovery refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the recovery refusal did not name the committed goalplan: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the recovery refusal denied the artifacts this close published: %q", answer)
	}
}

// TestPromptDcloseRecoveryCleanupNamesTheCommittedPlan is the absent-target cleanup case: the target
// is gone and its recorded successor is already running, so the retry settles the plan with no write
// of its own - the first attempt had committed it. The resting state write then fails, and that
// refusal must name the plan this close published as well as the marker (CRW-930, d2).
func TestPromptDcloseRecoveryCleanupNamesTheCommittedPlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-cleanup-names"
	// The target wp-1 is gone and the recorded successor wp-2 is running on the cursor, which is the
	// cleanup answer: the first attempt's plan commit is on disk. A stored record whose write-back
	// would lose it makes the IDLE-write guard refuse the resting state.
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "cleanup " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseWrite(t, cwd, filepath.Join(".crw", "sessions", "s1.json"),
		promptDcloseCommittedRecoveryState(slug, "c-recovery"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", ""))
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Fatalf("the retry did not refuse at the IDLE-write guard: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the refusal did not name the committed goalplan: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the refusal denied the artifacts this close published: %q", answer)
	}
}

// promptDcloseCommittedRecoveryState is a recoverable session whose plan commit already landed: it
// carries the D-close marker of wp-1 and a stored unverified-subagent record whose write-back the
// reader would truncate, so the IDLE-write guard refuses the resting state while the file stays
// readable.
func promptDcloseCommittedRecoveryState(slug, epoch string) string {
	return promptDcloseCommittedRecoveryStateNext(slug, epoch, "\"wp-2\"")
}

// promptDcloseCommittedRecoveryStateNext is the same with the marker's recorded successor spelled by
// the caller: "null" for a marker that recorded none.
func promptDcloseCommittedRecoveryStateNext(slug, epoch, next string) string {
	claim := strings.Repeat("x", state.MaxReceiptClaimLen+1)
	return "{\"phase\":\"C\",\"sessionId\":\"s1\",\"slug\":\"" + slug + "\",\"orchestrationActive\":true,\"checkEpoch\":\"" + epoch +
		"\",\"flags\":{\"auditPassed\":true,\"checkPassed\":true}," +
		"\"dcloseRecovery\":{\"sessionId\":\"s1\",\"checkEpoch\":\"" + epoch + "\",\"closedWorkPhaseId\":\"wp-1\",\"nextWorkPhaseId\":" + next + "}," +
		"\"unverifiedSubagents\":[{\"agentId\":\"a1\",\"turnId\":\"t1\",\"agentType\":\"worker\",\"attempts\":1,\"receiptClaimed\":\"" + claim +
		"\",\"recordedAt\":\"2026-01-01T00:00:00.000Z\",\"resolvable\":false}]}"
}

// TestPromptDcloseRecoveryAlreadyCommittedPlanIsNamedByALaterRefusal is the d1 case: a marker-matched
// retry whose plan commit already landed (the settled shape is on disk, so the retry rewrites
// nothing) continues the same close, so a later refusal must name the goalplan that close published
// as well as the marker. The plan is committed by the first attempt and only the marker is inherited,
// so a branch that recorded the plan only when this invocation rewrote it would drop it.
func TestPromptDcloseRecoveryAlreadyCommittedPlanIsNamedByALaterRefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-plan-committed"
	// The settled shape the first attempt wrote: the target done, the recorded successor started, the
	// cursor on it. CloseFixedWorkPhase reads this as already_done, so the retry writes no plan.
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "committed " + slug})
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
		s.CheckEpoch = promptDcloseStr("c-recovery-committed")
		s.Flags = state.Flags{AuditPassed: true, CheckPassed: true}
		s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: "s1", CheckEpoch: "c-recovery-committed",
			ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: promptDcloseStr("wp-2")}
	})
	// A stored record whose write-back would lose it: the IDLE-write guard refuses the resting state
	// while the file stays readable, so the retry reaches the guard after the plan commit the first
	// attempt already landed.
	promptDcloseWrite(t, cwd, filepath.Join(".crw", "sessions", "s1.json"),
		promptDcloseCommittedRecoveryState(slug, "c-recovery-committed"))
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", ""), nil)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Errorf("the retry did not refuse at the IDLE-write guard: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the marker this close published: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the refusal did not name the goalplan this close already committed: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the refusal denied the artifacts this close published: %q", answer)
	}
}

// TestPromptDclosePlanFailureWhoseReasonHoldsTheDenialKeepsTheTrailingClaim is the d1 case: a plan
// write error whose own text contains "Nothing was written." must not make the partial refusal edit
// that inner phrase and leave the false trailing claim behind. The trailing claim is the one the
// close owns, so the answer must end with the publication list and still name the marker.
func TestPromptDclosePlanFailureWhoseReasonHoldsTheDenialKeepsTheTrailingClaim(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-plan-error-holds-denial"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-plan-error-holds-denial")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-plan-error-holds-denial")
	seams := &promptDcloseSeams{writePlan: func(string, *goalplan.Goalplan) error {
		// The writer's own message repeats the denial phrase, which the refusal template also ends with.
		return errors.New("the goalplan could not be written before the rename. Nothing was written.")
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the published marker: %q", answer)
	}
	if !strings.HasSuffix(answer, "Nothing else was written.]") {
		t.Errorf("the refusal kept the false trailing claim instead of the publication list: %q", answer)
	}
	if strings.HasSuffix(answer, "Nothing was written.]") {
		t.Errorf("the refusal ends with the denial the close already disproved: %q", answer)
	}
}

// TestPromptDcloseRecoveryPlanFailureNamesTheMarkerAlreadyOnTheSession is the c6 case for a recovery
// retry: the marker of the first attempt is already on the session, so the retry's plan write failing
// must name that marker instead of claiming nothing was written. It is the recovering branch of
// promptDclosePlanWork's record of what this close published.
func TestPromptDcloseRecoveryPlanFailureNamesTheMarkerAlreadyOnTheSession(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-plan-failure"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
	seams := &promptDcloseSeams{writePlan: func(string, *goalplan.Goalplan) error {
		return errors.New("the goalplan could not be written before the rename")
	}}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", attest, seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "refused") {
		t.Errorf("the recovery plan failure did not refuse: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the recovery plan failure did not name the marker already on the session: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the recovery plan failure denied the marker on the session: %q", answer)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil {
		t.Errorf("the recovery plan failure lost the marker: %+v", s)
	}
}

// TestPromptDcloseGuardRefusalNamesThePublishedPlan is the CRW-930 case: a wp-1 -> wp-2 bound close
// publishes the goalplan (the real write, then the post-rename directory-sync failure a
// *state.PublishedError reports), the session file then becomes unreadable to the IDLE-write guard,
// and the guard refuses. The answer must name the published goalplan and carry its warning; the
// baseline answers the bare refusal, which denies that anything was written.
func TestPromptDcloseGuardRefusalNamesThePublishedPlan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	cwd := promptDcloseRepo(t)
	slug := "chat-guard-partial"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-guard-partial")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-guard-partial")
	sessionPath := filepath.Join(cwd, ".crw", "sessions", "s1.json")
	seams := &promptDcloseSeams{
		writePlan: func(planCwd string, plan *goalplan.Goalplan) error {
			// The real write first, so the plan is genuinely published, then the post-rename failure
			// a directory sync would report: the close counts that as landed and carries the warning.
			if err := goalplan.WriteGoalplan(planCwd, plan); err != nil {
				return err
			}
			return &state.PublishedError{Err: syscall.EIO}
		},
		afterGoalplanCommit: func() {
			// The IDLE-write guard re-reads the session file; make that read fail so the guard
			// refuses after the plan was published.
			if err := os.Chmod(sessionPath, 0o200); err != nil {
				t.Fatalf("chmod the session file: %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(sessionPath, 0o644) })
		},
	}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	// The guard's refusal left the session file as it was; restore the readable mode before the state
	// assertions read it back.
	if err := os.Chmod(sessionPath, 0o644); err != nil {
		t.Fatalf("restore the session file mode: %v", err)
	}
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Errorf("the guard did not refuse: %q", answer)
	}
	warning := promptDcloseGoalplanPublishedWarning(&state.PublishedError{Err: syscall.EIO})
	if !strings.Contains(answer, warning) {
		t.Errorf("the guard refusal did not name the published plan and its warning\n got %q\nwant it to contain %q", answer, warning)
	}
	// The marker published cleanly in this case, so it carries no warning and is named by the fixed
	// sentence: a branch that reported only warned artifacts would drop it (CRW-930, d2).
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the guard refusal did not name the cleanly published marker: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the guard refusal denied the published plan: %q", answer)
	}
	// The real write ran before the reported failure, so the published plan is the closed one and the
	// refusal left it in place for the operator.
	saved := promptDcloseReadPlan(t, cwd, slug)
	if saved.WorkPhases[0].Status != goalplan.WorkPhaseDone || saved.WorkPhases[1].Status != goalplan.WorkPhaseInProgress {
		t.Errorf("the plan after the guard refusal: %+v", saved.WorkPhases)
	}
	if s := state.ReadState(cwd, "s1"); s.DcloseRecovery == nil {
		t.Errorf("the guard refusal lost the recovery marker: %+v", s)
	}
}

// TestPromptDcloseGuardRefusalWithoutAWarningIsUnchanged is the control: an all-done close publishes
// nothing before the IDLE-write guard, so a guard refusal there has no earlier warning to carry and
// keeps today's text byte for byte.
func TestPromptDcloseGuardRefusalWithoutAWarningIsUnchanged(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-guard-control"
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "all done control " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-finished", Title: "finished", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	// A stored record whose write-back would lose it: the guard refuses the resting state, and the
	// all-done path wrote no marker and no plan before it, so warnings is empty.
	promptDcloseWrite(t, cwd, ".crw/sessions/s1.json", promptDcloseLossyState(slug, "c-guard-control"))
	before := promptDcloseSnapshot(t, cwd, "s1", slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-finished", promptDcloseReceipt(t, cwd, "s1", "c-guard-control")))
	if want := promptDcloseStateRefusal(); answer != want {
		t.Errorf("a guard refusal with no earlier warning\n got %q\nwant %q", answer, want)
	}
	if !strings.Contains(answer, "Nothing was written.") {
		t.Errorf("a guard refusal with no earlier warning lost the bare claim: %q", answer)
	}
	promptDcloseUnchanged(t, before, promptDcloseSnapshot(t, cwd, "s1", slug), "a guard refusal with no earlier warning")
}

// --- CRW-930 generation 2: the three pre-merge findings of the f487e8a8 head -------------------

// promptDcloseRecoveryCommittedShape writes the plan a committed close of wp-1 leaves behind: the
// target done, the recorded successor running on the cursor. criterionIDs, when non-empty, is hung
// on the successor so the plan also fails its definition integrity check.
func promptDcloseRecoveryCommittedShape(t *testing.T, cwd, slug string, criterionIDs []string) {
	t.Helper()
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "committed " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: criterionIDs},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
}

// TestPromptDcloseRecoveryCorruptMarkerDoesNotClaimThePlan is the d1 case: a marker that names the
// target as its own successor is corrupt, so the retry refuses, but the target is still open and the
// plan therefore cannot carry this close's commit. The refusal names the marker it inherited and
// must not claim the goalplan was published; the generation-1 head read the self-successor as the
// commit's settled shape and named both.
func TestPromptDcloseRecoveryCorruptMarkerDoesNotClaimThePlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-corrupt-shape"
	// wp-1 is still in progress with the cursor on it, and the marker records wp-1 as its successor:
	// the corrupt marker the existing recovery case pins. No close can produce this, so no commit of
	// this close is on disk.
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-1"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "names that same work-phase as its successor") {
		t.Fatalf("the recovery did not refuse the corrupt marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the refusal claimed a goalplan this close never committed: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the refusal denied the marker this close published: %q", answer)
	}
}

// TestPromptDcloseRecoveryIntegrityRefusalNamesTheCommittedPlan is the d2 case: the first attempt of
// this close committed the plan (the target is done and its successor runs on the cursor) and the
// operator then broke the plan's definition, so the retry refuses at the integrity check - which runs
// before the recovery accounting. The refusal must still name the goalplan this close published; the
// generation-1 head names the marker alone and denies the rest.
func TestPromptDcloseRecoveryIntegrityRefusalNamesTheCommittedPlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-integrity"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	// The committed shape, plus a criterion reference the plan does not define: structurally readable,
	// but its definition is refused.
	promptDcloseRecoveryCommittedShape(t, cwd, slug, []string{"missing"})
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "invalid goalplan:") {
		t.Fatalf("the recovery did not refuse at the integrity check: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the integrity refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the integrity refusal dropped the goalplan this close committed: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the integrity refusal denied the artifacts this close published: %q", answer)
	}
}

// TestPromptDcloseRecoveryIntegrityRefusalWithoutACommitKeepsTheBareClaim is the control for the case
// above: a fresh close (no inherited marker, so nothing of this close is on disk) that fails the same
// integrity check keeps today's text, with no publication sentence added.
func TestPromptDcloseRecoveryIntegrityRefusalWithoutACommitKeepsTheBareClaim(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-integrity-fresh"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-integrity-fresh")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-integrity-fresh")
	plan := promptDcloseReadPlan(t, cwd, slug)
	plan.WorkPhases[0].CriteriaIDs = []string{"missing"}
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "invalid goalplan:") {
		t.Fatalf("the close did not refuse at the integrity check: %q", answer)
	}
	if strings.Contains(answer, promptDcloseMarkerPublishedSentence()) || strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("a fresh close claimed a publication it never made: %q", answer)
	}
	if !strings.Contains(answer, "Nothing was written.") {
		t.Errorf("a fresh close lost its bare claim: %q", answer)
	}
}

// TestPromptDcloseRecoveryBusyLockNamesWhatTheFirstAttemptPublished is the d3 case: the retry cannot
// take the goalplan lock, so it never reads the plan, but its first attempt's marker and committed
// goalplan are still on disk. The busy refusal must name both; the generation-1 head returns the busy
// text alone, so the operator cannot tell that a partial close is waiting.
func TestPromptDcloseRecoveryBusyLockNamesWhatTheFirstAttemptPublished(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-busy-lock"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	// Another writer holds the plan's write lock: the lock is the directory itself, so a leftover one
	// makes every attempt of this retry answer busy.
	planDir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(planDir, goalplan.GoalplanLockDir), 0o777); err != nil {
		t.Fatal(err)
	}
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "is busy") {
		t.Fatalf("the retry did not answer the busy lock: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the busy refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the busy refusal dropped the goalplan this close committed: %q", answer)
	}
}

// TestPromptDcloseFreshCloseBusyLockKeepsTheBareText is the control for the case above: a close with
// no inherited marker published nothing of its own, so its busy text is unchanged.
func TestPromptDcloseFreshCloseBusyLockKeepsTheBareText(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-fresh-busy-lock"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-fresh-busy-lock")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-fresh-busy-lock")
	planDir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(planDir, goalplan.GoalplanLockDir), 0o777); err != nil {
		t.Fatal(err)
	}
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "is busy") {
		t.Fatalf("the close did not answer the busy lock: %q", answer)
	}
	if strings.Contains(answer, promptDcloseMarkerPublishedSentence()) || strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("a fresh close claimed a publication it never made: %q", answer)
	}
}
