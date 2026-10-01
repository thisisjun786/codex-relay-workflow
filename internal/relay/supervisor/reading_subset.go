package supervisor

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// Subset ported for todo 24; todo 22 owns and extends omission observation.
func unusableReportReading(reading any) map[string]any {
	gap := map[string]any{"schema": "supervisor-obligation/1", "gap": "reading_unusable", "relationId": nil, "detail": "this reading was not placed in any project"}
	record, ok := reading.(map[string]any)
	if !ok {
		kind := pyvalue.TypeName(reading)
		gap["reason"] = "a reading is an object, not " + kind
		return gap
	}
	scope := record["relationshipId"]
	if s, ok := scope.(string); ok {
		gap["relationId"] = s
	}
	schema := record["schema"]
	switch {
	case schema != "reporting-observation/1":
		gap["reason"] = fmt.Sprintf("schema %s is not reporting-observation/1", pyvalue.Repr(schema))
	case !validReportState(record["reportingState"]):
		gap["reason"] = fmt.Sprintf("reportingState %s is not one a reporting-observation/1 reading carries", pyvalue.Repr(record["reportingState"]))
	default:
		missing := []string{}
		if !reportNamed(scope) {
			missing = append(missing, "relationship")
		}
		selectors, _ := record["selectors"].(map[string]any)
		if !reportNamed(selectors["turn"]) {
			missing = append(missing, "turn")
		}
		if len(missing) > 0 {
			gap["reason"] = "the reading names no usable " + strings.Join(missing, " or ")
		} else {
			gap["reason"] = "the reading is well formed, and nothing said why it could not be placed"
		}
	}
	return gap
}

func reportNamed(v any) bool { s, ok := v.(string); return ok && strings.TrimSpace(s) != "" }
func validReportState(v any) bool {
	switch v {
	case "reported", "in_progress", "unmanaged", "unmeasured", "unreported":
		return true
	}
	return false
}
