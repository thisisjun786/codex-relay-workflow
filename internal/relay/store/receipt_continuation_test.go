package store

import (
	"errors"
	"testing"
)

// A later turn of the registered child is refused unassigned_turn unless a continuation claim
// names the generation's anchor. The refusal carries the anchor as a typed cause only where a
// claim would admit the turn: a foreign thread is refused whatever it claims, and a claim that
// names another anchor is answered by the anchor the generation really has.
func TestContinuationRequired_marks_only_the_refusal_a_claim_would_cure(t *testing.T) {
	later := TurnReference{ThreadID: fixtureChild, TurnID: "turn-goal-continuation-2", Status: "completed"}
	claim := func(anchor string) AcceptOptions {
		return AcceptOptions{Continuation: []byte(`{"anchorTurnId": "` + anchor + `", "actor": "` + fixtureChild + `", "reason": "goal-continuation turn of the same child"}`)}
	}
	t.Run("a later turn without a claim names the anchor it needs", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "finished later")}, 1, later)
		_, err := f.acceptPayload(payload)
		requireReason(t, err, ReasonUnassignedTurn)
		var need *ContinuationRequired
		if !errors.As(err, &need) || need.Anchor != fixtureTurn {
			t.Fatalf("expected ContinuationRequired{%s}, got %v", fixtureTurn, err)
		}
	})
	t.Run("a foreign thread is refused whatever it claims and is never told to claim", func(t *testing.T) {
		f := newIntakeFixture(t)
		foreign := TurnReference{ThreadID: "someone-else", TurnID: later.TurnID, Status: "completed"}
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "not mine")}, 1, foreign)
		for _, options := range []AcceptOptions{{}, claim(fixtureTurn)} {
			_, err := f.intake.AcceptChildReceiptWith(t.Context(), payload.bytes(t), foreign, options)
			requireReason(t, err, ReasonUnassignedTurn)
			var need *ContinuationRequired
			if errors.As(err, &need) {
				t.Fatalf("a foreign thread was told a claim would help: %v", err)
			}
		}
		if n := f.count("SELECT COUNT(*) FROM generation_turns"); n != 0 {
			t.Fatalf("a refused foreign turn left %d admissions", n)
		}
	})
	t.Run("a claim naming another anchor is refused without the marker and admits nothing", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "finished later")}, 1, later)
		_, err := f.intake.AcceptChildReceiptWith(t.Context(), payload.bytes(t), later, claim("some-other-execution"))
		requireReason(t, err, ReasonUnassignedTurn)
		var need *ContinuationRequired
		if errors.As(err, &need) {
			t.Fatalf("a wrong-anchor claim was told to claim: %v", err)
		}
		if n := f.count("SELECT COUNT(*) FROM generation_turns"); n != 0 {
			t.Fatalf("a refused claim left %d admissions", n)
		}
	})
	t.Run("a claim naming the anchor admits the turn and records the claim unverified", func(t *testing.T) {
		f := newIntakeFixture(t)
		payload := f.readyPayload(f.relationship, []string{f.artifact("out.txt", "finished later")}, 1, later)
		if _, err := f.intake.AcceptChildReceiptWith(t.Context(), payload.bytes(t), later, claim(fixtureTurn)); err != nil {
			t.Fatal(err)
		}
		var evidence, detail string
		if err := f.store.DB.QueryRowContext(t.Context(), "SELECT evidence, detail FROM generation_turns WHERE turn_id = ?", later.TurnID).Scan(&evidence, &detail); err != nil {
			t.Fatal(err)
		}
		if evidence != boundExplicitPrefix+fixtureTurn || detail != fixtureChild+": goal-continuation turn of the same child | corroboration=not_corroborated" {
			t.Fatalf("admission recorded as %q / %q", evidence, detail)
		}
	})
	t.Run("a generation with no bound anchor has nothing to claim against", func(t *testing.T) {
		f := newIntakeFixture(t)
		unbound := f.register(registerOptions{issue: "REL-2"})
		payload := f.readyPayload(unbound, []string{f.artifact("out.txt", "finished later")}, 1, later)
		_, err := f.acceptPayload(payload)
		if err == nil {
			t.Fatal("a turn of a generation with no bound anchor was accepted")
		}
		var need *ContinuationRequired
		if errors.As(err, &need) {
			t.Fatalf("a generation with no anchor was told to claim one: %v", err)
		}
	})
}
