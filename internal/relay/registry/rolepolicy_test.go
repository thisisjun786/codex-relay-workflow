package registry

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
)

// Every ROL test replays its scenario from testdata/python_rolepolicy.json (gen_rolepolicy.py,
// the real Python rolepolicy/registry/linkage/delivery.authorized_settings/cmd_settings_show
// driven by the same steps) against the Go registry and compares every answer whole, then
// asserts the property's own values. The send-time gate is delivery.authorized_settings, the
// one check every sender runs before any transport call (Registry.AuthorizedSettings).

func refusedReasonOf(v any) string {
	m, _ := v.(map[string]any)
	r, _ := m["refused"].(map[string]any)
	s, _ := r["reason"].(string)
	return s
}

func refusedDetailOf(v any) string {
	m, _ := v.(map[string]any)
	r, _ := m["refused"].(map[string]any)
	s, _ := r["detail"].(string)
	return s
}

func okMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	ok, _ := m["ok"].(map[string]any)
	return ok
}

// ROL-1: the relay and the bridge spell the three roles identically.
func Test25_ROL1_the_relay_and_the_bridge_spell_the_roles_alike(t *testing.T) {
	sameRoleScenario(t, "__roles__")
	for _, role := range []string{execution.Supervisor, execution.Parent, execution.Child} {
		if _, ok := roleScope[role]; !ok {
			t.Fatalf("bridge role %s is not a relay role", role)
		}
	}
	if len(roleScope) != 3 {
		t.Fatal(roleScope)
	}
}

// ROL-2: resolution: unset -> "not set", no roles -> "declares no roles", declared -> each
// role's own pair (supervisor none) with a digest equal to Python's.
func Test25_ROL2_policy_resolution(t *testing.T) {
	unset := sameRoleScenario(t, "resolve_unset")[0].(map[string]any)
	if unset["declared"] != false || !strings.Contains(unset["detail"].(string), "not set") {
		t.Fatal(unset)
	}
	none := sameRoleScenario(t, "resolve_no_roles")[1].(map[string]any)
	if none["declared"] != false || !strings.Contains(none["detail"].(string), "declares no roles") {
		t.Fatal(none)
	}
	declared := sameRoleScenario(t, "resolve_declared")[1].(map[string]any)
	e := declared["expectations"].(map[string]any)
	if e["parent"].(map[string]any)["model"] != "anthropic/claude-opus-5-5" || e["supervisor"].(map[string]any)["model"] != nil {
		t.Fatal(declared)
	}
}

// ROL-3: a bound task whose record is not its role's declared pair is withheld before any send
// with settings_record_stale_for_role, naming the expected model and user_transition.
func Test25_ROL3_a_stale_record_for_the_role_is_refused_before_any_send(t *testing.T) {
	checks := sameRoleScenario(t, "check_record_pairs")
	for i, stale := range []bool{true, false, true, true, false} {
		finding, _ := checks[i+1].(map[string]any)
		if stale != (finding != nil) || (stale && finding["code"] != "settings_record_stale_for_role") {
			t.Fatalf("check %d: %v", i, finding)
		}
	}
	gate := sameRoleScenario(t, "gate_stale_for_role")[3]
	detail := refusedDetailOf(gate)
	if refusedReasonOf(gate) != "settings_record_stale_for_role" || !strings.Contains(detail, "anthropic/claude-opus-5-5") || !strings.Contains(detail, "user_transition") {
		t.Fatal(gate)
	}
}

// ROL-4: re-recording the transition onto the declared pair lets the same task through.
func Test25_ROL4_re_recording_the_transition_releases_the_withhold(t *testing.T) {
	steps := sameRoleScenario(t, "gate_released_by_re_record")
	if refusedReasonOf(steps[3]) == "" || okMap(steps[5]) == nil {
		t.Fatal(steps[3], steps[5])
	}
}

// ROL-5: no resolvable policy, or a policy lacking the bound role, is role_policy_unconfigured.
func Test25_ROL5_an_unconfigured_policy_withholds(t *testing.T) {
	if got := sameRoleScenario(t, "gate_unconfigured")[3]; refusedReasonOf(got) != "role_policy_unconfigured" {
		t.Fatal(got)
	}
	got := sameRoleScenario(t, "gate_partial_policy")[3]
	if refusedReasonOf(got) != "role_policy_unconfigured" || !strings.Contains(refusedDetailOf(got), "declares no such role") {
		t.Fatal(got)
	}
}

// ROL-6: a task bound to no scope is outside the policy: any pair passes, not settings-free.
func Test25_ROL6_an_unbound_task_is_outside_the_policy(t *testing.T) {
	steps := sameRoleScenario(t, "gate_unbound")
	gate := okMap(steps[4])
	if steps[3] != nil || gate == nil || gate["settingsFree"] != false || gate["settings"].(map[string]any)["model"] != "anthropic/claude-opus-5" {
		t.Fatal(steps)
	}
}

// ROL-7: the created-as role and the bound role must agree, whichever arrives second.
func Test25_ROL7_the_created_role_and_the_bound_role_must_agree(t *testing.T) {
	after := sameRoleScenario(t, "settings_after_binding")
	if refusedReasonOf(after[2]) != "role_binding_mismatch" || after[3] != nil {
		t.Fatal(after)
	}
	before := sameRoleScenario(t, "binding_after_settings")
	if refusedReasonOf(before[2]) != "role_binding_mismatch" || len(before[3].([]any)) != 0 {
		t.Fatal(before)
	}
	if got := sameRoleScenario(t, "pair_not_the_bound_roles")[2]; refusedReasonOf(got) != "role_binding_mismatch" {
		t.Fatal(got)
	}
	match := sameRoleScenario(t, "matching_role_and_pair")
	if okMap(match[2])["settings"].(map[string]any)["citedRole"] != "parent" || match[3] != "parent" {
		t.Fatal(match)
	}
}

// ROL-8: two live role bindings are Contested (child, parent) and withheld; a superseded binding
// is not a held role.
func Test25_ROL8_two_live_roles_are_contested(t *testing.T) {
	steps := sameRoleScenario(t, "two_live_roles")
	if !reflect.DeepEqual(steps[3], map[string]any{"contested": []any{"child", "parent"}}) || refusedReasonOf(steps[5]) != "role_binding_mismatch" {
		t.Fatal(steps)
	}
	if got := sameRoleScenario(t, "superseded_binding")[2]; got != nil {
		t.Fatal(got)
	}
}

// ROL-9: a re-record naming no role keeps the creation's citedRole, so another role still
// refuses.
func Test25_ROL9_a_re_record_keeps_the_created_role(t *testing.T) {
	steps := sameRoleScenario(t, "re_record_keeps_cited_role")
	if steps[3].(map[string]any)["citedRole"] != "parent" || refusedReasonOf(steps[4]) != "role_binding_mismatch" {
		t.Fatal(steps)
	}
}

// ROL-10: doctor.rolePolicy reports declared, its own digest, not_observable_from_here and
// get_capabilities. Its Python-vs-Go whole comparison is internal/relay/cli's doctor parity
// (todo 20); here the Go summary is checked to carry the digest Python computes for the file.
func Test25_ROL10_the_policy_reports_its_own_digest(t *testing.T) {
	steps := sameRoleScenario(t, "resolve_declared")
	summary := steps[1].(map[string]any)["summary"].(map[string]any)
	if summary["state"] != "declared" || summary["digest"] != "${DIGEST}" {
		t.Fatal(summary)
	}
}

// ROL-11: a supervisor (pair never policy-derived) is flagged settings-free; a recipient on its
// declared pair is not; a settings-free resume that loads as something else is refused
// settings_differ_after_load with the model finding.
func Test25_ROL11_a_supervisor_is_resumed_settings_free(t *testing.T) {
	steps := sameRoleScenario(t, "settings_free_supervisor")
	if okMap(steps[3])["settingsFree"] != true || okMap(steps[6])["settingsFree"] != false {
		t.Fatal(steps[3], steps[6])
	}
	loaded := sameRoleScenario(t, "loaded_as_something_else")[0].(map[string]any)
	if loaded["code"] != SettingsDifferAfterLoad || loaded["findings"].([]any)[0].(map[string]any)["field"] != "model" {
		t.Fatal(loaded)
	}
}

// ROL-12: the unloaded guard refuses a pair differing in model only, or in effort only.
func Test25_ROL12_the_unloaded_guard_checks_both_halves_of_the_pair(t *testing.T) {
	steps := sameRoleScenario(t, "unloaded_guard")
	if steps[1] != nil || refusedReasonOf(steps[2]) != "unverified_pair_for_unloaded_thread" || refusedReasonOf(steps[3]) != "unverified_pair_for_unloaded_thread" {
		t.Fatal(steps)
	}
}

// ROL-13: a legacy record citing an unauthorized exception is withheld naming the id.
func Test25_ROL13_a_legacy_unauthorized_citation_is_withheld(t *testing.T) {
	gate := sameRoleScenario(t, "legacy_unauthorized_citation")[3]
	if refusedReasonOf(gate) != "role_binding_mismatch" || !strings.Contains(refusedDetailOf(gate), "not-written") {
		t.Fatal(gate)
	}
}

// ROL-14: operator exceptions are verified against the policy file.
func Test25_ROL14_operator_exceptions_are_verified(t *testing.T) {
	steps := sameRoleScenario(t, "exceptions")
	for i, refused := range map[int]bool{2: true, 3: true, 4: true, 5: false, 6: false} {
		if (refusedReasonOf(steps[i]) == "role_binding_mismatch") != refused {
			t.Fatalf("step %d: %v", i, steps[i])
		}
	}
	if _, cited := okMap(steps[5])["settings"].(map[string]any)["citedException"]; cited {
		t.Fatal(steps[5])
	}
	if okMap(steps[6])["settings"].(map[string]any)["citedException"] != "one-task" {
		t.Fatal(steps[6])
	}
	undeclared := sameRoleScenario(t, "exception_for_undeclared_role")
	if undeclared[1] != nil || undeclared[2] != nil || undeclared[3].(map[string]any)["code"] != "role_policy_unconfigured" {
		t.Fatal(undeclared)
	}
}

// ROL-15: an exception equal to the role's pair is still an exception; a user-transition
// re-record onto the declared pair drops it and the guard then passes.
func Test25_ROL15_an_exception_equal_to_the_pair_is_still_an_exception(t *testing.T) {
	steps := sameRoleScenario(t, "exception_equal_to_the_role_pair")
	if refusedReasonOf(steps[1]) != "unverified_pair_for_unloaded_thread" {
		t.Fatal(steps[1])
	}
	if _, cited := steps[5].(map[string]any)["citedException"]; cited || steps[6] != nil {
		t.Fatal(steps[5], steps[6])
	}
}

// ROL-16: citation carry-forward, the supervisor's user transition, the explicit clear and an
// exception literally named "__clear__".
func Test25_ROL16_citation_carry_forward(t *testing.T) {
	carried := sameRoleScenario(t, "citation_carry_forward")
	if carried[4].(map[string]any)["citedException"] != "one-task" || carried[6].(map[string]any)["citedException"] != "one-task" {
		t.Fatal(carried)
	}
	for _, name := range []string{"supervisor_user_transition_drops", "explicit_clear"} {
		steps := sameRoleScenario(t, name)
		stored := steps[len(steps)-1].(map[string]any)
		if _, cited := stored["citedException"]; cited || stored["citedRole"] != "supervisor" {
			t.Fatal(name, stored)
		}
	}
	sentinel := sameRoleScenario(t, "sentinel_named_exception")
	if sentinel[3].(map[string]any)["citedException"] != "__clear__" || sentinel[5].(map[string]any)["citedException"] != "__clear__" {
		t.Fatal(sentinel)
	}
}

// ROL-17: register --project that will bind the child refuses a parent citation first, leaving
// no relationship, no binding and no settings.
func Test25_ROL17_register_refuses_a_parent_citation_for_a_child_first(t *testing.T) {
	steps := sameRoleScenario(t, "register_refuses_parent_citation_for_child")
	if refusedReasonOf(steps[2]) != "role_binding_mismatch" || steps[3] != float64(0) || steps[4] != nil || steps[5] != nil {
		t.Fatal(steps)
	}
}

// ROL-18: replaying a live binding survives a policy edit (same bindingId); a new binding is
// still refused under the policy in force.
func Test25_ROL18_a_replayed_binding_survives_a_policy_edit(t *testing.T) {
	steps := sameRoleScenario(t, "replay_survives_policy_edit")
	if okMap(steps[2])["bindingId"] != okMap(steps[4])["bindingId"] || steps[5] != "parent" {
		t.Fatal(steps)
	}
	fresh := sameRoleScenario(t, "new_binding_refused_under_policy")
	if refusedReasonOf(fresh[2]) != "role_binding_mismatch" || fresh[3] != nil {
		t.Fatal(fresh)
	}
}

// ROL-19: settings-show separates complete from deliverable.
func Test25_ROL19_settings_show_separates_complete_from_deliverable(t *testing.T) {
	steps := sameRoleScenario(t, "settings_show_deliverable")
	clean, stale := okMap(steps[3]), okMap(steps[5])
	if clean["usable"] != true || clean["deliverable"] != true || clean["roleFinding"] != nil {
		t.Fatal(clean)
	}
	if stale["usable"] != true || len(stale["missing"].([]any)) != 0 || stale["deliverable"] != false || stale["roleFinding"] == nil {
		t.Fatal(stale)
	}
	unresolved := okMap(sameRoleScenario(t, "settings_show_unresolved")[3])
	if unresolved["deliverable"] != false || unresolved["rolePolicy"] != "unresolved" || unresolved["roleFinding"].(map[string]any)["code"] != "role_policy_unconfigured" {
		t.Fatal(unresolved)
	}
}

// ROL-20: the snapshot is taken once per process: an edit to the file after the first read is
// not adopted (digest unchanged) until the snapshot is dropped, and the relay CLI takes it
// before its handler even for a command that asks no role question.
func Test25_ROL20_the_policy_snapshot_is_this_process(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "execution-policy.json")
	write := func(parentModel string) {
		text := `{"roles": {"supervisor": {"expectation": "record"}, "parent": {"model": "` + parentModel + `", "reasoningEffort": "xhigh"}, "child": {"model": "anthropic/claude-opus-5-5", "reasoningEffort": "xhigh"}}}`
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("anthropic/claude-opus-5-5")
	t.Setenv(execution.EnvPolicy, path)
	ResetRolePolicySnapshot()
	t.Cleanup(ResetRolePolicySnapshot)
	var stdout, stderr bytes.Buffer
	ExecuteAs(ctx(), "codex-session-relay", []string{"--state", filepath.Join(dir, "state"), "settings-show", "--task", parent}, &stdout, &stderr, nil)
	started := EnvironmentRolePolicy().Digest()
	write("devin/swe-2")
	if started == "" || EnvironmentRolePolicy().Digest() != started {
		t.Fatalf("the edit was adopted without a restart: %q", started)
	}
	ResetRolePolicySnapshot()
	if EnvironmentRolePolicy().Digest() == started {
		t.Fatal("a new process snapshot did not read the edited file")
	}
}

// ROL-21: register with an incomplete child settings file refuses before the relationship
// commits.
func Test25_ROL21_incomplete_settings_refuse_before_the_relationship_commits(t *testing.T) {
	steps := sameRoleScenario(t, "register_refuses_incomplete_settings")
	if refusedReasonOf(steps[1]) != SettingsIncomplete || steps[2] != float64(0) {
		t.Fatal(steps)
	}
}

// ROL-22: archiving is never blocked by the role policy.
func Test25_ROL22_archiving_is_never_blocked_by_the_policy(t *testing.T) {
	steps := sameRoleScenario(t, "archive_never_blocked")
	if okMap(steps[4])["status"] != "archived" {
		t.Fatal(steps[4])
	}
}
