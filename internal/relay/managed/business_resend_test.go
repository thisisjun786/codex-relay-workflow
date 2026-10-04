package managed

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// Synthetic version of the copied legacy receipt: the adapter stored a resume
// settings refusal without attemptedEffects/delivery, before turn/start.
func businessResendLegacyReceipt(thread string) map[string]any {
	return map[string]any{"operation": "send_message_to_thread", "status": "failed", "threadId": thread, "retrySafe": false,
		"statusBeforeResume": "idle", "mcpOverridesTransmitted": true,
		"error":    "thread/resume: settings_not_preserved: mcpServers returned different settings; message withheld",
		"rpcError": map[string]any{"code": "settings_not_preserved", "message": "mcpServers returned different settings; message withheld"}}
}

func TestBusinessResendNeverRepeatsPossibleDelivery(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"unknown", func(r map[string]any) { r["status"] = "outcome_unknown" }},
		{"in-flight", func(r map[string]any) { r["status"] = "in_progress_or_unknown" }},
		{"turn-id", func(r map[string]any) { r["turnId"] = "business" }},
		{"started", func(r map[string]any) { r["delivery"] = "turn_started" }},
		{"rejected", func(r map[string]any) { r["delivery"] = "rejected" }},
		{"unknown-delivery", func(r map[string]any) { r["delivery"] = "outcome_unknown" }},
		{"turn-start", func(r map[string]any) { r["attemptedEffects"] = []any{"thread/resume", "turn/start"} }},
		{"contradictory-not-attempted", func(r map[string]any) { r["status"] = "not_attempted"; r["attemptedEffects"] = []any{"turn/start"} }},
		{"turn-error", func(r map[string]any) { r["error"] = "turn/start: settings_not_preserved" }},
		{"different-thread", func(r map[string]any) { r["threadId"] = "other" }},
		{"different-operation", func(r map[string]any) { r["operation"] = "create_thread" }},
		{"no-structured-refusal", func(r map[string]any) { delete(r, "rpcError") }},
		{"arbitrary-error", func(r map[string]any) { r["error"] = "settings_not_preserved" }},
		{"malformed-effects", func(r map[string]any) { r["attemptedEffects"] = []any{42} }},
		{"null-effects", func(r map[string]any) { r["attemptedEffects"] = nil }},
		{"label-without-trace", func(r map[string]any) { r["delivery"] = "not_delivered" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			k, _, failure := businessResendKit(t)
			scenario.change(failure)
			for range 2 {
				if got := k.run(); got["state"] == "admitted" {
					t.Fatal("unsafe record admitted")
				}
			}
			if k.host.sent != 0 || len(k.host.sends) != 0 {
				t.Fatal("possible delivery was resent")
			}
		})
	}
}

func TestBusinessResendModernTraceAndBound(t *testing.T) {
	for _, effects := range []any{[]any{"thread/resume"}, []string{"thread/resume"}} {
		k, business, failure := businessResendKit(t)
		failure["attemptedEffects"], failure["delivery"] = effects, "not_delivered"
		delete(failure, "rpcError")
		for attempt := 1; attempt < maxBusinessResendAttempts-1; attempt++ {
			k.host.operations[businessResendID("managed-1", business, attempt)] = businessResendLegacyReceipt("t-1")
		}
		if got := k.run(); got["state"] != "admitted" || k.host.sent != 1 || k.host.sends[0] != businessResendID("managed-1", business, 2) {
			t.Fatal("last bounded attempt did not recover")
		}
	}
	k, business, _ := businessResendKit(t)
	for attempt := 1; attempt < maxBusinessResendAttempts; attempt++ {
		k.host.operations[businessResendID("managed-1", business, attempt)] = businessResendLegacyReceipt("t-1")
	}
	for range 2 {
		k.expect(k.run(), "incomplete", "business_failed", "")
	}
	if k.host.sent != 0 || len(k.host.operations) != 4 {
		t.Fatal("exhausted operation chain changed")
	}
	// An unknown successor stops the chain, even with a safe original failure.
	k.host.operations[businessResendID("managed-1", business, 1)]["status"] = "outcome_unknown"
	k.expect(k.run(), "incomplete", "business_outcome_unknown", "")
	if k.host.sent != 0 {
		t.Fatal("unknown successor skipped")
	}
}

type businessResendObservationApp struct {
	Adapter
	list func() (map[string]any, error)
}

func (a *businessResendObservationApp) HostCall(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if method == "thread/turns/list" {
		return a.list()
	}
	return a.Adapter.HostCall(ctx, method, params)
}

func TestBusinessResendHostHistoryWithholds(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		rows   any
		cursor any
		err    error
		reason string
	}{
		{"another", []any{map[string]any{"id": "standby"}, map[string]any{"id": "other"}}, nil, nil, "business_identity_unobserved"},
		{"wrong-anchor", []any{map[string]any{"id": "other"}}, nil, nil, "business_identity_unobserved"},
		{"empty", []any{}, nil, nil, "lifecycle_unknown"},
		{"malformed", "bad", nil, nil, "lifecycle_unknown"},
		{"missing-id", []any{map[string]any{}}, nil, nil, "lifecycle_unknown"},
		{"more", []any{map[string]any{"id": "standby"}}, "more", nil, "lifecycle_unknown"},
		{"bad-cursor", []any{map[string]any{"id": "standby"}}, 3, nil, "lifecycle_unknown"},
		{"read-error", nil, nil, errors.New("observation failed"), "lifecycle_unknown"},
		{"no-rollout", nil, nil, errors.New("thread/turns/list: no rollout found"), "lifecycle_unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			k, _, _ := businessResendKit(t)
			k.start.Adapter = &businessResendObservationApp{Adapter: k.host, list: func() (map[string]any, error) {
				return map[string]any{"data": scenario.rows, "nextCursor": scenario.cursor}, scenario.err
			}}
			if got := k.run(); got["reason"] != scenario.reason {
				t.Fatalf("history refusal: %v", got)
			}
			if len(k.host.sends) != 0 {
				t.Fatal("unobserved/foreign history consumed operation")
			}
		})
	}
}

func TestBusinessResendLoadObservationIsLast(t *testing.T) {
	k, business, _ := businessResendKit(t)
	k.start.Adapter = &businessResendObservationApp{Adapter: k.host, list: func() (map[string]any, error) {
		k.host.threads["t-1"].status = "idle"
		return map[string]any{"data": []any{map[string]any{"id": "standby"}}}, nil
	}}
	for range 2 {
		k.expect(k.run(), "incomplete", "recipient_not_idle", "")
	}
	if len(k.host.sends) != 0 || k.host.operations[businessResendID("managed-1", business, 1)] != nil {
		t.Fatal("loaded observation consumed retry")
	}
	k.start.Adapter = k.host
	k.host.threads["t-1"].status = "notLoaded"
	if got := k.run(); got["state"] != "admitted" {
		t.Fatal("unloaded child did not recover")
	}
}

func TestBusinessResendRechecksHistoryJustBeforeStart(t *testing.T) {
	k, _, _ := businessResendKit(t)
	k.host.beforeSend = func(SendRequest) {
		k.host.threads["t-1"].turns = append(k.host.threads["t-1"].turns, "foreign-completed")
		k.host.threads["t-1"].status = "idle"
	}
	k.expect(k.run(), "incomplete", "business_failed", "")
	if k.host.sent != 0 {
		t.Fatal("final history guard bypassed")
	}
}

func TestBusinessResendRetainsAcceptedAttemptAfterAdmissionLoss(t *testing.T) {
	k, business, _ := businessResendKit(t)
	retry := businessResendID("managed-1", business, 1)
	k.host.operations[retry] = map[string]any{"status": "accepted", "threadId": "t-1", "turnId": "already-started"}
	k.host.threads["t-1"].turns = append(k.host.threads["t-1"].turns, "already-started")
	if got := k.run(); got["businessTurnId"] != "already-started" || got["state"] != "admitted" {
		t.Fatal("accepted retry was not admitted")
	}
	if k.host.sent != 0 {
		t.Fatal("accepted operation repeated")
	}
}

func businessResendKit(t *testing.T) (*reconcileKit, string, map[string]any) {
	t.Helper()
	k := newReconcileKit(t)
	k.host.initialTurns = []string{"standby"}
	k.host.standby = "inProgress"
	k.expect(k.run(), "incomplete", "standby_incomplete", "")
	k.host.standby = "completed"
	k.host.threads["t-1"].status = "notLoaded"
	_, business := OperationIDs("managed-1")
	failure := businessResendLegacyReceipt("t-1")
	k.host.operations[business] = failure
	return k, business, failure
}

func TestBusinessResendLegacySameRequest(t *testing.T) {
	k, business, failure := businessResendKit(t)
	before, _ := json.Marshal(failure)
	first := k.run()
	if first["state"] != "admitted" || first["businessTurnId"] != "business" {
		t.Fatalf("pre-turn failed receipt must recover: state=%v reason=%v", first["state"], first["reason"])
	}
	for range 3 {
		if got := k.run(); !reflect.DeepEqual(got, first) {
			t.Fatalf("replay changed managed identity: %v", got)
		}
	}
	after, _ := json.Marshal(k.host.operations[business])
	if string(before) != string(after) || k.host.created != 1 || k.host.sent != 1 || len(k.host.sends) != 1 || k.host.sends[0] == business || first["businessRequestId"] != business {
		t.Fatalf("failure/identity/effects changed: created=%d sent=%d IDs=%v", k.host.created, k.host.sent, k.host.sends)
	}
	if !reflect.DeepEqual(k.host.threads["t-1"].turns, []string{"standby", "business"}) {
		t.Fatal("business turn duplicated")
	}
}
