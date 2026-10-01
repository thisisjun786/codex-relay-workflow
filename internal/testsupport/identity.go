package testsupport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// Runtime identity.
//
// A whole-state comparison may normalize exactly two values, because each names the runtime that
// wrote it and nothing else does:
//
//   - schema_meta "owner", "go" in a store the Go runtime owns (decisions.md 14 and 30);
//   - a process record's "python_compatibility_build", which a Go process writes as null, a Go
//     process being no Python fence build (decisions.md 31). The process records are
//     daemon.json, scopes/<key>.json and the worker object of worker-policy.json.
//
// The goldens spell them RuntimeOwner and RuntimeBuild. OwnerNeutral and RuntimeIdentityText first
// require the Go runtime's own value and fail the test on anything else - the retired Python
// fence's "python" or its build included - and only then normalize. Everything else is compared as
// written: the schema_meta python_compatibility_build row every store stamps alike, and a record
// that lacks the key or puts it elsewhere.

// RuntimeOwner is what OwnerNeutral reports for a schema_meta owner that is Go's own.
const RuntimeOwner = "<runtime owner>"

// RuntimeBuild is what RuntimeIdentityText writes for a process record's
// python_compatibility_build that is Go's own (null).
const RuntimeBuild = "<runtime build>"

// OwnerNeutral is value with the runtime identity of a schema_meta row removed: the row whose key
// is "owner" must read "go", which becomes RuntimeOwner; any other owner value fails the test and
// is returned unchanged. Every other key is returned unchanged. Apply it to schema_meta rows only.
func OwnerNeutral[V any](t testing.TB, key string, value V) V {
	t.Helper()
	if key != "owner" {
		return value
	}
	if text, ok := any(value).(string); !ok || text != "go" {
		t.Errorf("runtime identity: schema_meta owner is %#v, want the Go runtime's own %q", any(value), "go")
		return value
	}
	if out, ok := any(RuntimeOwner).(V); ok {
		return out
	}
	t.Errorf("runtime identity: the schema_meta owner is held as %T, which cannot carry the neutral value", value)
	return value
}

// OwnerNeutralRows applies OwnerNeutral, in place, to the schema_meta rows of a table dump decoded
// from JSON: each row an object {"key": ..., "value": ...} or a pair [key, value]. A row of
// another shape fails the test.
func OwnerNeutralRows(t testing.TB, rows []any) {
	t.Helper()
	for _, row := range rows {
		switch cells := row.(type) {
		case map[string]any:
			if key, ok := cells["key"].(string); ok {
				cells["value"] = OwnerNeutral(t, key, cells["value"])
			}
		case []any:
			if len(cells) != 2 {
				t.Fatalf("schema_meta row %v", row)
			}
			if key, ok := cells[0].(string); ok {
				cells[1] = OwnerNeutral(t, key, cells[1])
			}
		default:
			t.Fatalf("schema_meta row %v", row)
		}
	}
}

// RuntimeIdentityText removes the runtime identity from the JSON text of a process record the Go
// runtime wrote, for a comparison of the record's bytes: every python_compatibility_build member,
// at any depth, must be null (a string, an object or an array there fails the test and is kept)
// and becomes RuntimeBuild in place; no other byte changes, so key order, spacing and every other
// value are still compared. Text that does not parse as JSON is returned unchanged.
func RuntimeIdentityText(t testing.TB, raw string) string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	type frame struct{ object, expectKey bool }
	var stack []frame
	var out strings.Builder
	copied, member := 0, ""
	for {
		before := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			if !errors.Is(err, io.EOF) || len(stack) != 0 {
				return raw
			}
			break
		}
		after := decoder.InputOffset()
		top := len(stack) - 1
		if top >= 0 && stack[top].object && stack[top].expectKey {
			if delim, ok := token.(json.Delim); ok && delim == '}' {
				stack = stack[:top]
				continue
			}
			member, stack[top].expectKey = token.(string), false
			continue
		}
		name := ""
		if top >= 0 && stack[top].object {
			name, stack[top].expectKey = member, true
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			if name == "python_compatibility_build" {
				t.Errorf("runtime identity: process record python_compatibility_build is a JSON %v, want the Go runtime's own null", token)
			}
			stack = append(stack, frame{object: token == json.Delim('{'), expectKey: token == json.Delim('{')})
			continue
		case json.Delim(']'), json.Delim('}'):
			stack = stack[:top]
			continue
		}
		if name != "python_compatibility_build" {
			continue
		}
		if token != nil {
			t.Errorf("runtime identity: process record python_compatibility_build is %#v, want the Go runtime's own null", token)
			continue
		}
		// The value's own bytes are what follows the separators and whitespace before it.
		start := before + int64(len(raw[before:after])-len(strings.TrimLeft(raw[before:after], " \t\r\n:,")))
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(RuntimeBuild) != nil {
			return raw
		}
		out.WriteString(raw[copied:start])
		out.WriteString(strings.TrimSuffix(encoded.String(), "\n"))
		copied = int(after)
	}
	out.WriteString(raw[copied:])
	return out.String()
}
