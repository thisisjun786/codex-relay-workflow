package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const boundExplicitPrefix = "explicit_admission_bound:"

// Roots are the relationship's authorized artifact roots, stored as a JSON array.
func (r Relationship) Roots() ([]string, error) {
	var roots []string
	if err := json.Unmarshal([]byte(r.ArtifactRoots), &roots); err != nil {
		return nil, fmt.Errorf("artifact roots of %q: %w", r.ID, err)
	}
	return roots, nil
}

// activeGeneration is registry.require_active followed by _check_generation.
func (in ReceiptIntake) activeGeneration(ctx context.Context, id string, number int64) (Relationship, Generation, error) {
	relationship, err := in.Store.CurrentRelationship(ctx, id)
	if err != nil {
		return Relationship{}, Generation{}, err
	}
	if relationship.Status != StatusActive {
		return Relationship{}, Generation{}, refuse(ReasonRelationshipNotActive, "relationship %q is %q and is never auto-resumed", id, relationship.Status)
	}
	generation, err := in.Store.Generation(ctx, id, number)
	if errors.Is(err, sql.ErrNoRows) {
		return Relationship{}, Generation{}, refuse(ReasonUnknownGeneration, "generation %d was never opened on this relationship", number)
	}
	if err != nil {
		return Relationship{}, Generation{}, err
	}
	if number < relationship.Generation {
		return Relationship{}, Generation{}, refuse(ReasonStaleGeneration, "generation %d is older than the current %d", number, relationship.Generation)
	}
	if generation.AnchorState != AnchorBound {
		return Relationship{}, Generation{}, refuse(ReasonUnboundGeneration, "generation %d has no bound anchor; it stays pending and reportable until an exact dispatch turn id is supplied", number)
	}
	return relationship, generation, nil
}

// DaemonObservation synthesizes a receipt from an observed terminal turn. Only failure and
// interruption: a daemon cannot know a completed turn produced something reviewable.
func (in ReceiptIntake) DaemonObservation(ctx context.Context, relationshipID string, turn TurnReference) (StoredReceipt, error) {
	if turn.Status != "failed" && turn.Status != "interrupted" {
		return StoredReceipt{}, refuse(ReasonProducerNotPermitted, "a daemon observation cannot assert anything from a %q turn", turn.Status)
	}
	current, err := in.Store.CurrentRelationship(ctx, relationshipID)
	if err != nil {
		return StoredReceipt{}, err
	}
	relationship, generation, err := in.activeGeneration(ctx, relationshipID, current.Generation)
	if err != nil {
		return StoredReceipt{}, err
	}
	if err := in.checkTurnIdentity(ctx, relationship, generation, turn, nil); err != nil {
		return StoredReceipt{}, err
	}
	event, err := EventID(relationshipID, int(generation.Number), NoDeliverable, turn.Status, turn.TurnID, nil)
	if err != nil {
		return StoredReceipt{}, err
	}
	str := func(text string) jsonValue { return jsonValue{kind: jsonScalar, scalar: text} }
	null := jsonValue{kind: jsonScalar}
	turnRef := jsonValue{kind: jsonObject, object: []jsonField{{"threadId", str(turn.ThreadID)}, {"turnId", str(turn.TurnID)}, {"turnStatus", str(turn.Status)}}}
	document := jsonValue{kind: jsonObject, object: []jsonField{
		{"relationshipId", str(relationshipID)},
		{"executionGeneration", jsonValue{kind: jsonScalar, scalar: json.Number(strconv.FormatInt(generation.Number, 10))}},
		{"attempt", null},
		{"revisionHash", str(NoDeliverable)},
		{"outcome", str(turn.Status)},
		{"producer", str(ProducerDaemon)},
		{"turnRef", turnRef},
		{"manifest", null},
		{"emittedAt", str(in.Now())},
		{"eventId", str(event)},
	}}
	claim := ReceiptClaim{EventID: event, RelationshipID: relationshipID, Generation: generation.Number, RevisionHash: NoDeliverable, Outcome: ObservationOutcome(turn.Status), Producer: ProducerDaemon, Turn: turn, manifest: null, document: document}
	return in.storeEvent(ctx, claim, sql.NullString{}, nil)
}

// RecordObservation is record_observation: the daemon's (thread, turn, terminal status) key
// deduplicates its own stream, and the per-assignment settlement is recorded beside it.
func (in ReceiptIntake) RecordObservation(ctx context.Context, turn TurnReference, classification ObservationOutcome, relationshipID sql.NullString) error {
	now := in.Now()
	return in.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO observations (thread_id,turn_id,terminal_status,relationship_id,classification,event_id,observed_at) VALUES (?,?,?,?,?,NULL,?)`, turn.ThreadID, turn.TurnID, turn.Status, relationshipID, string(classification), now); err != nil {
			return fmt.Errorf("record observation: %w", err)
		}
		if !relationshipID.Valid {
			return nil
		}
		_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO assignment_settlements (relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES (?,?,?,?,?)`, relationshipID, turn.ThreadID, turn.TurnID, turn.Status, now)
		return err
	})
}
