package execution

import (
	"reflect"
	"testing"
)

// TestAuthorize_whenPythonExceptionOrAllowlistDecides re-expresses the exception and allowlist
// cases of test_execution.py, plus the policy half of one bridge-level (todo 15) case.
func TestAuthorize_whenPythonExceptionOrAllowlistDecides(t *testing.T) {
	withException := func(t *testing.T, entry doc) Policy {
		document := rolesPolicy(nil, true)
		document["exceptions"] = doc{"one-task": entry}
		return mustLoad(t, document)
	}
	runPython(t, []pythonCase{
		{"test_an_exception_answers_the_role_question_and_the_receipt_names_it", func(t *testing.T) {
			p := withException(t, doc{"model": unapproved, "reasoningEffort": "high", "cwd": []any{"/tmp"}, "role": "parent"})
			got := authorize(t, p, Input{Model: unapproved, Effort: "high", CWD: "/tmp", Exception: "one-task", Role: "parent"})
			if got.Model != unapproved || got.Receipt["roleExpectation"].(map[string]any)["overriddenBy"] != "one-task" || got.Provenance != "exception" {
				t.Fatal(got)
			}
		}},
		{"test_an_exception_does_not_cover_a_role_it_was_not_written_for", func(t *testing.T) {
			for _, row := range []struct{ declared, cited string }{{"parent", "child"}, {"", "parent"}} {
				entry := doc{"model": unapproved, "reasoningEffort": "high", "cwd": []any{"/tmp"}}
				if row.declared != "" {
					entry["role"] = row.declared
				}
				r := refused(t, second(withException(t, entry).Authorize(Input{Model: unapproved, Effort: "high", CWD: "/tmp", Exception: "one-task", Role: row.cited})))
				if r.Code != ExceptionOutOfScope {
					t.Fatalf("%+v: %+v", row, r)
				}
			}
		}},
		{"test_an_exception_written_before_roles_existed_still_works_for_a_caller_that_names_none", func(t *testing.T) {
			p := withException(t, doc{"model": unapproved, "reasoningEffort": "high", "cwd": []any{"/tmp"}})
			if authorize(t, p, Input{Model: unapproved, Effort: "high", CWD: "/tmp", Exception: "one-task"}).Model != unapproved {
				t.Fatal("refused")
			}
		}},
		{"test_efforts_are_scoped_to_their_model", func(t *testing.T) {
			p := mustLoad(t, policyFor(canonicalTemp(t)))
			authorize(t, p, Input{Model: model, Effort: effort})
			authorize(t, p, Input{Model: solModel, Effort: "high"})
			r := refused(t, second(p.Authorize(Input{Model: model, Effort: "high"})))
			if r.Code != NotAllowed || r.Field != "reasoning_effort" || !sameStrings(r.Allowed, effort) {
				t.Fatal(r)
			}
		}},
		{"test_an_exception_authorizes_one_triple_and_not_a_family", func(t *testing.T) {
			here := canonicalTemp(t)
			p := mustLoad(t, policyFor(here))
			authorize(t, p, Input{Model: unapproved, Effort: "high", CWD: here, Exception: "one-task"})
			for _, row := range []struct{ model, effort, field string }{{model, "high", "model"}, {unapproved, effort, "reasoning_effort"}} {
				r := refused(t, second(p.Authorize(Input{Model: row.model, Effort: row.effort, CWD: here, Exception: "one-task"})))
				if r.Code != NotAllowed || r.Field != row.field {
					t.Fatalf("%+v: %+v", row, r)
				}
			}
			for _, elsewhere := range []string{"", "/somewhere/else"} {
				r := refused(t, second(p.Authorize(Input{Model: unapproved, Effort: "high", CWD: elsewhere, Exception: "one-task"})))
				if r.Code != ExceptionOutOfScope {
					t.Fatal(r)
				}
			}
		}},
		{"approved_pair_receipt_names_its_mode", func(t *testing.T) {
			// Policy half of a todo-15 (D) bridge case: the exact executionPolicy receipt stored.
			p := mustLoad(t, policyFor(canonicalTemp(t)))
			p.digest = "test-digest" // conftest configured_bridge passes digest="test-digest"
			want := map[string]any{"mode": "allowlist", "digest": "test-digest", "exception": nil, "role": nil, "model": model, "reasoningEffort": effort, "limits": Limits}
			if got := authorize(t, p, Input{Model: model, Effort: effort}).Receipt; !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v", got)
			}
		}},
	})
}
