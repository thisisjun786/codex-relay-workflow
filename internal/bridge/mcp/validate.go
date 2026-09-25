package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Argument validation in the shape FastMCP reports it. pydantic validates every parameter of
// the tool function in signature order and lists every failure under
// "<N> validation error(s) for <tool>Arguments", one entry per failing location. That prefix,
// the count and the locations (field, or field.index for a list item) are Python's; each
// entry's body is this bridge's own one-line reason, not pydantic's text (pending a decision).
//
// The rules are pydantic's lax mode for the annotations server.py uses: a str takes only a
// string; an int takes an integral number of at most 64 bits, a bool, or a decimal string; a
// float takes any number, a bool, or a numeric string; a Literal takes only its listed values;
// a dict takes only an object and a list[str] only an array of strings. Optional fields also
// take null. A key the signature does not declare is ignored, as pydantic ignores it.

type failure struct{ location, reason string }

// validateArguments returns the error text for arguments that do not validate against tool's
// signature, or "" when they do. arguments has already been through preParseArguments.
func validateArguments(tool string, arguments map[string]json.RawMessage) string {
	failures := []failure{}
	for _, f := range signatures[tool] {
		raw, present := arguments[f.name]
		if !present {
			if f.required {
				failures = append(failures, failure{f.name, "Field required"})
			}
			continue
		}
		failures = append(failures, checkField(f, raw)...)
	}
	if len(failures) == 0 {
		return ""
	}
	noun := "errors"
	if len(failures) == 1 {
		noun = "error"
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Error executing tool %s: %d validation %s for %sArguments", tool, len(failures), noun, tool)
	for _, fail := range failures {
		fmt.Fprintf(&text, "\n%s\n  %s", fail.location, fail.reason)
	}
	return text.String()
}

func decodeAny(raw json.RawMessage) any {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return value
}

func checkField(f field, raw json.RawMessage) []failure {
	value := decodeAny(raw)
	schema := f.schema
	if anyOf, ok := schema["anyOf"].([]any); ok {
		// Optional[X]: null, or X.
		if value == nil {
			return nil
		}
		schema = anyOf[0].(map[string]any)
	}
	if choices, ok := schema["enum"].([]any); ok {
		return literal(f.name, value, choices)
	}
	if constant, ok := schema["const"]; ok {
		return literal(f.name, value, []any{constant})
	}
	switch schema["type"] {
	case "string":
		if _, ok := value.(string); !ok {
			return []failure{{f.name, "Input should be a valid string"}}
		}
	case "integer":
		if !isInteger(value) {
			return []failure{{f.name, "Input should be a valid integer"}}
		}
	case "number":
		if !isNumber(value) {
			return []failure{{f.name, "Input should be a valid number"}}
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return []failure{{f.name, "Input should be a valid dictionary"}}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return []failure{{f.name, "Input should be a valid list"}}
		}
		out := []failure{}
		for i, item := range items {
			if _, ok := item.(string); !ok {
				out = append(out, failure{fmt.Sprintf("%s.%d", f.name, i), "Input should be a valid string"})
			}
		}
		return out
	}
	return nil
}

func literal(name string, value any, choices []any) []failure {
	for _, choice := range choices {
		if value == choice {
			return nil
		}
	}
	quoted := make([]string, len(choices))
	for i, choice := range choices {
		quoted[i] = fmt.Sprintf("'%v'", choice)
	}
	list := quoted[0]
	if len(quoted) > 1 {
		list = strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
	}
	return []failure{{name, "Input should be " + list}}
}

// isInteger is pydantic's lax int: an integral JSON number that fits in 64 bits, a bool, or a
// string laxNumber accepts as an integer.
func isInteger(value any) bool {
	switch v := value.(type) {
	case bool:
		return true
	case json.Number:
		f, err := v.Float64()
		// Within int64, as pydantic's int_parsing_size bound.
		return err == nil && f == math.Trunc(f) && f >= math.MinInt64 && f < math.MaxInt64
	case string:
		encoded, _ := json.Marshal(v)
		_, ok := laxNumber(encoded, "integer")
		return ok
	}
	return false
}

func isNumber(value any) bool {
	switch v := value.(type) {
	case bool:
		return true
	case json.Number:
		_, err := v.Float64()
		return err == nil
	case string:
		encoded, _ := json.Marshal(v)
		_, ok := laxNumber(encoded, "number")
		return ok
	}
	return false
}
