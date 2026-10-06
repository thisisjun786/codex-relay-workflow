package dagsched

// CRW-676: the Check struct carries the collector's notRun mark, the rebuilt rows write it back
// only when it is set, and a value that is not a boolean reads false.

import "testing"

// The projection carries notRun for the jobs that began no step and leaves it false otherwise.
func TestCRW676ForgeProjectionCarriesNotRun(t *testing.T) {
	pr := crw676IncidentScript(false).read(t)
	carried := map[string]bool{}
	for _, c := range pr.Checks {
		carried[c.Name] = c.NotRun
	}
	if carried["dev-gate"] || !carried["validate"] || !carried["secrets"] || !carried["test-2"] {
		t.Fatalf("projected notRun = %v", carried)
	}
}

// A row without notRun gains no field, and a row with it keeps the field set to true.
func TestCRW676RebuiltRowsCarryNotRunOnlyWhenSet(t *testing.T) {
	rows := forgeRows([]Check{
		{RunID: "check-run:1", Name: "dev-gate", HeadSHA: forgeHead, Conclusion: "failure", Attempt: 1},
		{RunID: "workflow-run:9:validate#0", Name: "validate", HeadSHA: forgeHead, Conclusion: "cancelled", Attempt: 1, NotRun: true},
	})
	if len(rows) != 2 {
		t.Fatalf("rows = %v", rows)
	}
	plain, _ := rows[0].(map[string]any)
	if _, present := plain["notRun"]; present {
		t.Fatalf("a row without notRun gained the field: %v", plain)
	}
	marked, _ := rows[1].(map[string]any)
	if value, present := marked["notRun"]; !present || value != true {
		t.Fatalf("a row with notRun lost it: %v", marked)
	}
}

// A restated handoff record that states notRun as anything but a boolean reads false, the same
// strict read merge-evidence's notRunJobs uses.
func TestCRW676ANonBooleanNotRunReadsFalse(t *testing.T) {
	for _, value := range []any{"true", 1, nil} {
		snapshot := map[string]any{"pinned": map[string]any{"state": "open", "headSha": forgeHead},
			"handoff": map[string]any{"checks": []any{map[string]any{"runId": "check-run:1", "name": "dev-gate", "headSha": forgeHead, "conclusion": "cancelled", "attempt": 1, "notRun": value}}}}
		pr := projectSnapshot(snapshot, "owner/name", 7)
		if len(pr.Checks) != 1 || pr.Checks[0].NotRun {
			t.Fatalf("notRun %#v read as set: %+v", value, pr.Checks)
		}
	}
}
