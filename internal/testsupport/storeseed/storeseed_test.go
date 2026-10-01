package storeseed_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// anchorPending is the anchor_state the product writes for a generation that has no dispatch turn
// yet (registry.AnchorPending, delivery's generation insert). The store package keeps its own copy
// of the name in a test file only, so the literal is also the independent expectation here.
const anchorPending = "anchor_pending"

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// firstGeneration is a relationship and its generation 1 in the given anchor state and turn.
func firstGeneration(anchor string, turn, bound sql.NullString) (store.Relationship, store.Generation) {
	relationship := store.Relationship{ID: "rel-0123456789abcdef", IssueKey: "REL-1", Status: store.StatusActive, ParentTaskID: "01parent-task", ChildTaskID: "01child-task", Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t0", UpdatedAt: "t0"}
	generation := store.Generation{RelationshipID: relationship.ID, Number: 1, DispatchRequestID: "dispatch-1", AnchorState: anchor, DispatchTurnID: turn, OpenedAt: "t0", BoundAt: bound}
	return relationship, generation
}

func rowCount(t *testing.T, s *store.Store, table string) int {
	t.Helper()
	var n int
	if err := s.Querier(context.Background()).QueryRowContext(context.Background(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func text(value string) sql.NullString { return sql.NullString{String: value, Valid: true} }

// The refusals are the two the store's own RecordRelationship made before refactor R1 moved it
// here: a bound generation needs its dispatch turn id, and a turn id that is present is not blank.
func TestRecordRelationship_refuses_a_generation_the_product_never_writes(t *testing.T) {
	for _, c := range []struct {
		name   string
		anchor string
		turn   sql.NullString
	}{
		{"bound without a dispatch turn", store.AnchorBound, sql.NullString{}},
		{"bound with an empty dispatch turn", store.AnchorBound, text("")},
		{"bound with a blank dispatch turn", store.AnchorBound, text("   ")},
		{"bound with a dispatch turn of only a tab and a newline", store.AnchorBound, text("\t\n")},
		{"pending with an empty dispatch turn", anchorPending, text("")},
		{"pending with a blank dispatch turn", anchorPending, text("  ")},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := openStore(t)
			relationship, generation := firstGeneration(c.anchor, c.turn, sql.NullString{})
			err := storeseed.RecordRelationship(context.Background(), s, relationship, generation, "host-a", "host-a")
			if got := store.RefusalReason(err); got != store.ReasonUnboundGeneration {
				t.Fatalf("refusal reason %q (err %v), want %q", got, err, store.ReasonUnboundGeneration)
			}
			// The refusal comes before the transaction: nothing was written.
			for _, table := range []string{"relationships", "generations"} {
				if n := rowCount(t, s, table); n != 0 {
					t.Errorf("%s holds %d rows after a refused seed, want 0", table, n)
				}
			}
		})
	}
}

// The generations the product does write: bound to an exact turn, or pending with no turn.
func TestRecordRelationship_records_the_generations_the_product_writes(t *testing.T) {
	for _, c := range []struct {
		name                  string
		anchor                string
		turn, bound           sql.NullString
		wantAnchor            string
		wantTurn, wantBoundAt sql.NullString
	}{
		{"bound to an exact dispatch turn", store.AnchorBound, text("turn-dispatch-1"), text("t1"),
			"bound", text("turn-dispatch-1"), text("t1")},
		{"pending with no dispatch turn", anchorPending, sql.NullString{}, sql.NullString{},
			"anchor_pending", sql.NullString{}, sql.NullString{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			s := openStore(t)
			relationship, generation := firstGeneration(c.anchor, c.turn, c.bound)
			if err := storeseed.RecordRelationship(ctx, s, relationship, generation, "host-parent", "host-child"); err != nil {
				t.Fatal(err)
			}
			var anchor string
			var turn, boundAt sql.NullString
			if err := s.Querier(ctx).QueryRowContext(ctx, "SELECT anchor_state, dispatch_turn_id, bound_at FROM generations"+
				" WHERE relationship_id = ? AND execution_generation = ?", relationship.ID, 1).Scan(&anchor, &turn, &boundAt); err != nil {
				t.Fatal(err)
			}
			if anchor != c.wantAnchor || turn != c.wantTurn || boundAt != c.wantBoundAt {
				t.Errorf("generation row = (%q, %+v, %+v), want (%q, %+v, %+v)", anchor, turn, boundAt, c.wantAnchor, c.wantTurn, c.wantBoundAt)
			}
			var status, parentHost, childHost string
			var current int64
			if err := s.Querier(ctx).QueryRowContext(ctx, "SELECT status, parent_host_id, child_host_id, execution_generation FROM relationships"+
				" WHERE relationship_id = ?", relationship.ID).Scan(&status, &parentHost, &childHost, &current); err != nil {
				t.Fatal(err)
			}
			if status != "active" || parentHost != "host-parent" || childHost != "host-child" || current != 1 {
				t.Errorf("relationship row = (%q, %q, %q, %d), want (active, host-parent, host-child, 1)", status, parentHost, childHost, current)
			}
		})
	}
}
