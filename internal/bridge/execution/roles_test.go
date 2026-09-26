package execution

import (
	"strings"
	"testing"
)

// pythonCase is one Python test re-expressed; python is its name in the Python suite.
type pythonCase struct {
	python string
	run    func(t *testing.T)
}

func runPython(t *testing.T, cases []pythonCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.python, c.run)
	}
}

// TestRoles_whenPythonRoleQuestionIsAsked re-expresses the roles block of test_execution.py.
func TestRoles_whenPythonRoleQuestionIsAsked(t *testing.T) {
	declared := func(t *testing.T) Policy { return mustLoad(t, rolesPolicy(nil, true)) }
	runPython(t, []pythonCase{
		{"test_a_role_this_host_never_declared_is_refused_rather_than_defaulted", func(t *testing.T) {
			// Given: a host with no policy file.
			p, err := FromEnvironment(map[string]string{})
			if err != nil {
				t.Fatal(err)
			}
			// When: a role is named. Then: refused as unknown, while naming none still passes.
			r := refused(t, second(p.Authorize(Input{Model: model, Effort: effort, Role: "parent"})))
			if r.Code != RoleUnknown || r.Field != "role" {
				t.Fatal(r)
			}
			if authorize(t, p, Input{Model: model, Effort: effort}).Model != model {
				t.Fatal("unnamed role changed")
			}
		}},
		{"test_a_word_that_is_not_a_role_is_refused_like_one_that_was_never_declared", func(t *testing.T) {
			r := refused(t, second(declared(t).Authorize(Input{Model: model, Effort: effort, Role: "coordinator"})))
			if r.Code != RoleUnknown {
				t.Fatal(r)
			}
		}},
		{"test_a_pair_that_is_not_this_roles_pair_is_refused", func(t *testing.T) {
			rows := []struct{ role, model, effort, field string }{
				{"parent", supersededParent[0], supersededParent[1], "model"},
				{"parent", parentModel, supersededParent[1], "reasoning_effort"},
				{"child", model, supersededParent[1], "reasoning_effort"},
				{"child", supersededParent[0], effort, "model"},
			}
			for _, row := range rows {
				r := refused(t, second(declared(t).Authorize(Input{Model: row.model, Effort: row.effort, Role: row.role})))
				if r.Code != RoleMismatch || r.Field != row.field {
					t.Fatalf("%+v: %+v", row, r)
				}
			}
		}},
		{"test_an_effort_name_belongs_to_its_model_and_never_stands_in_for_another", func(t *testing.T) {
			p := mustLoad(t, rolesPolicy(doc{"parent": doc{"model": parentModel, "reasoningEffort": parentEffort}, "child": doc{"model": supersededParent[0], "reasoningEffort": supersededParent[1]}}, false))
			roles := p.Summary()["roles"].(map[string]any)
			if roles["parent"].(map[string]any)["reasoningEffort"] != parentEffort || roles["child"].(map[string]any)["reasoningEffort"] != supersededParent[1] {
				t.Fatal(roles)
			}
			if authorize(t, p, Input{Model: parentModel, Effort: parentEffort, Role: "parent"}).Effort != "xhigh" {
				t.Fatal("parent pair")
			}
			refused(t, second(p.Authorize(Input{Model: parentModel, Effort: supersededParent[1], Role: "parent"})))
			refused(t, second(p.Authorize(Input{Model: supersededParent[0], Effort: parentEffort, Role: "child"})))
		}},
		{"test_the_pair_a_role_used_to_run_on_is_refused_like_any_other_wrong_pair", func(t *testing.T) {
			p := declared(t)
			r := refused(t, second(p.Authorize(Input{Model: supersededParent[0], Effort: supersededParent[1], Role: "parent"})))
			if r.Code != RoleMismatch || !sameStrings(r.Allowed, parentModel) {
				t.Fatal(r)
			}
			if authorize(t, p, Input{Model: supersededParent[0], Effort: supersededParent[1]}).Model != supersededParent[0] {
				t.Fatal("allowlist refused the unnamed request")
			}
			authorize(t, p, Input{Model: parentModel, Effort: parentEffort, Role: "parent"})
		}},
		{"test_the_pair_the_child_used_to_run_on_is_refused_on_the_model_alone", func(t *testing.T) {
			p := mustLoad(t, rolesPolicy(nil, false))
			r := refused(t, second(p.Authorize(Input{Model: supersededChild[0], Effort: supersededChild[1], Role: "child"})))
			if r.Code != RoleMismatch || r.Field != "model" {
				t.Fatal(r)
			}
			authorize(t, p, Input{Model: model, Effort: effort, Role: "child"})
		}},
		{"test_presence_is_settled_before_the_role", func(t *testing.T) {
			// Python _stated: None is missing, "" and "   " are invalid.
			rows := []struct {
				value any
				code  string
			}{{nil, Missing}, {"", Invalid}, {"   ", Invalid}}
			for _, row := range rows {
				r := refused(t, second(declared(t).Authorize(Input{Model: row.value, Effort: effort, Role: "parent"})))
				if r.Code != row.code || r.Field != "model" {
					t.Fatalf("%q: %+v", row.value, r)
				}
			}
		}},
		{"test_a_supervisor_keeps_the_pair_it_was_given_and_the_receipt_says_where_it_came_from", func(t *testing.T) {
			got := authorize(t, mustLoad(t, rolesPolicy(nil, false)), Input{Model: "gpt-6-astra", Effort: "high", Role: "supervisor"})
			expectation := got.Receipt["roleExpectation"].(map[string]any)
			if got.Model != "gpt-6-astra" || got.Effort != "high" || expectation["expectation"] != "record" || expectation["model"] != nil {
				t.Fatal(got)
			}
		}},
		{"test_a_supervisor_is_exempt_from_the_role_question_and_not_from_the_allowlist", func(t *testing.T) {
			r := refused(t, second(declared(t).Authorize(Input{Model: "gpt-6-astra", Effort: "high", Role: "supervisor"})))
			if r.Code != NotAllowed {
				t.Fatal(r)
			}
		}},
		{"test_a_policy_that_pins_the_supervisors_model_does_not_load", func(t *testing.T) {
			_, err := load(t, rolesPolicy(doc{"supervisor": doc{"model": parentModel, "reasoningEffort": parentEffort}}, true))
			if !strings.Contains(policyError(t, err).Error(), "supervisor") {
				t.Fatal(err)
			}
		}},
		{"test_roles_can_be_declared_without_imposing_an_allowlist_on_every_other_task", func(t *testing.T) {
			p := mustLoad(t, rolesPolicy(nil, false))
			if p.Mode() != "presence_only" {
				t.Fatal(p.Mode())
			}
			authorize(t, p, Input{Model: parentModel, Effort: parentEffort, Role: "parent"})
			authorize(t, p, Input{Model: unapproved, Effort: "low"})
		}},
		{"test_a_policy_declaring_neither_an_allowlist_nor_a_role_is_still_a_mistake", func(t *testing.T) {
			policyError(t, second(load(t, doc{})))
		}},
		{"test_a_role_cannot_declare_a_value_longer_than_a_request_may_state", func(t *testing.T) {
			// roles.SETTING_MAXIMUM == execution.MAXIMUM == 500 in Python; Go has one constant.
			longest := strings.Repeat("m", 500)
			mustLoad(t, rolesPolicy(doc{"parent": doc{"model": longest, "reasoningEffort": "max"}}, false))
			policyError(t, second(load(t, rolesPolicy(doc{"parent": doc{"model": longest + "m", "reasoningEffort": "max"}}, false))))
		}},
		{"test_a_role_that_is_not_the_supervisor_cannot_opt_out_of_its_own_pair", func(t *testing.T) {
			_, err := load(t, rolesPolicy(doc{"parent": doc{"expectation": "record"}}, true))
			if !strings.Contains(policyError(t, err).Error(), "parent") {
				t.Fatal(err)
			}
		}},
		{"test_the_supervisor_cannot_be_given_a_pinned_pair_through_the_expectation_key", func(t *testing.T) {
			policyError(t, second(load(t, rolesPolicy(doc{"supervisor": doc{"expectation": "pair", "model": model, "reasoningEffort": effort}}, true))))
		}},
	})
}

// TestRoles_whenExpectationGuardIsTheOnlyRefusal gives each roles.py expectation guard an
// input that no later check refuses, and asserts the message roles.py produced for it.
func TestRoles_whenExpectationGuardIsTheOnlyRefusal(t *testing.T) {
	rows := []struct {
		name  string
		roles doc
		want  string
	}{
		{"supervisor declares pair", doc{"supervisor": doc{"expectation": "pair"}}, "execution_policy_unreadable: role 'supervisor' expectation must be 'record': its model and effort are the user's own selection, not something the policy pins"},
		{"parent declares record with a full pair", doc{"parent": doc{"expectation": "record", "model": "m", "reasoningEffort": "e"}}, "execution_policy_unreadable: role 'parent' expectation must be 'pair': only 'supervisor' defers to the recorded authorization, and letting another role do so would exempt it from the role check"},
		{"bogus expectation value", doc{"parent": doc{"expectation": "bogus", "model": "m", "reasoningEffort": "e"}}, "execution_policy_unreadable: role 'parent' expectation must be 'pair' or 'record'"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// Given: a roles-only policy. When: it is loaded. Then: Python's exact refusal.
			_, err := load(t, doc{"roles": row.roles})
			if got := policyError(t, err).Error(); got != row.want {
				t.Fatalf("got %q", got)
			}
		})
	}
}

// second returns the error of a two-value call so a refusal can be asserted inline.
func second[T any](_ T, err error) error { return err }
