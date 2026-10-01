package delivery

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// ORD-6..ORD-9. The Python test drove its REAL adapter (the pinned bridge's guarded send over a
// fake RPC endpoint) and its real supervisor channel; what the endpoint answered (the recorded
// settings and the resumed thread, testdata/fixtures/ordadapter.json) is the input here, and the
// delivery-layer decisions those paths rest on are checked against the golden, which began as the
// Python receipts:
//   - the resume the guarded send transmits (ResumeParams / the settings-free form),
//   - the verification between resume and turn/start (VerifyResume: refusal, findings, notes),
//   - the classification of the receipt (Classify), and
//   - the chronology rule a folded turn is judged by (certainlyBefore, turn_predates_send).
// The wire sequence of the adapter is todo 28's and the channel's record is todo 24's.

// ordAdapter is the named case of the fixture.
func ordAdapter(t *testing.T, mode string) map[string]any {
	t.Helper()
	var cases map[string]map[string]any
	mustDo(t, json.Unmarshal(golden.Fixture(t, "ordadapter.json"), &cases))
	c, ok := cases[mode]
	if !ok {
		t.Fatalf("no case %s in ordadapter.json", mode)
	}
	return c
}

func toObj(t *testing.T, v any) any {
	raw, err := json.Marshal(v)
	mustDo(t, err)
	o, err := loads(string(raw))
	mustDo(t, err)
	return o
}

func guarded(t *testing.T, input map[string]any) (Obj, []any) {
	settings := TaskSettings{Data: toObj(t, input["settings"]).(Obj), SettingsFreeResume: input["settingsFree"] == true}
	resumed := toObj(t, input["resumed"])
	params := settings.ResumeParams("thread-1")
	if settings.SettingsFreeResume {
		params = Obj{{Key: "threadId", Value: "thread-1"}, {Key: "excludeTurns", Value: true}}
	}
	golden.CheckJSON(t, "resumes", normalizeJSON(t, jsonable([]any{params})))
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
	golden.CheckJSON(t, "receipt", normalizeJSON(t, jsonable(without(receipt, "turnId"))))
	golden.CheckJSON(t, "methods", methods)
	full := append(Obj(nil), receipt...)
	full = set(full, "resumed", resumed)
	return full, methods
}

func TestORD06_an_on_request_thread_is_started_and_its_difference_noted(t *testing.T) {
	for _, route := range []string{"transmitted", "settings_free"} {
		t.Run(route, func(t *testing.T) {
			receipt, _ := guarded(t, ordAdapter(t, route))
			golden.CheckJSON(t, "delivery_state", Classify(receipt).DeliveryState)
			if f := Classify(receipt); f.DeliveryState != Dispatched {
				t.Fatalf("classified %v", f)
			}
		})
	}
}

func TestORD07_untrusted_stays_stored_not_woken(t *testing.T) {
	receipt, methods := guarded(t, ordAdapter(t, "untrusted"))
	golden.CheckJSON(t, "delivery_state", Classify(receipt).DeliveryState)
	if f := Classify(receipt); f.DeliveryState != InboxOnly || len(methods) != 2 {
		t.Fatalf("classified %v", f)
	}
}

func TestORD08_a_supervisor_push_follows_the_same_policy_rule(t *testing.T) {
	// The channel's own record (withheld_pre_send carrying transportDeliveryState inbox_only)
	// is todo 24's; what it rests on here is the transport classification of the two answers.
	for _, tc := range []struct{ mode, policy, transport string }{
		{"sup_on_request", "on-request", Dispatched},
		{"sup_untrusted", "untrusted", InboxOnly},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			settings := TaskSettings{Data: loadsObj(rawSettings("/supervisor", "never")), SettingsFreeResume: true}
			resumed := loadsObj(rawResume("/supervisor", tc.policy))
			rpcError, _, _ := verifyResume(settings, resumed, "idle")
			receipt := Obj{{Key: "status", Value: Accepted}, {Key: "resumed", Value: resumed}, {Key: "turnId", Value: "t"}}
			if rpcError != nil {
				receipt = Obj{{Key: "status", Value: FailedStatus}, {Key: "resumed", Value: resumed}, {Key: "rpcError", Value: rpcError}, {Key: "error", Value: "thread/resume: x"}}
			}
			state := Classify(receipt).DeliveryState
			golden.CheckJSON(t, "transport", state)
			if state != tc.transport {
				t.Fatalf("classified %s", state)
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
	// The readback of the folded push (turn_predates_send, the message staying dispatched and
	// nothing resent) is the supervisor channel's, todo 24's; the chronology rule it rests on is
	// judged here on the start and send times the Python channel read.
	folded := ordAdapter(t, "folded")
	if !certainlyBefore(folded["turnStartedAt"].(float64), folded["sentAt"].(string)) {
		t.Fatalf("the folded turn predates the send: %v", folded)
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
		message := code + ": " + str(first, "field") + " returned " + pyvalue.Repr(returned) + "; message withheld"
		if !transmitted {
			if code == registry.SettingsNotPreserved {
				code = registry.SettingsDifferAfterLoad
			}
			message = code + ": " + str(first, "field") + " is " + pyvalue.Repr(returned) + " on the loaded thread and " + pyvalue.Repr(expected) + " in the record; nothing was transmitted and no turn was started"
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

// SPR-10: every settings refusal a resume answers with is a completed pre-send refusal
// (withheld_pre_send, nothing sent, retry-safe, failed at thread/resume); an unrecognised code is
// not one. The facts of a failed receipt whose resume was answered and whose error carries the
// code are checked against the golden, which began as transport.classify_operation_receipt's.
func Test25_SPR10_every_settings_refusal_is_a_pre_send_refusal(t *testing.T) {
	codes := append(slices.Clone(SettingsRefusals), "thread_busy", "unknown", "unsupported_approval_policy")
	classified := map[string]any{}
	for _, code := range codes {
		receipt := Obj{{Key: "status", Value: FailedStatus}, {Key: "rpcError", Value: Obj{{Key: "code", Value: code}}},
			{Key: "resumed", Value: Obj{{Key: "approvalPolicy", Value: "never"}}}}
		got := Classify(receipt)
		classified[code] = map[string]any{"deliveryState": got.DeliveryState, "sendAttempted": got.SendAttempted, "retrySafe": got.RetrySafe, "failedOperation": got.FailedOperation}
		if preSend, listed := got.DeliveryState == WithheldPreSend, slices.Contains(SettingsRefusals, code); preSend != listed {
			t.Errorf("%s: withheld pre-send %v, listed as a settings refusal %v", code, preSend, listed)
		}
	}
	golden.CheckJSON(t, "classified", classified)
}
