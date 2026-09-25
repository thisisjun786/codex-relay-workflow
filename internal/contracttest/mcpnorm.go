package contracttest

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// NormaliseMCPTools reduces a whole tools/list result object to what the contract compares:
// the frozen contract/schema/bridge-mcp-tools.json and a live result are equal after this and
// only then.
//
// Exactly three differences are removed, because none of them changes what a tool accepts:
//   - a "$schema" key anywhere in a tool schema (dialect declaration some SDKs add);
//   - key order in every object (JSON objects are unordered; decoding discards it);
//   - "additionalProperties": true inside a tool schema, which is JSON Schema's default.
//
// Everything else is kept and compared, at every level of the result: every top-level member
// (so an extra member such as a cache hint fails), tool names and their order, descriptions,
// annotations, every property's type, default, enum, const and title, and each "required"
// list in order.
func NormaliseMCPTools(raw []byte) (any, error) {
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("mcpnorm: decode: %w", err)
	}
	tools, ok := document["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("mcpnorm: result has no tools array")
	}
	out := shallow(document)
	normalised := make([]any, len(tools))
	for i, tool := range tools {
		entry := shallow(asObject(tool))
		for _, key := range []string{"inputSchema", "outputSchema"} {
			if schema, present := entry[key]; present {
				entry[key] = normaliseSchema(schema)
			}
		}
		normalised[i] = entry
	}
	out["tools"] = normalised
	return out, nil
}

func normaliseSchema(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if key == "$schema" {
				continue
			}
			if key == "additionalProperties" && item == true {
				continue
			}
			out[key] = normaliseSchema(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = normaliseSchema(item)
		}
		return out
	default:
		return value
	}
}

// EqualMCPTools compares two tools/list documents after NormaliseMCPTools and names the first
// tool that differs.
func EqualMCPTools(want, got []byte) error {
	a, err := NormaliseMCPTools(want)
	if err != nil {
		return err
	}
	b, err := NormaliseMCPTools(got)
	if err != nil {
		return err
	}
	if reflect.DeepEqual(a, b) {
		return nil
	}
	for key := range asObject(b) {
		if _, expected := asObject(a)[key]; !expected {
			return fmt.Errorf("mcpnorm: the listed result carries %q, which the contract does not", key)
		}
	}
	for key := range asObject(a) {
		if _, listed := asObject(b)[key]; !listed {
			return fmt.Errorf("mcpnorm: the listed result lacks %q", key)
		}
	}
	wantTools, gotTools := asObject(a)["tools"].([]any), asObject(b)["tools"].([]any)
	if len(wantTools) != len(gotTools) {
		return fmt.Errorf("mcpnorm: %d tools listed, the contract has %d", len(gotTools), len(wantTools))
	}
	for i := range wantTools {
		if !reflect.DeepEqual(wantTools[i], gotTools[i]) {
			wantJSON, _ := json.Marshal(wantTools[i])
			gotJSON, _ := json.Marshal(gotTools[i])
			return fmt.Errorf("mcpnorm: tool %d differs:\ncontract %s\nlisted   %s", i, wantJSON, gotJSON)
		}
	}
	return fmt.Errorf("mcpnorm: documents differ")
}
