//go:build dev

package ci

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
)

// GateJobs is the required-check set the dev gate aggregates; it must stay equal to the
// workflow's jobs minus dev-gate and to dev-gate's needs list (scripts/ci/gate.py JOBS).
var GateJobs = []string{"selection", "validate", "tests", "secrets", "packages", "go-product"}

// keyError is Python's KeyError: its text is repr(key).
type keyError struct{ key string }

func (e keyError) Error() string { return pyRepr(e.key) }

// GateCheck is gate.py's check(env): every selected job succeeded and every unselected one
// was skipped, for a selection bound to the actual candidate.
func GateCheck(env func(string) (string, bool)) (Selection, error) {
	get := func(key string) (string, error) {
		if value, ok := env(key); ok {
			return value, nil
		}
		return "", keyError{key}
	}
	raw, err := get("NEEDS_JSON")
	if err != nil {
		return Selection{}, err
	}
	decoded, order, err := pyJSONLoadsKeys(raw)
	if err != nil {
		return Selection{}, err
	}
	required := slices.Clone(GateJobs)
	sort.Strings(required)
	needs, ok := decoded.(map[string]any)
	if ok {
		names := make([]string, 0, len(needs))
		for name := range needs {
			names = append(names, name)
		}
		sort.Strings(names)
		ok = slices.Equal(names, required)
	}
	if !ok {
		return Selection{}, valueError{fmt.Sprintf("Expected exactly %s results", strings.Join(required, ", "))}
	}
	selection, ok := needs["selection"].(map[string]any)
	if !ok || selection["result"] != "success" {
		return Selection{}, errors.New("selection did not succeed")
	}
	outputs, present := selection["outputs"]
	if !present {
		return Selection{}, keyError{"outputs"}
	}
	outputMap, ok := outputs.(map[string]any)
	if !ok {
		return Selection{}, fmt.Errorf("%s", pyTypeErrorSubscript(outputs))
	}
	encoded, present := outputMap["scope"]
	if !present {
		return Selection{}, keyError{"scope"}
	}
	text, ok := encoded.(string)
	if !ok {
		return Selection{}, fmt.Errorf("the JSON object must be str, bytes or bytearray, not %s", pyTypeName(encoded))
	}
	value, err := pyJSONLoads(text)
	if err != nil {
		return Selection{}, err
	}
	scope, err := ValidateSelection(value)
	if err != nil {
		return Selection{}, err
	}
	for _, pair := range [][2]string{{"EVENT_NAME", scope.Event}, {"BASE_REF", scope.BaseRef},
		{"REF", scope.Ref}, {"HEAD_SHA", scope.Head}} {
		actual, err := get(pair[0])
		if err != nil {
			return Selection{}, err
		}
		if actual != pair[1] {
			return Selection{}, valueError{fmt.Sprintf("Selection does not match %s", pair[0])}
		}
	}
	for _, name := range order {
		expected := "success"
		if (name == "tests" && !scope.Selected.Tests) || (name == "packages" && !scope.Selected.Packages) {
			expected = "skipped"
		}
		job, ok := needs[name].(map[string]any)
		if !ok || job["result"] != expected {
			return Selection{}, fmt.Errorf("%s must report %s", name, expected)
		}
	}
	return scope, nil
}

func pyTypeName(value any) string {
	switch v := value.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case json.Number:
		if isPythonInt(v) {
			return "int"
		}
		return "float"
	case string:
		return "str"
	case []any:
		return "list"
	default:
		return "dict"
	}
}

// pyTypeErrorSubscript is the TypeError of value["scope"] on a non-dict value.
func pyTypeErrorSubscript(value any) string {
	switch value.(type) {
	case string:
		return "string indices must be integers, not 'str'"
	case []any:
		return "list indices must be integers or slices, not str"
	default:
		return fmt.Sprintf("'%s' object is not subscriptable", pyTypeName(value))
	}
}

// Gate is `crw-dev ci gate`: the dev gate's verdict over the jobs' results.
func Gate(args []string, stdout, stderr io.Writer) int {
	if _, code := parseFlags("gate", "Require successful selected checks and explicitly skipped unselected checks.",
		nil, args, stdout, stderr); code >= 0 {
		return code
	}
	if _, err := GateCheck(os.LookupEnv); err != nil {
		return failf(stderr, "Gate failed: %s", err)
	}
	fmt.Fprintln(stdout, "Every selected check succeeded; unselected checks were explicitly skipped.")
	return 0
}
