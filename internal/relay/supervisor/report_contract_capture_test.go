package supervisor

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// compareReportCapture compares a report value, as JSON decodes it, with the golden under id; the
// golden began as what the Python report test (the former testdata/report_capture.py) captured.
func compareReportCapture(t *testing.T, id string, got any) {
	t.Helper()
	golden.CheckJSON(t, id, asJSON(t, got), golden.Substitute(repoRoot(t), "<repo>"))
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
