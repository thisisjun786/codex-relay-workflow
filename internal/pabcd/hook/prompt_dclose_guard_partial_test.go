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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptDcloseGoalplanUnknownWant is the exact sentence a refusal of a retry must carry instead of
// a definite claim or denial about the goalplan: the plan on disk cannot prove who wrote its shape,
// so the answer says the plan may already hold this close's commit and tells the operator to read
// it. It is pinned as a literal here (not read from the production helper) so a wording change is
// caught by the suite (CRW-930, generation-2 decision on d2/d3).
const promptDcloseGoalplanUnknownWant = "the goalplan may already hold this close's commit from the first attempt; read it before retrying."

// promptDclosePlanUnreadableMode makes the bound plan unreadable to a retry that could not take the
// write lock: the plan file is still there, so the close can neither read its shape nor rewrite it.
func promptDclosePlanUnreadableMode(t *testing.T, cwd, slug string) {
	t.Helper()
	path := promptDclosePlanPath(t, cwd, slug)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
}

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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the early refusal denied the marker on the session: %q", answer)
	}
}

// TestPromptDcloseRecoveryRefusalWithoutASuccessorNamesOnlyTheMarker is the d3 case: a retry whose
// marker recorded no successor answers the absent-target cleanup, but that answer is not proof the
// first attempt's plan commit landed. A later refusal must name the marker alone, never claim the
// goalplan was published.
func TestPromptDcloseRecoveryRefusalWithoutASuccessorDoesNotClaimThePlan(t *testing.T) {
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseRecoveryRefusalNamesTheCommittedMarkerAndPlan is the recovery-refusal case: the
// first attempt of this close wrote the marker and committed the plan (the target is closed on
// disk), and the operator then left an open task under it, so the retry refuses. The refusal must
// name the artifacts this close published - the inherited marker and the committed plan - instead of
// ending in "Nothing was written." (CRW-930, d1).
func TestPromptDcloseRecoveryRefusalLeavesThePlanUnknown(t *testing.T) {
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the recovery refusal denied the artifacts this close published: %q", answer)
	}
}

// TestPromptDcloseRecoveryCleanupNamesTheCommittedPlan is the absent-target cleanup case: the target
// is gone and its recorded successor is already running, so the retry settles the plan with no write
// of its own - the first attempt had committed it. The resting state write then fails, and that
// refusal must name the plan this close published as well as the marker (CRW-930, d2).
func TestPromptDcloseRecoveryCleanupDoesNotClaimThePlan(t *testing.T) {
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
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
func TestPromptDcloseRecoveryCommittedPlanIsNotClaimedByALaterRefusal(t *testing.T) {
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
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
func TestPromptDcloseRecoveryIntegrityRefusalDoesNotClaimThePlan(t *testing.T) {
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
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
func TestPromptDcloseRecoveryBusyLockNamesTheMarkerAndLeavesThePlanUnknown(t *testing.T) {
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
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
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

// promptDclosePlanWithExtraField rewrites the bound plan with one unknown top-level field, which the
// reader keeps but a revival would drop. The write lock classifies that as "unreadable" while the
// plan itself is structurally readable.
func promptDclosePlanWithExtraField(t *testing.T, cwd, slug, field, value string) {
	t.Helper()
	path := promptDclosePlanPath(t, cwd, slug)
	raw := promptDcloseFileText(path)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatal(err)
	}
	parsed[field] = value
	next, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	promptDcloseWrite(t, cwd, filepath.Join(crwdir.DirName, goalplan.GoalplansSubdir, slug, goalplan.GoalplanFile), string(next))
}

// TestPromptDcloseRecoveryStartedSuccessorDoesNotProveACommit is the d1 case: the target is still
// open and only its recorded successor was started, so this close never committed the plan. A retry
// refused here names the marker it inherited and must not claim a goalplan publication; the
// generation-2 head judged the commit from the successor alone and named both.
func TestPromptDcloseRecoveryStartedSuccessorDoesNotClaimThePlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-started-only"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	// An operator started the successor by hand and left an open task under the still-open target:
	// the reference-valid plan CloseFixedWorkPhase refuses with tasks_pending.
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{{ID: "t-late", Title: "added late", Status: goalplan.TaskPending}}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}, promptDcloseStr("wp-2"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "gained 1 open task(s) after its marker was written") {
		t.Fatalf("the recovery did not refuse: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseRecoveryUnreadablePlanStillNamesTheCommittedPlan is the d2 case: the write lock
// answers "unreadable" for a plan that read cleanly but would lose an unknown field on revival. The
// committed shape is still on disk, so the refusal must name the goalplan this close published; the
// generation-2 head named the inherited marker alone.
func TestPromptDcloseRecoveryUnreadablePlanDoesNotClaimThePlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-unreadable-plan"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	promptDclosePlanWithExtraField(t, cwd, slug, "operatorNote", "hand edit")
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "could not be read") {
		t.Fatalf("the retry did not answer the unreadable goalplan: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseFreshUnreadablePlanKeepsTheBareClaim is the control for the case above: a fresh
// close with no inherited marker published nothing, so its unreadable-goalplan text is unchanged.
func TestPromptDcloseFreshUnreadablePlanKeepsTheBareClaim(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-fresh-unreadable-plan"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-fresh-unreadable")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-fresh-unreadable")
	promptDclosePlanWithExtraField(t, cwd, slug, "operatorNote", "hand edit")
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "could not be read") {
		t.Fatalf("the close did not answer the unreadable goalplan: %q", answer)
	}
	if strings.Contains(answer, promptDcloseMarkerPublishedSentence()) || strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("a fresh close claimed a publication it never made: %q", answer)
	}
	if !strings.Contains(answer, "Nothing was written.") {
		t.Errorf("a fresh close lost its bare claim: %q", answer)
	}
}

// TestPromptDcloseRecoveryBusySessionLockNamesWhatTheFirstAttemptPublished is the d3 case for the
// session lock the bound close itself takes: it stays busy, so the close never runs, but the first
// attempt's marker and committed goalplan are on disk and the answer must name them. The
// generation-2 head returned the bare busy text.
func TestPromptDcloseRecoveryBusySessionLockNamesTheMarkerAndLeavesThePlanUnknown(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-busy-session-lock"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	// The stamp takes the session lock, then the bound close's own acquisition finds it busy.
	answer, panicked := promptDcloseRunLocked(t, cwd, "s1", "t-busy", attest, promptDcloseLockFailingAfter(1))
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "lock unavailable") {
		t.Fatalf("the retry did not answer the busy session lock: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the busy refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseBusyRefusalSeparatesThePublicationSentences is the formatting control for the busy
// refusals: the publication sentences follow the busy text's own final period, so a space must
// separate them. Without it the answer runs two sentences together ("...changed.the recovery marker
// was published."), which is unreadable for the operator who has to act on it.
func TestPromptDcloseBusyRefusalSeparatesThePublicationSentences(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-busy-separator"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
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
	if strings.Contains(answer, "changed.the") {
		t.Errorf("the publication sentences run into the busy text: %q", answer)
	}
	if !strings.Contains(answer, "changed. the recovery marker was published.") {
		t.Errorf("the busy refusal did not separate the marker sentence: %q", answer)
	}
	if !strings.Contains(answer, "the recovery marker was published. "+promptDcloseGoalplanUnknownWant) {
		t.Errorf("the busy refusal did not separate the plan sentence: %q", answer)
	}
}

// TestPromptDcloseLegacyMarkerDoesNotClaimACommittedPlan is the d2 case: a legacy marker records no
// successor because the field was absent or malformed, not because the close had none. A plan whose
// target is done with the cursor cleared is therefore not evidence that this close committed it, and
// the legacy refusal must name the marker alone. The generation-3 head read the nil successor as an
// authoritative "no successor" and claimed the goalplan too.
func TestPromptDcloseLegacyMarkerDoesNotClaimACommittedPlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-legacy-marker"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, nil)
	// A hand edit that leaves the committed shape but was not made by this close, plus the legacy
	// marker: the successor field is unknown, so no shape can be read from it.
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseDone, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}, nil)
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
		s.DcloseRecovery.NextWorkPhaseID = nil
		s.DcloseRecovery.Legacy = true
	})
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "predates the successor field") {
		t.Fatalf("the retry did not answer the legacy marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the legacy refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseRecoveryUnreadableRereadNamesWhatTheFirstAttemptPublished is the d1 case for the
// close's own stricter reread: the leading snapshot matched this close's marker, then the session
// file became unreadable before the close's locked reread, which refuses. The first attempt's marker
// and committed goalplan are on disk, so the refusal names them; the generation-3 head answered the
// bare state refusal.
func TestPromptDcloseRecoveryUnreadableRereadNamesTheMarkerAndLeavesThePlanUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-unreadable-reread"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	sessionPath := filepath.Join(promptDcloseSessionsDir(cwd), "s1.json")
	// The stamp takes the session lock first; on the close's own acquisition the file becomes
	// unreadable, so ReadStateStrict fails while the leading snapshot already matched the marker.
	lock := func(cwd, sessionID string, fn func() error) error {
		if err := os.Chmod(sessionPath, 0o000); err != nil {
			t.Fatalf("chmod the session file: %v", err)
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	answer, panicked := promptDcloseRunLocked(t, cwd, "s1", "t1", attest, lock)
	_ = os.Chmod(sessionPath, 0o644)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Fatalf("the retry did not refuse at the strict reread: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the strict-reread refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseRecoverySourceRootRefusalNamesWhatTheFirstAttemptPublished is the d1 case for the
// entry gate: a bound D-close whose pinned source binding no longer resolves is refused before the
// bound handler runs. A matching retry's first attempt may already have published its marker and
// committed its goalplan, so the refusal names them; the generation-3 head returned the bare
// SOURCE-ROOT text.
func TestPromptDcloseRecoverySourceRootRefusalNamesTheMarkerAndLeavesThePlanUnknown(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-source-root"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	// A pinned worktree with no binding: the entry gate refuses before the bound handler.
	other := t.TempDir()
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) { s.BoundSourceRoot = &other })
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "SOURCE-ROOT") {
		t.Fatalf("the retry did not refuse at the entry gate: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the entry refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not say the goalplan may already hold the commit: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseFreshSourceRootRefusalKeepsTheBareText is the control: a fresh close published
// nothing, so its SOURCE-ROOT text is unchanged.
func TestPromptDcloseFreshSourceRootRefusalKeepsTheBareText(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-fresh-source-root"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-fresh-source-root")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-fresh-source-root")
	other := t.TempDir()
	promptSubmitStateFile(t, cwd, "s1", func(s *state.State) { s.BoundSourceRoot = &other })
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	if !strings.Contains(answer, "SOURCE-ROOT") {
		t.Fatalf("the close did not refuse at the entry gate: %q", answer)
	}
	if strings.Contains(answer, promptDcloseMarkerPublishedSentence()) || strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("a fresh close claimed a publication it never made: %q", answer)
	}
}

// --- CRW-930 generation 2, second correction: a retry neither claims nor denies the goalplan ------

// TestPromptDcloseRecoveryUnreadablePlanLeavesThePlanUnknown is the d2 case for a plan the retry
// cannot read at all: the write lock refuses and the fallback read returns nothing, so the shape on
// disk is unknowable rather than absent. The refusal names the marker it inherited and says the
// goalplan may already hold the first attempt's commit; it must not deny that a goalplan was written.
func TestPromptDcloseRecoveryUnreadablePlanLeavesThePlanUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode this case needs")
	}
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-plan-unreadable"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	promptDclosePlanUnreadableMode(t, cwd, slug)
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "could not be read") {
		t.Fatalf("the retry did not answer the unreadable goalplan: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not leave the goalplan unknown: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseRecoveryChangedSuccessorLeavesThePlanUnknown is the second d2 trigger: the
// recorded successor changed state after the marker was written, so the retry refuses successor_lost.
// That refusal cannot tell whether the first attempt's commit landed, so it must leave the goalplan
// unknown instead of denying it.
func TestPromptDcloseRecoveryChangedSuccessorLeavesThePlanUnknown(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-successor-changed"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	// The successor the marker recorded is blocked: the retry refuses, and the plan's shape proves
	// nothing about who wrote it.
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseBlocked, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}, promptDcloseStr("wp-1"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "CYCLE-COMPLETION-01") {
		t.Fatalf("the retry did not refuse the changed successor: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not leave the goalplan unknown: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseRecoveryAbsentTargetHandStartDoesNotClaimThePlan is the d3 case: an operator
// removed the target and started the recorded successor by hand, which is the same plan shape a
// real commit leaves. The retry settles that cleanup without writing a plan, so a later refusal
// must not claim this close published the goalplan; it leaves the plan unknown.
func TestPromptDcloseRecoveryAbsentTargetHandStartDoesNotClaimThePlan(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-hand-start"
	// wp-1 is gone and wp-2 runs on the cursor: the shape of a commit, but also of a hand repair
	// after a marker-only failure. The marker records wp-2, so the resume answers cleanup.
	plan := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "hand start " + slug})
	plan.Slug = slug
	plan.WorkPhases = []goalplan.GoalplanWorkPhase{
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}
	plan.ActiveWorkPhaseID = promptDcloseStr("wp-2")
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	promptDcloseWrite(t, cwd, filepath.Join(".crw", "sessions", "s1.json"),
		promptDcloseCommittedRecoveryState(slug, "c-hand-start"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", ""))
	if !strings.Contains(answer, "cannot be rewritten without losing a stored record") {
		t.Fatalf("the retry did not refuse at the IDLE-write guard: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer)
	}
	if strings.Contains(answer, promptDcloseGoalplanPublishedSentence()) {
		t.Errorf("the refusal claimed a goalplan this close never proved it wrote: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not leave the goalplan unknown: %q", answer)
	}
	if strings.Contains(answer, "Nothing else was written.") {
		t.Errorf("the refusal denied a goalplan it cannot prove: %q", answer)
	}
}

// TestPromptDcloseBusyStopBudgetStampNamesTheInheritedMarker is the d1 case: the Stop-budget stamp
// takes the session lock before the bound close, so a busy lock fails there and the close never
// runs. A matching retry's first attempt already published its marker, so that return must name it
// and leave the goalplan unknown; the generation-2 head returned the bare busy text.
func TestPromptDcloseBusyStopBudgetStampNamesTheInheritedMarker(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-busy-stamp"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	answer, panicked := promptDcloseRunLocked(t, cwd, "s1", "t-busy", attest, promptDcloseLockFailingAfter(0))
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "lock unavailable") {
		t.Fatalf("the retry did not answer the busy stamp: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the busy stamp did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the busy stamp did not leave the goalplan unknown: %q", answer)
	}
}

// TestPromptDcloseBusyStopBudgetStampFreshCloseKeepsTheBareText is the control for the case above: a
// fresh close published nothing of this close, so its stamp's busy text stays exactly as it was.
func TestPromptDcloseBusyStopBudgetStampFreshCloseKeepsTheBareText(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-fresh-busy-stamp"
	promptDclosePlan(t, cwd, slug, nil)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-fresh-busy-stamp")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-fresh-busy-stamp")
	answer, panicked := promptDcloseRunLocked(t, cwd, "s1", "t-busy", promptDcloseAttest("wp-1", receipt), promptDcloseLockFailingAfter(0))
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if want := promptDcloseNotApplied("lock unavailable"); answer != want {
		t.Errorf("a fresh close's busy stamp\n got %q\nwant %q", answer, want)
	}
}

// TestPromptDcloseRecoveryMovedSessionNamesTheInheritedMarker is the c6 case for the state-moved
// refusal: the session moved between the leading read and the close's own locked read, so the close
// writes nothing. A matching retry's first attempt has already published its marker, so that refusal
// must name it and leave the goalplan unknown instead of denying that anything was written.
func TestPromptDcloseRecoveryMovedSessionNamesTheInheritedMarker(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-moved-session"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	// The stamp takes the session lock first; on the close's own acquisition the session has moved to
	// another slug, which is the state-moved refusal.
	moved := false
	lock := func(cwd, sessionID string, fn func() error) error {
		if !moved {
			moved = true
			promptSubmitStateFile(t, cwd, sessionID, func(s *state.State) { s.Slug = "chat-recovery-moved-elsewhere" })
		}
		return state.WithSessionLock(cwd, sessionID, fn)
	}
	answer, panicked := promptDcloseRunLocked(t, cwd, "s1", "t1", attest, lock)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	if !strings.Contains(answer, "changed while this close was being applied") {
		t.Fatalf("the retry did not refuse at the state-moved check: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the state-moved refusal did not name the inherited marker: %q", answer)
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the state-moved refusal did not leave the goalplan unknown: %q", answer)
	}
	if strings.Contains(answer, "Nothing was written.") {
		t.Errorf("the state-moved refusal denied the marker this close published: %q", answer)
	}
}

// TestPromptDcloseHugePendingListKeepsThePublicationAccounting is the d1 case found by the pre-merge
// evaluation of the generation-2 head: a retry whose refusal embeds an unbounded task listing can
// push the publication accounting past the harness's MaxContext cut, so the operator receives a
// refusal without it. The listing is detail - the count precedes it - so it is bounded and the
// remainder is named, and the accounting survives the envelope.
func TestPromptDcloseHugePendingListKeepsThePublicationAccounting(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-huge-pending"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	// The first attempt committed the close; the operator then left a very large task listing under
	// the closed target, which the retry's tasks_pending refusal embeds.
	tasks := make([]goalplan.GoalplanTask, 0, 400)
	for i := 0; i < 400; i++ {
		tasks = append(tasks, goalplan.GoalplanTask{
			ID: "t-" + strconv.Itoa(i), Title: strings.Repeat("x", 100), Status: goalplan.TaskPending,
		})
	}
	promptDcloseRecoveryPlan(t, cwd, slug, []goalplan.GoalplanWorkPhase{
		{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseDone, Tasks: tasks, CriteriaIDs: []string{}},
		{ID: "wp-2", Title: "two", Status: goalplan.WorkPhaseInProgress, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}},
	}, promptDcloseStr("wp-2"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "open task(s) after its marker was written") {
		t.Fatalf("the retry did not refuse at the tasks_pending check: %q", answer[:min(len(answer), 200)])
	}
	// The hook wraps this answer in the harness envelope, which cuts a context over 32000 UTF-16
	// units at 31936 and appends "[truncated]". The refusal must stay under that so the accounting
	// the operator needs is not the part that is cut.
	if units := len(utf16.Encode([]rune(answer))); units > 31936 {
		t.Errorf("the refusal is %d UTF-16 units, so the harness would cut its accounting", units)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer[:min(len(answer), 200)])
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not leave the goalplan unknown: %q", answer[:min(len(answer), 200)])
	}
	// The listing is detail, so it is bounded and the remainder is named rather than dropped.
	if !strings.Contains(answer, "more task(s)") {
		t.Errorf("the bounded listing did not name the tasks it left out: %q", answer[:min(len(answer), 400)])
	}
	if !strings.Contains(answer, "t-0 (") {
		t.Errorf("the bounded listing dropped the tasks it should still name: %q", answer[:min(len(answer), 400)])
	}
}

// TestPromptDcloseFreshPendingListKeepsTheOracleText is the control the pre-merge evaluation of the
// generation-2 head asked for: a fresh close publishes nothing before its tasks_pending refusal, so
// that refusal must keep the oracle's own text byte for byte - the unbounded listing included. A
// shared bound that also shortened this path would change a refusal this issue promised to leave
// alone.
func TestPromptDcloseFreshPendingListKeepsTheOracleText(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-fresh-huge-pending"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-fresh-huge-pending")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-fresh-huge-pending")
	tasks := make([]goalplan.GoalplanTask, 0, 400)
	items := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		tasks = append(tasks, goalplan.GoalplanTask{
			ID: "t-" + strconv.Itoa(i), Title: strings.Repeat("y", 100), Status: goalplan.TaskPending,
		})
		items = append(items, "t-"+strconv.Itoa(i)+" ("+strings.Repeat("y", 100)+")")
	}
	plan := promptDcloseReadPlan(t, cwd, slug)
	plan.WorkPhases[0].Tasks = tasks
	if err := goalplan.WriteGoalplan(cwd, plan); err != nil {
		t.Fatal(err)
	}
	answer := promptDcloseRun(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt))
	want := promptOrchestrateRefusal("work-phase wp-1 still has 400 open task(s), so this cycle cannot close (CYCLE-COMPLETION-01): " +
		strings.Join(items, "; ") + ". Nothing was written.")
	if answer != want {
		t.Errorf("a fresh close's tasks_pending refusal changed\n got %d bytes\nwant %d bytes", len(answer), len(want))
	}
}

// TestPromptDcloseHugeIntegrityReasonsKeepThePublicationAccounting is the d1 case for the integrity
// refusal: every integrity reason is joined before the publication accounting is appended, so a plan
// with a very large reason list can push the accounting past the harness's cut and the operator
// receives a refusal without it. The reasons are detail; the accounting must survive.
func TestPromptDcloseHugeIntegrityReasonsKeepThePublicationAccounting(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-huge-integrity"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	// The committed shape, with many criterion references the plan does not define: structurally
	// readable, but its definition is refused with one reason per reference.
	missing := make([]string, 0, 400)
	for i := 0; i < 400; i++ {
		missing = append(missing, "criterion-"+strconv.Itoa(i)+"-"+strings.Repeat("z", 100))
	}
	promptDcloseRecoveryCommittedShape(t, cwd, slug, missing)
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "invalid goalplan:") {
		t.Fatalf("the retry did not refuse at the integrity check: %q", answer[:min(len(answer), 200)])
	}
	if units := len(utf16.Encode([]rune(answer))); units > 31936 {
		t.Errorf("the refusal is %d UTF-16 units, so the harness would cut its accounting", units)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer[:min(len(answer), 300)])
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not leave the goalplan unknown: %q", answer[:min(len(answer), 300)])
	}
	if !strings.Contains(answer, "more integrity reason(s)") {
		t.Errorf("the bounded reasons did not name what they left out: %q", answer[:min(len(answer), 400)])
	}
}

// TestPromptDcloseWarnedMarkerIsNamedAsTheMarker is the c1 case for a marker whose write published
// and then failed the directory sync: the shared state warning says only "the session state", so a
// refusal that carried it would not identify the artifact this close published. The line must name
// the recovery marker.
func TestPromptDcloseWarnedMarkerIsNamedAsTheMarker(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-warned-marker"
	promptDcloseTwoPhases(t, cwd, slug)
	promptDcloseSeedState(t, cwd, "s1", slug, "c-warned-marker")
	receipt := promptDcloseReceipt(t, cwd, "s1", "c-warned-marker")
	seams := &promptDcloseSeams{
		// The marker reaches its final path, then the directory sync fails; the plan write then fails
		// before its rename, so the refusal names the marker alone.
		writeMarker: func(string, state.State, string, *string) error {
			return &state.PublishedError{Err: syscall.EIO}
		},
		writePlan: func(string, *goalplan.Goalplan) error {
			return errors.New("the goalplan could not be written before the rename")
		},
	}
	answer, panicked := promptDcloseRunWith(t, cwd, "s1", "t1", promptDcloseAttest("wp-1", receipt), seams)
	if panicked != nil {
		t.Fatalf("the close panicked: %v", panicked)
	}
	want := promptDcloseMarkerPublishedWarning(&state.PublishedError{Err: syscall.EIO})
	if !strings.Contains(answer, want) {
		t.Errorf("the refusal did not name the warned marker\n got %q\nwant it to contain %q", answer, want)
	}
	if strings.Contains(answer, "the session state was published but its directory could not be synced") {
		t.Errorf("the refusal named the session state instead of the marker it published: %q", answer)
	}
}

// TestPromptDcloseHugeDependencyListKeepsThePublicationAccounting is the d2 case for the
// dependencies_unmet refusal: its dependency list is joined before the publication accounting is
// appended, so a very large list can push the accounting past the harness's cut.
func TestPromptDcloseHugeDependencyListKeepsThePublicationAccounting(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-huge-deps"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	// The target is still open and waits on a very large list of real phases that are not done, so
	// the retry refuses dependencies_unmet with that list embedded before the accounting.
	deps := make([]string, 0, 400)
	phases := make([]goalplan.GoalplanWorkPhase, 0, 402)
	phases = append(phases, goalplan.GoalplanWorkPhase{ID: "wp-1", Title: "one", Status: goalplan.WorkPhaseInProgress,
		Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
	phases = append(phases, goalplan.GoalplanWorkPhase{ID: "wp-2", Title: "two", Status: goalplan.WorkPhasePending,
		Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
	for i := 0; i < 400; i++ {
		id := "wp-dep-" + strconv.Itoa(i) + "-" + strings.Repeat("d", 100)
		deps = append(deps, id)
		phases = append(phases, goalplan.GoalplanWorkPhase{ID: id, Title: "dep", Status: goalplan.WorkPhasePending,
			Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}})
	}
	phases[0].DependsOn = deps
	promptDcloseRecoveryPlan(t, cwd, slug, phases, promptDcloseStr("wp-1"))
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "now waits for") {
		t.Fatalf("the retry did not refuse dependencies_unmet: %q", answer[:min(len(answer), 200)])
	}
	if units := len(utf16.Encode([]rune(answer))); units > 31936 {
		t.Errorf("the refusal is %d UTF-16 units, so the harness would cut its accounting", units)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer[:min(len(answer), 300)])
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not leave the goalplan unknown: %q", answer[:min(len(answer), 300)])
	}
	if !strings.Contains(answer, "more dependency(ies)") {
		t.Errorf("the bounded list did not name what it left out: %q", answer[:min(len(answer), 400)])
	}
}

// TestPromptDcloseHugeLockDiagnosticKeepsThePublicationAccounting is the c6 case for a refusal
// whose own text is long: the goalplan lock's busy diagnostic embeds the whole owner file, so a
// large owner.json can push the publication accounting past the harness's cut. The bound is on the
// decorated answer, not on each detail source, so the accounting survives whatever the refusal's
// own text holds.
func TestPromptDcloseHugeLockDiagnosticKeepsThePublicationAccounting(t *testing.T) {
	cwd := promptDcloseRepo(t)
	slug := "chat-recovery-huge-lock"
	attest := promptDcloseRecoverable(t, cwd, "s1", slug, promptDcloseStr("wp-2"))
	promptDcloseRecoveryCommittedShape(t, cwd, slug, nil)
	planDir, err := goalplan.GoalplanDir(cwd, slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(planDir, goalplan.GoalplanLockDir), 0o777); err != nil {
		t.Fatal(err)
	}
	// A lock whose owner file is far larger than the harness keeps: the busy text embeds all of it.
	owner := "{\"pid\":1,\"acquiredAt\":\"2026-01-01T00:00:00.000Z\",\"note\":\"" + strings.Repeat("o", 60000) + "\"}"
	promptDcloseWrite(t, cwd, filepath.Join(crwdir.DirName, goalplan.GoalplansSubdir, slug, goalplan.GoalplanLockDir, goalplan.GoalplanLockOwnerFile), owner)
	answer := promptDcloseRun(t, cwd, "s1", "t1", attest)
	if !strings.Contains(answer, "is busy") {
		t.Fatalf("the retry did not answer the busy lock: %q", answer[:min(len(answer), 200)])
	}
	if units := len(utf16.Encode([]rune(answer))); units > 31936 {
		t.Errorf("the refusal is %d UTF-16 units, so the harness would cut its accounting", units)
	}
	if !strings.Contains(answer, promptDcloseMarkerPublishedSentence()) {
		t.Errorf("the refusal did not name the inherited marker: %q", answer[:min(len(answer), 300)])
	}
	if !strings.Contains(answer, promptDcloseGoalplanUnknownWant) {
		t.Errorf("the refusal did not leave the goalplan unknown: %q", answer[:min(len(answer), 300)])
	}
	if !strings.Contains(answer, "[detail truncated]") {
		t.Errorf("the trimmed refusal did not mark what it cut: %q", answer[:min(len(answer), 400)])
	}
}
