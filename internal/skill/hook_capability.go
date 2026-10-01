package skill

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// probeCapabilityMatrix reduces the schemas to what each event can do, in the order the binary
// first embeds each title, walking each schema's definitions in the order the schema spells
// them, so the first schema or definition that cannot be read is the one refused.
func probeCapabilityMatrix(titles []string, schemas map[string]contract.OrderedObject) (map[string]any, error) {
	events := map[string]any{}
	for _, title := range titles {
		ordered := schemas[title]
		schema, _ := orderedPlain(ordered).(map[string]any)
		name, kind, _ := strings.Cut(title, ".command.")
		entry, _ := events[name].(map[string]any)
		if entry == nil {
			entry = map[string]any{"event": name, "input": nil, "output": nil}
			events[name] = entry
		}
		props := schema["properties"]
		if !pyvalue.Truthy(props) {
			props = map[string]any{}
		}
		if kind == "input" {
			required, err := hostSorted(schema["required"])
			if err != nil {
				return nil, err
			}
			properties, err := hostSorted(props)
			if err != nil {
				return nil, err
			}
			entry["input"] = map[string]any{"required": required, "properties": properties}
		} else if kind == "output" {
			propertyMap, ok := props.(map[string]any)
			if !ok {
				return nil, notObject(props)
			}
			defsValue := schema["definitions"]
			if !pyvalue.Truthy(defsValue) {
				defsValue = map[string]any{}
			}
			defs, ok := defsValue.(map[string]any)
			if !ok {
				return nil, notObject(defsValue)
			}
			decision := defs["BlockDecisionWire"]
			if !pyvalue.Truthy(decision) {
				decision = defs["PreToolUseDecisionWire"]
			}
			additional := false
			// A falsy definitions is {} and has none.
			definitionsOrdered, _ := objGet(ordered, "definitions").(contract.OrderedObject)
			for _, field := range definitionsOrdered {
				v := defs[field.Key]
				definition, ok := v.(map[string]any)
				if !ok {
					return nil, notObject(v)
				}
				properties := definition["properties"]
				switch p := properties.(type) {
				case map[string]any:
					_, found := p["additionalContext"]
					additional = additional || found
				case string:
					additional = additional || strings.Contains(p, "additionalContext")
				case []any:
					for _, item := range p {
						if item == "additionalContext" {
							additional = true
						}
					}
				default:
					if pyvalue.Truthy(properties) {
						return nil, notList(properties)
					}
				}
			}
			if !pyvalue.Truthy(decision) {
				decision = map[string]any{}
			}
			decisionMap, ok := decision.(map[string]any)
			if !ok {
				return nil, notObject(decision)
			}
			values, err := hostSorted(decisionMap["enum"])
			if err != nil {
				return nil, err
			}
			properties, err := hostSorted(props)
			if err != nil {
				return nil, err
			}
			entry["output"] = map[string]any{"properties": properties, "topLevelDecisionValues": values, "hasHookSpecificOutput": propertyMap["hookSpecificOutput"] != nil, "canEmitAdditionalContext": additional}
		}
	}
	for _, raw := range events {
		entry := raw.(map[string]any)
		output, ok := entry["output"].(map[string]any)
		entry["canInfluence"] = ok
		entry["blocksViaTopLevelDecision"] = ok && pyvalue.Truthy(output["topLevelDecisionValues"])
	}
	return events, nil
}
