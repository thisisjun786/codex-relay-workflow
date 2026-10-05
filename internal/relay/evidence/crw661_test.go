package evidence

// CRW-661: a required check that never ran is told apart from one that failed. A workflow job
// that concluded cancelled without beginning any step never reached a runner, so the lane may
// rerun it on the same head; every other non-success stays checks_stale and the lane returns its
// turn. Both answers stay not ready, so no merge condition widens.

import (
	"strings"
	"testing"
)

const crw661Head = "c68be165ae8ee4a645f3266eae3e9c543a851382"

// crw661Entry is one check entry of the collector's shape.
func crw661Entry(run, name, conclusion string, attempt int, notRun bool) map[string]any {
	entry := map[string]any{"runId": run, "name": name, "headSha": crw661Head, "conclusion": conclusion, "attempt": attempt}
	if notRun {
		entry["notRun"] = true
	}
	return entry
}

// crw661Incident is the Actions incident's shape: dev-gate failed on its own aggregate step while
// the six jobs of the same workflow run concluded cancelled without a step. With jobsNotRun false
// the same six jobs are cancelled after beginning a step, which is a different thing.
func crw661Incident(gateNotRun, jobsNotRun bool) []any {
	checks := []any{crw661Entry("workflow-run:37363308811:dev-gate#0", "dev-gate", "failure", 1, gateNotRun)}
	for _, name := range []string{"validate", "secrets", "test-1", "test-2", "test-3", "test-4"} {
		checks = append(checks, crw661Entry("workflow-run:37363308811:"+name+"#0", name, "cancelled", 1, jobsNotRun))
	}
	return checks
}

func TestCRW661IncidentShapeAnswersChecksNotRun(t *testing.T) {
	problems := ChecksProblems(crw661Head, []string{"dev-gate"}, crw661Incident(false, true))
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
	detail := problems[0].Detail
	if !strings.Contains(detail, "37363308811") {
		t.Fatalf("the detail must name the run that did not run: %q", detail)
	}
	for _, name := range []string{"validate", "test-4"} {
		if !strings.Contains(detail, name) {
			t.Fatalf("the detail must name the job %s that did not run: %q", name, detail)
		}
	}
	if VerdictOf(problems) != NotReady {
		t.Fatalf("checks_not_run must stay not ready, got %s", VerdictOf(problems))
	}
}

// A cancelled job that did begin a step is not a job that never ran: it says something about the
// commit, so the answer stays checks_stale.
func TestCRW661CancelledJobThatRanKeepsChecksStale(t *testing.T) {
	problems := ChecksProblems(crw661Head, []string{"dev-gate"}, crw661Incident(false, false))
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
}

// One job that never ran among jobs that did is still a job that never ran: the lane reruns.
func TestCRW661OneStepLessJobAmongJobsThatRan(t *testing.T) {
	checks := crw661Incident(false, false)
	checks[3] = crw661Entry("workflow-run:37363308811:test-1#0", "test-1", "cancelled", 1, true)
	problems := ChecksProblems(crw661Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
}

// The required check itself may be the job that never got a runner.
func TestCRW661RequiredCheckItselfNeverRan(t *testing.T) {
	checks := []any{crw661Entry("workflow-run:7:dev-gate#0", "dev-gate", "cancelled", 1, true)}
	problems := ChecksProblems(crw661Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksNotRun {
		t.Fatalf("want one %s, got %v", ChecksNotRun, problems)
	}
}

// A step-less cancelled job of a different workflow run says nothing about this run: the code
// failure is still a code failure.
func TestCRW661NotRunOfAnotherRunStaysChecksStale(t *testing.T) {
	checks := []any{
		crw661Entry("workflow-run:100:dev-gate#0", "dev-gate", "failure", 1, false),
		crw661Entry("workflow-run:99:dev-gate#0", "dev-gate", "cancelled", 1, true),
	}
	problems := ChecksProblems(crw661Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
}

// A notRun job of an older attempt does not speak for the newest one.
func TestCRW661NotRunOfAnOlderAttemptStaysChecksStale(t *testing.T) {
	checks := []any{
		crw661Entry("workflow-run:100:dev-gate#0", "dev-gate", "failure", 2, false),
		crw661Entry("workflow-run:100:test-1#0", "test-1", "cancelled", 1, true),
	}
	problems := ChecksProblems(crw661Head, []string{"dev-gate"}, checks)
	if len(problems) != 1 || problems[0].Code != ChecksStale {
		t.Fatalf("want one %s, got %v", ChecksStale, problems)
	}
}

func TestCRW661AllSuccessStaysClean(t *testing.T) {
	checks := []any{crw661Entry("workflow-run:100:dev-gate#0", "dev-gate", "success", 1, false)}
	if problems := ChecksProblems(crw661Head, []string{"dev-gate"}, checks); len(problems) != 0 {
		t.Fatalf("want no problem, got %v", problems)
	}
}

// The restated record may carry the optional field; a value that is not a boolean is malformed,
// because a string "false" would otherwise read as a job that never ran.
func TestCRW661ShapeAcceptsTheOptionalNotRun(t *testing.T) {
	good := crw661Entry("workflow-run:100:dev-gate#0", "dev-gate", "cancelled", 1, true)
	if problems := ShapeProblems(cleanReview(), []any{good}, nil, nil); len(problems) != 0 {
		t.Fatalf("a boolean notRun must pass the shape check, got %v", problems)
	}
	bad := crw661Entry("workflow-run:100:dev-gate#0", "dev-gate", "cancelled", 1, false)
	bad["notRun"] = "true"
	problems := ShapeProblems(cleanReview(), []any{bad}, nil, nil)
	if len(problems) != 1 || problems[0].Code != Malformed {
		t.Fatalf("a non-boolean notRun must be malformed, got %v", problems)
	}
}

// The collector marks the step-less cancelled job and no other, in the check entry and in the
// checkDetail entry alike.
func TestCRW661CollectorMarksOnlyStepLessCancelledJobs(t *testing.T) {
	jobs := []any{
		map[string]any{"id": 11, "name": "dev-gate", "run_attempt": 1, "status": "completed", "conclusion": "failure", "started_at": "2026-10-05T19:11:00Z", "steps": []any{map[string]any{"name": "Prerequisites", "started_at": "2026-10-05T19:11:01Z"}}},
		map[string]any{"id": 12, "name": "validate", "run_attempt": 1, "status": "completed", "conclusion": "cancelled", "started_at": nil, "completed_at": nil, "steps": []any{}},
		map[string]any{"id": 13, "name": "secrets", "run_attempt": 1, "status": "completed", "conclusion": "cancelled", "started_at": nil, "completed_at": nil},
		map[string]any{"id": 14, "name": "test-1", "run_attempt": 1, "status": "completed", "conclusion": "cancelled", "started_at": "2026-10-05T19:12:00Z", "steps": []any{map[string]any{"name": "Set up job", "started_at": "2026-10-05T19:12:01Z"}}},
		map[string]any{"id": 15, "name": "test-2", "run_attempt": 1, "status": "completed", "conclusion": "success", "started_at": "2026-10-05T19:13:00Z", "steps": []any{map[string]any{"name": "Run tests", "started_at": "2026-10-05T19:13:01Z"}}},
	}
	snapshot, _ := Collect(fixedForge(&collectorScript{
		threads: 1, unresolved: map[int]bool{},
		runs: []any{map[string]any{"id": 1, "name": "CI", "head_sha": collectorHead, "workflow_id": 100, "event": "pull_request"}},
		jobs: map[int][]any{1: jobs},
	}), "owner/name", 7)

	got := map[string]bool{}
	for _, raw := range listOf(mapOf(snapshot["handoff"])["checks"]) {
		entry := mapOf(raw)
		if value, present := entry["notRun"]; present {
			if flag, isBool := value.(bool); !isBool || !flag {
				t.Fatalf("notRun must be true when present, got %v", value)
			}
			got[strOf(entry["name"])] = true
		}
	}
	if !got["validate"] || !got["secrets"] {
		t.Fatalf("the step-less cancelled jobs must carry notRun: %v", got)
	}
	for _, name := range []string{"dev-gate", "test-1", "test-2"} {
		if got[name] {
			t.Fatalf("%s must not carry notRun: %v", name, got)
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
	if !detail["validate"] || !detail["secrets"] || detail["dev-gate"] {
		t.Fatalf("checkDetail must carry the same value: %v", detail)
	}
}
