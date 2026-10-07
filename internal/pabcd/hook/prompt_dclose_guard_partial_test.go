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
