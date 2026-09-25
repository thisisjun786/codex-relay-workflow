package store

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// Each Python test in test_attempt_message_atomicity.py is replayed by the real Python
// DeliveryService; Go must read back the same frozen bytes and report the same statuses.
func TestAttemptMessageAtomicity_python_properties(t *testing.T) {
	ctx := context.Background()
	t.Run("test_ordinary_send_carries_its_own_request_id", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "send")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 1 || len(python.Sends) != 1 {
			t.Fatalf("rows %+v sends %+v: %v", rows, python.Sends, err)
		}
		want, err := RequestID(python.Event, 1)
		if err != nil || rows[0].RequestID != want || python.Sends[0][0] != want || requestToken(rows[0].Message.String) != want {
			t.Fatalf("request id %q row %+v send %q", want, rows[0], python.Sends[0][0])
		}
	})
	t.Run("test_retry_carries_the_retry_request_id", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "busy_retry")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 2 || rows[1].Number != rows[0].Number+1 || rows[0].RequestID == rows[1].RequestID {
			t.Fatalf("retry rows %+v: %v", rows, err)
		}
		last := python.Sends[len(python.Sends)-1]
		if last[0] != rows[1].RequestID || requestToken(last[1]) != last[0] || rows[1].Message.String != last[1] {
			t.Fatalf("retry send %q vs %+v", last[0], rows[1])
		}
	})
	t.Run("test_interleaved_earlier_caller_cannot_shift_the_token", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "interleave")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 2 {
			t.Fatalf("history %+v: %v", rows, err)
		}
		requireSameHistory(t, python, rows)
		for _, sent := range python.Sends {
			message, err := s.AttemptMessage(ctx, sent[0])
			if err != nil || message.Message != sent[1] || requestToken(sent[1]) != sent[0] {
				t.Fatalf("sent %q frozen %+v: %v", sent[0], message, err)
			}
		}
		requireConcurrentAttemptsKeepTheirTokens(t)
	})
	t.Run("test_persisted_bytes_belong_to_their_own_attempt", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "busy_retry")
		rows, err := s.AttemptMessages(ctx, python.Event)
		if err != nil || len(rows) != 2 {
			t.Fatalf("messages %+v: %v", rows, err)
		}
		for _, row := range rows {
			sent, err := s.AttemptMessage(ctx, row.RequestID)
			if requestToken(row.Message) != row.RequestID || err != nil || sent.Message != row.Message {
				t.Fatalf("row %+v sent %+v: %v", row, sent, err)
			}
		}
	})
	t.Run("test_inspection_after_send_reports_the_sent_attempt_not_the_next_one", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "send")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 1 || rows[0].Status != MessageDispatched || requestToken(rows[0].Message.String) != python.Sends[0][0] {
			t.Fatalf("inspection %+v: %v", rows, err)
		}
		requireSameHistory(t, python, rows)
	})
	t.Run("test_lost_response_reconciliation_searches_the_token_that_was_sent", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "unknown")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 1 || rows[0].DeliveryState.String != "held_uncertain" {
			t.Fatalf("uncertain history %+v: %v", rows, err)
		}
		sent, err := s.AttemptMessage(ctx, rows[0].RequestID)
		if err != nil || sent.Message != python.Sends[0][1] || !strings.Contains(sent.Message, rows[0].RequestID) {
			t.Fatalf("frozen %+v vs transport %q: %v", sent, python.Sends[0][1], err)
		}
	})
	t.Run("test_bytes_prepared_but_never_dispatched_are_not_called_sent", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "busy_retry")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || rows[0].Status != MessageConfirmedUnsent || rows[0].RequestID != python.Returned[0] {
			t.Fatalf("unsent %+v returned %v: %v", rows, python.Returned, err)
		}
		requireSameHistory(t, python, rows)
	})
	t.Run("test_an_unknown_transport_outcome_is_uncertain_not_sent", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "unknown")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 1 || rows[0].Status != MessageUncertain || rows[0].RequestID != python.Returned[0] {
			t.Fatalf("uncertain %+v returned %v: %v", rows, python.Returned, err)
		}
		requireSameHistory(t, python, rows)
	})
	t.Run("test_attempt_older_than_the_table_is_unavailable_not_reinvented", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "legacy")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 1 || rows[0].Status != MessageUnavailable || rows[0].Message.Valid {
			t.Fatalf("legacy %+v: %v", rows, err)
		}
		requireSameHistory(t, python, rows)
	})
	t.Run("test_dispatched_state_is_unchanged_by_the_new_bookkeeping", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "send")
		attempt, err := s.Attempt(ctx, python.Sends[0][0])
		if err != nil || attempt.State.String != "dispatched" {
			t.Fatalf("attempt %+v: %v", attempt, err)
		}
		delivery, err := s.Delivery(ctx, python.Event)
		if err != nil || delivery.State != "dispatched" || python.DeliveryState != "dispatched" {
			t.Fatalf("delivery %+v: %v", delivery, err)
		}
	})
	t.Run("test_show_message_returns_the_sent_attempt_after_a_send", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "send")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 1 || rows[0].Status != MessageDispatched || rows[0].RequestID != python.Sends[0][0] {
			t.Fatalf("sent %+v: %v", rows, err)
		}
	})
	t.Run("test_show_message_offers_a_preview_only_before_anything_is_prepared", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "none")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 0 || len(python.Messages) != 0 {
			t.Fatalf("unprepared history %+v: %v", rows, err)
		}
	})
	t.Run("test_show_message_after_a_retry_lists_both_attempts_distinctly", func(t *testing.T) {
		python, s := pythonDeliveryStore(t, "busy_retry")
		rows, err := s.FrozenAttempts(ctx, python.Event)
		if err != nil || len(rows) != 2 || rows[0].Status != MessageConfirmedUnsent || rows[1].Status != MessageDispatched {
			t.Fatalf("history %+v: %v", rows, err)
		}
		for _, row := range rows {
			if requestToken(row.Message.String) != row.RequestID {
				t.Fatalf("shifted token %+v", row)
			}
		}
		requireSameHistory(t, python, rows)
	})
}

// requireConcurrentAttemptsKeepTheirTokens is the store half of the interleaving: two
// concurrent Go writers each freeze their own bytes beside their own attempt.
func requireConcurrentAttemptsKeepTheirTokens(t *testing.T) {
	t.Helper()
	s := recordStore(t)
	ctx := context.Background()
	event := strings.Repeat("b", 32)
	start := make(chan struct{})
	results := make(chan error, 2)
	var callers sync.WaitGroup
	for number := 1; number <= 2; number++ {
		request, err := RequestID(event, number)
		if err != nil {
			t.Fatal(err)
		}
		callers.Add(1)
		go func() {
			defer callers.Done()
			<-start
			results <- s.RecordAttempt(ctx,
				Attempt{RequestID: request, EventID: event, Number: int64(number), Kind: "completion", InternalState: "in_flight", ObservedAt: "t"},
				AttemptMessage{RequestID: request, EventID: event, Number: int64(number), Kind: "completion", Message: "requestId: " + request, RenderedAt: "t"})
		}()
	}
	close(start)
	callers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.FrozenAttempts(ctx, event)
	if err != nil || len(rows) != 2 {
		t.Fatalf("history %+v: %v", rows, err)
	}
	for _, row := range rows {
		if requestToken(row.Message.String) != row.RequestID || row.Status != MessagePrepared {
			t.Fatalf("shifted token %+v", row)
		}
	}
}
