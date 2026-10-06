package evidence

// CRW-681: the checks_not_run reading is refined in four ways. A job that ended without beginning a
// step is not only a cancelled one, so a step-less failure or timeout is a job the runner never
// picked up; a sibling that began a step and concluded timed_out is a real failure like one that
// failed; a sibling failure the same job later superseded says nothing; and a restated record that
// marks notRun on a conclusion a job that began no step cannot carry is refused as malformed. Both
// readings stay not ready, so no merge condition widens.

import "testing"

const crw681Head = "5c1e7a93b0d4f62a8e5b3c9d7f1a2b4c6d8e0f13"

// crw681Entry is one check entry of the collector's shape.
func crw681Entry(run, name, conclusion string, attempt int, notRun bool) map[string]any {
	entry := map[string]any{"runId": run, "name": name, "headSha": crw681Head, "conclusion": conclusion, "attempt": attempt}
	if notRun {
		entry["notRun"] = true
	}
	return entry
}

// A sibling that ended without beginning a step is a job the runner never picked up, so it is not a
// real failure standing beside the required check: the required failure is the whole answer and the
// lane may rerun that run once on the same head. No cancelled row is present, because one would make
// notRunJobs non-empty before the change and the case would not be red (decision 1).
func TestCRW681StepLessFailureSiblingKeepsChecksNotRun(t *testing.T) {
	checks := []any{
		crw681Entry("workflow-run:500:dev-gate#0", "dev-gate", "failure", 1, false),
		crw681Entry("workflow-run:500:test-1#0", "test-1", "failure", 1, true),
	}
	problems := ChecksProblems(crw681Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
}

// A sibling that began a step and timed out is a real failure, the same as one that failed: the
// commit was tested and the lane returns its turn (decision 2).
func TestCRW681TimedOutSiblingAnswersChecksStale(t *testing.T) {
	checks := []any{
		crw681Entry("workflow-run:501:dev-gate#0", "dev-gate", "failure", 1, false),
		crw681Entry("workflow-run:501:test-2#0", "test-2", "cancelled", 1, true),
		crw681Entry("workflow-run:501:test-1#0", "test-1", "timed_out", 1, false),
	}
	problems := ChecksProblems(crw681Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
}

// A sibling failure the same job later superseded says nothing about its newest attempt: the
// required check's own run still never started, so the lane may rerun it (decision 3).
func TestCRW681SupersededSiblingFailureIsNotCounted(t *testing.T) {
	checks := []any{
		crw681Entry("workflow-run:502:dev-gate#0", "dev-gate", "failure", 1, false),
		crw681Entry("workflow-run:502:test-2#0", "test-2", "cancelled", 1, true),
		crw681Entry("workflow-run:502:test-1#0", "test-1", "failure", 1, false),
		crw681Entry("workflow-run:502:test-1#0", "test-1", "success", 2, false),
	}
	problems := ChecksProblems(crw681Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
}

// A sibling that still fails at its own newest attempt is a real failure, and it holds whether or
// not the same job also failed at an older attempt (decision 3).
func TestCRW681SiblingFailureAtTheNewestAttemptIsCounted(t *testing.T) {
	checks := []any{
		crw681Entry("workflow-run:503:dev-gate#0", "dev-gate", "failure", 2, false),
		crw681Entry("workflow-run:503:test-2#0", "test-2", "cancelled", 2, true),
		crw681Entry("workflow-run:503:test-1#0", "test-1", "failure", 1, false),
		crw681Entry("workflow-run:503:test-1#0", "test-1", "failure", 2, false),
	}
	problems := ChecksProblems(crw681Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
}

// The shape check refuses a mark the collector cannot produce: a job that began no step concluded
// cancelled, failure or timed_out, and nothing else (decision 4).
func TestCRW681ShapeRefusesNotRunOnAnotherConclusion(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "failure", "timed_out"} {
		entry := crw681Entry("workflow-run:504:dev-gate#0", "dev-gate", conclusion, 1, true)
		if problems := ShapeProblems(cleanReview(), []any{entry}, nil, nil); len(problems) != 0 {
			t.Fatalf("notRun true on %s must pass the shape check, got %v", conclusion, problems)
		}
	}
	for _, conclusion := range []string{"success", "skipped", "neutral", "action_required"} {
		entry := crw681Entry("workflow-run:504:dev-gate#0", "dev-gate", conclusion, 1, true)
		problems := ShapeProblems(cleanReview(), []any{entry}, nil, nil)
		if len(problems) != 1 || problems[0].Code != Malformed {
			t.Fatalf("notRun true on %s must be malformed, got %v", conclusion, problems)
		}
	}
}

// The predicate counts a step-less job of the three conclusions as one that never ran, so a
// required check of that shape answers checks_not_run (decision 4).
func TestCRW681NotRunOnAStepLessJobCounts(t *testing.T) {
	for _, conclusion := range []string{"cancelled", "failure", "timed_out"} {
		checks := []any{crw681Entry("workflow-run:505:dev-gate#0", "dev-gate", conclusion, 1, true)}
		problems := ChecksProblems(crw681Head, []string{"dev-gate"}, checks)
		if len(problems) != 1 || problems[0].Code != ChecksNotRun {
			t.Fatalf("a step-less %s must answer %s, got %v", conclusion, ChecksNotRun, problems)
		}
	}
}

// The collector marks a step-less job of the three conclusions and no other: a job that began a
// step, a success and a skipped job are not jobs the runner never picked up (decision 1).
func TestCRW681CollectorMarksStepLessJobsOfTheThreeConclusions(t *testing.T) {
	jobs := []any{
		map[string]any{"id": 21, "name": "dev-gate", "run_attempt": 1, "status": "completed", "conclusion": "failure", "started_at": "2026-10-06T04:00:00Z", "completed_at": "2026-10-06T04:05:00Z", "steps": []any{map[string]any{"name": "Prerequisites", "started_at": "2026-10-06T04:00:01Z"}}},
		map[string]any{"id": 22, "name": "validate", "run_attempt": 1, "status": "completed", "conclusion": "cancelled", "started_at": nil, "completed_at": nil, "steps": []any{}},
		map[string]any{"id": 23, "name": "secrets", "run_attempt": 1, "status": "completed", "conclusion": "cancelled", "started_at": nil, "completed_at": nil},
		map[string]any{"id": 24, "name": "test-1", "run_attempt": 1, "status": "completed", "conclusion": "failure", "started_at": nil, "completed_at": nil, "steps": []any{}},
		map[string]any{"id": 25, "name": "test-2", "run_attempt": 1, "status": "completed", "conclusion": "timed_out", "started_at": nil, "completed_at": nil, "steps": []any{}},
		map[string]any{"id": 26, "name": "test-3", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-06T04:10:00Z", "steps": []any{map[string]any{"name": "Run tests", "started_at": "2026-10-06T04:10:01Z"}}},
		map[string]any{"id": 27, "name": "test-4", "run_attempt": 1, "status": "completed", "conclusion": "skipped", "started_at": nil, "completed_at": nil, "steps": []any{}},
		map[string]any{"id": 28, "name": "test-5", "run_attempt": 1, "status": "completed", "conclusion": "timed_out", "started_at": "2026-10-06T04:20:00Z", "steps": []any{map[string]any{"name": "Set up job", "started_at": "2026-10-06T04:20:01Z"}}},
	}
	snapshot, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{map[string]any{"id": 5, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}},
		jobs: map[int][]any{5: jobs},
	}), "owner/name", 7)

	marked := map[string]bool{}
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		entry := mapOf(raw)
		if value, present := entry["notRun"]; present {
			flag, isBool := value.(bool)
			if !isBool || !flag {
				t.Fatalf("notRun must be true when present, got %v", value)
			}
			marked[strOf(entry["name"])] = true
		}
	}
	for _, name := range []string{"validate", "secrets", "test-1", "test-2"} {
		if !marked[name] {
			t.Fatalf("%s began no step and must carry notRun: %v", name, marked)
		}
	}
	for _, name := range []string{"dev-gate", "test-3", "test-4", "test-5"} {
		if marked[name] {
			t.Fatalf("%s must not carry notRun: %v", name, marked)
		}
	}

	detail := map[string]bool{}
	for _, raw := range listOf(snapshot["checkDetail"]) {
		entry := mapOf(raw)
		if value, present := entry["notRun"]; present {
			if flag, isBool := value.(bool); isBool && flag {
				detail[strOf(entry["name"])] = true
			}
		}
	}
	for _, name := range []string{"validate", "secrets", "test-1", "test-2"} {
		if !detail[name] {
			t.Fatalf("checkDetail must carry the same mark for %s: %v", name, detail)
		}
	}
}
