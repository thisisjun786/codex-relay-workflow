package store

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestIdentity_python_derivations(t *testing.T) {
	const rel = "rel-0123456789abcdef"
	const sentinel = "0000000000000000000000000000000000000000000000000000000000000000"
	t.Run("test_relationship_id_is_deterministic_over_task_ids", func(t *testing.T) {
		v, e := RelationshipID("p", "c", "JUN-1")
		again, e2 := RelationshipID("p", "c", "JUN-1")
		if e != nil || e2 != nil || v != again || !regexp.MustCompile(`^rel-[0-9a-f]{16}$`).MatchString(v) {
			t.Fatalf("id %q %q %v %v", v, again, e, e2)
		}
		if v != "rel-"+digest("p|c|JUN-1", 16) || v != "rel-de39f45b5bfa32be" {
			t.Fatalf("id %q", v)
		}
	})
	t.Run("test_swapping_parent_and_child_changes_identity", func(t *testing.T) {
		a, _ := RelationshipID("p", "c", "JUN-1")
		b, _ := RelationshipID("c", "p", "JUN-1")
		if a == b {
			t.Fatal("swapped identity unchanged")
		}
	})
	t.Run("test_separator_cannot_be_smuggled_into_a_field", func(t *testing.T) {
		for _, fields := range [][3]string{{"p|c", "child", "JUN-1"}, {"parent", "c|d", "JUN-1"}, {"parent", "child", "JUN|1"}} {
			if _, e := RelationshipID(fields[0], fields[1], fields[2]); !errors.Is(e, ErrInvalidIdentity) {
				t.Fatalf("accepted %+v: %v", fields, e)
			}
		}
	})
	t.Run("test_ready_branch_is_product_level", func(t *testing.T) {
		revision := strings.Repeat("a", 64)
		a, e := EventID(rel, 2, revision, "ready_for_review", "", nil)
		if e != nil || a != "5092942c0673adb2c3143d884214fe64" {
			t.Fatalf("event %q %v", a, e)
		}
		attempt := 7
		b, e := EventID(rel, 2, revision, "ready_for_review", "other", &attempt)
		if e != nil || a != b {
			t.Fatalf("turn changed product event %q %q %v", a, b, e)
		}
	})
	t.Run("test_execution_branch_keeps_two_interruptions_apart", func(t *testing.T) {
		one := 1
		a, _ := EventID(rel, 1, sentinel, "interrupted", "t1", &one)
		b, _ := EventID(rel, 1, sentinel, "interrupted", "t2", &one)
		if a == b {
			t.Fatal("two turns share event")
		}
	})
	t.Run("test_null_attempt_renders_as_the_literal_null", func(t *testing.T) {
		three := 3
		if RenderAttempt(nil) != "null" || RenderAttempt(&three) != "3" {
			t.Fatalf("render %q %q", RenderAttempt(nil), RenderAttempt(&three))
		}
		v, e := EventID(rel, 1, sentinel, "failed", "t1", nil)
		if e != nil || v != "18c5376bb12b7b5188f9864e597dd35d" {
			t.Fatalf("event %q %v", v, e)
		}
		if v, e := EventID(rel, 1, sentinel, "failed", "t1", &three); e != nil || v != "e13d6c323480870c47fba5b1f9b40281" {
			t.Fatalf("integer attempt %q %v", v, e)
		}
	})
	t.Run("test_zero_attempt_renders_as_zero_not_null", func(t *testing.T) {
		zero := 0
		if RenderAttempt(&zero) != "0" {
			t.Fatalf("zero rendered %q", RenderAttempt(&zero))
		}
		v, e := EventID(rel, 1, sentinel, "failed", "t1", &zero)
		if e != nil || v != digest(rel+"|1|failed|t1|0", 32) {
			t.Fatalf("event %q %v", v, e)
		}
	})
	t.Run("test_execution_branch_requires_the_turn_it_was_observed_on", func(t *testing.T) {
		_, e := EventID(rel, 1, sentinel, "failed", "", nil)
		if !errors.Is(e, ErrInvalidIdentity) {
			t.Fatalf("missing turn: %v", e)
		}
	})
	t.Run("test_ready_branch_refuses_the_no_deliverable_sentinel", func(t *testing.T) {
		_, e := EventID(rel, 1, sentinel, "ready_for_review", "", nil)
		if !errors.Is(e, ErrInvalidIdentity) {
			t.Fatalf("sentinel accepted: %v", e)
		}
	})
	t.Run("test_request_id_shape_and_round_trip", func(t *testing.T) {
		event := strings.Repeat("b", 32)
		request, e := RequestID(event, 2)
		if e != nil || request != "del-bbbbbbbbbbbb-a2" {
			t.Fatalf("request %q %v", request, e)
		}
		prefix, n, e := ParseRequestID(request)
		if e != nil || prefix != "bbbbbbbbbbbb" || n != 2 {
			t.Fatalf("parsed %q %d %v", prefix, n, e)
		}
		if _, e := RequestID(event, 0); !errors.Is(e, ErrInvalidIdentity) {
			t.Fatalf("zero attempt: %v", e)
		}
	})
	t.Run("test_ack_proof_uses_the_recipient_own_turn_id", func(t *testing.T) {
		event := strings.Repeat("c", 32)
		a, e := AckProof(event, "parent-turn-77")
		if e != nil || a != digest(event+"|parent-turn-77", 64) {
			t.Fatalf("proof %q %v", a, e)
		}
		b, _ := AckProof(event, "parent-turn-78")
		if a == b {
			t.Fatal("different turn same proof")
		}
	})
	t.Run("test_revision_request_id_cannot_collide_with_a_completion_id", func(t *testing.T) {
		revision, _ := RevisionRequestEventID(rel, strings.Repeat("e", 32), "verdict-turn-1")
		if want := pythonStoreValue(t, `from codex_session_relay import identity; print(",".join(identity.OUTCOMES))`); strings.Join(Outcomes, ",") != want {
			t.Fatalf("outcomes %v, python %s", Outcomes, want)
		}
		for _, outcome := range Outcomes {
			if outcome == "ready_for_review" {
				continue
			}
			for generation := 1; generation <= 3; generation++ {
				completion, _ := EventID(rel, generation, sentinel, outcome, "verdict-turn-1", nil)
				if completion == revision {
					t.Fatalf("collided with %s generation %d", outcome, generation)
				}
			}
		}
	})
	t.Run("test_a_revision_id_is_stable_across_a_replayed_verdict", func(t *testing.T) {
		a, _ := RevisionRequestEventID(rel, strings.Repeat("a", 32), "verdict-turn-1")
		b, _ := RevisionRequestEventID(rel, strings.Repeat("a", 32), "verdict-turn-1")
		c, _ := RevisionRequestEventID(rel, strings.Repeat("b", 32), "verdict-turn-1")
		if a != b || a == c {
			t.Fatalf("revision ids %q %q %q", a, b, c)
		}
	})
}
