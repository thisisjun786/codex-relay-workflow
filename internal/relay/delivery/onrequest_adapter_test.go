package delivery

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// ORD-6..ORD-9. Python drives its REAL adapter (the pinned bridge's guarded send over a fake
// RPC endpoint) and its real supervisor channel; Go asserts the delivery-layer decisions those
// paths rest on and compares them with Python's receipts:
//   - the resume the guarded send transmits (ResumeParams / the settings-free form),
//   - the verification between resume and turn/start (VerifyResume: refusal, findings, notes),
//   - the classification of the receipt (Classify), and
//   - the chronology rule a folded turn is judged by (certainlyBefore, turn_predates_send).
// The wire sequence of the adapter is todo 28's and the channel's record is todo 24's.

func runOrdAdapter(t *testing.T, mode string) map[string]any {
	t.Helper()
	// The supervisor channel's message id is derived from the fixture's paths: a fixed tree.
	home, tree := t.TempDir(), parityTree(t)
	out := pyAnswer(t, "ordadapter "+mode, func() ([]byte, error) {
		root := repoRoot(t)
		script, _ := filepath.Abs("testdata/ordadapter.py")
		cmd := exec.Command("uv", "run", "--no-sync", "python", script, tree, mode)
		cmd.Dir = filepath.Join(root, "packages", "codex-session-relay")
		cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR="+home, "PYTHONPATH="+filepath.Join(root, "packages", "codex-session-relay", "src")+":"+filepath.Join(root, "packages", "codex-session-relay"))
		out, err := pythonOutput(cmd)
		if err != nil {
			return nil, fmt.Errorf("python %s: %w", mode, err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		return []byte(lines[len(lines)-1]), nil
	}, pyoracle.Substitute(tree, "<tree>"), pyoracle.Substitute(home, "<home>"))
	var got map[string]any
	mustDo(t, json.Unmarshal(out, &got))
	return got
}

func toObj(t *testing.T, v any) any {
	raw, err := json.Marshal(v)
	mustDo(t, err)
	o, err := loads(string(raw))
	mustDo(t, err)
	return o
}

func guarded(t *testing.T, python map[string]any) (Obj, []any) {
	settings := TaskSettings{Data: toObj(t, python["settings"]).(Obj), SettingsFreeResume: python["settingsFree"] == true}
	resumed := toObj(t, python["resumed"])
	params := settings.ResumeParams("thread-1")
	if settings.SettingsFreeResume {
		params = Obj{{Key: "threadId", Value: "thread-1"}, {Key: "excludeTurns", Value: true}}
	}
	requireSameJSON(t, "resume params", []any{params}, python["resumes"])
	if _, present := get(params, "approvalPolicy"); present {
		t.Fatal("the relay sent an approval policy")
	}
	rpcError, findings, notes := verifyResume(settings, resumed, "idle")
	receipt := Obj{{Key: "status", Value: Accepted}, {Key: "rpcError", Value: nil}, {Key: "settingsNotes", Value: nil}, {Key: "settingsFindings", Value: nil}, {Key: "error", Value: nil}, {Key: "statusBeforeResume", Value: "idle"}}
	methods := []any{"thread/read", "thread/resume"}
	if rpcError != nil {
		receipt = set(set(set(set(receipt, "status", FailedStatus), "rpcError", rpcError), "settingsFindings", findings), "error", "thread/resume: "+str(rpcError, "message"))
	} else {
		methods = append(methods, "turn/start")
		if len(notes) > 0 {
			receipt = set(receipt, "settingsNotes", notes)
		}
		receipt = set(receipt, "turnId", "fake-turn-1")
	}
	requireSameJSON(t, "receipt", without(receipt, "turnId"), python["receipt"])
	requireSameJSON(t, "methods", methods, python["methods"])
	full := append(Obj(nil), receipt...)
	full = set(full, "resumed", resumed)
	return full, methods
}

func TestORD06_an_on_request_thread_is_started_and_its_difference_noted(t *testing.T) {
	for _, route := range []string{"transmitted", "settings_free"} {
		t.Run(route, func(t *testing.T) {
			python := runOrdAdapter(t, route)
			receipt, _ := guarded(t, python)
			if f := Classify(receipt); f.DeliveryState != Dispatched || f.DeliveryState != python["delivery_state"] {
				t.Fatalf("classified %v", f)
			}
		})
	}
}

func TestORD07_untrusted_stays_stored_not_woken(t *testing.T) {
	python := runOrdAdapter(t, "untrusted")
	receipt, methods := guarded(t, python)
	if f := Classify(receipt); f.DeliveryState != InboxOnly || f.DeliveryState != python["delivery_state"] || len(methods) != 2 {
		t.Fatalf("classified %v", f)
	}
}

func TestORD08_a_supervisor_push_follows_the_same_policy_rule(t *testing.T) {
	// The channel's own record (withheld_pre_send carrying transportDeliveryState inbox_only)
	// is todo 24's; what it rests on here is the transport classification of the two answers.
	for _, tc := range []struct{ mode, policy, transport, channel string }{
		{"sup_on_request", "on-request", Dispatched, Dispatched},
		{"sup_untrusted", "untrusted", InboxOnly, WithheldPreSend},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			python := runOrdAdapter(t, tc.mode)
			record := python["record"].(map[string]any)
			settings := TaskSettings{Data: loadsObj(rawSettings("/supervisor", "never")), SettingsFreeResume: true}
			resumed := loadsObj(rawResume("/supervisor", tc.policy))
			rpcError, _, _ := verifyResume(settings, resumed, "idle")
			receipt := Obj{{Key: "status", Value: Accepted}, {Key: "resumed", Value: resumed}, {Key: "turnId", Value: "t"}}
			if rpcError != nil {
				receipt = Obj{{Key: "status", Value: FailedStatus}, {Key: "resumed", Value: resumed}, {Key: "rpcError", Value: rpcError}, {Key: "error", Value: "thread/resume: x"}}
			}
			state := Classify(receipt).DeliveryState
			transport := record["deliveryState"]
			if v, ok := record["transportDeliveryState"]; ok {
				transport = v
			}
			if state != tc.transport || transport != tc.transport || python["state"] != tc.channel {
				t.Fatalf("go %s python %v/%v", state, transport, python["state"])
			}
		})
	}
}

func rawResume(cwd, approval string) string {
	return dumps(Obj{{Key: "approvalPolicy", Value: approval},
		{Key: "sandbox", Value: Obj{{Key: "type", Value: "workspaceWrite"}, {Key: "writableRoots", Value: []any{}}, {Key: "networkAccess", Value: false}, {Key: "excludeTmpdirEnvVar", Value: false}, {Key: "excludeSlashTmp", Value: false}}},
		{Key: "cwd", Value: cwd}, {Key: "runtimeWorkspaceRoots", Value: []any{cwd}}, {Key: "model", Value: "anthropic/claude-opus-5"}, {Key: "reasoningEffort", Value: "xhigh"}, {Key: "activePermissionProfile", Value: nil},
		{Key: "thread", Value: Obj{{Key: "id", Value: "t"}, {Key: "environments", Value: []any{Obj{{Key: "environmentId", Value: "local"}, {Key: "cwd", Value: cwd}, {Key: "runtimeWorkspaceRoots", Value: []any{cwd}}}}}}}})
}

// TurnPredatesSend is supervisorchannel.TURN_PREDATES_SEND, the readback answer for a turn that
// began before the send (the channel is todo 24's; the word and the chronology rule are shared).
const TurnPredatesSend = "turn_predates_send"

func TestORD09_a_push_folded_into_a_running_turn_is_not_a_completion(t *testing.T) {
	python := runOrdAdapter(t, "folded")
	started := python["turnStartedAt"].(float64)
	if !certainlyBefore(started, python["sentAt"].(string)) || python["verified"] != TurnPredatesSend {
		t.Fatalf("the folded turn predates the send: python %v", python)
	}
	record := python["record"].(map[string]any)
	if python["state"] != Dispatched || record["deliveryState"] != Dispatched || python["resent"] != float64(0) {
		t.Fatalf("stays dispatched and sends nothing more: %v", python)
	}
}

// verifyResume is the guarded send's check between thread/resume and turn/start as the bridge
// adapter runs it (internal/relay/adapter settings.go): registry's recorded-settings predicate,
// the refusal the receipt carries, or the notes an accepted send carries.
func verifyResume(settings TaskSettings, resumed any, statusBefore string) (Obj, []any, []any) {
	transmitted := !settings.SettingsFreeResume
	recorded := registry.TaskSettings{Data: settings.Data}
	findings := []any{}
	for _, finding := range recorded.Mismatches(resumed, transmitted, false, transmitted && statusBefore == "idle") {
		findings = append(findings, finding)
	}
	if len(findings) > 0 {
		first := findings[0].(Obj)
		code := str(first, "code")
		expected, _ := get(first, "expected")
		returned, _ := get(first, "returned")
		message := code + ": " + str(first, "field") + " returned " + evidence.Repr(returned) + "; message withheld"
		if !transmitted {
			if code == registry.SettingsNotPreserved {
				code = registry.SettingsDifferAfterLoad
			}
			message = code + ": " + str(first, "field") + " is " + evidence.Repr(returned) + " on the loaded thread and " + evidence.Repr(expected) + " in the record; nothing was transmitted and no turn was started"
		}
		return Obj{{Key: "code", Value: code}, {Key: "message", Value: message}}, findings, nil
	}
	var notes []any
	if note := settings.ApprovalDivergence(resumed); note != nil {
		notes = append(notes, note)
	}
	for _, note := range recorded.RootsNarrowing(resumed, statusBefore) {
		notes = append(notes, note)
	}
	return nil, nil, notes
}

func without(o Obj, key string) Obj {
	var out Obj
	for _, f := range o {
		if f.Key != key {
			out = append(out, f)
		}
	}
	return out
}
