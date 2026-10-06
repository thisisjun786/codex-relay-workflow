package evidence

// CRW-676: a required check that failed for real is told apart from one whose runner never picked
// it up even when the same run attempt also holds a job that began no step. A matrix fail-fast run
// cancels not-yet-started siblings after a real test failure; without this the required check's
// failure reads as checks_not_run and the lane reruns a real failure once as a runner problem.

import "testing"

const crw676Head = "7d1b2c3a4e5f60718293a4b5c6d7e8f901234567"

// crw676Entry is one check entry of the collector's shape.
func crw676Entry(run, name, conclusion string, attempt int, notRun bool) map[string]any {
	entry := map[string]any{"runId": run, "name": name, "headSha": crw676Head, "conclusion": conclusion, "attempt": attempt}
	if notRun {
		entry["notRun"] = true
	}
	return entry
}

// The fail-fast shape: dev-gate failed, test-1 began a step and failed for real, and test-2 was
// cancelled before it started. The real failure is the answer, so the lane returns its turn
// instead of rerunning the head once as if the runner had never picked it up.
func TestCRW676RealFailureBesideANotRunJobAnswersChecksStale(t *testing.T) {
	checks := []any{
		crw676Entry("workflow-run:400:dev-gate#0", "dev-gate", "failure", 1, false),
		crw676Entry("workflow-run:400:test-1#0", "test-1", "failure", 1, false),
		crw676Entry("workflow-run:400:test-2#0", "test-2", "cancelled", 1, true),
	}
	problems := ChecksProblems(crw676Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
}

// CRW-661's incident shape keeps its answer: only the required check failed, and every sibling is a
// step-less cancelled job, so nothing says the commit was tested.
func TestCRW676IncidentShapeStaysChecksNotRun(t *testing.T) {
	problems := ChecksProblems(crw661Head, []string{"dev-gate"}, crw661Incident(false, true))
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
}

// A real failure of another workflow run says nothing about this run: the required check's own run
// still never started, so the lane may rerun it.
func TestCRW676RealFailureOfAnotherRunKeepsChecksNotRun(t *testing.T) {
	checks := []any{
		crw676Entry("workflow-run:400:dev-gate#0", "dev-gate", "failure", 1, false),
		crw676Entry("workflow-run:400:test-2#0", "test-2", "cancelled", 1, true),
		crw676Entry("workflow-run:401:test-1#0", "test-1", "failure", 1, false),
	}
	problems := ChecksProblems(crw676Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
}

// A real failure of an older attempt does not speak for the newest one, and the required check's
// own run still never started on that newest attempt.
func TestCRW676RealFailureOfAnOlderAttemptKeepsChecksNotRun(t *testing.T) {
	checks := []any{
		crw676Entry("workflow-run:400:dev-gate#0", "dev-gate", "failure", 2, false),
		crw676Entry("workflow-run:400:test-2#0", "test-2", "cancelled", 2, true),
		crw676Entry("workflow-run:400:test-1#0", "test-1", "failure", 1, false),
	}
	problems := ChecksProblems(crw676Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
}
