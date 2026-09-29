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
// A whole-state comparison of what the two runtimes left behind may normalize exactly two
// values, because each legitimately names the runtime that wrote it and nothing else does:
//
//   - schema_meta "owner": "python" in a store the Python fence owns and "go" in one Go owns
//     (decisions.md 14 and 30);
//   - a process record's "python_compatibility_build": the pinned fence build in a record the
//     Python fence wrote and null in one Go wrote, a Go process being no Python fence build
//     (decisions.md 31). The process records are daemon.json, scopes/<key>.json and the worker
//     object of worker-policy.json.
//
// RuntimeIdentity is that rule, and it is told which runtime wrote the value. It first requires
// that runtime's own value and fails the test on anything else - the other runtime's value
// included, so a Go record carrying the fence build, or a Python store stamped "go", is a
// failure rather than a match - and only then normalizes. Everything else is compared as
// written: the schema_meta python_compatibility_build row both runtimes stamp alike, and a
// record that lacks the key or puts it elsewhere.

// RuntimeBuild is what RuntimeIdentity reports for a process record's
// python_compatibility_build once it is the writing runtime's own.
const RuntimeBuild = "<runtime build>"

// Runtime names the runtime that wrote a value RuntimeIdentity is given, spelled as the
// schema_meta owner it stamps.
type Runtime string

const (
	// Python is the retained Python fence runtime.
	Python Runtime = "python"
	// Go is the Go runtime.
	Go Runtime = "go"
)

// Surface says where a value RuntimeIdentity is given was read.
type Surface int

const (
	// SchemaMeta is a row of a store's schema_meta table; the key is the row's key.
	SchemaMeta Surface = iota
	// ProcessRecord is a member of a process record (daemon.json, scopes/<key>.json,
	// worker-policy.json's worker), at any depth; the key is the member's name.
	ProcessRecord
)

// ownValue is the value writer itself leaves under key on surface, and whether the rule covers
// that key at all: the schema_meta owner writer stamps, and the process-record
// python_compatibility_build writer records (the fence build from Python, null from Go).
func ownValue(writer Runtime, surface Surface, key string) (want any, covered bool) {
	switch {
	case surface == SchemaMeta && key == "owner":
		return string(writer), true
	case surface == ProcessRecord && key == "python_compatibility_build":
		if writer == Python {
			return PythonCompatibilityBuild, true
		}
		return nil, true
	}
	return nil, false
}

// isOwn reports whether value is want: the same string, or null for a null want.
func isOwn(value, want any) bool {
	if want == nil {
		return value == nil
	}
	text, ok := value.(string)
	return ok && text == want
}

// RuntimeIdentity returns value with the runtime identity removed. For a schema_meta owner it
// requires writer's own name ("python" or "go") and returns RuntimeOwner; for a process record's
// python_compatibility_build it requires writer's own value (the fence build from Python, null
// from Go) and returns RuntimeBuild. Any other value under those two keys fails the test and is
// returned unchanged. Every other key is returned unchanged.
func RuntimeIdentity[V any](t testing.TB, writer Runtime, surface Surface, key string, value V) V {
	t.Helper()
	want, covered := ownValue(writer, surface, key)
	if !covered {
		return value
	}
	if writer != Python && writer != Go {
		t.Errorf("runtime identity: %q is not a runtime; say which runtime wrote %s", writer, key)
		return value
	}
	if !isOwn(any(value), want) {
		where := "schema_meta"
		if surface == ProcessRecord {
			where = "process record"
		}
		t.Errorf("runtime identity: %s %s written by the %s runtime is %#v, want its own %#v",
			where, key, writer, any(value), want)
		return value
	}
	neutral := RuntimeOwner
	if surface == ProcessRecord {
		neutral = RuntimeBuild
	}
	if out, ok := any(neutral).(V); ok {
		return out
	}
	t.Errorf("runtime identity: %s written by the %s runtime is held as %T, which cannot carry the neutral value", key, writer, value)
	return value
}

// RuntimeIdentityText applies RuntimeIdentity(t, writer, ProcessRecord, ...) to the JSON text of
// a process record writer wrote, for a comparison of the record's bytes: every
// python_compatibility_build member, at any depth, must be writer's own value (an object or
// array there fails too), and each is rewritten in place; no other byte changes, so key order,
// spacing and every other value are still compared. Text that does not parse as JSON is
// returned unchanged.
func RuntimeIdentityText(t testing.TB, writer Runtime, raw string) string {
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
			if want, covered := ownValue(writer, ProcessRecord, name); covered {
				t.Errorf("runtime identity: process record %s written by the %s runtime is a JSON %v, want its own %#v", name, writer, token, want)
			}
			stack = append(stack, frame{object: token == json.Delim('{'), expectKey: token == json.Delim('{')})
			continue
		case json.Delim(']'), json.Delim('}'):
			stack = stack[:top]
			continue
		}
		if name == "" {
			continue
		}
		if _, covered := ownValue(writer, ProcessRecord, name); !covered {
			continue
		}
		neutral := RuntimeIdentity(t, writer, ProcessRecord, name, token)
		if neutral == token {
			continue
		}
		// The value's own bytes are what follows the separators and whitespace before it.
		start := before + int64(len(raw[before:after])-len(strings.TrimLeft(raw[before:after], " \t\r\n:,")))
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(neutral) != nil {
			return raw
		}
		out.WriteString(raw[copied:start])
		out.WriteString(strings.TrimSuffix(encoded.String(), "\n"))
		copied = int(after)
	}
	out.WriteString(raw[copied:])
	return out.String()
}
