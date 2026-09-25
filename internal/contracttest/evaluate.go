package contracttest

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

// An observation is JSON-shaped data: map[string]any, []any, string, float64, bool or nil.
// Runners build it from those types only, so fixture values (decoded by encoding/json) and
// observed values compare with reflect.DeepEqual.

var errCheck = errors.New("check failed")

// lookup walks path through an observation. A string indexes an object; a number indexes an
// array, negative from the end, as Python subscripting does.
func lookup(value any, path []any) (any, error) {
	for _, key := range path {
		switch k := key.(type) {
		case string:
			object, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("key %q on non-object %T", k, value)
			}
			next, present := object[k]
			if !present {
				return nil, fmt.Errorf("missing key %q", k)
			}
			value = next
		case float64:
			array, ok := value.([]any)
			if !ok {
				return nil, fmt.Errorf("index %v on non-array %T", k, value)
			}
			index := int(k)
			if index < 0 {
				index += len(array)
			}
			if index < 0 || index >= len(array) {
				return nil, fmt.Errorf("index %v out of range (len %d)", k, len(array))
			}
			value = array[index]
		default:
			return nil, fmt.Errorf("path element %v is neither a key nor an index", key)
		}
	}
	return value, nil
}

// Evaluate applies every check to the observation. The first failing check is returned with
// the scenario ID, the check and the observed value.
func Evaluate(id string, actual map[string]any, checks []Check) error {
	for _, check := range checks {
		value, err := lookup(actual, check.Path)
		if err == nil && check.DecodeJSON {
			value, err = decodeJSONText(value)
		}
		if err == nil {
			err = apply(actual, value, check)
		}
		if err != nil {
			return fmt.Errorf("%s: %+v: %w", id, check, err)
		}
	}
	return nil
}

func apply(actual map[string]any, value any, check Check) error {
	expected := check.Value
	var passed bool
	switch check.Kind {
	case "eq", "ordered_calls", "exception_code", "timeout", "signal":
		passed = reflect.DeepEqual(value, expected)
	case "ne":
		passed = !reflect.DeepEqual(value, expected)
	case "json_eq":
		decoded, err := decodeJSONText(value)
		if err != nil {
			return err
		}
		passed = reflect.DeepEqual(decoded, expected)
	case "contains", "excludes":
		found, err := contains(value, expected)
		if err != nil {
			return err
		}
		passed = found == (check.Kind == "contains")
	case "truthy":
		passed = truthy(value)
	case "falsy", "absent_effects":
		passed = !truthy(value)
	case "length":
		n, err := length(value)
		if err != nil {
			return err
		}
		passed = reflect.DeepEqual(float64(n), expected)
	case "regex":
		text, ok1 := value.(string)
		pattern, ok2 := expected.(string)
		if !ok1 || !ok2 {
			return fmt.Errorf("regex needs strings, got %T and %T", value, expected)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("regex %q: %w", pattern, err)
		}
		passed = re.MatchString(text)
	case "lt", "gt":
		a, ok1 := value.(float64)
		b, ok2 := expected.(float64)
		if !ok1 || !ok2 {
			return fmt.Errorf("%s needs numbers, got %T and %T", check.Kind, value, expected)
		}
		passed = (check.Kind == "lt" && a < b) || (check.Kind == "gt" && a > b)
	case "after":
		array, ok := value.([]any)
		index := slices.IndexFunc(array, func(item any) bool { return item == any(check.Flag) })
		if !ok || index < 0 || index+1 >= len(array) {
			return fmt.Errorf("flag %q has no following value in %v: %w", check.Flag, value, errCheck)
		}
		passed = reflect.DeepEqual(array[index+1], expected)
	case "same":
		other, err := lookup(actual, check.Other)
		if err != nil {
			return fmt.Errorf("other: %w", err)
		}
		passed = reflect.DeepEqual(value, other)
	case "set_eq", "subset":
		have, ok1 := value.([]any)
		want, ok2 := expected.([]any)
		if !ok1 || !ok2 {
			return fmt.Errorf("%s needs arrays, got %T and %T", check.Kind, value, expected)
		}
		passed = coveredBy(want, have) && (check.Kind == "subset" || coveredBy(have, want))
	case "bytes":
		a, err1 := hexOf(value)
		b, err2 := hexOf(expected)
		if err := errors.Join(err1, err2); err != nil {
			return err
		}
		passed = a == b
	default:
		return fmt.Errorf("unsupported assertion %q", check.Kind)
	}
	if !passed {
		return fmt.Errorf("observed %#v: %w", value, errCheck)
	}
	return nil
}

func decodeJSONText(value any) (any, error) {
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("JSON text expected, got %T", value)
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, fmt.Errorf("decode JSON text: %w", err)
	}
	return decoded, nil
}

// contains is Python's `expected in value`: substring, array element, or object key.
func contains(value, expected any) (bool, error) {
	switch v := value.(type) {
	case string:
		needle, ok := expected.(string)
		if !ok {
			return false, fmt.Errorf("substring must be a string, got %T", expected)
		}
		return strings.Contains(v, needle), nil
	case []any:
		return slices.ContainsFunc(v, func(item any) bool { return reflect.DeepEqual(item, expected) }), nil
	case map[string]any:
		key, ok := expected.(string)
		_, present := v[key]
		return ok && present, nil
	default:
		return false, fmt.Errorf("membership on %T", value)
	}
}

// truthy is Python's bool() over JSON-shaped values.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

func length(value any) (int, error) {
	switch v := value.(type) {
	case string:
		return len([]rune(v)), nil
	case []any:
		return len(v), nil
	case map[string]any:
		return len(v), nil
	default:
		return 0, fmt.Errorf("length of %T", value)
	}
}

func coveredBy(items, pool []any) bool {
	for _, item := range items {
		if !slices.ContainsFunc(pool, func(p any) bool { return reflect.DeepEqual(p, item) }) {
			return false
		}
	}
	return true
}

func hexOf(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("hex string expected, got %T", value)
	}
	raw, err := hex.DecodeString(text)
	if err != nil {
		return "", fmt.Errorf("hex %q: %w", text, err)
	}
	return string(raw), nil
}

// Assert compares the fixed expectations (exit, stdout_json, files) and then the checks.
func Assert(scenario Scenario, actual map[string]any) error {
	expect := scenario.Expect
	if got := actual["exit"]; !reflect.DeepEqual(got, float64(expect.Exit)) {
		return fmt.Errorf("%s: exit: %v != %d (stderr %v): %w", scenario.ID, got, expect.Exit, actual["stderr"], errCheck)
	}
	if got := actual["stdout_json"]; expect.HasStdoutJSON && !reflect.DeepEqual(got, expect.StdoutJSON) {
		return fmt.Errorf("%s: stdout_json: %#v != %#v: %w", scenario.ID, got, expect.StdoutJSON, errCheck)
	}
	files, _ := actual["files"].(map[string]any)
	for name, want := range expect.Files {
		if got := files[name]; got != want {
			return fmt.Errorf("%s: files[%s]: %v != %v: %w", scenario.ID, name, got, want, errCheck)
		}
	}
	return Evaluate(scenario.ID, actual, expect.Checks)
}
