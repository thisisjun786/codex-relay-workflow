package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Receipt stages (receipts.py STAGED/FINAL/SUPPRESSED).
const (
	StageStaged     = "staged"
	StageFinal      = "final"
	StageSuppressed = "suppressed"
)

// Clock supplies the ISO timestamps the intake writes, as the Python services receive a clock.
type Clock func() string

// ReceiptIntake validates asserted outcomes against the registry and stores them, or refuses
// and records why (receipts.py ReceiptIntake).
type ReceiptIntake struct {
	Store   *Store
	Now     Clock
	Minimum PathBinding
}

// StoredReceipt is what an accepted receipt reports beside its persisted record.
type StoredReceipt struct {
	EventID     string
	Record      string
	Duplicate   bool
	Stage       string
	PathBinding sql.NullString
}

// AcceptChildReceipt is accept_child_receipt: every refusal is recorded for an operator.
func (in ReceiptIntake) AcceptChildReceipt(ctx context.Context, payload []byte, observation TurnReference) (StoredReceipt, error) {
	stored, err := in.accept(ctx, payload, observation)
	if reason := RefusalReason(err); reason != "" {
		refusal := Refusal{At: in.Now(), Reason: reason, Detail: sql.NullString{String: err.Error(), Valid: true}, Payload: sql.NullString{String: string(payload), Valid: true}}
		if document, decodeErr := decodeOrdered(payload); decodeErr == nil && document.kind == jsonObject {
			if rid, ok := document.field("relationshipId"); ok {
				refusal.RelationshipID = sql.NullString{String: pythonStr(rid), Valid: !rid.isNull()}
			}
			if event, ok := document.field("eventId"); ok {
				refusal.EventID = sql.NullString{String: pythonStr(event), Valid: !event.isNull()}
			}
			if dumped, dumpErr := pythonDumps(document); dumpErr == nil {
				refusal.Payload.String = dumped
			}
		}
		if recordErr := in.Store.RecordRefusal(ctx, refusal); recordErr != nil {
			return StoredReceipt{}, errors.Join(err, recordErr)
		}
	}
	return stored, err
}

func (in ReceiptIntake) accept(ctx context.Context, payload []byte, observation TurnReference) (StoredReceipt, error) {
	claim, err := ParseReceipt(payload)
	if err != nil {
		return StoredReceipt{}, err
	}
	relationship, generation, err := in.activeGeneration(ctx, claim.RelationshipID, claim.Generation)
	if err != nil {
		return StoredReceipt{}, err
	}
	if (claim.Outcome == ReadyForReview || claim.Outcome == BlockedNeedsInput) && claim.Producer != ProducerChild {
		return StoredReceipt{}, refuse(ReasonProducerNotPermitted, "only the child may assert %q; no terminal turn status identifies it", claim.Outcome)
	}
	if claim.Producer == ProducerChild && claim.Attempt == nil {
		return StoredReceipt{}, refuse(ReasonOutcomeInconsistent, "a child receipt carries its own rerun counter as a positive integer")
	}
	if claim.Producer == ProducerDaemon && claim.Attempt != nil {
		return StoredReceipt{}, refuse(ReasonOutcomeInconsistent, "a daemon observation cannot supply the child's rerun counter")
	}
	if claim.Turn != observation {
		return StoredReceipt{}, refuse(ReasonTurnRefMismatch, "turnRef must equal the observation that accompanied this receipt")
	}
	if _, err := in.Store.admitTurn(ctx, relationship, generation, claim.Turn); err != nil {
		return StoredReceipt{}, err
	}
	allowed := compatibleTurnStatus(claim.Outcome, claim.Turn.Status)
	if claim.Producer == ProducerDaemon {
		allowed = daemonTurnStatus(claim.Outcome, claim.Turn.Status)
	}
	if !allowed {
		return StoredReceipt{}, refuse(ReasonContradictoryObservation, "the host observed a %q turn, which cannot carry an outcome of %q", claim.Turn.Status, claim.Outcome)
	}
	binding, err := in.checkDeliverable(claim, relationship)
	if err != nil {
		return StoredReceipt{}, err
	}
	var attempt *int
	if claim.Attempt != nil {
		value := int(*claim.Attempt)
		attempt = &value
	}
	expected, err := EventID(claim.RelationshipID, int(claim.Generation), claim.RevisionHash, string(claim.Outcome), claim.Turn.TurnID, attempt)
	if err != nil {
		return StoredReceipt{}, refuse(ReasonOutcomeInconsistent, "%v", err)
	}
	if claim.EventID != expected {
		return StoredReceipt{}, refuse(ReasonEventIDMismatch, "event id should be %s for these fields", expected)
	}
	return in.storeEvent(ctx, claim, binding)
}

// checkDeliverable branches on OUTCOME before producer: a reviewable receipt must verify its
// manifest over real bytes; an execution-only one carries no manifest and the sentinel.
func (in ReceiptIntake) checkDeliverable(claim ReceiptClaim, relationship Relationship) (sql.NullString, error) {
	if claim.Outcome != ReadyForReview {
		if !claim.manifest.isNull() && !(claim.manifest.kind == jsonArray && len(claim.manifest.array) == 0) {
			return sql.NullString{}, refuse(ReasonManifestForbidden, "an execution-only receipt (%s) carries no manifest, for any producer", claim.Outcome)
		}
		if claim.RevisionHash != NoDeliverable {
			return sql.NullString{}, refuse(ReasonOutcomeInconsistent, "an execution-only receipt (%s) carries the no-deliverable sentinel", claim.Outcome)
		}
		return sql.NullString{}, nil
	}
	if claim.manifest.kind != jsonArray || len(claim.manifest.array) == 0 {
		return sql.NullString{}, refuse(ReasonManifestRequired, "a reviewable receipt carries the manifest it hashed")
	}
	if claim.RevisionHash == NoDeliverable {
		return sql.NullString{}, refuse(ReasonOutcomeInconsistent, "a reviewable receipt cannot carry the no-deliverable sentinel")
	}
	entries, err := validateManifest(claim.manifest.array)
	if err != nil {
		return sql.NullString{}, err
	}
	roots, err := relationship.Roots()
	if err != nil {
		return sql.NullString{}, err
	}
	for _, entry := range entries {
		if _, err := assertWithin(entry.Path, roots); err != nil {
			return sql.NullString{}, err
		}
	}
	mode, err := in.verifyBytes(entries, roots, claim.ManifestRef)
	if err != nil {
		return sql.NullString{}, err
	}
	recomputed, err := ManifestRevision(entries)
	if err != nil {
		return sql.NullString{}, err
	}
	if recomputed != claim.RevisionHash {
		return sql.NullString{}, refuse(ReasonRevisionMismatch, "manifest hashes to %s but the receipt claims %s", recomputed, claim.RevisionHash)
	}
	return sql.NullString{String: string(mode), Valid: true}, nil
}

// verifyBytes is _verify_bytes: a lease is requested only when the store requires the enforced
// tier, and a frozen copy establishes no live binding, so it can only meet best-effort.
func (in ReceiptIntake) verifyBytes(entries []ManifestEntry, roots []string, manifestRef *string) (PathBinding, error) {
	problems, mode := verifyAgainstDisk(entries, roots, in.Minimum == LeaseEnforced)
	if len(problems) > 0 && manifestRef != nil {
		frozen := VerifyFrozen(*manifestRef, entries)
		if len(frozen) == 0 {
			return in.requireMinimum(BestEffortDetection)
		}
		problems = append(problems, frozen...)
	}
	if len(problems) > 0 {
		return "", refuse(ReasonManifestUnverified, "%s", strings.Join(problems[:min(len(problems), 5)], "; "))
	}
	return in.requireMinimum(mode)
}

func (in ReceiptIntake) requireMinimum(mode PathBinding) (PathBinding, error) {
	if in.Minimum == LeaseEnforced && mode != LeaseEnforced {
		return "", refuse(ReasonInsufficientPathBinding, "artifacts were read at %q but this store requires %q", mode, in.Minimum)
	}
	return mode, nil
}

// storeEvent is _store_event: re-observing one revision is one fact seen twice.
func (in ReceiptIntake) storeEvent(ctx context.Context, claim ReceiptClaim, binding sql.NullString) (StoredReceipt, error) {
	now := in.Now()
	record, err := pythonDumps(claim.document)
	if err != nil {
		return StoredReceipt{}, fmt.Errorf("serialize receipt: %w", err)
	}
	stage := StageFinal
	if claim.Turn.Status == "inProgress" {
		stage = StageStaged
	}
	result := StoredReceipt{EventID: claim.EventID, Record: record, Stage: stage, PathBinding: binding}
	err = in.Store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var existing, existingStage string
		lookup := conn.QueryRowContext(ctx, `SELECT receipt,stage FROM events WHERE event_id=?`, claim.EventID).Scan(&existing, &existingStage)
		if lookup == nil {
			result = StoredReceipt{EventID: claim.EventID, Record: existing, Duplicate: true, Stage: existingStage}
			if _, err := conn.ExecContext(ctx, `UPDATE events SET observation_count=observation_count+1,last_seen_at=? WHERE event_id=?`, now, claim.EventID); err != nil {
				return fmt.Errorf("reobserve event: %w", err)
			}
			return journal(ctx, conn, "event_reobserved", claim.EventID, "", now)
		}
		if !errors.Is(lookup, sql.ErrNoRows) {
			return fmt.Errorf("lookup event: %w", lookup)
		}
		staged := sql.NullString{String: now, Valid: stage == StageStaged}
		if _, err := conn.ExecContext(ctx, `INSERT INTO events (event_id,relationship_id,execution_generation,revision_hash,outcome,producer,attempt,turn_thread_id,turn_id,turn_status,receipt,manifest_ref,path_binding_mode,stage,staged_at,first_seen_at,last_seen_at,observation_count) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,1)`,
			claim.EventID, claim.RelationshipID, claim.Generation, claim.RevisionHash, string(claim.Outcome), claim.Producer, claim.Attempt, claim.Turn.ThreadID, claim.Turn.TurnID, claim.Turn.Status, record, claim.ManifestRef, binding, stage, staged, now, now); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
		if err := journal(ctx, conn, "event_accepted", claim.EventID, `{"outcome": `+quoteJSON(string(claim.Outcome))+`, "stage": `+quoteJSON(stage)+`}`, now); err != nil {
			return err
		}
		if claim.Outcome != ReadyForReview {
			return nil
		}
		// Atomic with the receipt: a revision stored without its declaration reads as undeclared.
		_, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO revision_lineage (relationship_id,execution_generation,event_id,revision_hash,supersedes_hash,declared_by,recorded_at) VALUES (?,?,?,?,NULL,'undeclared',?)`, claim.RelationshipID, claim.Generation, claim.EventID, claim.RevisionHash, now)
		return err
	})
	return result, err
}

// Deliverable is intake.deliverable: only a final event may be handed to a parent.
func (s *Store) Deliverable(ctx context.Context, eventID string) (bool, error) {
	var stage string
	err := s.q(ctx).QueryRowContext(ctx, `SELECT stage FROM events WHERE event_id=?`, eventID).Scan(&stage)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deliverable %q: %w", eventID, err)
	}
	return stage == StageFinal, nil
}
