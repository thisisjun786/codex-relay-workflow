package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func pythonReportCapture(t *testing.T, id string) map[string]any {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	script, err := filepath.Abs("testdata/report_capture.py")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("uv", "run", "--no-sync", "python", script, id, filepath.Join(root, "capture"))
	cmd.Dir = filepath.Join(repo, "packages/codex-session-relay")
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "XDG_DATA_HOME="+root, "XDG_CONFIG_HOME="+root, "CODEX_HOME="+root, "TMPDIR="+os.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python %s: %v: %s", id, err, output)
	}
	var result map[string]any
	if err = json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func reportRefusalValue(err error) any {
	if err == nil {
		return nil
	}
	var r *store.RefusedError
	if !errors.As(err, &r) {
		return err.Error()
	}
	return map[string]any{"reason": r.Reason, "detail": r.Detail}
}
func compareReportCapture(t *testing.T, id string, got any) {
	t.Helper()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var normalized any
	if err = json.Unmarshal(raw, &normalized); err != nil {
		t.Fatal(err)
	}
	want := pythonReportCapture(t, id)
	if !reflect.DeepEqual(normalized, want) {
		t.Errorf("%s Go=%s Python=%s", id, raw, jsonText(want))
	}
}
func Test24_RC_1_NonVerification(t *testing.T) {
	got := map[string]string{}
	for _, key := range []string{"cxc_done", "pull_request_opened", "review_pass", "required_checks_green"} {
		got[key] = nonVerification[key]
	}
	compareReportCapture(t, "RC-1", got)
}
func Test24_RC_2_StatusCompatibility(t *testing.T) {
	human := map[string]any{}
	for _, name := range []string{"BLOCKED", "UNSAFE", "NEEDS_HUMAN"} {
		human[name] = map[string]any{"outcomes": reportOutcomes[name], "meaning": reportMeanings[name]}
	}
	compareReportCapture(t, "RC-2", map[string]any{"unknown": reportRefusalValue(checkReportStatus("SHIPPED", "ready_for_review")), "contradiction": reportRefusalValue(checkReportStatus("BLOCKED", "ready_for_review")), "human": human})
}
func Test24_RC_3_ReportCurrent(t *testing.T) {
	head := "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
	compareReportCapture(t, "RC-3", map[string]any{"head": reportRefusalValue(assertReportCurrent(head, 1, "9999999999999999999999999999999999999999", 1)), "generation": reportRefusalValue(assertReportCurrent(head, 1, "", 99)), "current": reportRefusalValue(assertReportCurrent(head, 1, head, 1))})
}
func Test24_RC_5_RequiredReportFields(t *testing.T) {
	got := map[string]any{"blank": map[string]any{}, "mapping": map[string]any{}}
	for _, field := range []string{"summary", "next_action", "repository"} {
		_, err := reportRequired("   ", field)
		got["blank"].(map[string]any)[field] = reportRefusalValue(err)
	}
	for _, field := range []string{"summary", "next_action", "repository", "cxc_reason"} {
		_, err := reportRequired(map[string]any{"result": "done"}, field)
		got["mapping"].(map[string]any)[field] = reportRefusalValue(err)
	}
	compareReportCapture(t, "RC-5", got)
}
func Test24_SR_11_UnusableReading(t *testing.T) {
	base := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "relationshipId": "rel-0123456789abcdef", "selectors": map[string]any{"turn": "turn-7"}}
	readings := []any{nil, "text", []any{}, map[string]any{"schema": "reporting-observation/1"},
		map[string]any{"schema": base["schema"], "reportingState": base["reportingState"], "relationshipId": base["relationshipId"], "selectors": map[string]any{"turn": []any{"turn-7"}}},
		map[string]any{"schema": base["schema"], "reportingState": base["reportingState"], "relationshipId": []any{"rel-0123456789abcdef"}, "selectors": base["selectors"]},
		map[string]any{"schema": "something-else/1", "reportingState": base["reportingState"], "relationshipId": base["relationshipId"], "selectors": base["selectors"]},
		map[string]any{"schema": base["schema"], "reportingState": "something", "relationshipId": base["relationshipId"], "selectors": base["selectors"]}}
	gaps := []any{}
	for _, r := range readings {
		gaps = append(gaps, unusableReportReading(r))
	}
	compareReportCapture(t, "SR-11", map[string]any{"gaps": gaps})
}
func Test24_SR_2_BlockIdentity(t *testing.T) {
	causes := []string{}
	ids := []string{}
	for _, pair := range [][2]string{{"waiting on API", "schema update"}, {"waiting on", "API schema update"}, {"waiting on API", "schema update"}} {
		cause, err := json.Marshal([]string{"BLOCKED", pair[0], pair[1]})
		if err != nil {
			t.Fatal(err)
		}
		subject := fmt.Sprintf("g1:%s", hash32(string(cause))[:16])
		causes = append(causes, subject)
		ids = append(ids, hash32("blocked|rel-one|"+subject))
	}
	compareReportCapture(t, "SR-2", map[string]any{"subjects": causes, "ids": ids})
}
func Test24_SR_8_ObservationOmission(t *testing.T) {
	reading := map[string]any{"schema": "reporting-observation/1", "reportingState": "unreported", "reason": "terminal_without_report", "relationshipId": "rel-0123456789abcdef", "executionGeneration": float64(1), "selectors": map[string]any{"turn": "turn-7"}}
	omission := ObservationObligation(reading)
	settled := map[string]any{}
	for _, state := range []string{"reported", "in_progress", "unmanaged", "unmeasured"} {
		r := map[string]any{}
		for k, v := range reading {
			r[k] = v
		}
		r["reportingState"] = state
		settled[state] = ObservationObligation(r)
	}
	compareReportCapture(t, "SR-8", map[string]any{"omission": omission, "settled": settled, "unmeasured": map[string]any{"schema": "supervisor-obligation/1", "gap": "reporting_unmeasured", "relationId": "rel-0123456789abcdef", "reason": "marker_unreadable", "detail": "nothing was established about whether a report was owed here"}, "badRelation": nil, "badTurn": nil, "blankTurn": nil})
}
func Test24_RC_15_VerdictGrammar(t *testing.T) {
	lines := []string{}
	for _, tc := range []struct {
		kind  string
		count any
	}{{"PASS", nil}, {"FAIL", nil}, {"GO-WITH-FIXES", 2}, {"GO-WITH-FIXES", 9999}} {
		line, err := reportVerdictLine(tc.kind, tc.count)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, line)
	}
	parsed := []any{}
	for _, line := range lines {
		parsed = append(parsed, parseReportVerdict(line))
	}
	prose := []any{}
	for _, line := range []string{"we think this is a PASS", "VERDICT: LOOKS FINE", "VERDICT: GO-WITH-FIXES", "VERDICT: GO-WITH-FIXES (blockers=1000000)"} {
		prose = append(prose, parseReportVerdict(line))
	}
	invalid := []any{}
	for _, tc := range []struct {
		kind  string
		count any
	}{{"GO-WITH-FIXES", nil}, {"GO-WITH-FIXES", 0}, {"GO-WITH-FIXES", -1}, {"GO-WITH-FIXES", true}, {"PASS", 2}, {"FAIL", 2}, {"GO-WITH-FIXES", 1000000000000}} {
		_, err := reportVerdictLine(tc.kind, tc.count)
		if err == nil {
			t.Fatal("expected invalid verdict")
		}
		invalid = append(invalid, err.Error())
	}
	unreviewed := reportAssertReviewed(false)
	if unreviewed == nil {
		t.Fatal("progress must not claim a verdict")
	}
	compareReportCapture(t, "RC-15", map[string]any{"lines": lines, "parsed": parsed, "prose": prose, "invalid": invalid, "unreviewed": unreviewed.Error()})
}
func Test24_RC_14_MessageVersions(t *testing.T) {
	compareReportCapture(t, "RC-14", map[string]any{"legacy": reportVersion(false), "report": reportVersion(true)})
}
func Test24_RC_4_QualifiedPR(t *testing.T) {
	compareReportCapture(t, "RC-4", map[string]any{"mine": reportPRRef("thisisjun786/codex-relay-workflow", 12), "other": reportPRRef("someone-else/other-project", 12), "keyMine": []any{"rel-one", "thisisjun786/codex-relay-workflow", 12}, "keyOther": []any{"rel-different", "someone-else/other-project", 12}})
}
