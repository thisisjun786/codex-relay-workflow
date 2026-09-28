package skill

import (
	"fmt"
	"sort"
	"strings"
)

func probeSorted(value any) ([]any, error) {
	items, err := hostList(value)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []any{}
	}
	sort.SliceStable(items, func(i, j int) bool { return fmt.Sprint(items[i]) < fmt.Sprint(items[j]) })
	return items, nil
}

func probeCapabilityMatrix(schemas map[string]map[string]any) (map[string]any, error) {
	events := map[string]any{}
	for title, schema := range schemas {
		name, kind, _ := strings.Cut(title, ".command.")
		entry, _ := events[name].(map[string]any)
		if entry == nil {
			entry = map[string]any{"event": name, "input": nil, "output": nil}
			events[name] = entry
		}
		props := schema["properties"]
		if !hostTruthy(props) {
			props = map[string]any{}
		}
		if kind == "input" {
			required, err := probeSorted(schema["required"])
			if err != nil {
				return nil, err
			}
			properties, err := probeSorted(props)
			if err != nil {
				return nil, err
			}
			entry["input"] = map[string]any{"required": required, "properties": properties}
		} else if kind == "output" {
			propertyMap, ok := props.(map[string]any)
			if !ok {
				return nil, pythonAttribute(props, "get")
			}
			defsValue := schema["definitions"]
			if !hostTruthy(defsValue) {
				defsValue = map[string]any{}
			}
			defs, ok := defsValue.(map[string]any)
			if !ok {
				return nil, pythonAttribute(defsValue, "get")
			}
			decision := defs["BlockDecisionWire"]
			if !hostTruthy(decision) {
				decision = defs["PreToolUseDecisionWire"]
			}
			additional := false
			for _, v := range defs {
				definition, ok := v.(map[string]any)
				if !ok {
					return nil, pythonAttribute(v, "get")
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
					if hostTruthy(properties) {
						return nil, pythonNotIterable(properties, true)
					}
				}
			}
			if !hostTruthy(decision) {
				decision = map[string]any{}
			}
			decisionMap, ok := decision.(map[string]any)
			if !ok {
				return nil, pythonAttribute(decision, "get")
			}
			values, err := probeSorted(decisionMap["enum"])
			if err != nil {
				return nil, err
			}
			properties, err := probeSorted(props)
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
		entry["blocksViaTopLevelDecision"] = ok && hostTruthy(output["topLevelDecisionValues"])
	}
	return events, nil
}
