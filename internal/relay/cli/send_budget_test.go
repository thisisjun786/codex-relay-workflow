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
// parent; relationship a has spent its hour (eleven delivery events and one supervisor report,
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
	for i := 1; i <= 11; i++ {
		// eleven earlier events, one attempt each: the hour counts events, not attempts (CRW-1210).
		event := fmt.Sprintf("earlier-%d", i)
		exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES (?,'rel-a','completion','parent','parent','acknowledged',1,?,?)", event, stamp, stamp)
		exec("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES (?,?,1,'completion','settled','dispatched',?,?)", event+"-1", event, stamp, stamp)
	}
	exec("INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES ('report','report','report','rel-a','p','k','sender','parent','s','{}','sent',?,?)", stamp, stamp)
	exec("INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) VALUES ('report-1','report',1,'m','settled','yes',0,'{}',?,?,?)", stamp, stamp, stamp)

	if got, err := s.RelationshipSends(ctx, "rel-a", "parent", window); err != nil || got != 12 {
		t.Fatalf("relationship a's hour is %d, %v; want 12", got, err)
	}

	was := clockNow
	clockNow = func() float64 { return now }
	t.Cleanup(func() { clockNow = was })
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

// CRW-1210: status reads the hour cap of the delivery's relationship class. A relationship whose parent holds
// the live parent binding of a project has the orchestrator's cap (sixty), not the ordinary twelve.
func TestSendBudget_status_reads_an_orchestrators_own_cap(t *testing.T) {
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
	exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-o','ISSUE-O','active','orch','host','child-o','host',1,'[]','[]',?,?)", stamp, stamp)
	send := func(i int) {
		event := fmt.Sprintf("o-%d", i)
		exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES (?,'rel-o','completion','orch','orch','acknowledged',1,?,?)", event, stamp, stamp)
		exec("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES (?,?,1,'completion','settled','dispatched',?,?)", event+"-1", event, stamp, stamp)
	}
	for i := 1; i <= 12; i++ {
		send(i)
	}
	// twelve events into the ordinary cap of twelve: held.
	if reading, err := sendPacing(ctx, s, "rel-o", "orch", now, false); err != nil || get(reading, "reason") != hourlyCap || get(reading, "cap") != int64(12) {
		t.Fatalf("an ordinary relationship's twelfth event: %v, %v", reading, err)
	}
	exec("INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b-o','parent','project','PRJ-O','orch','host','active',1,?,?)", stamp, stamp)
	if reading, err := sendPacing(ctx, s, "rel-o", "orch", now, false); err != nil || reading != nil {
		t.Fatalf("an orchestrator's relationship after twelve events: %v, %v; want room under sixty", reading, err)
	}
	for i := 13; i <= 60; i++ {
		send(i)
	}
	if reading, err := sendPacing(ctx, s, "rel-o", "orch", now, false); err != nil || get(reading, "reason") != hourlyCap || get(reading, "cap") != int64(60) {
		t.Fatalf("an orchestrator's relationship after sixty events: %v, %v", reading, err)
	}
}

// CRW-1210: the hour cap counts distinct events, so a retry of an event the hour already counts is not held by
// it. Status reads that the way the claim and assignment-show do: a relationship at its cap of twelve whose
// requeued event is one of the twelve shows no hourly_cap for it, and still shows the cap for a new event.
func TestSendBudget_status_does_not_hold_the_retry_of_a_counted_event(t *testing.T) {
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
	exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-r','ISSUE-R','active','parent','host','child-r','host',1,'[]','[]',?,?)", stamp, stamp)
	event := func(id, state string, attempts int) {
		exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) VALUES (?,'rel-r',1,'h','ready_for_review','child','t','turn','completed','{}',?,?)", id, stamp, stamp)
		exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES (?,'rel-r','completion','parent','parent',?,?,?,?)", id, state, attempts, stamp, stamp)
		if attempts > 0 {
			exec("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES (?,?,1,'completion','settled','dispatched',?,?)", id+"-1", id, stamp, stamp)
		}
	}
	// twelve distinct events sent this hour; the first was requeued (host loss) and waits to be sent again.
	event("counted", "queued", 1)
	for i := 2; i <= 12; i++ {
		event(fmt.Sprintf("earlier-%d", i), "acknowledged", 1)
	}
	event("fresh", "queued", 0)
	if got, err := s.RelationshipSends(ctx, "rel-r", "parent", window); err != nil || got != 12 {
		t.Fatalf("the hour is %d, %v; want 12", got, err)
	}

	was := clockNow
	clockNow = func() float64 { return now }
	t.Cleanup(func() { clockNow = was })
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
		event  string
		capped bool
	}{{"counted", false}, {"fresh", true}} {
		anchored, err := view.Anchored(ctx, c.event, 1)
		if err != nil {
			t.Fatal(err)
		}
		item, err := service.SnapshotItem(ctx, c.event)
		if err != nil {
			t.Fatal(err)
		}
		for name, pacing := range map[string]any{"status": status[c.event], "assignment-show": get(get(anchored, "delivery"), "pacing"), "delivery snapshot": get(item, "pacing")} {
			if reason := get(pacing, "reason"); (reason == "hourly_cap") != c.capped {
				t.Errorf("%s reads pacing %v for %s; hourly_cap held = %v, want %v", name, pacing, c.event, reason == "hourly_cap", c.capped)
			}
		}
	}
}
