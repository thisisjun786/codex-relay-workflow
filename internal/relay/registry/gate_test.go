package registry

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// gateAnswers runs delivery.authorized_settings' Go port over testdata/gate_cases.json (each row
// written raw for an unbound task) and returns them with Python's.
func gateAnswers(t *testing.T) (map[string]any, map[string]any) {
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
				parent, pyDumps(c.Value, false), "raw", "t"); err != nil {
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
		got[c.Key] = answer
	}
	pyRaw, err := os.ReadFile("testdata/python_gate.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(pyRaw, &want); err != nil {
		t.Fatal(err)
	}
	for name, w := range want {
		if !reflect.DeepEqual(got[name], w) {
			g, _ := json.Marshal(got[name])
			p, _ := json.Marshal(w)
			t.Errorf("%s: go %s, python %s", name, g, p)
		}
	}
	return got, want
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
	_, want := gateAnswers(t)
	if refusedReason(want["unrecorded"]) != SettingsUnavailable || refusedReason(want["incomplete"]) != SettingsIncomplete {
		t.Fatal(want["unrecorded"], want["incomplete"])
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
	_, want := gateAnswers(t)
	for name, reason := range map[string]string{"mistyped_cwd": SettingsMistyped, "external": UnsupportedSandboxType,
		"untrusted": UnsupportedApprovalPolicy, "malformed_sandbox": UnsupportedSandboxType, "bare_sandbox": UnsupportedSandboxType} {
		if refusedReason(want[name]) != reason {
			t.Fatal(name, want[name])
		}
	}
}

// SPR-5: the ordinary path hands the recorded settings to the adapter: resume params carry the
// workspace-write mode, the recorded policy fields as config, and no approval policy.
func Test25_SPR5_the_ordinary_path_hands_the_settings_to_the_adapter(t *testing.T) {
	_, want := gateAnswers(t)
	params := obj(obj(want["ordinary"])["resumeParams"])
	if params["sandbox"] != "workspace-write" || params["approvalPolicy"] != nil {
		t.Fatal(params)
	}
	if obj(obj(want["on_request"])["settings"])["approvalPolicy"] != "on-request" {
		t.Fatal(want["on_request"])
	}
}

// SPR-9: the record rule and the response rule stay two facts: untrusted on the RECORD is a
// retryable pre-send withhold (and re-recording releases it); untrusted in the RESPONSE is the
// closed channel, decided alone as unsupported_approval_policy.
func Test25_SPR9_the_record_rule_and_the_response_rule_stay_two_facts(t *testing.T) {
	_, want := gateAnswers(t)
	if refusedReason(want["untrusted"]) != UnsupportedApprovalPolicy {
		t.Fatal(want["untrusted"])
	}
	settings := sameSettingsAsPython(t, "resp_approval_untrusted")
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

// SPR-10: every settings refusal a resume answers with is a completed pre-send refusal
// (withheld_pre_send, nothing sent, retry-safe, failed at thread/resume); an unrecognised code is
// not one. Compared with transport.classify_operation_receipt's answers.
func Test25_SPR10_every_settings_refusal_is_a_pre_send_refusal(t *testing.T) {
	raw, err := os.ReadFile("testdata/python_classify.json")
	if err != nil {
		t.Fatal(err)
	}
	var classified map[string]map[string]any
	if err := json.Unmarshal(raw, &classified); err != nil {
		t.Fatal(err)
	}
	for code, facts := range classified {
		preSend := facts["deliveryState"] == "withheld_pre_send" && facts["sendAttempted"] == "no" && facts["retrySafe"] == true && facts["failedOperation"] == "thread/resume"
		if code == "thread_busy" || code == UnsupportedApprovalPolicy {
			continue // their own transport answers (deferred_busy, inbox_only), not settings refusals
		}
		if IsPreSendSettingsRefusal(code) != preSend {
			t.Errorf("%s: go pre-send %v, python %v", code, IsPreSendSettingsRefusal(code), facts)
		}
	}
	if classified["unknown"]["deliveryState"] != "held_uncertain" || IsPreSendSettingsRefusal("unknown") {
		t.Fatal("an unrecognised code became a settings refusal")
	}
}

// SPR-12 (store half): a post-dispatch violation is annotated, never reclassified: the delivery
// row stays dispatched, the findings are readable afterwards (an unknown request reads none), and
// the annotation is one journal line beside it.
func Test25_SPR12_a_post_dispatch_violation_annotates_without_reclassifying(t *testing.T) {
	r := newRegistry(t)
	if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO deliveries (event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,created_at,updated_at)"+
		" VALUES ('e1','rel-x','completion_event',?,?,'dispatched',1,'t','t')", parent, parent); err != nil {
		t.Fatal(err)
	}
	findings := []any{contractObject{{Key: "code", Value: SettingsNotPreserved}, {Key: "field", Value: "sandbox"},
		{Key: "expected", Value: contractObject{{Key: "type", Value: "workspaceWrite"}}}, {Key: "returned", Value: contractObject{{Key: "type", Value: "dangerFullAccess"}}}}}
	if _, err := r.RecordSettingsViolation(ctx(), "del-000000000000-a1", "e1", findings); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := r.Store.DB.QueryRowContext(ctx(), "SELECT state FROM deliveries WHERE event_id='e1'").Scan(&state); err != nil || state != "dispatched" {
		t.Fatal(state, err)
	}
	stored, err := r.SettingsViolation(ctx(), "del-000000000000-a1")
	if err != nil {
		t.Fatal(err)
	}
	got := plain(t, stored).(map[string]any)
	if obj(got["findings"].([]any)[0])["code"] != SettingsNotPreserved || got["observedAt"] != fakeISO {
		t.Fatal(got)
	}
	if none, err := r.SettingsViolation(ctx(), "del-never-seen-a1"); err != nil || none != nil {
		t.Fatal(none, err)
	}
	var detail string
	if err := r.Store.DB.QueryRowContext(ctx(), "SELECT detail FROM journal WHERE kind='dispatch_settings_violation' AND subject='e1'").Scan(&detail); err != nil {
		t.Fatal(err)
	}
	want := `{"requestId": "del-000000000000-a1", "findings": [{"code": "settings_not_preserved", "field": "sandbox", "expected": {"type": "workspaceWrite"}, "returned": {"type": "dangerFullAccess"}}]}`
	if detail != want {
		t.Fatalf("journal detail %s", detail)
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
			parent, pyDumps(row, false), "raw", "t"); err != nil {
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

// CLI-23: where a command's own refusal falls relative to its store, against the live Python
// fence. A write form opens its store in cli.main's _ownership_preflight before its handler reads
// its arguments, so a usage refusal of its own arguments (settings-record's two exception flags)
// and a settings file that cannot be read (register's host error) leave the store it would have
// written - initialized, as any writer initializes an absent store (decision 30). A command line
// argparse cannot parse ends before cli.main and creates nothing. Each runtime starts from its
// own absent store; stdout, exit code and what the state directory holds must agree.
func Test25_CLI23_a_write_forms_own_refusal_comes_after_its_store(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
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
		goState, pyState := filepath.Join(home, "go"), filepath.Join(home, "python")
		var goOut, goErr bytes.Buffer
		goCode := Execute(ctx(), append([]string{"--state", goState}, c.argv...), &goOut, &goErr)
		// What Python printed, its exit and what it left in its own state directory, recorded
		// (pyoracle) with the directory spelled <STATE> and the home <HOME>.
		var py struct {
			Code   int      `json:"code"`
			Stdout string   `json:"stdout"`
			Names  []string `json:"names"`
		}
		pyoracle.JSON(t, fmt.Sprintf("case-%d %s", i, c.argv[0]), &py, func() (any, error) {
			oracle := exec.Command(filepath.Join(root, ".venv/bin/python"), append([]string{"-m", "codex_session_relay.cli", "--state", pyState}, c.argv...)...)
			oracle.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xs", "XDG_CONFIG_HOME="+home+"/xc", "CODEX_HOME="+home+"/ch")
			var pyOut bytes.Buffer
			oracle.Stdout = &pyOut
			pyCode := 0
			if err := oracle.Run(); err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					return nil, err
				}
				pyCode = exit.ExitCode()
			}
			return map[string]any{"code": pyCode, "stdout": strings.ReplaceAll(pyOut.String(), pyState, "<STATE>"), "names": listing(pyState)}, nil
		}, pyoracle.Substitute(home, "<HOME>"))
		goStdout := strings.ReplaceAll(goOut.String(), goState, "<STATE>")
		pyCode, pyStdout := py.Code, py.Stdout
		if goCode != pyCode || goStdout != pyStdout {
			t.Errorf("%v: go exit %d %q, python exit %d %q", c.argv[0], goCode, goStdout, pyCode, pyStdout)
		}
		goNames, pyNames := listing(goState), py.Names
		if !reflect.DeepEqual(goNames, pyNames) || (goNames != nil) != c.created {
			t.Errorf("%v: go left %v, python left %v; a store expected: %t", c.argv[0], goNames, pyNames, c.created)
		}
	}
}
