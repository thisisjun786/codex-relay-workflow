package cli_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestDoctorAndStatusReadOldAndNewAttemptRecords(t *testing.T) {
	home := tempHome(t)
	state := filepath.Join(home, "state")
	s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(context.Background(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	const stamp = "2026-01-01T00:00:00Z"
	exec("INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at) VALUES ('rel-synthetic','SYNTHETIC','active','parent','host','child','host',1,'[]','[]',?,?)", stamp, stamp)
	exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, first_seen_at, last_seen_at) VALUES ('0123456789abcdef0123456789abcdef','rel-synthetic',1,'h','ready_for_review','child','child','turn','completed','{}',?,?)", stamp, stamp)
	exec("INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES ('0123456789abcdef0123456789abcdef','rel-synthetic','completion','parent','parent','held_uncertain',1,?,?)", stamp, stamp)
	exec("INSERT INTO attempts (request_id, event_id, attempt_no, kind, internal_state, state, sent_at, observed_at) VALUES ('del-0123456789ab-a1','0123456789abcdef0123456789abcdef',1,'completion','settled','held_uncertain',?,?)", stamp, stamp)
	_, alias := packageBinary(t)
	for _, old := range []bool{true, false} {
		record := map[string]any{"requestId": "del-0123456789ab-a1", "eventId": "0123456789abcdef0123456789abcdef", "attemptNo": 1, "recipientTaskId": "parent", "deliveryState": "held_uncertain", "sendAttempted": "unknown", "retrySafe": false, "observedAt": stamp}
		if !old {
			record["runtime"] = map[string]any{"build": "historic-sender", "executable": "/synthetic/bin/crw"}
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		exec("UPDATE attempts SET record=?", string(raw))
		doctor := binaryRun(t, alias, "--state", state, "doctor")
		if doctor.code != 0 || decode(t, doctor.stdout)["access"].(map[string]any)["dbReadable"] != true {
			t.Fatalf("doctor over old=%v: %+v", old, doctor)
		}
		status := binaryRun(t, alias, "--state", state, "status")
		if status.code != 0 {
			t.Fatal(status)
		}
		delivery := decode(t, status.stdout)["deliveries"].([]any)[0].(map[string]any)
		got := delivery["attemptDetail"].([]any)[0].(map[string]any)["record"]
		if got != string(raw) {
			t.Fatalf("status changed old=%v record: %v", old, got)
		}
	}
}
