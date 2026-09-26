package registry

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// settingsAnswers runs every row of testdata/settings_cases.json through TaskSettings and
// returns the answers in the shape gen_settings.py records from Python.
func settingsAnswers(t *testing.T) (map[string]any, map[string]any) {
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
		rowValue, _ := getField(c, "row")
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
		if response, ok := getField(c, "response"); ok {
			transmitted, loaded := true, false
			if v, ok := getField(c, "transmitted"); ok {
				transmitted = v.(bool)
			}
			if v, ok := getField(c, "loadedBefore"); ok {
				loaded = v.(bool)
			}
			found := settings.Mismatches(response, transmitted, false, loaded)
			list := make([]any, len(found))
			for i, f := range found {
				list[i] = f
			}
			answer = append(answer, contract.Field{Key: "mismatches", Value: list})
			if v, ok := getField(c, "narrowing"); ok && v == true {
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
	pyRaw, err := os.ReadFile("testdata/python_settings.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(pyRaw, &want); err != nil {
		t.Fatal(err)
	}
	return got, want
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

// sameSettingsAsPython compares the named cases (every case with prefix) whole with Python.
func sameSettingsAsPython(t *testing.T, prefixes ...string) map[string]any {
	t.Helper()
	got, want := settingsAnswers(t)
	var names []string
	for name := range want {
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
		if !reflect.DeepEqual(got[name], want[name]) {
			g, _ := json.Marshal(got[name])
			w, _ := json.Marshal(want[name])
			t.Errorf("%s differs from Python\n go: %s\n py: %s", name, g, w)
		}
	}
	return want
}

func usableOf(answer any) map[string]any {
	u, _ := answer.(map[string]any)["usable"].(map[string]any)
	return u
}

func Test25_SPR2_completeness_environments_empty_is_a_decision_and_absence_is_decided_first(t *testing.T) {
	want := sameSettingsAsPython(t, "environments_", "absent_before_mistyped", "approval_null", "sandbox_null", "sandbox_omitted", "complete")
	if usableOf(want["environments_empty"]) != nil || usableOf(want["absent_before_mistyped"])["detail"] != "missing cwd" {
		t.Fatal("python table changed")
	}
}

func Test25_SPR3_every_mistyped_string_field_is_named_in_one_refusal(t *testing.T) {
	want := sameSettingsAsPython(t, "mistyped_", "roots_string", "env_bad")
	if usableOf(want["mistyped_all"])["detail"] != "cwd is int, not str; model is bool, not str; reasoningEffort is list, not str" {
		t.Fatal(want["mistyped_all"])
	}
}

func Test25_SPR6_only_never_and_on_request_are_carriable_approval_policies(t *testing.T) {
	want := sameSettingsAsPython(t, "approval_")
	for _, name := range []string{"approval_0", "approval_1", "approval_2", "approval_3", "approval_4"} {
		if usableOf(want[name])["reason"] != UnsupportedApprovalPolicy {
			t.Fatal(name)
		}
	}
}

func Test25_SPR7_the_gates_decide_in_one_order(t *testing.T) {
	want := sameSettingsAsPython(t, "ladder_")
	for i, reason := range []string{SettingsIncomplete, SettingsMistyped, UnsupportedApprovalPolicy, UnsupportedSandboxType} {
		if usableOf(want["ladder_"+string(rune('0'+i))])["reason"] != reason {
			t.Fatalf("ladder %d", i)
		}
	}
}

func Test25_SPR11_approval_policy_in_the_response_is_decided_first_and_alone(t *testing.T) {
	want := sameSettingsAsPython(t, "resp_approval_", "resp_not_object")
	for name, code := range map[string]string{"resp_approval_absent": SettingUnobservable, "resp_approval_untrusted": UnsupportedApprovalPolicy} {
		found := want[name].(map[string]any)["mismatches"].([]any)
		if len(found) != 1 || found[0].(map[string]any)["code"] != code {
			t.Fatalf("%s: %v", name, found)
		}
	}
}

func Test25_SPR14_two_unreadable_sandbox_policies_never_agree(t *testing.T) {
	want := sameSettingsAsPython(t, "sandbox_malformed")
	found := want["sandbox_malformed"].(map[string]any)["mismatches"].([]any)
	if first := found[0].(map[string]any); first["field"] != "sandbox" || first["code"] != SettingsNotPreserved {
		t.Fatal(found)
	}
}

func Test25_SPR15_a_sandbox_that_is_not_a_readable_policy_is_unsupported_with_its_exact_detail(t *testing.T) {
	want := sameSettingsAsPython(t, "sandbox_kind_", "sandbox_type_")
	if usableOf(want["sandbox_kind_0"])["detail"] != "the recorded sandbox is str, not the policy object a creation result reports, so it does not record the full policy a resume would have to restore" {
		t.Fatal(want["sandbox_kind_0"])
	}
}

func Test25_SPR16_a_readable_policy_is_unchanged_and_external_sandbox_is_named_exactly(t *testing.T) {
	want := sameSettingsAsPython(t, "complete", "sandbox_external", "sandbox_readonly")
	if usableOf(want["sandbox_external"])["detail"] != "'externalSandbox' has no ThreadResumeParams.sandbox mode, so it cannot be restored on a resume" {
		t.Fatal(want["sandbox_external"])
	}
	if want["complete"].(map[string]any)["resumeParams"].(map[string]any)["sandbox"] != "workspace-write" {
		t.Fatal("resume mode")
	}
}

// QA failure scenario of todo 25 and the comparison half of SPR-10/CLI-35: a response that
// widens the writable roots, the sandbox, or any field a send carries is settings_not_preserved,
// in Python's order; narrower roots pass only after a load that transmitted nothing or a loaded
// recipient.
func Test25_QA_widened_writable_roots_are_settings_not_preserved_in_pythons_order(t *testing.T) {
	want := sameSettingsAsPython(t, "resp_")
	found := want["resp_widened_roots"].(map[string]any)["mismatches"].([]any)
	if len(found) == 0 || found[0].(map[string]any)["code"] != SettingsNotPreserved {
		t.Fatal(found)
	}
	if n := want["resp_narrower_loaded"].(map[string]any)["mismatches"].([]any); len(n) != 0 {
		t.Fatal(n)
	}
}

// SHN-11: an accepted narrowing is noted once per place where the reported SET is a strict
// subset of the record (a reordering is no narrowing), with the recipient's prior status.
func Test25_SHN11_an_accepted_narrowing_is_noted_where_the_set_shrank(t *testing.T) {
	want := sameSettingsAsPython(t, "narrow_")
	notes := want["narrow_all"].(map[string]any)["narrowing"].([]any)
	if len(notes) != 3 || notes[0].(map[string]any)["code"] != RuntimeRootsNarrower {
		t.Fatal(notes)
	}
	if n := want["narrow_reordered"].(map[string]any)["narrowing"].([]any); len(n) != 0 {
		t.Fatal(n)
	}
}
