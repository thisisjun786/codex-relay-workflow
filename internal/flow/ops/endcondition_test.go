package ops

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExploreRecordWarnsOfTheEndCondition (CRW-1382 d2): the answer of the explorer that was given the end condition commands
// (end_conditions_asked) records each command's exit code and the tests it ran, from the end_conditions of its FINDINGS, on the
// crw.exploreRecord row; a command that exited non-zero, one that ran no test and one the explorer did not report are warnings of the
// row, never an error of the op. An answer that was given none records none and warns of nothing.
func TestExploreRecordWarnsOfTheEndCondition(t *testing.T) {
	w := newWorld(t)
	findings := func(ran []EndConditionRun) json.RawMessage {
		raw, _ := json.Marshal(Findings{Answer: "a", Evidence: []string{}, Verified: []string{}, Inferred: []string{}, Unknown: []string{}, EndConditions: ran})
		return raw
	}
	asked := []EndConditionAsk{{Issue: "CRW-A", Command: "go test ./a"}, {Issue: "CRW-B", Command: "go test ./b"}, {Issue: "CRW-C", Command: "go test ./c"}}
	w.must("exploreRecord", ExploreOpen, ExploreContext{Milestone: "M"}, []Answer{
		{Digest: "d1", Question: "q1", Answer: "a", Route: RouteSpawn, EndConditionsAsked: asked,
			Findings: findings([]EndConditionRun{{Command: "go test ./a", ExitCode: 1, Tests: 4, Summary: "FAIL"}, {Command: "go test ./b", ExitCode: 0, Tests: 0, Summary: "no tests"}})},
		{Digest: "d2", Question: "q2", Answer: "b", Route: RouteSpawn, Findings: findings([]EndConditionRun{})},
	})
	row := w.j.Events()[len(w.j.Events())-1]
	var warnings []string
	for _, x := range row.Data["warnings"].([]any) {
		warnings = append(warnings, x.(string))
	}
	joined := strings.Join(warnings, " | ")
	if len(warnings) != 3 || !strings.Contains(joined, WarnEndConditionFailed+": `go test ./a`") || !strings.Contains(joined, WarnEndConditionNoTests+": `go test ./b`") ||
		!strings.Contains(joined, WarnEndConditionUnreported+": `go test ./c`") {
		t.Errorf("the warnings: %q", warnings)
	}
	list := row.Data["answers"].([]any)
	first, second := list[0].(map[string]any), list[1].(map[string]any)
	ran, _ := first["end_conditions"].([]any)
	if len(ran) != 2 || ran[0].(map[string]any)["exit_code"] != float64(1) || ran[1].(map[string]any)["tests"] != float64(0) {
		t.Errorf("the first answer's end conditions: %v", first["end_conditions"])
	}
	if _, has := second["end_conditions"]; has {
		t.Errorf("an answer that was given no end condition records some: %v", second)
	}
}
