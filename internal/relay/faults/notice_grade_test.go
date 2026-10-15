package faults

import (
	"context"
	"testing"
)

// gradedResolve is the supervisor channel above a store seat laid down for a management seat
// (CRW-1486/S4): the level above answers a user-grade notice and refuses every other grade.
type gradedResolve struct {
	NoticeChannel
	live   map[string]any
	grades []bool
}

func (c *gradedResolve) Resolve(_ context.Context, _ string, userGrade bool) (map[string]any, error) {
	c.grades = append(c.grades, userGrade)
	if !userGrade {
		return nil, &NoticeError{Kind: "unregistered_scope", Detail: "only a user-grade message is heard through the laid-down store seat"}
	}
	return c.live, nil
}

// The deliverer's waiting check resolves a notice at the notice's own grade: a decision notice is
// user grade and is carried through the successor; a blocking notice is a notification and waits on
// the refusal. Resolving every notice as a notification would leave the decision notice waiting.
func TestWaitingForResolvesANoticeAtItsGrade(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		user    bool
		waiting string
	}{
		{"decision", true, ""},
		{"blocking", false, "unregistered_scope: only a user-grade message is heard through the laid-down store seat"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			l, ctx := testLedger(t)
			for _, statement := range []string{
				"INSERT INTO fault_ledger (fault_id, product, fault_class, component, severity, signature, scope, scope_key, state, first_seen_at, last_seen_at, updated_at) VALUES ('abc123','crw','report_omitted','c','broken','{}','{\"projectKey\": \"P\"}','P','open','t','t','t')",
				"INSERT INTO fault_notifications (notification_id, fault_id, product, kind, cycle, state, created_at, updated_at) VALUES ('n1','abc123','crw','" + tc.kind + "',1,'pending','t','t')",
			} {
				if _, err := l.Store.Q(ctx).ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			channel := &gradedResolve{live: map[string]any{"sender": "parent", "recipient": "successor", "projectKey": "P"}}
			d := &NoticeDeliverer{Ledger: l, Channel: channel}
			if got := d.WaitingFor(ctx, map[string]any{"notificationId": "n1", "faultId": "abc123"}, 100000); got != tc.waiting {
				t.Fatalf("a %s notice waits for %q, want %q", tc.kind, got, tc.waiting)
			}
			if len(channel.grades) != 1 || channel.grades[0] != tc.user {
				t.Fatalf("a %s notice was resolved at user grade %v, want [%v]", tc.kind, channel.grades, tc.user)
			}
		})
	}
}
