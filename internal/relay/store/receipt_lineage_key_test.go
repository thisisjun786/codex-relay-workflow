package store

import (
	"context"
	"testing"
)

// CRW-928: the head judgment finds a revision's lineage row by its primary key (relationship,
// generation, event) instead of joining on event_id alone (CRW-416's cost). That is only sound if
// the writer that stores a reviewable event stores its lineage row under the same key, in the same
// transaction. This pins that premise: one accepted receipt leaves an events row and a
// revision_lineage row under one relationship and one generation, with and without a declared
// predecessor.
func TestReceiptIntakeStoresTheLineageRowUnderTheEventsOwnKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		supersedes *string
	}{
		{name: "a receipt declaring no predecessor"},
		{name: "a receipt declaring a predecessor", supersedes: ptr("predecessor-hash")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newIntakeFixture(t)
			payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "the deliverable")}, 1, assignedTurn("completed"))
			stored, err := f.intake.AcceptChildReceiptWith(context.Background(), payload.bytes(t), payload.TurnRef, AcceptOptions{SupersedesRevision: tc.supersedes})
			if err != nil {
				t.Fatal(err)
			}
			if stored.EventID != payload.EventID {
				t.Fatalf("stored %q, accepted %q", stored.EventID, payload.EventID)
			}
			// The event's own key, read back from the row the writer stored.
			var eventRelationship string
			var eventGeneration int64
			if err := f.store.DB.QueryRowContext(context.Background(), "SELECT relationship_id, execution_generation FROM events WHERE event_id = ?", stored.EventID).Scan(&eventRelationship, &eventGeneration); err != nil {
				t.Fatal(err)
			}
			// The lineage row's key and the declaration it holds.
			var lineageRelationship, lineageRevision string
			var lineageGeneration int64
			var lineageSupersedes *string
			if err := f.store.DB.QueryRowContext(context.Background(), "SELECT relationship_id, execution_generation, revision_hash, supersedes_hash FROM revision_lineage WHERE event_id = ?", stored.EventID).Scan(&lineageRelationship, &lineageGeneration, &lineageRevision, &lineageSupersedes); err != nil {
				t.Fatal(err)
			}
			if lineageRelationship != eventRelationship || lineageGeneration != eventGeneration {
				t.Fatalf("lineage row is under %s/%d and the event is under %s/%d: the head judgment's primary-key join would not find it",
					lineageRelationship, lineageGeneration, eventRelationship, eventGeneration)
			}
			if lineageRelationship != f.relationship.ID {
				t.Fatalf("lineage row is under %q, want the registered relationship %q", lineageRelationship, f.relationship.ID)
			}
			if lineageRevision != payload.RevisionHash {
				t.Fatalf("lineage revision %q, want the event's %q", lineageRevision, payload.RevisionHash)
			}
			switch {
			case tc.supersedes == nil && lineageSupersedes != nil:
				t.Fatalf("an undeclared receipt left supersedes_hash %q", *lineageSupersedes)
			case tc.supersedes != nil && (lineageSupersedes == nil || *lineageSupersedes != *tc.supersedes):
				t.Fatalf("supersedes_hash %v, want %q", lineageSupersedes, *tc.supersedes)
			}
		})
	}
}

// A lineage row under another relationship or generation is a store inconsistency the head
// judgment does not see (its join is the primary key); the recovery reconciliation reports it.
// The writer never produces one, which is what the test above pins.
func TestReceiptIntakeWritesNoLineageRowUnderAnotherKey(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "the deliverable")}, 1, assignedTurn("completed"))
	if _, err := f.intake.AcceptChildReceiptWith(context.Background(), payload.bytes(t), payload.TurnRef, AcceptOptions{}); err != nil {
		t.Fatal(err)
	}
	other := f.count("SELECT COUNT(*) FROM revision_lineage WHERE relationship_id != ? OR execution_generation != ?", f.relationship.ID, f.relationship.Generation)
	if other != 0 {
		t.Fatalf("%d lineage rows outside the event's own key", other)
	}
	orphan := f.count("SELECT COUNT(*) FROM revision_lineage WHERE event_id NOT IN (SELECT event_id FROM events)")
	if orphan != 0 {
		t.Fatalf("%d lineage rows name no event", orphan)
	}
}

func ptr(v string) *string { return &v }
