package delivery

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-1274: a settings refusal no retry fixes (settings_not_preserved, setting_unobservable) holds the delivery at once, under the
// hold held_settings_mismatch, with no retry scheduled and a journal row naming the expected and observed values. The refusals that
// retrying can fix keep the backoff. These tests use temporary synthetic stores and a fake host only.

// settingsRefusalHost answers the send as the bridge adapter does when the resumed thread runs other settings than the record: a
// completed pre-send refusal (no turn started) carrying the finding that names the field and both values.
type settingsRefusalHost struct {
	*fakeHost
	code            string
	field           string
	expected, found any
}

func (h *settingsRefusalHost) SendMessage(_ context.Context, requestID, thread, message string, _ *TaskSettings) (Obj, error) {
	receipt := Obj{
		{Key: "requestId", Value: requestID}, {Key: "operation", Value: "send_message_to_thread"},
		{Key: "status", Value: FailedStatus}, {Key: "threadId", Value: thread},
		{Key: "resumed", Value: Obj{{Key: "approvalPolicy", Value: "never"}}},
		{Key: "settingsFindings", Value: []any{Obj{{Key: "code", Value: h.code}, {Key: "field", Value: h.field}, {Key: "expected", Value: h.expected}, {Key: "returned", Value: h.found}}}},
		{Key: "rpcError", Value: Obj{{Key: "code", Value: h.code}, {Key: "message", Value: h.code + ": " + h.field + " differs; message withheld"}}},
		{Key: "error", Value: "thread/resume: " + h.code + ": " + h.field + " differs; message withheld"},
	}
	h.ledger[requestID] = receipt
	h.sends = append(h.sends, fakeSend{requestID, thread, message, h.code})
	return receipt, nil
}

func TestSettingsRefusalHoldsTheDeliveryAtOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		host            func(*fixture) *settingsRefusalHost
		field           string
		expected, found any
	}{
		{"settings_not_preserved", func(f *fixture) *settingsRefusalHost {
			return &settingsRefusalHost{fakeHost: f.host, code: registry.SettingsNotPreserved, field: "model", expected: "anthropic/claude-sonnet-5-5", found: "anthropic/claude-opus-5-5"}
		}, "model", "anthropic/claude-sonnet-5-5", "anthropic/claude-opus-5-5"},
		{"setting_unobservable", func(f *fixture) *settingsRefusalHost {
			return &settingsRefusalHost{fakeHost: f.host, code: registry.SettingUnobservable, field: "reasoningEffort", expected: "xhigh", found: nil}
		}, "reasoningEffort", "xhigh", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, "")
			event := f.queuedEvent(regOpts{})
			host := tc.host(f)
			record, err := f.delivery.Attempt(f.ctx, event, host, nil, "")
			mustDo(t, err)
			if pyjson.Text(record.Get("deliveryState")) != WithheldPreSend {
				t.Fatalf("the refused send = %v, want withheld_pre_send", record)
			}
			row := f.row(event)
			if row.S("state") != WithheldPreSend || row.S("hold_reason") != HeldSettingsMismatch || !row.N("next_eligible_at") || row.I("attempt_count") != 1 {
				t.Fatalf("delivery after the first refusal: state %s hold %q next_eligible_at %v attempts %d, want withheld_pre_send held_settings_mismatch, no retry scheduled, 1 attempt",
					row.S("state"), row.S("hold_reason"), row.Opt("next_eligible_at"), row.I("attempt_count"))
			}
			// No time brings it back: it is not eligible and a forced attempt claims nothing.
			f.clock.Advance(1_000_000)
			if len(f.eligible()) != 0 {
				t.Fatalf("a held delivery is eligible: %v", f.eligible())
			}
			if again, err := f.delivery.Attempt(f.ctx, event, host, at(f.clock.Now()), ""); err != nil || again != nil {
				t.Fatalf("a held delivery was attempted again: %v %v", again, err)
			}
			if n := len(host.sends); n != 1 {
				t.Fatalf("the host was sent to %d times, want once", n)
			}
			// The journal row names the hold with the values expected and observed.
			held := f.one("SELECT detail FROM journal WHERE kind = ? AND subject = ?", DeliverySettingsHeld, event)
			if held == nil {
				t.Fatal("no delivery_settings_held journal row")
			}
			detail := loadsObj(held.S("detail"))
			if detail.Get("hold") != HeldSettingsMismatch || detail.Get("reason") != tc.name || detail.Get("field") != tc.field || detail.Get("expected") != tc.expected || detail.Get("observed") != tc.found {
				t.Fatalf("journal detail = %v", detail)
			}
			// The attempt's own settlement row keeps the refusal with the same values (status and assignment-show read the hold from it).
			settled := loadsObj(f.one("SELECT detail FROM journal WHERE kind = 'delivery_attempted' AND subject = ?", event).S("detail"))
			refusal, _ := settled.Get("settingsRefusal").(Obj)
			if refusal.Get("reason") != tc.name || refusal.Get("expected") != tc.expected || refusal.Get("observed") != tc.found || settled.Get("hold") != HeldSettingsMismatch {
				t.Fatalf("delivery_attempted detail = %v", settled)
			}
			// The operator view shows the hold.
			item, err := f.delivery.SnapshotItem(f.ctx, event)
			mustDo(t, err)
			if item.Get("holdReason") != HeldSettingsMismatch || !strings.Contains(pyjson.Text(item.Get("reported")), HeldSettingsMismatch) {
				t.Fatalf("snapshot item = %v", item)
			}
			// The failure the stalled-delivery fault is raised from stays recorded.
			if failure := f.one("SELECT error_code FROM failed_operations WHERE scope_key = ?", event); failure == nil || failure.S("error_code") != tc.name {
				t.Fatalf("recorded failure = %v", failure)
			}
		})
	}
}

// The same refusal learned by the reconciliation of an attempt whose answer was lost holds the delivery the same way.
func TestSettingsRefusalFoundByReconciliationHoldsTheDeliveryAtOnce(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	f.host.script = []string{"in_progress"}
	record := f.mustAttempt(event, nil)
	request := pyjson.Text(record.Get("requestId"))
	f.host.ledger[request] = Obj{
		{Key: "requestId", Value: request}, {Key: "operation", Value: "send_message_to_thread"}, {Key: "status", Value: FailedStatus}, {Key: "threadId", Value: pyjson.Text(record.Get("recipientTaskId"))},
		{Key: "resumed", Value: Obj{{Key: "approvalPolicy", Value: "never"}}},
		{Key: "settingsFindings", Value: []any{Obj{{Key: "code", Value: registry.SettingsNotPreserved}, {Key: "field", Value: "model"}, {Key: "expected", Value: "gpt-5"}, {Key: "returned", Value: "gpt-4"}}}},
		{Key: "rpcError", Value: Obj{{Key: "code", Value: registry.SettingsNotPreserved}, {Key: "message", Value: "settings_not_preserved: model differs; message withheld"}}},
		{Key: "error", Value: "thread/resume: settings_not_preserved: model differs; message withheld"},
	}
	_, err := NewReconciler(f.delivery).ReconcileAttempt(f.ctx, request, f.host, at(f.clock.Now()))
	mustDo(t, err)
	row := f.row(event)
	if row.S("state") != WithheldPreSend || row.S("hold_reason") != HeldSettingsMismatch || !row.N("next_eligible_at") {
		t.Fatalf("delivery after the reconciled refusal: state %s hold %q next_eligible_at %v", row.S("state"), row.S("hold_reason"), row.Opt("next_eligible_at"))
	}
	f.clock.Advance(1_000_000)
	if len(f.eligible()) != 0 {
		t.Fatalf("a held delivery is eligible: %v", f.eligible())
	}
	held := f.one("SELECT detail FROM journal WHERE kind = ? AND subject = ?", DeliverySettingsHeld, event)
	if held == nil {
		t.Fatal("no delivery_settings_held journal row")
	}
	if detail := loadsObj(held.S("detail")); detail.Get("expected") != "gpt-5" || detail.Get("observed") != "gpt-4" || detail.Get("field") != "model" {
		t.Fatalf("journal detail = %v", detail)
	}
}

// The refusals retrying can fix keep the backoff: a transport that refused the read and a settings refusal of another kind (the attempt cap after
// the last try is DEL13's).
func TestTransientRefusalsStillBackOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		send func(*fixture) (Adapter, string)
	}{
		{"the transport refused thread/read", func(f *fixture) (Adapter, string) {
			f.host.script = []string{"read_fail"}
			return f.host, ""
		}},
		{"environments the host does not expose", func(f *fixture) (Adapter, string) {
			return &settingsRefusalHost{fakeHost: f.host, code: registry.EnvironmentsUnknown, field: "environments", expected: []any{}, found: nil}, ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, "")
			event := f.queuedEvent(regOpts{})
			adapter, _ := tc.send(f)
			record, err := f.delivery.Attempt(f.ctx, event, adapter, nil, "")
			mustDo(t, err)
			if pyjson.Text(record.Get("deliveryState")) != WithheldPreSend {
				t.Fatalf("the refused send = %v", record)
			}
			row := f.row(event)
			if row.S("state") != WithheldPreSend || !row.N("hold_reason") || row.N("next_eligible_at") {
				t.Fatalf("delivery: state %s hold %q next_eligible_at %v, want withheld_pre_send, no hold and a retry scheduled", row.S("state"), row.S("hold_reason"), row.Opt("next_eligible_at"))
			}
			if n := f.count("SELECT COUNT(*) AS c FROM journal WHERE kind = ? AND subject = ?", DeliverySettingsHeld, event); n != 0 {
				t.Fatalf("%d delivery_settings_held rows for a retryable refusal", n)
			}
			f.clock.Advance(1_000_000)
			if got := f.eligible(); len(got) != 1 || got[0] != event {
				t.Fatalf("eligible after the backoff = %v, want the delivery", got)
			}
		})
	}
}
