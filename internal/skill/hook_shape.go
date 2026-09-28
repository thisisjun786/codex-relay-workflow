package skill

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

var probeFactLists = []string{"attempts", "claims", "conflicts", "resolutions"}
var probeFactObjects = []string{"intent", "bound", "relationship", "resolution"}
var probeIdentities = map[string][]string{
	"intent":       {"dispatchRequestIdHash", "dbPath"},
	"bound":        {"sessionId", "taskId"},
	"relationship": {"relationshipId"},
	"attempts":     {"taskId", "outcome"},
	"claims":       {"sessionId", "dispatchRequestId"},
	"conflicts":    {"attemptedSessionId", "attemptedTaskId"},
	"resolutions":  {"chosenTaskId", "chosenSessionId"},
	"resolution":   {"chosenTaskId", "chosenSessionId"},
}

func objHas(o hook.Object, key string) bool {
	for _, field := range o {
		if field.Key == key {
			return true
		}
	}
	return false
}

// Probe shape rules differ from the persisted relay reader: optional null facts
// are absent, and a list entry error names the entry rather than its container.
func probeMalformedFact(key string, record hook.Object) string {
	if (key == "resolution" || key == "resolutions") && objHas(record, "adjudicated") {
		items, ok := objGet(record, "adjudicated").([]any)
		if !ok {
			return key + ".adjudicated"
		}
		for _, item := range items {
			if asObject(item) == nil {
				return key + ".adjudicated entry"
			}
		}
	}
	for _, field := range probeIdentities[key] {
		if objHas(record, field) {
			if _, ok := objGet(record, field).(string); !ok {
				return key + "." + field
			}
		}
	}
	if key == "relationship" && objHas(record, "executionGeneration") {
		if value, ok := objGet(record, "executionGeneration").(int64); !ok || value < 1 {
			return key + ".executionGeneration"
		}
	}
	return ""
}

func probeMalformed(o hook.Object, reached *hookReplayReach) (hook.Object, string) {
	for _, key := range []string{"stop_input", "disposition", "receipt", "marker", "workspace"} {
		if value := objGet(o, key); value != nil && asObject(value) == nil {
			return o, key
		}
	}
	candidates := []any{objGet(o, "marker")}
	if workspace := asObject(objGet(o, "workspace")); workspace != nil {
		value := objGet(workspace, "assignments")
		items, ok := value.([]any)
		if value != nil && !ok {
			return o, "workspace.assignments"
		}
		for _, item := range items {
			if asObject(item) == nil {
				return o, "workspace.assignments entry"
			}
			candidates = append(candidates, item)
		}
	}
	for _, candidate := range candidates {
		marker := asObject(candidate)
		if value := objGet(marker, "claims"); value != nil {
			if _, ok := value.([]any); !ok {
				return o, "claims"
			}
		}
		if value := objGet(marker, "intent"); value != nil && asObject(value) == nil {
			return o, "intent"
		}
	}
	o = selectedObservationTrace(o, reached)
	marker := asObject(objGet(o, "marker"))
	for _, key := range probeFactLists {
		value := objGet(marker, key)
		items, ok := value.([]any)
		if value != nil && !ok {
			return o, key
		}
		for _, item := range items {
			record := asObject(item)
			if record == nil {
				return o, key + " entry"
			}
			if bad := probeMalformedFact(key, record); bad != "" {
				return o, bad
			}
		}
	}
	for _, key := range probeFactObjects {
		value := objGet(marker, key)
		record := asObject(value)
		if value != nil && record == nil {
			return o, key
		}
		if bad := probeMalformedFact(key, record); bad != "" {
			return o, bad
		}
	}
	return o, ""
}
