package crwconfig

import (
	"bytes"
	"encoding/json"
)

// DefaultChildCheckDisabledServers is the default of manage.child_check.disabled_servers: the MCP servers crw manage
// child-resume switches off on a resume. The installation defines no server to stop, so the default is the empty list
// (CRW-1398). A new slice on every call, so a caller may keep or change it.
func DefaultChildCheckDisabledServers() []string { return []string{} }

// withChildCheckDefault writes the default of manage.child_check.disabled_servers into doc where the document does not name it: a
// missing manage object becomes one, a manage object without child_check gets the section, a section without the key gets the key.
// A key the document names (any list, empty included, or any value) is the operator's and stays; so does a manage or child_check
// value that is no object, which this writer does not repair. The other keys of every object are kept as they are.
func withChildCheckDefault(doc map[string]json.RawMessage) {
	manage := map[string]json.RawMessage{}
	if raw, ok := doc["manage"]; ok {
		if !isJSONObject(raw) || json.Unmarshal(raw, &manage) != nil || manage == nil {
			return
		}
	}
	section := map[string]json.RawMessage{}
	if raw, ok := manage["child_check"]; ok {
		if !isJSONObject(raw) || json.Unmarshal(raw, &section) != nil || section == nil {
			return
		}
	}
	if _, ok := section["disabled_servers"]; ok {
		return
	}
	section["disabled_servers"], _ = json.Marshal(DefaultChildCheckDisabledServers())
	manage["child_check"], _ = json.Marshal(section)
	doc["manage"], _ = json.Marshal(manage)
}

// isJSONObject reports whether raw is a JSON object (not null, an array or a scalar).
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}
