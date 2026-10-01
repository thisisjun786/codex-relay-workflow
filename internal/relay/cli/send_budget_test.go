package cli

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-259: the readers of the send budget (status, assignment-show, the delivery snapshot) read the
// delivery's own relationship's hour, the same one the claim spends. Two relationships share one
// parent; relationship a has spent its hour (eleven delivery attempts and one supervisor report,
// both to the parent), relationship b has sent nothing.
func TestSendBudget_the_readers_agree_on_a_relationships_own_hour(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	const now = 1_700_000_100.0
	window := math.Floor(now/3600) * 3600
	stamp := store.SendStamp(window + 1)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	for _, r := range []string{"a", "b"} {
		exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES (?,?,'active','parent','host',?,'host',1,'[]','[]',?,?)", "rel-"+r, "ISSUE-"+r, "child-"+r, stamp, stamp)
		exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) VALUES (?,?,1,'h','ready_for_review','child','t','turn','completed','{}',?,?)", "event-"+r, "rel-"+r, stamp, stamp)
		exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES (?,?,'completion','parent','parent','queued',0,?,?)", "event-"+r, "rel-"+r, stamp, stamp)
	}
	exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES ('earlier','rel-a','completion','parent','parent','acknowledged',11,?,?)", stamp, stamp)
	for i := 1; i <= 11; i++ {
		exec("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES (?,?,?,'completion','settled','dispatched',?,?)", fmt.Sprintf("earlier-%d", i), "earlier", i, stamp, stamp)
	}
	exec("INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES ('report','report','report','rel-a','p','k','sender','parent','s','{}','sent',?,?)", stamp, stamp)
	exec("INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) VALUES ('report-1','report',1,'m','settled','yes',0,'{}',?,?,?)", stamp, stamp, stamp)

	if got, err := s.RelationshipSends(ctx, "rel-a", "parent", window); err != nil || got != 12 {
		t.Fatalf("relationship a's hour is %d, %v; want 12", got, err)
	}

	clockNow = func() float64 { return now }
	t.Cleanup(func() { clockNow = func() float64 { return 0 } })
	snap, err := snapshot(ctx, s, "")
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]any{}
	for _, item := range get(snap, "deliveries").([]any) {
		status[get(item, "eventId").(string)] = get(item, "pacing")
	}
	view := &registry.AssignmentView{R: &registry.Registry{Store: s}, Clock: func() float64 { return now }, Policy: registry.DefaultRetryPolicy}
	service := delivery.NewService(s, &delivery.FakeClock{T: now})
	for _, c := range []struct {
		event     string
		capped    bool
		sendsWant int64
	}{{"event-a", true, 12}, {"event-b", false, 0}} {
		anchored, err := view.Anchored(ctx, c.event, 1)
		if err != nil {
			t.Fatal(err)
		}
		item, err := service.SnapshotItem(ctx, c.event)
		if err != nil {
			t.Fatal(err)
		}
		for name, pacing := range map[string]any{"status": status[c.event], "assignment-show": get(get(anchored, "delivery"), "pacing"), "delivery snapshot": get(item, "pacing")} {
			if !c.capped {
				if pacing != nil {
					t.Errorf("%s reads pacing %v for %s, which has sent nothing", name, pacing, c.event)
				}
				continue
			}
			reason, sends, capacity := get(pacing, "reason"), get(pacing, "sends"), get(pacing, "cap")
			if reason != "hourly_cap" || !pyEqual(sends, c.sendsWant) || !pyEqual(capacity, int64(12)) {
				t.Errorf("%s reads reason %v, sends %v, cap %v for %s; want hourly_cap, %d, 12", name, reason, sends, capacity, c.event, c.sendsWant)
			}
		}
	}
}
