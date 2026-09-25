package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// The frozen schema declares no foreign key, so no contract write can violate one; this proves
// only that the PRAGMA each connection sets is enforced inside Store.Transaction.
func TestTransaction_enforces_foreign_keys_on_a_scratch_table(t *testing.T) {
	// Given: scratch tables declaring a foreign key, which the contract tables do not.
	s := recordStore(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE fk_parent (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE fk_child (id TEXT PRIMARY KEY,parent TEXT REFERENCES fk_parent(id))`); err != nil {
		t.Fatal(err)
	}
	// When: the transaction writes a child without its parent.
	err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO fk_child VALUES ('c','missing')`)
		return err
	})
	// Then: the error preserves the Python SQLite reason and no row survives.
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("foreign-key reason: %v", err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM fk_child`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial child: %d %v", count, err)
	}
}

func TestRecordRelationship_rolls_back_when_generation_conflicts(t *testing.T) {
	// Given: an existing generation key that will conflict with a new assignment.
	s := recordStore(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,opened_at) VALUES ('r',1,'old','pending','t')`); err != nil {
		t.Fatal(err)
	}
	r := Relationship{ID: "r", IssueKey: "CRW-1", Status: "active", ParentTaskID: "parent", ChildTaskID: "child", Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t", UpdatedAt: "t"}
	g := Generation{RelationshipID: "r", Number: 1, DispatchRequestID: "new", AnchorState: "pending", OpenedAt: "t"}
	// When: the second insert fails after the first one succeeded in the transaction.
	err := s.RecordRelationship(ctx, r, g, "host", "host")
	// Then: the relationship has not escaped the transaction.
	if err == nil {
		t.Fatal("conflicting generation accepted")
	}
	if _, err := s.Relationship(ctx, "r"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial registration: %v", err)
	}
}

func TestRecordAttempt_rolls_back_when_message_conflicts(t *testing.T) {
	// Given: an already-frozen message key.
	s := recordStore(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO attempt_messages (request_id,event_id,attempt_no,kind,message,rendered_at) VALUES ('old','e',1,'parent','old bytes','t')`); err != nil {
		t.Fatal(err)
	}
	attempt := Attempt{RequestID: "new", EventID: "e", Number: 1, Kind: "parent", InternalState: "prepared", ObservedAt: "t"}
	message := AttemptMessage{RequestID: "new", EventID: "e", Number: 1, Kind: "parent", Message: "new bytes", RenderedAt: "t"}
	// When: the frozen message insert conflicts.
	err := s.RecordAttempt(ctx, attempt, message)
	// Then: no attempt claims a message it never froze.
	if err == nil {
		t.Fatal("duplicate message accepted")
	}
	if _, err := s.Attempt(ctx, "new"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("orphan attempt: %v", err)
	}
}

func TestRecordEvent_preserves_python_json_bytes_on_rewrite(t *testing.T) {
	// Given: Python's json.dumps insertion order and default spaces.
	s := recordStore(t)
	ctx := context.Background()
	payload := pythonStoreValue(t, `import json; print(json.dumps(dict(z=1, a={"second":2,"first":1}), ensure_ascii=True))`)
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,first_seen_at,last_seen_at) VALUES ('old','r',1,'hash','failed','child','thread','turn','completed',?,'t','t')`, payload); err != nil {
		t.Fatal(err)
	}
	original, err := s.Event(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	original.ID = "rewritten"
	// When: Go reads and writes that receipt to a second event.
	if err := s.RecordEvent(ctx, original, "t"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Event(ctx, "rewritten")
	// Then: not even the JSON key order or whitespace changes.
	if err != nil || got.Receipt != payload {
		t.Fatalf("receipt = %q; want %q: %v", got.Receipt, payload, err)
	}
}

func TestRecordDelivery_reads_queued_recipient(t *testing.T) {
	// Given: an event intended for a parent task.
	s := recordStore(t)
	ctx := context.Background()
	delivery := Delivery{EventID: "e", RelationshipID: "r", Kind: "completion", RecipientTaskID: "parent", RecipientThreadID: "thread", State: "pending", CreatedAt: "t", UpdatedAt: "t"}
	// When: the delivery is enqueued.
	if err := s.RecordDelivery(ctx, delivery); err != nil {
		t.Fatal(err)
	}
	// Then: the recipient and state survive a typed read.
	got, err := s.Delivery(ctx, "e")
	if err != nil || got.RecipientTaskID != "parent" || got.State != "pending" {
		t.Fatalf("delivery: %+v: %v", got, err)
	}
}

func TestRecordAck_and_verdict_preserve_payload_bytes(t *testing.T) {
	// Given: Python-rendered ordered JSON records.
	s := recordStore(t)
	ctx := context.Background()
	payload := pythonStoreValue(t, `import json; print(json.dumps(dict(z=1, a=2)))`)
	// When: acknowledgement and verdict records are persisted.
	if err := s.RecordAck(ctx, Ack{EventID: "e", Record: payload, TurnID: "parent-turn", Accepted: true, Verified: "host_read", At: "t"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordVerdict(ctx, Verdict{EventID: "e", Record: payload, Decision: "accepted", TurnID: "parent-turn", DecidedAt: "t"}); err != nil {
		t.Fatal(err)
	}
	// Then: neither record has its JSON fields reordered.
	ack, err := s.Ack(ctx, "e")
	if err != nil || ack.Record != payload {
		t.Fatalf("ack: %+v: %v", ack, err)
	}
	verdict, err := s.Verdict(ctx, "e")
	if err != nil || verdict.Record != payload {
		t.Fatalf("verdict: %+v: %v", verdict, err)
	}
}

func TestReobserveEvent_preserves_original_receipt(t *testing.T) {
	// Given: an already-accepted receipt.
	s := recordStore(t)
	ctx := context.Background()
	event := Event{ID: "e", RelationshipID: "r", Generation: 1, RevisionHash: "hash", Outcome: "failed", Producer: "child", TurnThreadID: "thread", TurnID: "turn", TurnStatus: "completed", Receipt: `{"z":1,"a":2}`, Stage: "final"}
	if err := s.RecordEvent(ctx, event, "first"); err != nil {
		t.Fatal(err)
	}
	// When: that event is observed again.
	if err := s.ReobserveEvent(ctx, "e", "second"); err != nil {
		t.Fatal(err)
	}
	// Then: the original bytes remain and only the observation counter advances.
	var count int
	var receipt string
	if err := s.DB.QueryRowContext(ctx, `SELECT observation_count,receipt FROM events WHERE event_id='e'`).Scan(&count, &receipt); err != nil {
		t.Fatal(err)
	}
	if count != 2 || receipt != event.Receipt {
		t.Fatalf("count=%d receipt=%q", count, receipt)
	}
}

func TestRecordObservation_deduplicates_terminal_turn(t *testing.T) {
	// Given: the same terminal observation arrives twice.
	s := recordStore(t)
	ctx := context.Background()
	row := Observation{ThreadID: "thread", TurnID: "turn", TerminalStatus: "failed", Classification: "failed", ObservedAt: "first"}
	// When: a later observation repeats the same key.
	if err := s.RecordObservation(ctx, row); err != nil {
		t.Fatal(err)
	}
	row.ObservedAt = "second"
	if err := s.RecordObservation(ctx, row); err != nil {
		t.Fatal(err)
	}
	// Then: the first fact remains the one recorded.
	got, err := s.Observation(ctx, "thread", "turn", "failed")
	if err != nil || got.ObservedAt != "first" {
		t.Fatalf("observation: %+v: %v", got, err)
	}
}
