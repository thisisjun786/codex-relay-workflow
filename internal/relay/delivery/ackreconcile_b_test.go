package delivery

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// test_ack_reconcile.py ACR-11..ACR-20 (see ackreconcile_a_test.go).

func Test21_ACR11_a_revision_cannot_be_routed_to_an_unauthorized_child(t *testing.T) {
	mirror(t, acr, "Verdicts.test_a_revision_cannot_be_routed_to_an_unauthorized_child", func(h *hl) {
		event := h.verdictAcknowledged([]string{parent})
		h.refusal(h.verdict(event, "needs_changes", "verdict-1"))
	})
}

func Test21_ACR12_a_revision_request_is_never_acknowledged_by_the_parent_path(t *testing.T) {
	mirror(t, acr, "Verdicts.test_a_revision_request_is_never_acknowledged_by_the_parent_path", func(h *hl) {
		event := h.verdictAcknowledged([]string{parent, child})
		_, err := h.verdict(event, "needs_changes", "verdict-1")
		mustDo(t, err)
		revision := h.one("SELECT * FROM deliveries WHERE kind = ?", Revision)
		h.refusal(h.ack.Evaluate(h.ctx, revision.S("event_id")))
	})
}

func Test21_ACR13_a_dispatched_revision_binds_the_new_generation_anchor(t *testing.T) {
	mirror(t, acr, "Verdicts.test_a_dispatched_revision_binds_the_new_generation_anchor", func(h *hl) {
		event := h.verdictAcknowledged([]string{parent, child})
		_, err := h.verdict(event, "needs_changes", "verdict-1")
		mustDo(t, err)
		revision := h.one("SELECT * FROM deliveries WHERE kind = ?", Revision)
		h.attemptOn(revision.S("event_id"), h.host, at(h.clock.Now()))
		bound, err := h.ack.BindDispatchedRevision(h.ctx, revision.S("event_id"))
		mustDo(t, err)
		h.eq(field(bound, "anchorState"))
		h.eq(truthy(field(bound, "dispatchTurnId")))
	})
}

// uncertain is Reconciliation._uncertain.
func (h *hl) uncertain(outcome string) (string, string) {
	event := h.queuedEvent(regOpts{})
	h.host.script = []string{outcome}
	record := h.attemptOn(event, h.host, nil)
	h.eq(str(record, "deliveryState"))
	return event, str(record, "requestId")
}

func Test21_ACR14_the_operation_receipt_is_read_before_the_recipient_turns(t *testing.T) {
	mirror(t, acr, "Reconciliation.test_the_operation_receipt_is_checked_before_the_recipient_turns", func(h *hl) {
		_, request := h.uncertain("turn_start_fail")
		var order []any
		ordered := &hooked{Adapter: h.host,
			getOperation: func(id string) (Obj, error) { order = append(order, "operation"); return h.host.GetOperation(id) },
			findToken: func(thread, token string, limit int, messageOnly bool) (TokenScan, error) {
				order = append(order, "scan")
				return h.host.FindToken(thread, token, limit, messageOnly)
			}}
		h.reconcile(request, ordered)
		h.eq(order[:2])
	})
}

func Test21_ACR15_no_affirmative_evidence_keeps_the_attempt_held(t *testing.T) {
	mirror(t, acr, "Reconciliation.test_no_affirmative_evidence_keeps_the_attempt_held_and_says_what_is_missing", func(h *hl) {
		_, request := h.uncertain("turn_start_fail")
		outcome := h.reconcile(request, h.host)
		h.eq(str(outcome, "evidence"))
		h.eq(str(outcome, "state"))
		h.eq(pyIn("no confirmed pre-send rejection", field(outcome, "missing")))
		h.eq(pyIn("exhausted", field(outcome, "recipientScan")))
	})
	mirror(t, acr, "Reconciliation.test_elapsed_time_never_changes_the_outcome", func(h *hl) {
		event, request := h.uncertain("turn_start_fail")
		first := h.reconcile(request, h.host)
		h.clock.Advance(86400 * 30)
		second := h.reconcile(request, h.host)
		h.eq(str(first, "evidence"))
		h.eq(str(second, "state"))
		h.eq(len(h.attemptsFor(event)))
	})
}

func Test21_ACR16_a_token_in_the_recipient_items_advances_the_delivery_honestly(t *testing.T) {
	mirror(t, acr, "Reconciliation.test_a_token_found_in_the_recipient_items_advances_the_delivery_honestly", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"in_progress"}
		request := str(h.attemptOn(event, h.host, nil), "requestId")
		h.host.startTurn(parent, "", "completed", "..."+request+"...")
		outcome := h.reconcile(request, h.host)
		h.eq(str(outcome, "evidence"))
		h.eq(h.row(event).S("state"))
		h.eq(h.row(event).S("dispatch_evidence"))
		stored := loadsObj(h.attemptsFor(event)[0].S("record"))
		h.eq(field(stored, "transportReceiptStatus"))
		h.eq(field(stored, "sendAttempted"))
		h.eq(field(stored, "deliveryState"))
		h.eq(field(sub(stored, "reconciliation"), "affirmativeEvidence"))
	})
}

func Test21_ACR17_a_truncated_scan_is_inconclusive(t *testing.T) {
	mirror(t, acr, "Reconciliation.test_a_truncated_scan_is_inconclusive_rather_than_absent", func(h *hl) {
		_, request := h.uncertain("turn_start_fail")
		for i := 0; i < 30; i++ {
			h.host.startTurn(parent, "", "completed", fmt.Sprintf("noise %d", i))
		}
		h.host.scanLimit = 5
		outcome := h.reconcile(request, h.host)
		h.eq(str(outcome, "evidence"))
		h.eq(strings.Contains(str(outcome, "recipientScan"), "exhausted=False"))
	})
}

func Test21_ACR18_a_missing_ledger_row_is_an_observation_not_a_licence_to_resend(t *testing.T) {
	mirror(t, acr, "Reconciliation.test_a_missing_ledger_row_is_an_observation_not_a_licence_to_resend", func(h *hl) {
		_, request := h.uncertain("turn_start_fail")
		delete(h.host.ledger, request)
		outcome := h.reconcile(request, h.host)
		h.eq(str(outcome, "operationObservation"))
		h.eq(str(outcome, "evidence"))
		h.clock.Advance(86400)
		eligible := []any{}
		for _, e := range h.eligible() {
			eligible = append(eligible, e)
		}
		h.eq(eligible)
	})
}

func Test21_ACR19_a_confirmed_pre_send_rejection_permits_a_new_attempt(t *testing.T) {
	mirror(t, acr, "Reconciliation.test_a_confirmed_pre_send_rejection_permits_a_new_attempt", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"in_progress"}
		request := str(h.attemptOn(event, h.host, nil), "requestId")
		h.host.ledger[request] = preSendRejection(request)
		outcome := h.reconcile(request, h.host)
		h.eq(str(outcome, "evidence"))
		h.clock.Advance(100000)
		second := h.attemptOn(event, h.host, at(h.clock.Now()))
		h.eq(field(second, "attemptNo"))
	})
}

func (h *hl) recoverOnStart() Obj {
	h.t.Helper()
	report, err := h.rc.RecoverOnStart(h.ctx, h.host, nil)
	mustDo(h.t, err)
	return report
}

func listed(report Obj, key string, value any) bool {
	list, _ := field(report, key).([]any)
	return slices.Contains(list, value)
}

func Test21_ACR20_restart_recovery_never_sends(t *testing.T) {
	t.Run("crash before the ledger row", func(t *testing.T) {
		mirror(t, acr, "RestartRecovery.test_a_crash_before_the_transport_ledger_row_recovers_without_resending", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"process_death"}
			record := h.attemptOn(event, h.host, nil)
			h.eq(str(record, "deliveryState"))
			delete(h.host.ledger, str(record, "requestId"))
			report := h.recoverOnStart()
			h.eq(listed(report, "heldUncertain", str(record, "requestId")))
			h.eq(field(report, "resent"))
		})
	})
	t.Run("crash during the send", func(t *testing.T) {
		mirror(t, acr, "RestartRecovery.test_a_crash_during_the_send_recovers_as_uncertain", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			record := h.attemptOn(event, h.host, nil)
			report := h.recoverOnStart()
			h.eq(listed(report, "heldUncertain", str(record, "requestId")))
		})
	})
	t.Run("crash after acceptance", func(t *testing.T) {
		mirror(t, acr, "RestartRecovery.test_a_crash_after_acceptance_recovers_from_the_ledger", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			request := str(h.attemptOn(event, h.host, nil), "requestId")
			turn := h.host.startTurn(parent, "", "inProgress", "")
			h.host.ledger[request] = Obj{{Key: "requestId", Value: request}, {Key: "status", Value: "accepted"}, {Key: "resumed", Value: Obj{{Key: "approvalPolicy", Value: "never"}}}, {Key: "turnId", Value: turn.TurnID}}
			report := h.recoverOnStart()
			h.eq(h.row(event).S("state"))
			h.eq(field(report, "heldUncertain"))
		})
	})
	t.Run("app restart", func(t *testing.T) {
		mirror(t, acr, "RestartRecovery.test_an_app_restart_recovers_pending_work_without_duplicating_it", func(h *hl) {
			event := h.queuedEvent(regOpts{})
			h.host.script = []string{"in_progress"}
			h.attemptOn(event, h.host, nil)
			h.host.readFailures = map[string]bool{} // FakeHostAdapter.restart
			h.recoverOnStart()
			h.eq(len(h.host.sends))
			h.eq(len(h.attemptsFor(event)))
		})
	})
}

// pyIn is Python's `needle in haystack` over a list (membership) or a string (substring).
func pyIn(needle string, haystack any) bool {
	switch v := haystack.(type) {
	case []any:
		return slices.Contains(v, any(needle))
	case string:
		return strings.Contains(v, needle)
	}
	return false
}
