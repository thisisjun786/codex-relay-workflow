package routing

import "github.com/thisisjun786/codex-relay-workflow/internal/contract"

func fields(value Object, keys ...string) contract.OrderedObject {
	out := make(contract.OrderedObject, 0, len(keys))
	for _, key := range keys {
		if v, ok := value[key]; ok {
			out = append(out, contract.Field{Key: key, Value: v})
		}
	}
	return out
}

// PlacementRecord preserves placement._decision's insertion order at the JSON boundary.
func PlacementRecord(value Object) contract.OrderedObject {
	return fields(value, "disposition", "stage", "project", "owner", "hold", "relate", "reopen", "reason")
}

// CompletionRecord preserves completion.evaluate's full JSON shape and insertion order.
// Only validated/evaluated completion values belong here; arbitrary objects are not accepted.
func CompletionRecord(value Object) contract.OrderedObject {
	checks := []any{}
	for _, item := range list(value["checks"]) {
		entry := object(item)
		out := fields(entry, "check", "verdict", "reason")
		if closure := object(entry["closure"]); closure != nil {
			c := fields(closure, "ready", "missing")
			for _, name := range []string{"fix", "verification"} {
				if ref := object(closure[name]); ref != nil {
					c = append(c, contract.Field{Key: name, Value: fields(ref, "ref", "source")})
				}
			}
			if exception, ok := closure["exception"]; ok {
				c = append(c, contract.Field{Key: "exception", Value: exception})
			}
			out = append(out, contract.Field{Key: "closure", Value: c})
		}
		checks = append(checks, out)
	}
	recurrences := []any{}
	for _, item := range list(value["recurrences"]) {
		recurrences = append(recurrences, fields(object(item), "check", "verdict", "faultId", "reason"))
	}
	out := fields(value, "subject", "product", "origin", "verdict")
	return append(out, contract.Field{Key: "checks", Value: checks}, contract.Field{Key: "recurrences", Value: recurrences})
}
