package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1274: a delivery held on a settings refusal no retry fixes (held_settings_mismatch) is shown by status and by assignment-show
// with its hold, the refusal's reason and field, and the values expected and observed.
func TestSettingsHeld_status_and_assignment_show_name_the_hold_and_the_values(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	const now = 1_700_000_100.0
	stamp := store.SendStamp(now - 10)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-h','ISSUE-H','active','parent','host','child-h','host',1,'[]','[]',?,?)", stamp, stamp)
	exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) VALUES ('event-h','rel-h',1,'h','ready_for_review','child','t','turn','completed','{}',?,?)", stamp, stamp)
	exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, hold_reason, attempt_count, created_at, updated_at) VALUES ('event-h','rel-h','completion','parent','parent','withheld_pre_send','held_settings_mismatch',1,?,?)", stamp, stamp)
	exec("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES ('req-h','event-h',1,'completion','settled','withheld_pre_send',NULL,?)", stamp)
	exec("INSERT INTO failed_operations (scope_key, operation, occurred_at, detail, error_code, retry_safe) VALUES ('event-h','settings_check',?,'thread/resume: settings_not_preserved: model differs; message withheld','settings_not_preserved',1)", stamp)
	exec(`INSERT INTO journal (at, kind, subject, detail) VALUES (?, 'delivery_attempted', 'event-h', ?)`, stamp,
		`{"requestId": "req-h", "state": "withheld_pre_send", "retrySafe": true, "turnPreviouslyObserved": false, "hold": "held_settings_mismatch", "settingsRefusal": {"reason": "settings_not_preserved", "field": "model", "expected": "anthropic/claude-sonnet-5-5", "observed": "anthropic/claude-opus-5-5"}}`)

	was := clockNow
	clockNow = func() float64 { return now }
	t.Cleanup(func() { clockNow = was })
	snap, err := snapshot(ctx, s, "")
	if err != nil {
		t.Fatal(err)
	}
	var shown any
	for _, item := range get(snap, "deliveries").([]any) {
		if get(item, "eventId") == "event-h" {
			shown = item
		}
	}
	if shown == nil {
		t.Fatalf("status lists no delivery event-h: %v", get(snap, "deliveries"))
	}
	view := &registry.AssignmentView{R: &registry.Registry{Store: s}, Clock: func() float64 { return now }, Policy: registry.DefaultRetryPolicy}
	anchored, err := view.Anchored(ctx, "event-h", 1)
	if err != nil {
		t.Fatal(err)
	}
	for name, item := range map[string]any{"status": shown, "assignment-show": get(anchored, "delivery")} {
		if get(item, "holdReason") != "held_settings_mismatch" || get(item, "nextEligibleAt") != nil {
			t.Errorf("%s: holdReason %v, nextEligibleAt %v, want held_settings_mismatch and none", name, get(item, "holdReason"), get(item, "nextEligibleAt"))
		}
		hold := get(item, "settingsHold")
		if hold == nil {
			t.Errorf("%s shows no settingsHold: %v", name, item)
			continue
		}
		if get(hold, "reason") != "settings_not_preserved" || get(hold, "field") != "model" || get(hold, "expected") != "anthropic/claude-sonnet-5-5" || get(hold, "observed") != "anthropic/claude-opus-5-5" {
			t.Errorf("%s: settingsHold = %v, want settings_not_preserved on model, expected sonnet, observed opus", name, hold)
		}
	}
}
