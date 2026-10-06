package dagsched

// CRW-676: the forge rows must carry the collector's notRun mark, so the scheduler's evidence
// predicate can tell a required check whose runner never picked it up from one that failed for real.
// Today the projection drops the mark and the required failure reads as checks_stale.

import (
	"strconv"
	"strings"
	"testing"
)

// runIDOf is the workflow run id in an /actions/runs/<id>/jobs URL.
func (g *ghScript) runIDOf(url string) int {
	rest := url[strings.Index(url, "/actions/runs/")+len("/actions/runs/"):]
	text, _, _ := strings.Cut(rest, "/")
	id, _ := strconv.Atoi(text)
	return id
}

// crw676Job is one workflow job of the collector's shape. A step-less cancelled job is the one the
// collector marks notRun.
func crw676Job(id int, name, conclusion string, began bool) map[string]any {
	job := map[string]any{"id": id, "name": name, "run_attempt": 1, "status": "completed", "conclusion": conclusion, "steps": []any{}}
	if began {
		job["started_at"] = "2026-10-05T19:11:00Z"
		job["completed_at"] = "2026-10-05T19:12:00Z"
		job["steps"] = []any{map[string]any{"name": "Run", "started_at": "2026-10-05T19:11:01Z"}}
	}
	return job
}

// crw676IncidentScript is the CRW-676 shape over the real collector: the required dev-gate failed,
// the rest of the workflow's jobs were cancelled before they started, and with withRealFailure a
// matrix sibling (test-1) began a step and failed for real.
func crw676IncidentScript(withRealFailure bool) *ghScript {
	g := newGHScript()
	g.rules = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": false,
		"required_status_checks": []any{map[string]any{"context": "dev-gate"}}}}}
	g.checks = []any{}
	g.runs = []any{map[string]any{"id": 37363308811, "name": "CI", "head_sha": forgeHead, "workflow_id": 100, "event": "pull_request", "conclusion": "failure", "created_at": "2026-10-05T19:10:00Z", "run_started_at": "2026-10-05T19:10:00Z"}}
	jobs := []any{crw676Job(11, "dev-gate", "failure", true), crw676Job(12, "validate", "cancelled", false), crw676Job(13, "secrets", "cancelled", false), crw676Job(14, "test-2", "cancelled", false)}
	if withRealFailure {
		jobs = append(jobs, crw676Job(15, "test-1", "failure", true))
	}
	g.jobs = map[int][]any{37363308811: jobs}
	return g
}

// The required check's failure is a job that never ran, so the scheduler's answer is checks_not_run
// and the lane may rerun it once on the same head.
func TestCRW676ForgeRowsAnswerChecksNotRunForTheStepLessRun(t *testing.T) {
	pr := crw676IncidentScript(false).read(t)
	if len(pr.CheckProblems) != 1 || !strings.HasPrefix(pr.CheckProblems[0], "checks_not_run: ") {
		t.Fatalf("check problems = %v", pr.CheckProblems)
	}
}

// A matrix sibling that began a step and failed for real is not a job that never ran: the required
// failure is a real failure and the answer stays checks_stale.
func TestCRW676ARealFailureBesideTheNotRunJobsAnswersChecksStale(t *testing.T) {
	pr := crw676IncidentScript(true).read(t)
	if len(pr.CheckProblems) != 1 || !strings.HasPrefix(pr.CheckProblems[0], "checks_stale: ") {
		t.Fatalf("check problems = %v", pr.CheckProblems)
	}
}
