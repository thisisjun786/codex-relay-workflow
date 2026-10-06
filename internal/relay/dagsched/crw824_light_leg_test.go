package dagsched

// CRW-824: the forge rows must carry the collector's testSkipped mark, so the scheduler's evidence
// predicate refuses a run whose go-product test leg concluded success without running its tests
// (CI light mode). Today the projection drops the mark and the run reads as evidence.

import (
	"strings"
	"testing"
)

// crw824Job is one workflow job of the collector's shape. A light test leg concluded success with
// its test step skipped.
func crw824Job(id int, name, conclusion string, steps []any) map[string]any {
	return map[string]any{"id": id, "name": name, "run_attempt": 1, "status": "completed", "conclusion": conclusion, "steps": steps}
}

// crw824LightScript is the CRW-790 shape over the real collector: the required dev-gate succeeded,
// and a go-product test leg of the same run and attempt concluded success while its test step was
// skipped.
func crw824LightScript() *ghScript {
	g := newGHScript()
	g.rules = []any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"strict_required_status_checks_policy": false,
		"required_status_checks": []any{map[string]any{"context": "go-product (test-1)"}}}}}
	g.checks = []any{}
	g.runs = []any{map[string]any{"id": 37363308899, "name": "CI", "head_sha": forgeHead, "workflow_id": 100, "event": "pull_request", "conclusion": "success", "created_at": "2026-10-06T06:00:00Z", "run_started_at": "2026-10-06T06:00:00Z"}}
	g.jobs = map[int][]any{37363308899: {
		crw824Job(41, "go-product (test-1)", "success", []any{map[string]any{"name": "Light mode notice", "conclusion": "success"}, map[string]any{"name": "Test and replay the contract corpus (test-1)", "conclusion": "skipped"}}),
		crw824Job(42, "go-product (test-2)", "success", []any{map[string]any{"name": "Test and replay the contract corpus (test-2)", "conclusion": "success"}}),
	}}
	return g
}

// The projection carries testSkipped for the light leg and leaves it false otherwise.
func TestCRW824ForgeProjectionCarriesTestSkipped(t *testing.T) {
	pr := crw824LightScript().read(t)
	carried := map[string]bool{}
	for _, c := range pr.Checks {
		carried[c.Name] = c.TestSkipped
	}
	if !carried["go-product (test-1)"] {
		t.Fatalf("the light leg must carry testSkipped: %v", carried)
	}
	if carried["go-product (test-2)"] {
		t.Fatalf("a leg that ran its tests must not: %v", carried)
	}
}

// A row without testSkipped gains no field, and a row with it keeps the field set to true.
func TestCRW824RebuiltRowsCarryTestSkippedOnlyWhenSet(t *testing.T) {
	rows := forgeRows([]Check{
		{RunID: "workflow-run:9:go-product (test-2)#0", Name: "go-product (test-2)", HeadSHA: forgeHead, Conclusion: "success", Attempt: 1},
		{RunID: "workflow-run:9:go-product (test-1)#0", Name: "go-product (test-1)", HeadSHA: forgeHead, Conclusion: "success", Attempt: 1, TestSkipped: true},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	plain, _ := rows[0].(map[string]any)
	if _, present := plain["testSkipped"]; present {
		t.Fatalf("a row without testSkipped gained the field: %v", plain)
	}
	marked, _ := rows[1].(map[string]any)
	if value, present := marked["testSkipped"]; !present || value != true {
		t.Fatalf("a row with testSkipped lost it: %v", marked)
	}
}

// A restated handoff record that states testSkipped as anything but a boolean reads false, the same
// strict read merge-evidence's testSkippedJobs uses.
func TestCRW824ANonBooleanTestSkippedReadsFalse(t *testing.T) {
	for _, value := range []any{"true", 1, nil} {
		snapshot := map[string]any{"pinned": map[string]any{"state": "open", "headSha": forgeHead},
			"handoff": map[string]any{"checks": []any{map[string]any{"runId": "workflow-run:9:go-product (test-1)#0", "name": "go-product (test-1)", "headSha": forgeHead, "conclusion": "success", "attempt": 1, "testSkipped": value}}}}
		pr := projectSnapshot(snapshot, "owner/name", 7)
		if len(pr.Checks) != 1 || pr.Checks[0].TestSkipped {
			t.Fatalf("testSkipped %#v read as set: %+v", value, pr.Checks)
		}
	}
}

// The same reading answers checks_stale: the required test leg concluded success without running its
// tests, so the head is not merge evidence.
func TestCRW824ForgeRowsAnswerChecksStaleForTheLightLeg(t *testing.T) {
	pr := crw824LightScript().read(t)
	if len(pr.CheckProblems) != 1 || !strings.HasPrefix(pr.CheckProblems[0], "checks_stale: ") {
		t.Fatalf("check problems = %v", pr.CheckProblems)
	}
	if !strings.Contains(pr.CheckProblems[0], "crw-lane") {
		t.Fatalf("the reading must name the label that repairs it: %v", pr.CheckProblems)
	}
}
