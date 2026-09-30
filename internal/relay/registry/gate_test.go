package registry

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// gateAnswers runs delivery.authorized_settings' Go port over testdata/gate_cases.json (each row
// written raw for an unbound task), compares each answer whole with the golden stored under the
// case's name, and returns them.
func gateAnswers(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/gate_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]any{}
	for _, c := range decoded.(contract.OrderedObject) {
		r := newRegistry(t)
		if c.Value != nil {
			if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)",
				parent, pyjson.Dumps(c.Value, pyjson.Options{}), "raw", "t"); err != nil {
				t.Fatal(err)
			}
		}
		settings, free, err := r.AuthorizedSettings(ctx(), parent)
		var answer any
		if err != nil {
			answer = outcome(t, nil, err)
		} else {
			answer = plain(t, contract.OrderedObject{{Key: "settings", Value: settings.Data}, {Key: "settingsFree", Value: free},
				{Key: "resumeParams", Value: settings.ResumeParams("t-1")}})
		}
		golden.CheckJSON(t, c.Key, answer)
		got[c.Key] = answer
	}
	return got
}

func refusedReason(answer any) string {
	r, _ := obj(answer)["refused"].(map[string]any)
	s, _ := r["reason"].(string)
	return s
}

// SPR-1: an unrecorded recipient is withheld before any transport call with a reason naming
// what is missing (settings_unavailable; settings_incomplete "environments"), and recording the
// settings later makes the same recipient eligible.
func Test25_SPR1_settings_are_established_before_any_send(t *testing.T) {
	got := gateAnswers(t)
	if refusedReason(got["unrecorded"]) != SettingsUnavailable || refusedReason(got["incomplete"]) != SettingsIncomplete {
		t.Fatal(got["unrecorded"], got["incomplete"])
	}
	r := newRegistry(t)
	if _, _, err := r.AuthorizedSettings(ctx(), parent); refusalReason(err) != SettingsUnavailable {
		t.Fatal(err)
	}
	if _, err := r.RecordSettings(ctx(), parent, settingsFixture("/parent"), "creation_result", "", Citation{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.AuthorizedSettings(ctx(), parent); err != nil {
		t.Fatalf("recording the settings did not release the withhold: %v", err)
	}
}

// SPR-4: a record refused at the settings gate withholds before any claim, with the reason and
// detail a parent reads (mistyped, a sandbox with no resume mode, an untrusted policy, an
// unreadable policy, a bare sandbox string).
func Test25_SPR4_a_record_refused_at_the_gate_withholds_with_its_reason(t *testing.T) {
	got := gateAnswers(t)
	for name, reason := range map[string]string{"mistyped_cwd": SettingsMistyped, "external": UnsupportedSandboxType,
		"untrusted": UnsupportedApprovalPolicy, "malformed_sandbox": UnsupportedSandboxType, "bare_sandbox": UnsupportedSandboxType} {
		if refusedReason(got[name]) != reason {
			t.Fatal(name, got[name])
		}
	}
}

// SPR-5: the ordinary path hands the recorded settings to the adapter: resume params carry the
// workspace-write mode, the recorded policy fields as config, and no approval policy.
func Test25_SPR5_the_ordinary_path_hands_the_settings_to_the_adapter(t *testing.T) {
	got := gateAnswers(t)
	params := obj(obj(got["ordinary"])["resumeParams"])
	if params["sandbox"] != "workspace-write" || params["approvalPolicy"] != nil {
		t.Fatal(params)
	}
	if obj(obj(got["on_request"])["settings"])["approvalPolicy"] != "on-request" {
		t.Fatal(got["on_request"])
	}
}

// SPR-9: the record rule and the response rule stay two facts: untrusted on the RECORD is a
// retryable pre-send withhold (and re-recording releases it); untrusted in the RESPONSE is the
// closed channel, decided alone as unsupported_approval_policy.
func Test25_SPR9_the_record_rule_and_the_response_rule_stay_two_facts(t *testing.T) {
	got := gateAnswers(t)
	if refusedReason(got["untrusted"]) != UnsupportedApprovalPolicy {
		t.Fatal(got["untrusted"])
	}
	settings := sameSettingsAsGolden(t, "resp_approval_untrusted")
	found := obj(settings["resp_approval_untrusted"])["mismatches"].([]any)
	if len(found) != 1 || obj(found[0])["returned"] != "untrusted" {
		t.Fatal(found)
	}
	r := newRegistry(t)
	if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)",
		parent, `{"approvalPolicy": "untrusted"}`, "raw", "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordSettings(ctx(), parent, settingsFixture("/parent"), "creation_result", "", Citation{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.AuthorizedSettings(ctx(), parent); err != nil {
		t.Fatalf("re-recording did not release the record withhold: %v", err)
	}
}

// CLI-35: every recorded field a send transforms or constrains, mutated in the stored row, makes
// the recipient not deliverable (settings-show deliverable false, and the gate refuses it); a
// constraint mutant is refused naming the field and the value. Python derives the field set
// from its own source by AST (CLI-40, not ported); this is that derived set as a fixed table.
func Test25_CLI35_every_field_a_send_transforms_or_constrains_is_covered(t *testing.T) {
	transformations := []string{"sandbox", "cwd", "model", "reasoningEffort", "runtimeWorkspaceRoots", "environments"}
	constraints := map[string][]string{"approvalPolicy": CarriedApprovalPolicies}
	probe := func(field string, value any) (contractObject, error) {
		r := newRegistry(t)
		row := setField(copyObject(settingsFixture("/parent")), field, value)
		if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO authorized_settings (task_id, settings, source, recorded_at) VALUES (?,?,?,?)",
			parent, pyjson.Dumps(row, pyjson.Options{}), "raw", "t"); err != nil {
			t.Fatal(err)
		}
		shown, err := r.SettingsShow(ctx(), parent)
		if err != nil {
			t.Fatal(err)
		}
		_, _, gate := r.AuthorizedSettings(ctx(), parent)
		return shown, gate
	}
	for _, field := range transformations {
		shown, gate := probe(field, 7)
		if d, _ := getField(shown, "deliverable"); d != false || gate == nil {
			t.Errorf("transformation %s mutated to 7 is still deliverable", field)
		}
		if _, ok := getField(shown, "refusedIfPreserved"); ok {
			t.Errorf("%s: refusedIfPreserved key present", field)
		}
	}
	for field, allowed := range constraints {
		for _, literal := range allowed {
			mutant := "not-" + literal
			shown, gate := probe(field, mutant)
			if d, _ := getField(shown, "deliverable"); d != false || gate == nil {
				t.Fatalf("constraint %s=%s is deliverable", field, mutant)
			}
			if !strings.Contains(gate.Error(), field) || !strings.Contains(gate.Error(), mutant) {
				t.Errorf("constraint refusal does not name %s and %s: %v", field, mutant, gate)
			}
		}
	}
	// The record rule and the response rule constrain the same set.
	for _, policy := range CarriedApprovalPolicies {
		if found := (TaskSettings{settingsFixture("/parent")}).Mismatches(setField(copyObject(responseFor(settingsFixture("/parent"))), "approvalPolicy", policy), true, false, false); len(found) != 0 {
			t.Errorf("response policy %s refused: %v", policy, found)
		}
	}
}

func responseFor(row contractObject) contractObject {
	get := func(k string) any { v, _ := getField(row, k); return v }
	return contractObject{{Key: "approvalPolicy", Value: get("approvalPolicy")}, {Key: "sandbox", Value: get("sandbox")}, {Key: "cwd", Value: get("cwd")},
		{Key: "runtimeWorkspaceRoots", Value: get("runtimeWorkspaceRoots")}, {Key: "model", Value: get("model")},
		{Key: "reasoningEffort", Value: get("reasoningEffort")}, {Key: "thread", Value: contractObject{{Key: "environments", Value: get("environments")}}}}
}

// CLI-23: where a command's own refusal falls relative to its store, as the Python fence placed
// it (the golden began as its answer). A write form opens its store in cli.main's
// _ownership_preflight before its handler reads its arguments, so a usage refusal of its own arguments (settings-record's two exception flags)
// and a settings file that cannot be read (register's host error) leave the store it would have
// written - initialized, as any writer initializes an absent store (decision 30). A command line
// argparse cannot parse ends before cli.main and creates nothing. The command starts from an
// absent store; stdout, exit code and what the state directory holds are compared with the golden.
func Test25_CLI23_a_write_forms_own_refusal_comes_after_its_store(t *testing.T) {
	listing := func(state string) []string {
		entries, err := os.ReadDir(state)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names
	}
	for i, c := range []struct {
		argv    []string
		created bool
	}{
		{[]string{"settings-record", "--task", parent, "--settings", "{}", "--exception", "x", "--clear-exception"}, true},
		{[]string{"register", "--parent-task", parent}, false},
		{[]string{"register", "--parent-task", parent, "--parent-host", host, "--child-task", child, "--child-host", host, "--issue", issue,
			"--artifact-root", "/w", "--allowed-recipient", parent, "--dispatch-request-id", "d", "--parent-settings", "@/nonexistent/settings.json"}, true},
	} {
		home := t.TempDir()
		goState := filepath.Join(home, "go")
		var goOut, goErr bytes.Buffer
		goCode := Execute(ctx(), append([]string{"--state", goState}, c.argv...), &goOut, &goErr)
		goStdout := strings.ReplaceAll(goOut.String(), goState, "<STATE>")
		goNames := listing(goState)
		// The exit, stdout (the state directory spelled <STATE>) and the names the state
		// directory holds, compared whole with the golden.
		golden.CheckJSON(t, fmt.Sprintf("case-%d %s", i, c.argv[0]), map[string]any{"code": goCode, "stdout": goStdout, "names": goNames},
			golden.Substitute(home, "<HOME>"))
		if (goNames != nil) != c.created {
			t.Errorf("%v: go left %v; a store expected: %t", c.argv[0], goNames, c.created)
		}
	}
}
