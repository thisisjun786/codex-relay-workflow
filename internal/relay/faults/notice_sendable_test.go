package faults

import (
	"context"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// resolveOnly is the supervisor channel as the notice deliverer's waiting check meets it: a hierarchy
// that names one recipient and nothing else.
type resolveOnly struct {
	NoticeChannel
	live map[string]any
}

func (c resolveOnly) Resolve(context.Context, string) (map[string]any, error) { return c.live, nil }

func seedNoticeMessage(t *testing.T, ctx context.Context, s *store.Store, id, recipient, state, staged string, assignments ...string) {
	t.Helper()
	statements := []string{fmt.Sprintf("INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES ('%s','obl-%s','report','rel','p','k','parent','%s','s','{}','%s','%s','%s')", id, id, recipient, state, staged, staged)}
	for _, assignment := range assignments {
		statements = append(statements, fmt.Sprintf("UPDATE supervisor_messages SET %s WHERE message_id='%s'", assignment, id))
	}
	for _, statement := range statements {
		if _, err := s.Q(ctx).ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func TestWaitingFor_names_the_earliest_report_to_the_recipient_that_goes_first(t *testing.T) {
	// Given: a raised notification whose level above is one supervisor, and a store of messages to that
	// supervisor that are added one kind at a time.
	l, ctx := testLedger(t)
	s := l.Store
	const fault, notification = "abc123", "n1"
	for _, statement := range []string{
		"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES ('abc123','crw','report_omitted','c','broken','{}','{\"projectKey\": \"P\"}','P','open','t','t','t')",
		"INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, created_at, updated_at) VALUES ('n1','abc123','crw','blocking',1,'pending','t','t')",
	} {
		if _, err := s.Q(ctx).ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	d := &NoticeDeliverer{Ledger: l, Channel: resolveOnly{live: map[string]any{"sender": "parent", "recipient": "supervisor", "projectKey": "P"}}}
	waiting := func() string {
		t.Helper()
		return d.WaitingFor(ctx, map[string]any{"notificationId": notification, "faultId": fault}, 100000)
	}
	steps := []struct {
		name, id, state string
		assignments     []string
		goesFirst       bool
	}{
		{"a held message", "m1", "queued", []string{"hold_reason='held'"}, false},
		{"a message not yet due", "m2", "queued", []string{"next_eligible_at=100100"}, false},
		{"a message whose lease ran out", "m3", "sending", []string{"lease_until=99900"}, false},
		{"a message already sent", "m4", "dispatched", nil, false},
		{"a message to another recipient", "m5", "queued", []string{"recipient_task_id='other'"}, false},
		{"a message in flight", "m6", "sending", []string{"lease_until=100100"}, true},
		{"a message due now", "m7", "deferred_busy", []string{"next_eligible_at=99900"}, true},
	}
	if got := waiting(); got != "" {
		t.Fatalf("with no message to the supervisor: %q", got)
	}
	var first string
	for i, step := range steps {
		// When: one more message is staged to the supervisor, each later than the one before.
		seedNoticeMessage(t, ctx, s, step.id, "supervisor", step.state, fmt.Sprintf("2023-11-14T22:13:%02d.000000+00:00", i+1), step.assignments...)
		if step.goesFirst && first == "" {
			first = step.id
		}
		// Then: the notice waits for the earliest report that can go ahead of it, and for none that cannot.
		want := ""
		if first != "" {
			want = "an earlier report to supervisor goes first: message " + first
		}
		if got := waiting(); got != want {
			t.Fatalf("after %s: %q, want %q", step.name, got, want)
		}
	}
}

func TestWaitingFor_does_not_wait_for_a_notice_whose_message_has_left_the_queue(t *testing.T) {
	cases := []struct {
		state string
		want  string
	}{
		{"queued", ""},
		{"deferred_busy", ""},
		{"withheld_pre_send", ""},
		{"sending", "its message m1 is sending: a send may be under way, and it is settled from that send's answer"},
		{"held_uncertain", "its message m1 is held_uncertain: a send may be under way, and it is settled from that send's answer"},
		{"dispatched", ""},
		{"read", ""},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			// Given: a raised notification whose message is staged, in one state.
			l, ctx := testLedger(t)
			for _, statement := range []string{
				"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES ('abc123','crw','report_omitted','c','broken','{}','{\"projectKey\": \"P\"}','P','open','t','t','t')",
				"INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, created_at, updated_at) VALUES ('n1','abc123','crw','blocking',1,'pending','t','t')",
			} {
				if _, err := l.Store.Q(ctx).ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			seedNoticeMessage(t, ctx, l.Store, "m1", "supervisor", tc.state, "2023-11-14T22:13:20.000000+00:00", "obligation_kind='fault_notification'", "obligation_id='n1'")
			d := &NoticeDeliverer{Ledger: l, Channel: resolveOnly{live: map[string]any{"sender": "parent", "recipient": "supervisor", "projectKey": "P"}}}
			// When: the deliverer asks what the notice is waiting for.
			got := d.WaitingFor(ctx, map[string]any{"notificationId": "n1", "faultId": "abc123"}, 100000)
			// Then: a message that has left the queue is explained by the send that may be under way, and a queued one is not.
			if got != tc.want {
				t.Fatalf("%s: %q, want %q", tc.state, got, tc.want)
			}
		})
	}
}
