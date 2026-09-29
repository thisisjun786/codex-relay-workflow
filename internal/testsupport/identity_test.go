package testsupport_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// errorRecorder stands in for the test inside the rule: it records every Errorf so a case can
// require that the rule failed the test, or that it did not.
type errorRecorder struct {
	testing.TB
	errors []string
}

func (r *errorRecorder) Helper() {}
func (r *errorRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

// failures runs rule against a recorder and returns what it reported.
func failures(t *testing.T, rule func(testing.TB)) []string {
	t.Helper()
	recorder := &errorRecorder{TB: t}
	rule(recorder)
	return recorder.errors
}

// RuntimeIdentity neutralizes exactly the schema_meta owner and a process record's
// python_compatibility_build, and only as the runtime that wrote them writes them: the other
// runtime's value, or one that names neither, fails the test and is kept.
func TestRuntimeIdentity_requires_the_writers_own_value_then_neutralizes(t *testing.T) {
	build := testsupport.PythonCompatibilityBuild
	py, golang := testsupport.Python, testsupport.Go
	for _, c := range []struct {
		writer  testsupport.Runtime
		surface testsupport.Surface
		key     string
		in      any
		want    any
		fails   bool
	}{
		{py, testsupport.SchemaMeta, "owner", "python", testsupport.RuntimeOwner, false},
		{golang, testsupport.SchemaMeta, "owner", "go", testsupport.RuntimeOwner, false},
		// The other runtime's owner is not this writer's: the Python side of a comparison
		// reading a store stamped "go" fails, and so does the reverse.
		{py, testsupport.SchemaMeta, "owner", "go", "go", true},
		{golang, testsupport.SchemaMeta, "owner", "python", "python", true},
		{py, testsupport.SchemaMeta, "owner", "cobol", "cobol", true},
		{golang, testsupport.SchemaMeta, "owner", nil, nil, true},
		// Both runtimes stamp the same fence build into schema_meta: it is compared as it is.
		{py, testsupport.SchemaMeta, "python_compatibility_build", build, build, false},
		{golang, testsupport.SchemaMeta, "python_compatibility_build", build, build, false},
		{golang, testsupport.SchemaMeta, "python_compatibility_build", nil, nil, false},
		// A Python fence record names the fence build; a Go record names none (null).
		{py, testsupport.ProcessRecord, "python_compatibility_build", build, testsupport.RuntimeBuild, false},
		{golang, testsupport.ProcessRecord, "python_compatibility_build", nil, testsupport.RuntimeBuild, false},
		{golang, testsupport.ProcessRecord, "python_compatibility_build", build, build, true},
		{py, testsupport.ProcessRecord, "python_compatibility_build", nil, nil, true},
		{py, testsupport.ProcessRecord, "python_compatibility_build", "codex-session-relay/0.1.9", "codex-session-relay/0.1.9", true},
		{golang, testsupport.ProcessRecord, "python_compatibility_build", "", "", true},
		{golang, testsupport.ProcessRecord, "python_compatibility_build", false, false, true},
		// Nothing beside those two keys is touched or checked.
		{golang, testsupport.ProcessRecord, "owner", "python", "python", false},
		{py, testsupport.ProcessRecord, "build", build, build, false},
		{py, testsupport.SchemaMeta, "owner_epoch", "1", "1", false},
	} {
		var got any
		reported := failures(t, func(tb testing.TB) { got = testsupport.RuntimeIdentity(tb, c.writer, c.surface, c.key, c.in) })
		if got != c.want || (len(reported) != 0) != c.fails {
			t.Errorf("RuntimeIdentity(%s, %d, %q, %#v) = %#v with failures %q, want %#v failing=%t", c.writer, c.surface, c.key, c.in, got, reported, c.want, c.fails)
		}
		if c.surface == testsupport.SchemaMeta {
			reported = failures(t, func(tb testing.TB) { got = testsupport.OwnerNeutral(tb, c.writer, c.key, c.in) })
			if got != c.want || (len(reported) != 0) != c.fails {
				t.Errorf("OwnerNeutral(%s, %q, %#v) = %#v with failures %q, want %#v failing=%t", c.writer, c.key, c.in, got, reported, c.want, c.fails)
			}
		}
	}
	var typed string
	if reported := failures(t, func(tb testing.TB) {
		typed = testsupport.RuntimeIdentity(tb, testsupport.Python, testsupport.ProcessRecord, "python_compatibility_build", build)
	}); typed != testsupport.RuntimeBuild || len(reported) != 0 {
		t.Errorf("string-typed record build: %q, failures %q", typed, reported)
	}
	if reported := failures(t, func(tb testing.TB) {
		testsupport.RuntimeIdentity(tb, testsupport.Runtime("cobol"), testsupport.SchemaMeta, "owner", "cobol")
	}); len(reported) == 0 {
		t.Error("a writer that names no runtime was accepted")
	}
}

// RuntimeIdentityText rewrites only the value of a python_compatibility_build member, in place,
// and only when it is the writer's own: layout, key order, the key's presence and every other
// value still reach the comparison.
func TestRuntimeIdentityText_rewrites_only_the_writers_own_record_build(t *testing.T) {
	build := testsupport.PythonCompatibilityBuild
	neutral := `"` + testsupport.RuntimeBuild + `"`
	for _, c := range []struct {
		writer    testsupport.Runtime
		in, want  string
		failNamed string
	}{
		// service.py new_record, json.dumps(indent=2): the key first.
		{testsupport.Python, "{\n  \"python_compatibility_build\": \"" + build + "\",\n  \"pid\": null\n}", "{\n  \"python_compatibility_build\": " + neutral + ",\n  \"pid\": null\n}", ""},
		// Go's service record: the same key, null.
		{testsupport.Go, "{\n  \"python_compatibility_build\": null,\n  \"pid\": null\n}", "{\n  \"python_compatibility_build\": " + neutral + ",\n  \"pid\": null\n}", ""},
		// publish_worker_policy, json.dump's default separators: nested and last.
		{testsupport.Go, `{"schemaVersion": 1, "worker": {"pid": 7, "bootId": "b", "python_compatibility_build": null}, "service": {"pid": 7}}`, `{"schemaVersion": 1, "worker": {"pid": 7, "bootId": "b", "python_compatibility_build": ` + neutral + `}, "service": {"pid": 7}}`, ""},
		{testsupport.Python, `{"a":[{"python_compatibility_build":"` + build + `"}],"python_compatibility_build" : "` + build + `"}`, `{"a":[{"python_compatibility_build":` + neutral + `}],"python_compatibility_build" : ` + neutral + `}`, ""},
		// The other runtime's value, in either direction, fails and stays as written.
		{testsupport.Go, `{"python_compatibility_build": "` + build + `", "pid": null}`, `{"python_compatibility_build": "` + build + `", "pid": null}`, "go runtime"},
		{testsupport.Python, `{"python_compatibility_build": null}`, `{"python_compatibility_build": null}`, "python runtime"},
		{testsupport.Go, `{"a":[{"python_compatibility_build":null}],"python_compatibility_build":"` + build + `"}`, `{"a":[{"python_compatibility_build":` + neutral + `}],"python_compatibility_build":"` + build + `"}`, "go runtime"},
		{testsupport.Python, `{"python_compatibility_build": "other"}`, `{"python_compatibility_build": "other"}`, "python runtime"},
		{testsupport.Go, `{"python_compatibility_build": {"x": null}}`, `{"python_compatibility_build": {"x": null}}`, "JSON {"},
		// A missing key, the key elsewhere only as a value, and non-JSON stay and pass.
		{testsupport.Go, `{"pid": null}`, `{"pid": null}`, ""},
		{testsupport.Python, `{"k": "python_compatibility_build", "v": null}`, `{"k": "python_compatibility_build", "v": null}`, ""},
		{testsupport.Go, `["python_compatibility_build", null]`, `["python_compatibility_build", null]`, ""},
		{testsupport.Go, `{"python_compatibility_build": null`, `{"python_compatibility_build": null`, ""},
		{testsupport.Python, ``, ``, ""},
	} {
		var got string
		reported := failures(t, func(tb testing.TB) { got = testsupport.RuntimeIdentityText(tb, c.writer, c.in) })
		if got != c.want {
			t.Errorf("RuntimeIdentityText(%s, %s)\n got %s\nwant %s", c.writer, c.in, got, c.want)
		}
		switch {
		case c.failNamed == "" && len(reported) != 0:
			t.Errorf("RuntimeIdentityText(%s, %s) failed the test: %q", c.writer, c.in, reported)
		case c.failNamed != "" && (len(reported) != 1 || !strings.Contains(reported[0], c.failNamed)):
			t.Errorf("RuntimeIdentityText(%s, %s) reported %q, want one failure naming %q", c.writer, c.in, reported, c.failNamed)
		}
	}
	// The key's position is still compared: a record that moves it differs after the rule.
	python := testsupport.RuntimeIdentityText(t, testsupport.Python, `{"python_compatibility_build": "`+build+`", "pid": null}`)
	moved := testsupport.RuntimeIdentityText(t, testsupport.Go, `{"pid": null, "python_compatibility_build": null}`)
	absent := testsupport.RuntimeIdentityText(t, testsupport.Go, `{"pid": null}`)
	if python == moved || python == absent {
		t.Fatalf("position or presence normalized away: %s / %s / %s", python, moved, absent)
	}
}
