package store

import (
	"context"
	"fmt"
	"testing"
)

// The hour count of one relationship's sends to one recipient is read from rows the relay
// already writes: nothing is stored for it, so these rows are the whole fixture.
func TestRelationshipSends_counts_one_relationship_to_one_recipient_in_one_window(t *testing.T) {
	s := recordStore(t)
	ctx := context.Background()
	const window = 1_699_999_200.0
	inside, before, after := SendStamp(window+10), SendStamp(window-10), SendStamp(window+3610)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	delivery := func(event, relationship, kind, recipient string, stamps ...string) {
		t.Helper()
		exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES (?,?,?,?,?,'dispatched',?,?,?)", event, relationship, kind, recipient, recipient, len(stamps), inside, inside)
		for i, stamp := range stamps {
			exec("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, sent_at, observed_at) VALUES (?,?,?,?,'settled',?,?)", fmt.Sprintf("%s-%d", event, i+1), event, i+1, kind, stamp, stamp)
		}
	}
	message := func(id, relationship, recipient string) {
		t.Helper()
		exec("INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES (?,?,'report',?,'p','k','sender',?,'s','{}','sent',?,?)", id, id, relationship, recipient, inside, inside)
	}
	transport := func(message string, n int, started any, attempted string, retrySafe int) {
		t.Helper()
		exec("INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) VALUES (?,?,?,'m','settled',?,?,'{}',?,?,?)", fmt.Sprintf("%s-t%d", message, n), message, n, attempted, retrySafe, inside, started, inside)
	}
	// Deliveries: a completion to the parent with a send in the hour before and one in the hour
	// after, a request to the child, and a merge-turn grant of another relationship.
	delivery("d-a-parent", "rel-a", "completion", "parent", inside, inside, inside, before, after)
	delivery("d-a-child", "rel-a", "revision_request", "child", inside, inside)
	delivery("d-b-parent", "rel-b", "merge_turn_grant", "parent", inside)
	// Supervisor transports of relationship a to the parent: one that may have gone (counted), one that
	// started and sent nothing and may be retried (not a wake), one with a retry not shown safe (counted),
	// one never started, one an hour earlier.
	message("m-a-parent", "rel-a", "parent")
	transport("m-a-parent", 1, inside, "unknown", 0)
	transport("m-a-parent", 2, inside, "no", 1)
	transport("m-a-parent", 3, inside, "no", 0)
	transport("m-a-parent", 4, nil, "unknown", 0)
	transport("m-a-parent", 5, before, "yes", 1)

	for _, c := range []struct {
		relationship, recipient string
		window                  float64
		want                    int64
	}{
		{"rel-a", "parent", window, 3 + 2},
		{"rel-a", "child", window, 2},
		{"rel-b", "parent", window, 1},
		{"rel-b", "child", window, 0},
		{"rel-c", "parent", window, 0},
		{"rel-a", "parent", window - 3600, 1 + 1},
		{"rel-a", "parent", window + 3600, 1},
	} {
		got, err := s.RelationshipSends(ctx, c.relationship, c.recipient, c.window)
		if err != nil || got != c.want {
			t.Errorf("%s to %s in the window at %.0f: %d, %v; want %d", c.relationship, c.recipient, c.window, got, err, c.want)
		}
	}

	// The expression the assignment view embeds in its own statement, with columns for operands,
	// reads the same numbers as the method the delivery service and status use.
	rows, err := s.All(ctx, "SELECT d.event_id AS event_id, d.relationship_id AS relationship_id, d.recipient_task_id AS recipient, "+RelationshipSendsSQL("d.relationship_id", "d.recipient_task_id")+" AS sends FROM deliveries d ORDER BY d.event_id", RelationshipSendsArgs(window)...)
	if err != nil || len(rows) != 3 {
		t.Fatalf("embedded expression: %d rows, %v", len(rows), err)
	}
	for _, row := range rows {
		want, err := s.RelationshipSends(ctx, row.Get("relationship_id").(string), row.Get("recipient").(string), window)
		if err != nil || row.Get("sends").(int64) != want {
			t.Errorf("%v: embedded %v, method %d, %v", row.Get("event_id"), row.Get("sends"), want, err)
		}
	}
}
