package bridge

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

// policyOrder is settings.py POLICY_DEFAULTS order: normalise_policy builds each policy dict
// from its defaults first, then the host's other keys, then "type", and Python's repr keeps that
// insertion order.
var policyOrder = []string{"writableRoots", "networkAccess", "excludeTmpdirEnvVar", "excludeSlashTmp"}

// pyRepr renders a decoded JSON value exactly as Python's repr() renders the same value, so a
// finding message reads byte for byte like bridge.py's f"{value!r}".
func pyRepr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case string:
		return pyString(v)
	case json.Number:
		return v.String()
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case int:
		return strconv.Itoa(v)
	case []string:
		parts := make([]string, len(v))
		for i, s := range v {
			parts[i] = pyString(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		parts := []string{}
		for _, key := range mapOrder(v) {
			parts = append(parts, pyString(key)+": "+pyRepr(v[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(v)
	}
}

// mapOrder is the insertion order normalise_policy produces: default fields, then other keys
// (sorted, since a decoded Go map has lost the host's order), then "type" last.
func mapOrder(m map[string]any) []string {
	keys := []string{}
	for _, key := range policyOrder {
		if _, ok := m[key]; ok {
			keys = append(keys, key)
		}
	}
	rest := []string{}
	for key := range m {
		if key != "type" && !slices.Contains(policyOrder, key) {
			rest = append(rest, key)
		}
	}
	slices.Sort(rest)
	keys = append(keys, rest...)
	if _, ok := m["type"]; ok {
		keys = append(keys, "type")
	}
	return keys
}

func pyString(s string) string { return settings.Repr(s) }

// findingText is bridge.py's "{code}: {field} returned {returned!r}, expected {expected!r}".
func findingText(code, field string, returned, expected any) string {
	return fmt.Sprintf("%s: %s returned %s, expected %s", code, field, pyRepr(returned), pyRepr(expected))
}
