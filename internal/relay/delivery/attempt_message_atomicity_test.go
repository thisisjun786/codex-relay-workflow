package delivery

import (
	"strings"
	"testing"
)

const ama = "test_attempt_message_atomicity"
const amaClass = "AttemptMessageAtomicity."

type interleavedHost struct {
	Adapter
	before func()
	fired  bool
}

func (h *interleavedHost) ListTurnIDs(thread string, limit int) ([]string, error) {
	ids, err := h.Adapter.ListTurnIDs(thread, limit)
	if !h.fired {
		h.fired = true
		h.before()
	}
	return ids, err
}

func amaToken(t *testing.T, message string) string {
	t.Helper()
	for _, line := range strings.Split(message, "\n") {
		if strings.HasPrefix(line, "requestId: ") {
			return strings.TrimPrefix(line, "requestId: ")
		}
	}
	t.Fatalf("message carries no requestId: %q", message)
	return ""
}

func amaMessages(h *hl, event string) []any {
	h.t.Helper()
	rows, err := h.delivery.AttemptMessages(h.ctx, event)
	mustDo(h.t, err)
	return rows
}
func amaSent(h *hl, request string) any {
	h.t.Helper()
	row, err := one(h.ctx, h.store, "SELECT message FROM attempt_messages WHERE request_id = ?", request)
	mustDo(h.t, err)
	if row == nil {
		return nil
	}
	return row.Opt("message")
}

func Test21_AMA1_interleaved_earlier_caller_cannot_shift_the_token(t *testing.T) {
	mirror(t, ama, amaClass+"test_interleaved_earlier_caller_cannot_shift_the_token", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		earlier := NewService(h.store, h.clock)
		adapter := &interleavedHost{Adapter: h.host}
		adapter.before = func() {
			h.host.script = []string{"busy"}
			now := h.clock.Now() - 60
			_, err := earlier.Attempt(h.ctx, event, h.host, &now, "earlier-slow-caller")
			mustDo(t, err)
		}
		record, err := h.delivery.Attempt(h.ctx, event, adapter, nil, "")
		mustDo(t, err)
		h.eq(adapter.fired)
		h.eq(record != nil)
		sent := h.host.sends[len(h.host.sends)-1]
		h.eq(amaToken(t, sent.message))
		h.eq(str(record, "requestId"))
		for _, s := range h.host.sends {
			h.eq(amaToken(t, s.message))
		}
	})
}

func Test21_AMA2_inspection_after_send_reports_the_sent_attempt_not_the_next_one(t *testing.T) {
	mirror(t, ama, amaClass+"test_inspection_after_send_reports_the_sent_attempt_not_the_next_one", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		record := h.mustAttempt(event, nil)
		rows := amaMessages(h, event)
		h.eq(len(rows))
		entry := rows[0].(Obj)
		h.eq(str(entry, "requestId"))
		h.eq(str(entry, "status"))
		h.eq(amaToken(t, str(entry, "message")))
		preview, err := h.delivery.PreviewMessage(h.ctx, event)
		mustDo(t, err)
		h.eq(amaToken(t, preview))
		_ = record
	})
}

func Test21_AMA3_lost_response_reconciliation_searches_the_token_that_was_sent(t *testing.T) {
	mirror(t, ama, amaClass+"test_lost_response_reconciliation_searches_the_token_that_was_sent", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"transport_unknown"}
		record := h.mustAttempt(event, nil)
		request := str(record, "requestId")
		message := h.host.sends[len(h.host.sends)-1].message
		h.eq(amaToken(t, message))
		h.eq(amaSent(h, request))
		h.eq(str(record, "deliveryState"))
		h.host.startTurn(parent, "", "completed", message)
		resolved := h.reconcile(request, h.host)
		h.eq(str(resolved, "state"))
		h.eq(str(resolved, "evidence"))
		h.eq(strings.Contains(message, request))
	})
}

func Test21_AMA4_show_message_returns_the_sent_attempt_after_a_send(t *testing.T) {
	mirror(t, ama, amaClass+"test_show_message_returns_the_sent_attempt_after_a_send", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		record := h.mustAttempt(event, nil)
		entries := amaMessages(h, event)
		h.eq(len(entries))
		h.eq(str(entries[0].(Obj), "requestId"))
		h.eq(str(entries[0].(Obj), "status"))
		h.eq(amaToken(t, str(entries[0].(Obj), "message")))
		h.eq(len(entries) == 0) // cmd_show previewMessage absence is exercised at the CLI surface.
		_ = record
	})
}

func Test21_AMA5_show_message_offers_a_preview_only_before_anything_is_prepared(t *testing.T) {
	mirror(t, ama, amaClass+"test_show_message_offers_a_preview_only_before_anything_is_prepared", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		rows := amaMessages(h, event)
		h.eq(rows)
		preview, err := h.delivery.PreviewMessage(h.ctx, event)
		mustDo(t, err)
		h.eq(len(rows) == 0)
		h.eq(strings.HasSuffix(amaToken(t, preview), "-a1"))
		record := h.mustAttempt(event, nil)
		after := amaMessages(h, event)
		h.eq(len(after) == 0)
		h.eq(str(after[0].(Obj), "requestId"))
		_ = record
	})
}

func Test21_AMA6_show_message_after_a_retry_lists_both_attempts_distinctly(t *testing.T) {
	mirror(t, ama, amaClass+"test_show_message_after_a_retry_lists_both_attempts_distinctly", func(h *hl) {
		event := h.queuedEvent(regOpts{})
		h.host.script = []string{"busy"}
		h.mustAttempt(event, nil)
		now := h.clock.Now() + 10000
		h.mustAttempt(event, &now)
		entries := amaMessages(h, event)
		ids, statuses := []any{}, []any{}
		for _, v := range entries {
			e := v.(Obj)
			ids = append(ids, str(e, "requestId"))
			statuses = append(statuses, str(e, "status"))
		}
		h.eq(ids)
		h.eq(statuses)
		for _, v := range entries {
			e := v.(Obj)
			h.eq(amaToken(t, str(e, "message")))
		}
	})
}
