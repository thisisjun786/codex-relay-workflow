package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestReceipt_preserves_python_payload_when_stored_and_read(t *testing.T) {
	// Given: Python's JSON emitter wrote a non-alphabetic payload.
	s := recordStore(t)
	ctx := context.Background()
	payload := pythonStoreValue(t, `import json; print(json.dumps(dict(z=1, a={"second":2,"first":1})))`)
	event := Event{ID: "e", RelationshipID: "r", Generation: 1, RevisionHash: "hash", Outcome: "failed", Producer: "child", TurnThreadID: "thread", TurnID: "turn", TurnStatus: "completed", Receipt: payload, Stage: "final"}
	// When: Go stores and reads that receipt.
	if err := s.RecordEvent(ctx, event, "t"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Receipt(ctx, "e")
	// Then: the exact Python bytes and original stage survive.
	if err != nil || got.Payload != payload || got.Stage != "final" || got.ObservationCount != 1 {
		t.Fatalf("receipt: %+v: %v", got, err)
	}
}

func TestReceiptsForRelationship_is_ordered_and_scoped(t *testing.T) {
	// Given: two claims for one assignment and one unrelated claim.
	s := recordStore(t)
	ctx := context.Background()
	for _, row := range []struct{ id, relationship, seen string }{{"later", "r", "b"}, {"other", "other", "a"}, {"first", "r", "a"}} {
		event := Event{ID: row.id, RelationshipID: row.relationship, Generation: 1, RevisionHash: "hash", Outcome: "failed", Producer: "child", TurnThreadID: "thread", TurnID: "turn", TurnStatus: "completed", Receipt: `{"z":1,"a":2}`, Stage: "final"}
		if err := s.RecordEvent(ctx, event, row.seen); err != nil {
			t.Fatal(err)
		}
	}
	// When: the relationship's claims are queried.
	got, err := s.ReceiptsForRelationship(ctx, "r")
	// Then: only its two claims are returned in first-seen order.
	if err != nil || len(got) != 2 || got[0].EventID != "first" || got[1].EventID != "later" {
		t.Fatalf("receipts: %+v: %v", got, err)
	}
}

func TestReceipt_distinguishes_missing_event(t *testing.T) {
	// Given: an empty store.
	s := recordStore(t)
	// When: an unknown receipt is requested.
	_, err := s.Receipt(context.Background(), "missing")
	// Then: absence remains distinguishable from a stored claim.
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing receipt: %v", err)
	}
}
