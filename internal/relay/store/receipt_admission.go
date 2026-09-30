package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
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
	turnRef := pyjson.Object{{Key: "threadId", Value: turn.ThreadID}, {Key: "turnId", Value: turn.TurnID}, {Key: "turnStatus", Value: turn.Status}}
	document := pyjson.Object{
		{Key: "relationshipId", Value: relationshipID},
		{Key: "executionGeneration", Value: json.Number(strconv.FormatInt(generation.Number, 10))},
		{Key: "attempt", Value: nil},
		{Key: "revisionHash", Value: NoDeliverable},
		{Key: "outcome", Value: turn.Status},
		{Key: "producer", Value: ProducerDaemon},
		{Key: "turnRef", Value: turnRef},
		{Key: "manifest", Value: nil},
		{Key: "emittedAt", Value: in.Now()},
		{Key: "eventId", Value: event},
	}
	claim := ReceiptClaim{EventID: event, RelationshipID: relationshipID, Generation: generation.Number, RevisionHash: NoDeliverable, Outcome: ObservationOutcome(turn.Status), Producer: ProducerDaemon, Turn: turn, manifest: nil, document: document}
	return in.storeEvent(ctx, claim, sql.NullString{}, nil)
}
