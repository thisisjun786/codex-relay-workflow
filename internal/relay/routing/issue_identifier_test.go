package routing

import (
	"strings"
	"testing"
)

// An issue identifier is a Linear key (ABC-12) or a tracking ref "<owner>/<name>#N" that
// internal/tracking/ref.Parse reads; nothing else is one.
func TestIssueIdentifierIsALinearKeyOrATrackingRef(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"ABC-12", true},
		{"CRW-1345", true},
		{"o/r#7", true},
		{"thisisjun786/crw-work#12", true},
		{"my.org/my_repo.go#1", true},
		{"o/r", false},
		{"o/r#0", false},
		{"o/r#07", false},
		{"o/r#", false},
		{"o/r#7 ", false},
		{" o/r#7", false},
		{"o/r#7/extra", false},
		{"o/../r#7", false},
		{"./r#7", false},
		{"o#7", false},
		{"#7", false},
		{"abc-12", false},
		{"ABC-", false},
		{"ABC12", false},
		{"o/r#99999999999999999999999", false},
		{"", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()
			reading, err := ReadReading(Object{"schema": "completion-reading/1", "product": "crw", "subject": tc.value})
			if tc.valid {
				if err != nil {
					t.Fatalf("subject %q is an issue identifier: %v", tc.value, err)
				}
				if reading["subject"] != tc.value {
					t.Fatalf("subject = %v, want %q", reading["subject"], tc.value)
				}
				return
			}
			if err == nil {
				t.Fatalf("subject %q is not an issue identifier but was read", tc.value)
			}
			refusal, ok := err.(*Refusal)
			if !ok || refusal.Reason != "route_input_malformed" {
				t.Fatalf("refusal = %#v", err)
			}
			if !strings.Contains(refusal.Detail, "a Linear key like ABC-12 or a tracking ref like owner/name#7") {
				t.Fatalf("the refusal does not say both forms: %q", refusal.Detail)
			}
		})
	}
}

// Every field that names an issue takes both forms: a binding's ref and follow-up issues, and a
// run's current issue.
func TestIssueIdentifierFieldsTakeATrackingRef(t *testing.T) {
	t.Parallel()
	binding := Object{"schema": "product-binding/1", "product": "crw", "kind": "issue", "ref": "o/r#7", "title": "t", "state": "open", "source": "s",
		"followUpOf": []any{Object{"issue": "o/r#3", "checks": []any{"acceptance"}}, Object{"issue": "CRW-9", "checks": []any{"install"}}}}
	got, err := ReadBinding(binding, nil)
	if err != nil {
		t.Fatalf("a binding of a tracking ref: %v", err)
	}
	if got["ref"] != "o/r#7" {
		t.Fatalf("ref = %v", got["ref"])
	}
	follow := got["followUpOf"].([]any)
	if len(follow) != 2 || follow[0].(Object)["issue"] != "CRW-9" || follow[1].(Object)["issue"] != "o/r#3" {
		t.Fatalf("followUpOf = %v", follow)
	}
	binding["followUpOf"] = []any{Object{"issue": "o/r", "checks": []any{"acceptance"}}}
	if _, err = ReadBinding(binding, nil); err == nil {
		t.Fatal("a follow-up of a bare repository was read")
	}
	t.Run("ReadIncident reads a run's current issue", func(t *testing.T) {
		t.Parallel()
		incident := func(currentIssue string) Object {
			return Object{"schema": "product-incident/1", "product": "crw", "repository": "o/r", "workspace": "ws", "surface": "dev_run", "phase": "development", "component": "editor", "symptom": "cursor_jump", "occurrenceKey": "run-1:step-4", "severity": "notice", "expected": "cancelled", "observedAt": "2026-10-15T00:00:00Z",
				"context": Object{"currentIssue": currentIssue}}
		}
		for _, valid := range []string{"o/r#7", "CRW-9"} {
			read, err := ReadIncident(incident(valid))
			if err != nil {
				t.Fatalf("a current issue %q: %v", valid, err)
			}
			if read["context"].(Object)["currentIssue"] != valid {
				t.Fatalf("currentIssue = %v, want %q", read["context"], valid)
			}
		}
		_, err := ReadIncident(incident("o/r"))
		refusal, ok := err.(*Refusal)
		if !ok || refusal.Reason != "route_input_malformed" {
			t.Fatalf("a current issue of a bare repository: refusal = %#v", err)
		}
		for _, want := range []string{"context.currentIssue", "a Linear key like ABC-12 or a tracking ref like owner/name#7"} {
			if !strings.Contains(refusal.Detail, want) {
				t.Fatalf("the refusal does not contain %q: %q", want, refusal.Detail)
			}
		}
	})
}

// A test binding sits on the test target's team: the team of a Linear key is its prefix, and the
// team of a tracking ref is its repository.
func TestTestBindingTeamOfATrackingRefIsItsRepository(t *testing.T) {
	t.Parallel()
	registry := Object{"product": "crw", "testTarget": Object{"team": "o/r", "project": "TP"}}
	linear := Object{"product": "crw", "testTarget": Object{"team": "TST", "project": "TP"}}
	binding := func(ref string) Object {
		return Object{"schema": "product-binding/1", "product": "crw", "kind": "issue", "ref": ref, "title": "t", "state": "open", "source": "s", "project": "TP", "test": true}
	}
	for _, tc := range []struct {
		name     string
		registry Object
		ref      string
		ok       bool
	}{
		{"a tracking ref of the target repository", registry, "o/r#9", true},
		{"a tracking ref of another repository", registry, "o/other#9", false},
		{"a Linear key on a repository team", registry, "TST-9", false},
		{"a Linear key of the target team", linear, "TST-9", true},
		{"a Linear key of another team", linear, "CRW-9", false},
		{"a tracking ref on a Linear team", linear, "o/r#9", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ReadBinding(binding(tc.ref), tc.registry)
			if tc.ok && err != nil {
				t.Fatalf("%s: %v", tc.ref, err)
			}
			if !tc.ok && (err == nil || !strings.Contains(err.Error(), "a test issue belongs to the test target team")) {
				t.Fatalf("%s: err = %v", tc.ref, err)
			}
		})
	}
}
