package registry

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// settingsAnswers runs every row of testdata/settings_cases.json through TaskSettings and
// returns the answers by case: missing, usable, resumeParams, mismatches and narrowing.
func settingsAnswers(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/settings_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, field := range decoded.(contract.OrderedObject) {
		c := field.Value.(contract.OrderedObject)
		rowValue, _ := c.Lookup("row")
		settings := TaskSettings{rowValue.(contract.OrderedObject)}
		answer := contract.OrderedObject{{Key: "missing", Value: anyStrings(settings.Missing())}}
		if err := settings.RequireUsable(); err != nil {
			var refused *store.RefusedError
			if !errors.As(err, &refused) {
				t.Fatal(err)
			}
			answer = append(answer, contract.Field{Key: "usable", Value: contract.OrderedObject{{Key: "reason", Value: refused.Reason}, {Key: "detail", Value: refused.Detail}}})
		} else {
			answer = append(answer, contract.Field{Key: "usable", Value: nil}, contract.Field{Key: "resumeParams", Value: settings.ResumeParams("t-1")})
		}
		if response, ok := c.Lookup("response"); ok {
			transmitted, loaded := true, false
			if v, ok := c.Lookup("transmitted"); ok {
				transmitted = v.(bool)
			}
			if v, ok := c.Lookup("loadedBefore"); ok {
				loaded = v.(bool)
			}
			found := settings.Mismatches(response, transmitted, false, loaded)
			list := make([]any, len(found))
			for i, f := range found {
				list[i] = f
			}
			answer = append(answer, contract.Field{Key: "mismatches", Value: list})
			if v, ok := c.Lookup("narrowing"); ok && v == true {
				notes := settings.RootsNarrowing(response, "idle")
				items := []any{}
				for _, n := range notes {
					items = append(items, n)
				}
				answer = append(answer, contract.Field{Key: "narrowing", Value: items})
			}
		}
		got[field.Key] = plain(t, answer)
	}
	return got
}

func plain(t *testing.T, v any) any {
	t.Helper()
	var b bytesBuffer
	if err := contract.Emit(&b, v); err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// sameSettingsAsGolden compares the named cases (every case with prefix) whole with the golden
// and returns every answer.
func sameSettingsAsGolden(t *testing.T, prefixes ...string) map[string]any {
	t.Helper()
	got := settingsAnswers(t)
	var names []string
	for name := range got {
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				names = append(names, name)
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatalf("no cases for %v", prefixes)
	}
	for _, name := range names {
		golden.CheckJSON(t, name, got[name])
	}
	return got
}

func usableOf(answer any) map[string]any {
	u, _ := answer.(map[string]any)["usable"].(map[string]any)
	return u
}

func Test25_SPR2_completeness_environments_empty_is_a_decision_and_absence_is_decided_first(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "environments_", "absent_before_mistyped", "approval_null", "sandbox_null", "sandbox_omitted", "complete")
	if usableOf(got["environments_empty"]) != nil || usableOf(got["absent_before_mistyped"])["detail"] != "missing cwd" {
		t.Fatal("the table changed")
	}
}

func Test25_SPR3_every_mistyped_string_field_is_named_in_one_refusal(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "mistyped_", "roots_string", "env_bad")
	if usableOf(got["mistyped_all"])["detail"] != "cwd is int, not str; model is bool, not str; reasoningEffort is list, not str" {
		t.Fatal(got["mistyped_all"])
	}
}

func Test25_SPR6_only_never_and_on_request_are_carriable_approval_policies(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "approval_")
	for _, name := range []string{"approval_0", "approval_1", "approval_2", "approval_3", "approval_4"} {
		if usableOf(got[name])["reason"] != UnsupportedApprovalPolicy {
			t.Fatal(name)
		}
	}
}

func Test25_SPR7_the_gates_decide_in_one_order(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "ladder_")
	for i, reason := range []string{SettingsIncomplete, SettingsMistyped, UnsupportedApprovalPolicy, UnsupportedSandboxType} {
		if usableOf(got["ladder_"+string(rune('0'+i))])["reason"] != reason {
			t.Fatalf("ladder %d", i)
		}
	}
}

func Test25_SPR11_approval_policy_in_the_response_is_decided_first_and_alone(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "resp_approval_", "resp_not_object")
	for name, code := range map[string]string{"resp_approval_absent": SettingUnobservable, "resp_approval_untrusted": UnsupportedApprovalPolicy} {
		found := got[name].(map[string]any)["mismatches"].([]any)
		if len(found) != 1 || found[0].(map[string]any)["code"] != code {
			t.Fatalf("%s: %v", name, found)
		}
	}
}

func Test25_SPR14_two_unreadable_sandbox_policies_never_agree(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "sandbox_malformed")
	found := got["sandbox_malformed"].(map[string]any)["mismatches"].([]any)
	if first := found[0].(map[string]any); first["field"] != "sandbox" || first["code"] != SettingsNotPreserved {
		t.Fatal(found)
	}
}

func Test25_SPR15_a_sandbox_that_is_not_a_readable_policy_is_unsupported_with_its_exact_detail(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "sandbox_kind_", "sandbox_type_")
	if usableOf(got["sandbox_kind_0"])["detail"] != "the recorded sandbox is str, not the policy object a creation result reports, so it does not record the full policy a resume would have to restore" {
		t.Fatal(got["sandbox_kind_0"])
	}
}

func Test25_SPR16_a_readable_policy_is_unchanged_and_external_sandbox_is_named_exactly(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "complete", "sandbox_external", "sandbox_readonly")
	if usableOf(got["sandbox_external"])["detail"] != "'externalSandbox' has no ThreadResumeParams.sandbox mode, so it cannot be restored on a resume" {
		t.Fatal(got["sandbox_external"])
	}
	if got["complete"].(map[string]any)["resumeParams"].(map[string]any)["sandbox"] != "workspace-write" {
		t.Fatal("resume mode")
	}
}

// QA failure scenario of todo 25 and the comparison half of SPR-10/CLI-35: a response that
// widens the writable roots, the sandbox, or any field a send carries is settings_not_preserved,
// in Python's order, which the golden keeps; narrower roots pass only after a load that
// transmitted nothing or a loaded recipient.
func Test25_QA_widened_writable_roots_are_settings_not_preserved_in_pythons_order(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "resp_")
	found := got["resp_widened_roots"].(map[string]any)["mismatches"].([]any)
	if len(found) == 0 || found[0].(map[string]any)["code"] != SettingsNotPreserved {
		t.Fatal(found)
	}
	if n := got["resp_narrower_loaded"].(map[string]any)["mismatches"].([]any); len(n) != 0 {
		t.Fatal(n)
	}
}

// SHN-11: an accepted narrowing is noted once per place where the reported SET is a strict
// subset of the record (a reordering is no narrowing), with the recipient's prior status.
func Test25_SHN11_an_accepted_narrowing_is_noted_where_the_set_shrank(t *testing.T) {
	t.Parallel()
	got := sameSettingsAsGolden(t, "narrow_")
	notes := got["narrow_all"].(map[string]any)["narrowing"].([]any)
	if len(notes) != 3 || notes[0].(map[string]any)["code"] != RuntimeRootsNarrower {
		t.Fatal(notes)
	}
	if n := got["narrow_reordered"].(map[string]any)["narrowing"].([]any); len(n) != 0 {
		t.Fatal(n)
	}
}
