package store

import (
	"context"
	"database/sql"
	"testing"
)

func registeredStore(t *testing.T) (*Store, Relationship) {
	t.Helper()
	s := recordStore(t)
	relationship := Relationship{ID: "rel-0123456789abcdef", IssueKey: "REL-1", Status: StatusActive, ParentTaskID: "01parent-task", ChildTaskID: "01child-task", Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t", UpdatedAt: "t"}
	generation := Generation{RelationshipID: relationship.ID, Number: 1, DispatchRequestID: "dispatch-1", AnchorState: AnchorBound, DispatchTurnID: sql.NullString{String: "turn-dispatch-1", Valid: true}, OpenedAt: "t"}
	if err := s.RecordRelationship(context.Background(), relationship, generation, "host-a", "host-a"); err != nil {
		t.Fatal(err)
	}
	return s, relationship
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	if RefusalReason(err) != reason {
		t.Fatalf("expected %s, got %v", reason, err)
	}
}

func TestRegistry_python_wp1_status_and_anchor_guards(t *testing.T) {
	ctx := context.Background()
	t.Run("test_set_status_cannot_reactivate_a_paused_relationship", func(t *testing.T) {
		s, r := registeredStore(t)
		if err := s.SetStatus(ctx, r.ID, "paused", "t"); err != nil {
			t.Fatal(err)
		}
		requireReason(t, s.SetStatus(ctx, r.ID, StatusActive, "t"), ReasonRelationshipNotActive)
		got, err := s.CurrentRelationship(ctx, r.ID)
		if err != nil || got.Status != "paused" {
			t.Fatalf("status %+v %v", got, err)
		}
	})
	t.Run("test_set_status_cannot_reactivate_a_superseded_relationship", func(t *testing.T) {
		s, r := registeredStore(t)
		if err := s.Supersede(ctx, r.ID, "rel-aaaaaaaaaaaaaaaa", "t"); err != nil {
			t.Fatal(err)
		}
		requireReason(t, s.SetStatus(ctx, r.ID, StatusActive, "t"), ReasonRelationshipNotActive)
		// A live word spelled like a deactivation is still a reactivation of a dead assignment.
		requireReason(t, s.SetStatus(ctx, r.ID, "paused", "t"), ReasonRelationshipNotActive)
		got, err := s.CurrentRelationship(ctx, r.ID)
		if err != nil || got.Status != "archived" {
			t.Fatalf("status %+v %v", got, err)
		}
	})
	t.Run("set_status_still_moves_between_deactivations", func(t *testing.T) {
		// Guards the refusal above against a SetStatus that refuses everything.
		s, r := registeredStore(t)
		if err := s.SetStatus(ctx, r.ID, "paused", "t"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetStatus(ctx, r.ID, "cancelled", "t"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("test_a_corrupted_current_pointer_is_refused_on_every_public_read", func(t *testing.T) {
		s, r := registeredStore(t)
		if _, err := s.DB.ExecContext(ctx, `UPDATE relationships SET execution_generation=99 WHERE relationship_id=?`, r.ID); err != nil {
			t.Fatal(err)
		}
		_, err := s.CurrentRelationship(ctx, r.ID)
		requireReason(t, err, ReasonUnknownGeneration)
		_, err = s.RegistryGeneration(ctx, r.ID, 1)
		requireReason(t, err, ReasonUnknownGeneration)
	})
	t.Run("test_a_blank_dispatch_turn_cannot_bind_during_registration", func(t *testing.T) {
		s := recordStore(t)
		relationship := Relationship{ID: "rel-0123456789abcdef", IssueKey: "REL-1", Status: StatusActive, ParentTaskID: "p", ChildTaskID: "c", Generation: 1, ArtifactRoots: "[]", AllowedRecipients: "[]", CreatedAt: "t", UpdatedAt: "t"}
		generation := Generation{RelationshipID: relationship.ID, Number: 1, DispatchRequestID: "dispatch-1", AnchorState: AnchorBound, DispatchTurnID: sql.NullString{String: "   ", Valid: true}, OpenedAt: "t"}
		requireReason(t, s.RecordRelationship(ctx, relationship, generation, "h", "h"), ReasonUnboundGeneration)
	})
	t.Run("test_a_blank_dispatch_turn_cannot_bind_when_opening_a_generation", func(t *testing.T) {
		s, r := registeredStore(t)
		_, err := s.OpenGeneration(ctx, r.ID, "d2", ReasonNeedsChanges, sql.NullString{String: "  ", Valid: true}, "t")
		requireReason(t, err, ReasonUnboundGeneration)
		got, err := s.CurrentRelationship(ctx, r.ID)
		if err != nil || got.Generation != 1 {
			t.Fatalf("blank turn opened a generation: %+v %v", got, err)
		}
		number, err := s.OpenGeneration(ctx, r.ID, "d2", ReasonNeedsChanges, sql.NullString{String: "t2", Valid: true}, "t")
		if err != nil || number != 2 {
			t.Fatalf("a real turn must open generation 2: %d %v", number, err)
		}
	})
}

func TestOpenGeneration_refuses_when_relationship_is_not_active(t *testing.T) {
	ctx := context.Background()
	for _, deactivate := range []struct {
		name string
		run  func(*Store, string) error
	}{
		{"paused", func(s *Store, id string) error { return s.SetStatus(ctx, id, "paused", "t") }},
		{"superseded", func(s *Store, id string) error { return s.Supersede(ctx, id, "rel-aaaaaaaaaaaaaaaa", "t") }},
	} {
		t.Run(deactivate.name, func(t *testing.T) {
			// Given: a relationship with a generation 2 already opened, then deactivated.
			s, r := registeredStore(t)
			if _, err := s.OpenGeneration(ctx, r.ID, "d2", ReasonNeedsChanges, sql.NullString{String: "t2", Valid: true}, "t"); err != nil {
				t.Fatal(err)
			}
			if err := deactivate.run(s, r.ID); err != nil {
				t.Fatal(err)
			}
			// When: a new generation is opened, and when the old dispatch request is replayed.
			_, fresh := s.OpenGeneration(ctx, r.ID, "d3", ReasonNeedsChanges, sql.NullString{String: "t3", Valid: true}, "t")
			_, replay := s.OpenGeneration(ctx, r.ID, "d2", ReasonNeedsChanges, sql.NullString{String: "t2", Valid: true}, "t")
			// Then: both are refused, as Python's require_active runs before the replay lookup.
			requireReason(t, fresh, ReasonRelationshipNotActive)
			requireReason(t, replay, ReasonRelationshipNotActive)
			var count int
			if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM generations WHERE relationship_id=?`, r.ID).Scan(&count); err != nil || count != 2 {
				t.Fatalf("generations %d %v", count, err)
			}
		})
	}
}
