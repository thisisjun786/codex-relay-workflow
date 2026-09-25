package settings

import (
	"testing"
)

// resumed is the host's answer to a resume carrying c, echoing the thread's cwd, with the
// FakeServer knobs this block uses: override_resume and approval_policy.
func resumed(c Contract, approval any, overrides map[string]any) map[string]any {
	params := c.ResumeParams("thread-1")
	params["cwd"] = cwd
	if _, ok := params["sandbox"]; !ok {
		params["sandbox"] = "read-only"
	}
	answer := hostEcho(params)
	answer["approvalPolicy"] = approval
	for k, v := range overrides {
		answer[k] = v
	}
	return answer
}

// TestResume_whenPythonSettingsCaseRuns re-expresses the resume, annotation and declared
// approval blocks of test_settings.py over the contract.
func TestResume_whenPythonSettingsCaseRuns(t *testing.T) {
	declared := func(policy string) Contract {
		return Contract{Model: model, ReasoningEffort: effort, ApprovalPolicy: policy}
	}
	runPython(t, []pythonCase{
		{"test_a_resume_always_carries_the_authorized_pair", func(t *testing.T) {
			// The refused bare send (no pair, nothing sent) is the todo-15 half.
			c := Contract{Model: model, ReasoningEffort: effort}
			equal(t, "resume", c.ResumeParams("thread-1"), map[string]any{"threadId": "thread-1", "excludeTurns": true, "model": model, "config": map[string]any{"model_reasoning_effort": effort}})
			r := receiptOf(c, resumed(c, "never", nil), "resume")
			equal(t, "verification", r["verification"], "observed_at_resume")
			equal(t, "requested", r["requested"], map[string]any{"model": model, "reasoningEffort": effort})
			equal(t, "actual approval", r["actual"].(map[string]any)["approvalPolicy"], "never")
		}},
		{"test_resume_carries_the_settings_it_can_express", func(t *testing.T) {
			c := carried()
			c.CWD, c.Model, c.ReasoningEffort = cwd, opus, "xhigh"
			p := c.ResumeParams("thread-1")
			equal(t, "sandbox", p["sandbox"], "workspace-write")
			equal(t, "cwd", p["cwd"], cwd)
			equal(t, "model", p["model"], opus)
			equal(t, "effort", p["config"].(map[string]any)["model_reasoning_effort"], "xhigh")
			if _, ok := p["approvalPolicy"]; ok {
				t.Fatal(p)
			}
		}},
		{"test_a_widened_sandbox_withholds_the_message_before_any_turn", func(t *testing.T) {
			c := carried()
			f := c.Findings(resumed(c, "never", map[string]any{"sandbox": map[string]any{"type": "dangerFullAccess"}}))
			if len(f) == 0 || f[0].Code != NotPreserved || f[0].Field != "sandbox" {
				t.Fatal(f)
			}
		}},
		{"test_an_interactive_approval_policy_still_decides_alone", func(t *testing.T) {
			c := carried()
			f := c.Findings(resumed(c, "on-request", map[string]any{"sandbox": map[string]any{"type": "dangerFullAccess"}}))
			if len(f) != 1 || f[0].Code != UnsupportedApproval {
				t.Fatal(f)
			}
		}},
		{"test_the_annotation_records_what_the_thread_reported_after_dispatch", func(t *testing.T) {
			c := createContract(model, "xhigh")
			before := Observed(created(c, nil))
			thread := map[string]any{"id": "thread-1", "cwd": cwd, "model": model, "reasoningEffort": "xhigh"}
			note := Annotation(before, thread)
			equal(t, "concurrentChange", note["concurrentChange"], false)
			if !sameStrings(note["covers"], "cwd", "model", "reasoningEffort") || len(note["unobserved"].([]string)) != 0 {
				t.Fatal(note)
			}
			if !contains(note["limit"].(string), "neither sandbox nor approvalPolicy") {
				t.Fatal(note["limit"])
			}
		}},
		{"test_a_field_missing_after_dispatch_is_unobserved_not_unchanged", func(t *testing.T) {
			c := createContract(opus, effort)
			before := Observed(created(c, nil))
			note := Annotation(before, map[string]any{"cwd": cwd, "model": nil, "reasoningEffort": effort})
			unobserved := note["unobserved"].([]string)
			covers := note["covers"].([]string)
			if !contains(joined(unobserved), "model") || contains(joined(covers), "model") {
				t.Fatal(note)
			}
		}},
		{"test_an_idle_thread_on_on_request_accepts_a_declared_delivery", func(t *testing.T) {
			c := declared("on-request")
			if _, ok := c.ResumeParams("thread-1")["approvalPolicy"]; ok {
				t.Fatal("approvalPolicy transmitted")
			}
			answer := resumed(c, "on-request", nil)
			equal(t, "findings", len(c.Findings(answer)), 0)
			a := c.Approvals(answer)
			equal(t, "observed", a["observed"], "on-request")
			equal(t, "declared", a["declared"], "on-request")
			equal(t, "transmitted", a["transmitted"], false)
			equal(t, "preservation", a["preservation"], "omitted_from_resume")
		}},
		{"test_declaring_nothing_still_refuses_an_interactive_thread", func(t *testing.T) {
			c := Contract{Model: model, ReasoningEffort: effort}
			f := c.Findings(resumed(c, "on-request", nil))
			if len(f) == 0 || f[0].Code != UnsupportedApproval || f[0].Expected != "never" {
				t.Fatal(f)
			}
		}},
		{"test_a_policy_that_changed_under_the_caller_is_refused", func(t *testing.T) {
			c := declared("on-request")
			f := c.Findings(resumed(c, "untrusted", nil))
			if len(f) == 0 || f[0].Code != UnsupportedApproval || f[0].Returned != "untrusted" {
				t.Fatal(f)
			}
		}},
		{"test_a_granular_policy_is_observed_but_can_never_be_declared", func(t *testing.T) {
			c := declared("on-request")
			granular := map[string]any{"granular": map[string]any{"mcp_elicitations": true, "rules": true, "sandbox_approval": true}}
			f := c.Findings(resumed(c, granular, nil))
			if len(f) == 0 || f[0].Code != UnsupportedApproval || f[0].Returned != "granular" {
				t.Fatal(f)
			}
			if err := declared("granular").Validate(); err == nil || !contains(err.Error(), "approval_policy must be one of") {
				t.Fatal(err)
			}
		}},
	})
}

func joined(values []string) string {
	out := ""
	for _, v := range values {
		out += "|" + v + "|"
	}
	return out
}
