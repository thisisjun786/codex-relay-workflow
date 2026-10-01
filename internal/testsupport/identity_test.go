package testsupport_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// errorRecorder stands in for the test inside the rule: it records every Errorf so a
// case can require that the rule failed the test, or that it did not.
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

// OwnerNeutral neutralizes exactly the schema_meta owner, and only Go's own: any other owner, the
// retired Python fence's included, fails the test and is kept.
func TestOwnerNeutral_requires_the_go_owner_then_neutralizes(t *testing.T) {
	build := ownership.CompatibilityBuild
	for _, c := range []struct {
		key   string
		in    any
		want  any
		fails bool
	}{
		{"owner", "go", testsupport.RuntimeOwner, false},
		{"owner", "python", "python", true},
		{"owner", "cobol", "cobol", true},
		{"owner", nil, nil, true},
		// Every store stamps the same fence build into schema_meta: it is compared as it is.
		{"python_compatibility_build", build, build, false},
		{"python_compatibility_build", nil, nil, false},
		// Nothing beside the owner is touched or checked.
		{"owner_epoch", "1", "1", false},
		{"store_id", "go", "go", false},
	} {
		var got any
		reported := failures(t, func(tb testing.TB) { got = testsupport.OwnerNeutral(tb, c.key, c.in) })
		if got != c.want || (len(reported) != 0) != c.fails {
			t.Errorf("OwnerNeutral(%q, %#v) = %#v with failures %q, want %#v failing=%t", c.key, c.in, got, reported, c.want, c.fails)
		}
	}
	var typed string
	if reported := failures(t, func(tb testing.TB) { typed = testsupport.OwnerNeutral(tb, "owner", "go") }); typed != testsupport.RuntimeOwner || len(reported) != 0 {
		t.Errorf("string-typed owner: %q, failures %q", typed, reported)
	}
	rows := []any{map[string]any{"key": "owner", "value": "go"}, []any{"owner", "go"}, map[string]any{"key": "version", "value": "1"}}
	if reported := failures(t, func(tb testing.TB) { testsupport.OwnerNeutralRows(tb, rows) }); len(reported) != 0 ||
		rows[0].(map[string]any)["value"] != testsupport.RuntimeOwner || rows[1].([]any)[1] != testsupport.RuntimeOwner || rows[2].(map[string]any)["value"] != "1" {
		t.Errorf("OwnerNeutralRows: %v, failures %q", rows, reported)
	}
}

// RuntimeIdentityText rewrites only the value of a python_compatibility_build member, in place,
// and only when it is Go's own null: layout, key order, the key's presence and every other value
// still reach the comparison.
func TestRuntimeIdentityText_rewrites_only_the_go_record_build(t *testing.T) {
	build := ownership.CompatibilityBuild
	neutral := `"` + testsupport.RuntimeBuild + `"`
	for _, c := range []struct {
		in, want  string
		failNamed string
	}{
		// Go's service record: the key first, null.
		{"{\n  \"python_compatibility_build\": null,\n  \"pid\": null\n}", "{\n  \"python_compatibility_build\": " + neutral + ",\n  \"pid\": null\n}", ""},
		// worker-policy.json, json.dump's default separators: nested and last.
		{`{"schemaVersion": 1, "worker": {"pid": 7, "bootId": "b", "python_compatibility_build": null}, "service": {"pid": 7}}`, `{"schemaVersion": 1, "worker": {"pid": 7, "bootId": "b", "python_compatibility_build": ` + neutral + `}, "service": {"pid": 7}}`, ""},
		{`{"a":[{"python_compatibility_build":null}],"python_compatibility_build" : null}`, `{"a":[{"python_compatibility_build":` + neutral + `}],"python_compatibility_build" : ` + neutral + `}`, ""},
		// The Python fence's build, or any other value, fails and stays as written.
		{`{"python_compatibility_build": "` + build + `", "pid": null}`, `{"python_compatibility_build": "` + build + `", "pid": null}`, "Go runtime"},
		{`{"a":[{"python_compatibility_build":null}],"python_compatibility_build":"` + build + `"}`, `{"a":[{"python_compatibility_build":` + neutral + `}],"python_compatibility_build":"` + build + `"}`, "Go runtime"},
		{`{"python_compatibility_build": {"x": null}}`, `{"python_compatibility_build": {"x": null}}`, "JSON {"},
		// A missing key, the key elsewhere only as a value, and non-JSON stay and pass.
		{`{"pid": null}`, `{"pid": null}`, ""},
		{`{"k": "python_compatibility_build", "v": null}`, `{"k": "python_compatibility_build", "v": null}`, ""},
		{`["python_compatibility_build", null]`, `["python_compatibility_build", null]`, ""},
		{`{"python_compatibility_build": null`, `{"python_compatibility_build": null`, ""},
		{``, ``, ""},
	} {
		var got string
		reported := failures(t, func(tb testing.TB) { got = testsupport.RuntimeIdentityText(tb, c.in) })
		if got != c.want {
			t.Errorf("RuntimeIdentityText(%s)\n got %s\nwant %s", c.in, got, c.want)
		}
		switch {
		case c.failNamed == "" && len(reported) != 0:
			t.Errorf("RuntimeIdentityText(%s) failed the test: %q", c.in, reported)
		case c.failNamed != "" && (len(reported) != 1 || !strings.Contains(reported[0], c.failNamed)):
			t.Errorf("RuntimeIdentityText(%s) reported %q, want one failure naming %q", c.in, reported, c.failNamed)
		}
	}
	// The key's position is still compared: a record that moves it differs after the rule.
	first := testsupport.RuntimeIdentityText(t, `{"python_compatibility_build": null, "pid": null}`)
	moved := testsupport.RuntimeIdentityText(t, `{"pid": null, "python_compatibility_build": null}`)
	absent := testsupport.RuntimeIdentityText(t, `{"pid": null}`)
	if first == moved || first == absent {
		t.Fatalf("position or presence normalized away: %s / %s / %s", first, moved, absent)
	}
}
