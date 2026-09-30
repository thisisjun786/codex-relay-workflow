package skill

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func hookShapeCases(t *testing.T) []skillShapeCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(), "plugins", defaultFixture("decisions"), "t24-invalid-persisted-counts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	base := fixture["steps"].([]any)[0].(map[string]any)["observation"].(map[string]any)
	base["now"] = "2026-01-01T00:05:00Z"
	base["counters"] = map[string]any{"holdsThisTurn": 0, "holdsThisGeneration": 0, "holdsThisSessionWindow": 0}
	base["disposition"] = map[string]any{"outcome": "ready_for_review", "sessionId": "session-1111", "turnId": "turn-0001"}
	base["receipt"] = map[string]any{"atCurrentHead": false, "sessionId": "session-1111", "turnId": "turn-0001", "relationshipId": "rel-0001", "evidence": "absent", "detail": "missing"}
	base["store_unreadable"] = []any{}
	marker := base["marker"].(map[string]any)
	marker["attempts"] = []any{map[string]any{"taskId": "task-0001", "outcome": "accepted", "factId": "attempts/a.json"}}
	marker["conflicts"] = []any{map[string]any{"attemptedTaskId": "task-0001", "attemptedSessionId": "session-1111", "factId": "conflicts/a.json"}}
	resolution := map[string]any{"chosenTaskId": "task-0001", "chosenSessionId": "session-1111", "adjudicated": []any{map[string]any{"factId": "claims/session-1111/claim.json", "digest": "bad"}}}
	marker["resolution"] = resolution
	marker["resolutions"] = []any{shapeClone(t, resolution)}
	paths := []string{"", "stop_input", "stop_input.cwd", "stop_input.session_id", "stop_input.turn_id", "stop_input.stop_hook_active", "disposition", "disposition.outcome", "disposition.sessionId", "disposition.turnId", "receipt", "receipt.atCurrentHead", "receipt.sessionId", "receipt.turnId", "receipt.relationshipId", "receipt.evidence", "receipt.detail", "counters", "counters.holdsThisTurn", "counters.holdsThisGeneration", "counters.holdsThisSessionWindow", "counters.extra", "store_unreadable", "assignment", "now", "marker"}
	for _, key := range []string{"intent", "bound", "relationship", "resolution"} {
		paths = append(paths, "marker."+key)
	}
	for _, key := range []string{"attempts", "claims", "conflicts", "resolutions"} {
		paths = append(paths, "marker."+key, "marker."+key+".0")
	}
	for _, path := range []string{"intent.declaredAt", "intent.dispatchRequestIdHash", "intent.dbPath", "bound.sessionId", "bound.taskId", "relationship.relationshipId", "relationship.executionGeneration", "attempts.0.taskId", "attempts.0.outcome", "attempts.0.factId", "claims.0.sessionId", "claims.0.dispatchRequestId", "claims.0.factId", "conflicts.0.attemptedTaskId", "conflicts.0.attemptedSessionId", "conflicts.0.factId"} {
		paths = append(paths, "marker."+path)
	}
	for _, key := range []string{"resolution", "resolutions.0"} {
		for _, path := range []string{"chosenTaskId", "chosenSessionId", "adjudicated", "adjudicated.0", "adjudicated.0.factId", "adjudicated.0.digest"} {
			paths = append(paths, "marker."+key+"."+path)
		}
	}
	var cases []skillShapeCase
	add := func(name string, value any) {
		cases = append(cases, skillShapeCase{name: "hook/" + name, family: "hook-probe", args: []string{"decide", "$TMP/observation.json"}, files: map[string]any{"observation.json": map[string]any{"observation": value}}})
	}
	for _, path := range paths {
		var parts []string
		if path != "" {
			parts = strings.Split(path, ".")
		}
		for _, variant := range shapeVariants() {
			add(path+"/"+variant.name, shapeSet(shapeClone(t, base), parts, variant))
		}
	}
	workspace := shapeClone(t, base).(map[string]any)
	workspace["workspace"] = map[string]any{"assignments": []any{shapeClone(t, marker)}}
	for _, path := range []string{"workspace", "workspace.assignments", "workspace.assignments.0", "workspace.assignments.0.assignmentId", "workspace.assignments.0.intent", "workspace.assignments.0.claims"} {
		for _, variant := range shapeVariants() {
			add(path+"/"+variant.name, shapeSet(shapeClone(t, workspace), strings.Split(path, "."), variant))
		}
	}
	for _, stamp := range []string{"2026-01-01T00:00:00", "2026-01-01", "2026-01-01T09:00:00+09:00", "2026-01-01T00:00:00Z", "invalid"} {
		for _, path := range []string{"marker.intent.declaredAt", "now"} {
			add(path+"/"+stamp, shapeSet(shapeClone(t, base), strings.Split(path, "."), shapeVariant{value: stamp}))
		}
		add("workspace.timestamp/"+stamp, shapeSet(shapeClone(t, workspace), strings.Split("workspace.assignments.0.intent.declaredAt", "."), shapeVariant{value: stamp}))
	}
	replay := map[string]any{"observation": map[string]any{}, "expected": map[string]any{"decision": "release", "state": "unmanaged", "record": map[string]any{"held": false}}}
	for _, path := range []string{"", "observation", "expected", "expected.decision", "expected.state", "expected.observation", "expected.reason", "expected.receiptEvidence", "expected.receiptDetail", "expected.record", "expected.record.held", "steps"} {
		var parts []string
		if path != "" {
			parts = strings.Split(path, ".")
		}
		for _, variant := range shapeVariants() {
			cases = append(cases, skillShapeCase{name: "hook-replay/" + path + "/" + variant.name, family: "hook-probe", args: []string{"replay", "--fixtures", "$TMP/decisions", "--allow-unreached"}, files: map[string]any{"decisions/shape.json": shapeSet(shapeClone(t, replay), parts, variant)}})
		}
	}
	sequence := map[string]any{"steps": []any{shapeClone(t, replay)}}
	for _, path := range []string{"steps.0", "steps.0.observation", "steps.0.expected", "steps.0.expected.record"} {
		for _, variant := range shapeVariants() {
			cases = append(cases, skillShapeCase{name: "hook-replay/" + path + "/" + variant.name, family: "hook-probe", args: []string{"replay", "--fixtures", "$TMP/decisions", "--allow-unreached"}, files: map[string]any{"decisions/shape.json": shapeSet(shapeClone(t, sequence), strings.Split(path, "."), variant)}})
		}
	}
	for _, mode := range []string{"unmanaged", "unbound", "other-session", "declared", "unregistered"} {
		observation := shapeClone(t, base).(map[string]any)
		selected := observation["marker"].(map[string]any)
		switch mode {
		case "unmanaged":
			observation["marker"] = nil
		case "unbound":
			delete(selected, "bound")
		case "other-session":
			selected["bound"].(map[string]any)["sessionId"] = "other-session"
		case "declared":
			observation["disposition"].(map[string]any)["outcome"] = "in_progress"
		case "unregistered":
			delete(selected, "relationship")
		}
		for _, variant := range shapeVariants() {
			add("counter-precedence-"+mode+"/"+variant.name, shapeSet(shapeClone(t, observation), []string{"counters"}, variant))
		}
	}
	for _, stamp := range []string{"2026-01-01T00:00:00", "2026-01-01", "2026-01-01T09:00:00+09:00", "2026-01-01T00:00:00Z", "invalid"} {
		observation := shapeClone(t, base).(map[string]any)
		selected := observation["marker"].(map[string]any)
		delete(selected, "bound")
		selected["intent"].(map[string]any)["declaredAt"] = stamp
		observation["now"] = "2026-01-01T00:31:00Z"
		add("unbound-expiry/"+stamp, observation)
	}
	return append(cases, hookDecideSpellingCases()...)
}

// hookDecideSpellingCases echo input values into decide's printed JSON:
// record.turnId, record.sessionId, record.at and the receipt fields. Python
// prints them with json.dumps(indent=2, sort_keys=True), so non-ASCII text is
// escaped and each number keeps Python's spelling (1.0, 1e+19, NaN). The
// observations are raw JSON because Go's encoder cannot write 1.0 or NaN.
func hookDecideSpellingCases() []skillShapeCase {
	observations := []struct{ name, observation string }{
		{"non-ascii-ids", `{"stop_input": {"turn_id": "té", "session_id": "sé"}}`},
		{"astral-ids", `{"stop_input": {"turn_id": "😀", "session_id": "\ud83d\ude00"}}`},
		{"lone-surrogate-id", `{"stop_input": {"turn_id": "\ud800", "session_id": "\udfff"}}`},
		{"control-characters", `{"stop_input": {"turn_id": "a\u0001\b\f\u007f\u2028", "session_id": "</script>&'"}}`},
		{"integral-floats", `{"stop_input": {"turn_id": 1.0, "session_id": 1e19}, "now": 2.0}`},
		{"float-spellings", `{"stop_input": {"turn_id": -0.0, "session_id": 1e-7}, "now": 1.5e300}`},
		{"non-finite", `{"stop_input": {"turn_id": NaN, "session_id": Infinity}, "now": -Infinity}`},
		{"big-ints", `{"stop_input": {"turn_id": 12345678901234567890, "session_id": -12345678901234567890}, "now": 1e400}`},
		{"nested-ids", `{"stop_input": {"turn_id": {"b": 1.0, "a": ["é", 2]}, "session_id": [{"z": null, "y": true}]}}`},
		{"receipt-echo", `{"stop_input": {"turn_id": "t", "session_id": "s"}, "receipt": {"evidence": "ré", "detail": 2.50}}`},
	}
	cases := make([]skillShapeCase, 0, len(observations))
	for _, o := range observations {
		cases = append(cases, skillShapeCase{
			name:   "hook/spelling/" + o.name,
			family: "hook-probe",
			args:   []string{"decide", "$TMP/observation.json"},
			files:  map[string]any{"observation.json": shapeRawFile(`{"observation": ` + o.observation + `}`)},
		})
	}
	return cases
}
