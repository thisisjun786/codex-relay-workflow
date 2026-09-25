package settings

import (
	"testing"
)

// TestCreation_whenPythonSettingsCaseRuns re-expresses the creation block of test_settings.py
// over the contract: what create_thread transmits and how the host's answer is judged.
func TestCreation_whenPythonSettingsCaseRuns(t *testing.T) {
	runPython(t, []pythonCase{
		{"test_effort_is_transmitted_in_config_and_confirmed", func(t *testing.T) {
			c := createContract(opus, "xhigh")
			equal(t, "config", c.StartParams()["config"], map[string]any{"model_reasoning_effort": "xhigh"})
			r := receiptOf(c, created(c, nil), "creation")
			actual := r["actual"].(map[string]any)
			equal(t, "actual effort", actual["reasoningEffort"], "xhigh")
			equal(t, "actual model", actual["model"], opus)
			equal(t, "verification", r["verification"], "observed_at_creation")
			equal(t, "findings", len(findingsOf(r)), 0)
		}},
		{"test_a_swapped_model_is_not_preserved_and_withholds_the_prompt", func(t *testing.T) {
			// The withheld prompt (zero turn/start) is the todo-15 half.
			c := createContract(opus, effort)
			f := findingsOf(receiptOf(c, created(c, map[string]any{"model": "some-other-model"}), "creation"))
			if len(f) == 0 || f[0].Code != NotPreserved || f[0].Field != "model" {
				t.Fatal(f)
			}
		}},
		{"test_a_setting_the_host_never_reports_is_unobservable_not_a_mismatch", func(t *testing.T) {
			for _, missing := range []string{"reasoningEffort", "model"} {
				c := createContract(opus, "xhigh")
				answer := created(c, nil)
				delete(answer, missing)
				r := receiptOf(c, answer, "creation")
				if f := findingsOf(r); f[0].Code != Unobservable || !sameStrings(r["unobservable"], missing) {
					t.Fatalf("%s: %v", missing, r)
				}
			}
		}},
		{"test_an_explicit_null_reads_the_same_as_an_absent_field", func(t *testing.T) {
			c := createContract(model, "xhigh")
			r := receiptOf(c, created(c, map[string]any{"reasoningEffort": nil}), "creation")
			if f := findingsOf(r); f[0].Code != Unobservable || !sameStrings(r["unobservable"], "reasoningEffort") {
				t.Fatal(r)
			}
		}},
		{"test_every_workspace_write_policy_field_is_serialised_into_config", func(t *testing.T) {
			root := "/work/tree/extra"
			policy := map[string]any{"type": "workspaceWrite", "writableRoots": []any{root}, "networkAccess": true, "excludeTmpdirEnvVar": true, "excludeSlashTmp": true}
			c := Contract{CWD: cwd, Sandbox: "workspace-write", ExpectedPolicy: policy, Model: model, ReasoningEffort: effort}
			equal(t, "sandbox_workspace_write", c.StartParams()["config"].(map[string]any)["sandbox_workspace_write"], map[string]any{"writable_roots": []any{root}, "network_access": true, "exclude_tmpdir_env_var": true, "exclude_slash_tmp": true})
			r := receiptOf(c, created(c, nil), "creation")
			equal(t, "findings", len(findingsOf(r)), 0)
			equal(t, "actual sandbox", r["actual"].(map[string]any)["sandbox"], policy)
		}},
		{"test_a_default_valued_policy_field_is_still_transmitted", func(t *testing.T) {
			c := Contract{CWD: cwd, Sandbox: "workspace-write", ExpectedPolicy: writePolicy(), Model: model, ReasoningEffort: effort}
			equal(t, "sandbox_workspace_write", c.StartParams()["config"].(map[string]any)["sandbox_workspace_write"], map[string]any{"writable_roots": []any{}, "network_access": false, "exclude_tmpdir_env_var": false, "exclude_slash_tmp": false})
		}},
		{"test_read_only_network_access_is_refused_before_any_request", func(t *testing.T) {
			// Refused by the contract itself, before any params exist; "no call reached the
			// host" through create_thread is the todo-15 half.
			c := Contract{CWD: cwd, Sandbox: "read-only", ExpectedPolicy: map[string]any{"type": "readOnly", "networkAccess": true}, Model: model, ReasoningEffort: effort}
			u := untransmittable(t, c.Validate())
			if u.Code() != "setting_untransmittable" || u.Field != "sandbox.networkAccess" {
				t.Fatal(u)
			}
		}},
		{"test_filled_protocol_defaults_are_not_read_as_a_difference", func(t *testing.T) {
			c := Contract{CWD: cwd, Sandbox: "workspace-write", ExpectedPolicy: writePolicy(), Model: model, ReasoningEffort: effort}
			equal(t, "findings", len(c.Findings(created(c, nil))), 0)
			equal(t, "normalised", Normalise(map[string]any{"type": "workspaceWrite"}), writePolicy())
		}},
		{"test_the_receipt_never_claims_more_than_the_observation", func(t *testing.T) {
			c := createContract(model, "xhigh")
			r := receiptOf(c, created(c, nil), "creation")
			equal(t, "verification", r["verification"], "observed_at_creation")
			if !contains(r["observationLimits"].(string), "no host-side exclusivity") {
				t.Fatal(r["observationLimits"])
			}
		}},
		{"test_only_the_required_pair_is_requested_when_nothing_else_is", func(t *testing.T) {
			c := createContract(model, effort)
			start := c.StartParams()
			equal(t, "model", start["model"], model)
			equal(t, "config", start["config"], map[string]any{"model_reasoning_effort": effort})
			if _, ok := start["runtimeWorkspaceRoots"]; ok {
				t.Fatal(start)
			}
			r := receiptOf(c, created(c, nil), "creation")
			requested := []string{}
			for k := range r["requested"].(map[string]any) {
				requested = append(requested, k)
			}
			if !sameStrings(sortedCopy(requested), "cwd", "model", "reasoningEffort", "sandbox") || !sameStrings(r["verified"], "cwd", "model", "reasoningEffort", "sandbox") {
				t.Fatal(r)
			}
			actual := r["actual"].(map[string]any)
			equal(t, "actual model", actual["model"], model)
			equal(t, "actual roots", actual["runtimeWorkspaceRoots"], []any{cwd})
		}},
		{"test_an_unreadable_sandbox_answer_is_a_refusal_not_a_crash", func(t *testing.T) {
			malformed := []any{"not-a-policy", 42, []any{"workspaceWrite"}, map[string]any{"no_type": 1},
				map[string]any{"type": "workspaceWrite", "writableRoots": nil}, map[string]any{"type": "workspaceWrite", "writableRoots": 7},
				map[string]any{"type": "workspaceWrite", "writableRoots": map[string]any{"a": 1}}, map[string]any{"type": map[string]any{"unhashable": 1}}, map[string]any{"type": []any{"not-a-string"}}}
			for _, answer := range malformed {
				c := createContract(model, effort)
				r := receiptOf(c, created(c, map[string]any{"sandbox": answer}), "creation")
				f := findingsOf(r)
				if len(f) == 0 || f[0].Code != NotPreserved || f[0].Field != "sandbox" {
					t.Fatalf("%v: %v", answer, f)
				}
				equal(t, "actual sandbox", r["actual"].(map[string]any)["sandbox"], answer)
			}
		}},
		{"test_normalise_policy_never_raises_and_answers_none_for_what_it_cannot_read", func(t *testing.T) {
			for _, unreadable := range []any{nil, "workspaceWrite", 42, []any{"workspaceWrite"}, map[string]any{"no_type": 1}, map[string]any{"type": map[string]any{"unhashable": 1}}, map[string]any{"type": []any{"not-a-string"}}, map[string]any{"type": "workspaceWrite", "writableRoots": nil}, map[string]any{"type": "workspaceWrite", "writableRoots": 7}} {
				if got := Normalise(unreadable); got != nil {
					t.Fatalf("%v: %v", unreadable, got)
				}
			}
			equal(t, "workspaceWrite", Normalise(map[string]any{"type": "workspaceWrite"}), writePolicy())
			equal(t, "readOnly", Normalise(map[string]any{"type": "readOnly"}), map[string]any{"type": "readOnly", "networkAccess": false})
		}},
		{"test_a_matching_mode_does_not_rescue_an_unreadable_policy", func(t *testing.T) {
			c := Contract{CWD: cwd, Sandbox: "workspace-write", Model: model, ReasoningEffort: effort}
			f := c.Findings(created(c, map[string]any{"sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": nil}}))
			if len(f) == 0 || f[0].Code != NotPreserved {
				t.Fatal(f)
			}
		}},
	})
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
