package delivery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Acknowledgement refusals and words (ack.py).
const (
	WrongDeliveryKind        = "wrong_delivery_kind"
	AckProofMismatch         = "ack_proof_mismatch"
	AckTurnUnverified        = "ack_turn_unverified"
	NotAcknowledged          = "not_acknowledged"
	RestorationUndeliverable = "restoration_undeliverable"
	DeliveryUnconfirmed      = "delivery_unconfirmed"
	AckPredatesAttempt       = "ack_predates_attempt"
)

var verdicts = []string{"verified", "needs_changes", "unverified", "aborted"}
var unconfirmed = []string{HeldUncertain, Sending}
var rejections = []string{"stale_generation", "unknown_generation", "duplicate_event", "relationship_not_active", "revision_mismatch"}

var currencyReasons = map[string]string{StaleGeneration: StaleGeneration, SupersededRevision: SupersededRevision, RevisionAmbiguous: RevisionAmbiguous, RelationshipNotActive: RelationshipNotActive}

// SyncHook is the verdict's sync outbox obligation (sync.enqueue_verdict_in), owned by todo 23.
// Nil writes none, as Python does when no target is configured.
type SyncHook func(ctx context.Context, relationship Relationship, event Row, verdict string, findings []any, record Obj, digest any, ruling int64) error

// Ack is ack.AckService.
type Ack struct {
	Store    *store.Store
	Delivery *Service
	Clock    Clock
	Criteria *Criteria
	Sync     SyncHook
}

func NewAck(d *Service) *Ack {
	return &Ack{Store: d.Store, Delivery: d, Clock: d.Clock, Criteria: &Criteria{Store: d.Store, Clock: d.Clock}}
}

func basis(row Row) [3]any {
	return [3]any{row.S("state"), row.I("attempt_count"), row.Opt("dispatch_turn_id")}
}

// Evaluate is evaluate: the rejection a parent should use, or "".
func (a *Ack) Evaluate(ctx context.Context, eventID string) (string, error) {
	row, err := a.Delivery.Find(ctx, eventID)
	if err != nil {
		return "", err
	}
	if row != nil && row.S("kind") != Completion {
		return "", refuse(WrongDeliveryKind, "%s is a %s, which is not acknowledged by a parent", strconv.Quote(eventID), row.S("kind"))
	}
	event, err := a.Delivery.eventRow(ctx, eventID)
	if err != nil {
		return "", err
	}
	if event == nil {
		return "unknown_generation", nil
	}
	r, err := LoadRelationship(ctx, a.Store, event.S("relationship_id"))
	if err != nil {
		return "", err
	}
	if r.Status != "active" {
		return "relationship_not_active", nil
	}
	if r.generation(event.I("execution_generation")) == nil {
		return "unknown_generation", nil
	}
	if event.I("execution_generation") < r.Generation {
		return "stale_generation", nil
	}
	existing, err := one(ctx, a.Store, "SELECT * FROM acks WHERE event_id = ?", eventID)
	if err != nil {
		return "", err
	}
	if existing != nil && existing.S("verified") == "verified" {
		return "duplicate_event", nil
	}
	return "", nil
}

// ClaimVerification is claim_verification: idempotent per event, reopened only by a moved set.
func (a *Ack) ClaimVerification(ctx context.Context, eventID string, turnID any) (string, error) {
	now := a.Clock.ISO()
	result := "already_claimed"
	err := a.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		inserted, err := execSQL(ctx, a.Store, "INSERT OR IGNORE INTO verification_claims (event_id, claim_turn_id, claimed_at) VALUES (?,?,?)", eventID, turnID, now)
		if err != nil {
			return err
		}
		claimed, reclaimed := inserted == 1, false
		if !claimed {
			current, err := a.rulingIsCurrent(ctx, eventID)
			if err != nil {
				return err
			}
			if !current {
				bound, err := a.Criteria.BoundDigest(ctx, eventID)
				if err != nil {
					return err
				}
				open, err := a.reReviewOpen(ctx, eventID, bound)
				if err != nil {
					return err
				}
				if open {
					if _, err := execSQL(ctx, a.Store, "UPDATE verification_claims SET claim_turn_id = ?, claimed_at = ? WHERE event_id = ?", turnID, now, eventID); err != nil {
						return err
					}
					if _, err := execSQL(ctx, a.Store, "DELETE FROM claim_context WHERE event_id = ?", eventID); err != nil {
						return err
					}
					reclaimed = true
					if err := journal(ctx, a.Store, "review_reclaimed", eventID, Obj{{Key: "claimTurnId", Value: turnID}}, now); err != nil {
						return err
					}
				}
			}
		}
		if claimed || reclaimed {
			result = "proceed"
			event, err := a.Delivery.eventRow(ctx, eventID)
			if err != nil {
				return err
			}
			if event != nil {
				return a.Criteria.BindReview(ctx, event.S("relationship_id"), eventID)
			}
		}
		return nil
	})
	return result, err
}

func (a *Ack) currentDigest(ctx context.Context, rid string) (any, error) {
	registered, err := a.Criteria.Get(ctx, rid)
	if err != nil || registered == nil {
		return nil, err
	}
	return pyjson.Text(registered.Get("setDigest")), nil
}

func (a *Ack) rulingIsCurrent(ctx context.Context, eventID string) (bool, error) {
	row, err := one(ctx, a.Store, "SELECT set_digest FROM verdict_context WHERE event_id = ?", eventID)
	if err != nil || row == nil {
		return false, err
	}
	event, err := a.Delivery.eventRow(ctx, eventID)
	if err != nil || event == nil {
		return false, err
	}
	digest, err := a.currentDigest(ctx, event.S("relationship_id"))
	return row.Opt("set_digest") == digest, err
}

func (a *Ack) reReviewOpen(ctx context.Context, eventID string, decided any) (bool, error) {
	settled, err := one(ctx, a.Store, "SELECT verdict FROM verdicts WHERE event_id = ?", eventID)
	if err != nil {
		return false, err
	}
	if settled != nil && settled.S("verdict") != "verified" {
		return false, nil
	}
	event, err := a.Delivery.eventRow(ctx, eventID)
	if err != nil || event == nil {
		return false, err
	}
	digest, err := a.currentDigest(ctx, event.S("relationship_id"))
	if err != nil || decided == digest {
		return false, err
	}
	relationship, err := one(ctx, a.Store, "SELECT * FROM relationships WHERE relationship_id = ?", event.S("relationship_id"))
	if err != nil || relationship == nil {
		return false, err
	}
	state, err := Currency(ctx, a.Store, relationship, event)
	if err != nil {
		return false, err
	}
	current, _ := state.Lookup("current")
	return current == true, nil
}

// AckProof is identity.ack_proof.
func AckProof(eventID, turnID string) string {
	sum := sha256.Sum256([]byte(eventID + "|" + turnID))
	return hex.EncodeToString(sum[:])
}

// Acknowledge is acknowledge. adapter nil records the parent's authored intent as unverified.
func (a *Ack) Acknowledge(ctx context.Context, eventID, ackTurn, proof string, accepted bool, rejection any, adapter Adapter) (Obj, error) {
	row, err := a.Delivery.Find(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, refuse(NotClaimable, "no delivery for %s", strconv.Quote(eventID))
	}
	if row.S("kind") != Completion {
		return nil, refuse(WrongDeliveryKind, "%s is a %s; contract v1 acknowledgements are parent-authored for a completion event", strconv.Quote(eventID), row.S("kind"))
	}
	existing, err := one(ctx, a.Store, "SELECT * FROM acks WHERE event_id = ?", eventID)
	if err != nil {
		return nil, err
	}
	if existing != nil && existing.S("verified") == "verified" {
		return settledAck(existing), nil
	}
	state := row.S("state")
	if !slices.Contains(append([]string{Dispatched, InboxOnly}, unconfirmed...), state) {
		return nil, refuse(NotClaimable, "%s is %s; only a delivered event, or one whose send is still being confirmed, is acknowledged", strconv.Quote(eventID), strconv.Quote(state))
	}
	if proof != AckProof(eventID, ackTurn) {
		return nil, refuse(AckProofMismatch, "the proof does not match this event and turn; quoting the delivered fields back cannot produce it")
	}
	verification, err := a.verifyAckTurn(ctx, row, ackTurn, adapter)
	if err != nil {
		if !slices.Contains(unconfirmed, state) || Reason(err) != AckTurnUnverified {
			return nil, err
		}
		verification = AckTurnUnverified
	}
	event, err := a.Delivery.eventRow(ctx, eventID)
	if err != nil {
		return nil, err
	}
	now := a.Clock.ISO()
	var result Obj
	err = a.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		already, err := one(ctx, a.Store, "SELECT * FROM acks WHERE event_id = ?", eventID)
		if err != nil {
			return err
		}
		if already != nil && already.S("verified") == "verified" {
			result = settledAck(already)
			return nil
		}
		fresh, err := a.Delivery.Find(ctx, eventID)
		if err != nil {
			return err
		}
		if fresh == nil || !slices.Contains(append([]string{Dispatched, InboxOnly, Acknowledged}, unconfirmed...), fresh.S("state")) {
			what := "absent"
			if fresh != nil {
				what = fresh.S("state")
			}
			return refuse(NotClaimable, "%s became %s before this acknowledgement could be written", strconv.Quote(eventID), strconv.Quote(what))
		}
		isUnconfirmed := slices.Contains(unconfirmed, fresh.S("state"))
		kept := isUnconfirmed || basis(fresh) != basis(row)
		why := "changed while its turn was read"
		if isUnconfirmed {
			why = fresh.S("state")
		}
		stored := verification
		if kept {
			stored = "unverified_turn"
		}
		computed, err := a.Evaluate(ctx, eventID)
		if err != nil {
			return err
		}
		if accepted && computed != "" {
			return refuse(DispositionConflict, "this event cannot be accepted: %s", computed)
		}
		if !accepted && !truthy(rejection) {
			rejection = computed
			if computed == "" {
				rejection = "revision_mismatch"
			}
		}
		if accepted {
			rejection = nil
		} else if r, _ := rejection.(string); !slices.Contains(rejections, r) {
			return refuse(DispositionConflict, "unknown rejection %s", quote.Value(rejection))
		}
		record := Obj{{Key: "eventId", Value: eventID}, {Key: "relationshipId", Value: event.S("relationship_id")}, {Key: "executionGeneration", Value: event.I("execution_generation")}, {Key: "revisionHash", Value: event.S("revision_hash")},
			{Key: "ackTurnId", Value: ackTurn}, {Key: "accepted", Value: accepted}, {Key: "rejectionReason", Value: rejection}, {Key: "ackAt", Value: now}, {Key: "ackProof", Value: proof}}
		if _, err := execSQL(ctx, a.Store, "INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, rejection_reason, ack_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET record=excluded.record, ack_turn_id=excluded.ack_turn_id, accepted=excluded.accepted, verified=excluded.verified, rejection_reason=excluded.rejection_reason, ack_at=excluded.ack_at",
			eventID, dumps(record), ackTurn, boolFlag(accepted), stored, rejection, now); err != nil {
			return err
		}
		if stored == "verified" {
			if _, err := execSQL(ctx, a.Store, "UPDATE deliveries SET state = ?, updated_at = ? WHERE event_id = ?", Acknowledged, now, eventID); err != nil {
				return err
			}
		}
		if kept {
			err = a.writeAckEvidence(ctx, eventID, "unverified", fmt.Sprintf("kept as authored: the relay could not yet confirm this delivery for the turn it read (%s), and completes the acknowledgement once it does", why), DeliveryUnconfirmed, nil, nil, now, false)
		} else if verification == "verified" {
			err = a.writeAckEvidence(ctx, eventID, "host_read", nil, nil, nil, nil, now, false)
		} else {
			detail := "the host did not confirm this turn"
			if adapter == nil {
				detail = "no host adapter in this process"
			}
			err = a.writeAckEvidence(ctx, eventID, "unverified", detail, verification, nil, nil, now, false)
		}
		if err != nil {
			return err
		}
		journalled := Obj{{Key: "accepted", Value: accepted}, {Key: "verified", Value: stored}, {Key: "reason", Value: rejection}}
		if kept {
			journalled = append(journalled, F{Key: "deliveryUnconfirmed", Value: why})
		}
		if err := journal(ctx, a.Store, "acknowledged", eventID, journalled, now); err != nil {
			return err
		}
		result = append(append(Obj(nil), record...), F{Key: "_verified", Value: stored})
		if kept {
			result = append(result, F{Key: "_deliveryUnconfirmed", Value: why})
		}
		return nil
	})
	return result, err
}

func settledAck(existing Row) Obj {
	record := loadsObj(existing.S("record"))
	return append(record, F{Key: "_verified", Value: existing.S("verified")}, F{Key: "_replay", Value: true})
}

// certainlyBefore is ack.certainly_before at the host's whole-second precision.
func certainlyBefore(started float64, sentAt string) bool {
	sent, ok := epoch(sentAt)
	return ok && started+TurnStartPrecisionSeconds <= sent
}

func (a *Ack) verifyAckTurn(ctx context.Context, row Row, ackTurn string, adapter Adapter) (string, error) {
	if adapter == nil {
		return "unverified_turn", nil
	}
	attempt, err := one(ctx, a.Store, "SELECT * FROM attempts WHERE event_id = ? ORDER BY attempt_no DESC LIMIT 1", row.S("event_id"))
	if err != nil {
		return "", err
	}
	turn, err := adapter.ReadTurn(ctx, row.S("recipient_thread_id"), ackTurn)
	if err != nil || turn == nil || TurnStartedAt(turn) == nil {
		return "unverified_turn", nil
	}
	if attempt != nil {
		sentAt := attempt.S("sent_at")
		if attempt.N("sent_at") {
			sentAt = attempt.S("observed_at")
		}
		var dispatched any
		if !attempt.N("record") {
			dispatched, _ = loadsObj(attempt.S("record")).Lookup("turnId")
		}
		if !truthy(dispatched) && attempt.S("affirmative_evidence") == TurnFound {
			dispatched = row.Opt("dispatch_turn_id")
		}
		if ackTurn != dispatched && certainlyBefore(*TurnStartedAt(turn), sentAt) {
			return "", refuse(AckTurnUnverified, "turn %s started before the delivery, so it cannot be its acknowledgement", strconv.Quote(ackTurn))
		}
	}
	return "verified", nil
}

func (a *Ack) writeAckEvidence(ctx context.Context, eventID, tier string, detail, reason, fingerprint, nextCheck any, now string, bump bool) error {
	b := int64(0)
	if bump {
		b = 1
	}
	_, err := execSQL(ctx, a.Store, "INSERT INTO ack_evidence (event_id, tier, detail, attempts, last_reason, fingerprint, next_check_at, observed_at) VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET tier = excluded.tier, detail = excluded.detail, attempts = ack_evidence.attempts + ?, last_reason = excluded.last_reason, fingerprint = excluded.fingerprint, next_check_at = excluded.next_check_at, observed_at = excluded.observed_at",
		eventID, tier, detail, b, reason, fingerprint, nextCheck, now, b)
	return err
}

func (a *Ack) ackEvidenceTier(ctx context.Context, eventID string) (string, error) {
	row, err := one(ctx, a.Store, "SELECT tier FROM ack_evidence WHERE event_id = ?", eventID)
	if err != nil || row == nil {
		return "unrecorded", err
	}
	return row.S("tier"), nil
}

func restorationResult(outcome, basis string, criterion any, detail string) Obj {
	return Obj{{Key: "outcome", Value: outcome}, {Key: "basis", Value: basis}, {Key: "criterion", Value: criterion}, {Key: "detail", Value: detail}}
}

func projectCap(findings []any) Obj {
	for i, f := range findings {
		o := f.(Obj)
		if v, _ := o.Lookup("restoration"); truthy(v) {
			where := fmt.Sprintf("finding %d of %d", i+1, len(findings))
			id, _ := o.Lookup("id")
			if i < manifestLines {
				return restorationResult("carried", "relay-message/legacy", id, fmt.Sprintf("%s, within the %d this renderer shows", where, manifestLines))
			}
			return restorationResult("truncated", "relay-message/legacy", id, fmt.Sprintf("%s, past the %d this renderer shows", where, manifestLines))
		}
	}
	return restorationResult("not_carried", "relay-message/legacy", nil, "no finding declared a restoration block")
}

// RecordVerdict is record_verdict: the verdict, the generation it opens and the correction, in
// one transaction, with currency decided inside it. No parameter bypasses the check.
//
// An event has one standing ruling (rulingchange.go). The same verdict again is a replay. A different
// one is never answered with the recorded ruling: a verified ruling that nothing rests on is
// replaced by needs_changes (the correction opens as for a first needs_changes ruling) and every
// other different verdict is refused with disposition_conflict. An open re-review is decided first,
// as before: it is the route for a criteria set that moved, accepted head or not.
func (a *Ack) RecordVerdict(ctx context.Context, eventID, verdict, verdictTurn string, criteria, findings []any, reason, expected any) (Obj, error) {
	if !slices.Contains(verdicts, verdict) {
		return nil, refuse(DispositionConflict, "unknown verdict %s", strconv.Quote(verdict))
	}
	normalised, err := NormaliseFindings(criteria, findings)
	if err != nil {
		return nil, err
	}
	now := a.Clock.ISO()
	var record, replaced Obj
	err = a.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		replaced = nil
		settled, err := one(ctx, a.Store, "SELECT v.record AS record, v.verdict AS verdict, v.verdict_turn_id AS verdict_turn, v.next_generation AS next_generation, c.set_digest AS set_digest FROM verdicts v LEFT JOIN verdict_context c ON c.event_id = v.event_id WHERE v.event_id = ?", eventID)
		if err != nil {
			return err
		}
		reReview, changed := false, false
		if settled != nil {
			if reReview, err = a.reReviewOpen(ctx, eventID, settled.Opt("set_digest")); err != nil {
				return err
			}
		}
		if settled != nil && !reReview {
			recorded := settled.S("verdict")
			move, route := rulingTransition(recorded, verdict, settled.I("next_generation"))
			switch move {
			case moveReplay:
				record = append(loadsObj(settled.S("record")), F{Key: "_replay", Value: true})
				return nil
			case moveRefuse:
				return differentRuling(eventID, recorded, settled.S("verdict_turn"), verdict, route)
			}
			// a verified ruling is replaced only while nothing rests on it, read before anything else of
			// the existing path so that an event the plan accepted always names its acceptance
			rest, err := registry.RestsOn(ctx, a.Store, eventID)
			if err != nil {
				return err
			}
			if rest.Kind != "" {
				return builtOn(eventID, settled.S("verdict_turn"), rest)
			}
			changed = true
			prior := loadsObj(settled.S("record"))
			replaced = Obj{{Key: "verdict", Value: prior.Get("verdict")}, {Key: "verdictTurnId", Value: prior.Get("verdictTurnId")}, {Key: "decidedAt", Value: prior.Get("decidedAt")}}
		}
		if reReview && verdict != "verified" && verdict != "needs_changes" {
			return refuse(DispositionConflict, "%s cannot replace the verified ruling this re-review is reopening: it would leave the assignment with no state to act on. Rule verified or needs_changes, or change the relationship's status", strconv.Quote(verdict))
		}
		if reReview && expected == nil {
			return refuse(CriteriaSetChanged, "%s was ruled against a different criteria set, so this is a re-review, and a re-review names the set it read: pass the reviewed digest explicitly", strconv.Quote(eventID))
		}
		ack, err := one(ctx, a.Store, "SELECT * FROM acks WHERE event_id = ?", eventID)
		if err != nil {
			return err
		}
		if ack == nil || ack.I("accepted") == 0 || ack.S("verified") != "verified" {
			return refuse(NotAcknowledged, "%s has no verified acceptance, so there is nothing to rule on", strconv.Quote(eventID))
		}
		event, err := a.Delivery.eventRow(ctx, eventID)
		if err != nil {
			return err
		}
		if settled == nil {
			// a decision and a verdict never both answer one receipt (decision.go): a receipt that carries a decision is not ruled
			if answered, err := one(ctx, a.Store, "SELECT 1 FROM events WHERE event_id = ?", DecisionEventID(event.S("relationship_id"), eventID)); err != nil {
				return err
			} else if answered != nil {
				return refuse(DispositionConflict, "%s already carries a decision reply, and a decision and a verdict never both answer one receipt: the child's next receipt is the one to rule", strconv.Quote(eventID))
			}
		}
		relationship, err := RequireActive(ctx, a.Store, event.S("relationship_id"))
		if err != nil {
			if changed && Reason(err) == RelationshipNotActive {
				return changeRefusal(RelationshipNotActive, eventID, Detail(err))
			}
			return err
		}
		relRow, err := one(ctx, a.Store, "SELECT * FROM relationships WHERE relationship_id = ?", event.S("relationship_id"))
		if err != nil {
			return err
		}
		state, err := Currency(ctx, a.Store, relRow, event)
		if err != nil {
			return err
		}
		current, _ := state.Lookup("current")
		if (verdict == "verified" || verdict == "needs_changes") && current != true {
			if changed {
				return changeRefusal(currencyReasons[pyjson.Text(state.Get("reason"))], eventID, pyjson.Text(state.Get("detail")))
			}
			return refuse(currencyReasons[pyjson.Text(state.Get("reason"))], "%s cannot be ruled %s: %s", strconv.Quote(eventID), strconv.Quote(verdict), pyjson.Text(state.Get("detail")))
		}
		cover, err := a.Criteria.Coverage(ctx, event.S("relationship_id"), eventID, verdict, normalised, reason, expected)
		if err != nil {
			return err
		}
		var projected Obj
		if verdict == "needs_changes" {
			if !slices.Contains(relationship.AllowedRecipients, relationship.Child.TaskID) {
				return refuse(RecipientNotAuthorized, "child %s is not an allowed recipient, so a revision cannot be routed to it", strconv.Quote(relationship.Child.TaskID))
			}
			projected = projectCap(normalised)
			if o := pyjson.Text(projected.Get("outcome")); o == "truncated" || o == "budget_dropped" {
				return refuse(RestorationUndeliverable, "this correction declares a restoration block on %s that the revision message would not carry: %s. Move it within the first %d findings and rule again. No execution generation has been opened", quote.Value(func() any { v, _ := projected.Lookup("criterion"); return v }()), pyjson.Text(projected.Get("detail")), manifestLines)
			}
		} else {
			projected = restorationResult("not_carried", "relay-message/legacy", nil, fmt.Sprintf("a %s verdict opens no correction, so no message carries a restoration block", verdict))
		}
		record = Obj{{Key: "eventId", Value: eventID}, {Key: "relationshipId", Value: event.S("relationship_id")}, {Key: "executionGeneration", Value: event.I("execution_generation")}, {Key: "verdict", Value: verdict}, {Key: "verdictTurnId", Value: verdictTurn}, {Key: "decidedAt", Value: now}}
		if len(normalised) > 0 {
			record = append(record, F{Key: "criteria", Value: normalised})
		}
		var next any
		correction := ""
		if verdict == "needs_changes" {
			rid, child := relationship.ID, relationship.Child.TaskID
			revisionEvent, err := store.RevisionRequestEventID(rid, eventID, verdictTurn)
			if err != nil {
				return err
			}
			number, err := OpenGenerationIn(ctx, a.Store, a.Clock, rid, "revision-"+revisionEvent, "needs_changes_revision", nil)
			if err != nil {
				return err
			}
			criteriaList := normalised
			if criteriaList == nil {
				criteriaList = []any{}
			}
			payload := Obj{{Key: "eventId", Value: revisionEvent}, {Key: "relationshipId", Value: rid}, {Key: "executionGeneration", Value: number}, {Key: "kind", Value: Revision}, {Key: "supersedesEvent", Value: eventID}, {Key: "supersedesRevisionHash", Value: event.S("revision_hash")},
				{Key: "verdict", Value: verdict}, {Key: "verdictTurnId", Value: verdictTurn}, {Key: "criteria", Value: criteriaList}, {Key: "childTaskId", Value: child}, {Key: "emittedAt", Value: now},
				{Key: "note", Value: "relay-owned revision request; contract v1 defines no record for this direction"}}
			if err := a.queueToChild(ctx, revisionEvent, rid, number, Revision, relationship.Parent.TaskID, verdictTurn, child, payload, now); err != nil {
				return err
			}
			record = append(record, F{Key: "nextExecutionGeneration", Value: number})
			next, correction = number, revisionEvent
		}
		if _, err := execSQL(ctx, a.Store, "INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?,?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET record = excluded.record, verdict = excluded.verdict, next_generation = excluded.next_generation, verdict_turn_id = excluded.verdict_turn_id, decided_at = excluded.decided_at",
			eventID, dumps(record), verdict, next, verdictTurn, now); err != nil {
			return err
		}
		if err := journal(ctx, a.Store, "verdict_recorded", eventID, Obj{{Key: "verdict", Value: verdict}}, now); err != nil {
			return err
		}
		if err := journal(ctx, a.Store, "restoration_projected", eventID, projected, now); err != nil {
			return err
		}
		if correction != "" {
			if err := journal(ctx, a.Store, "restoration_projected", correction, projected, now); err != nil {
				return err
			}
		}
		setDigest, _ := cover.Lookup("setDigest")
		if reReview || changed {
			superseded := Obj{{Key: "supersededVerdict", Value: loadsObj(settled.S("record"))}, {Key: "reviewedSetDigest", Value: settled.Opt("set_digest")}, {Key: "currentSetDigest", Value: setDigest}}
			if changed {
				superseded = append(superseded, F{Key: "reason", Value: rulingChanged})
			}
			if err := journal(ctx, a.Store, "verdict_superseded", eventID, superseded, now); err != nil {
				return err
			}
		}
		var findingsText any
		if len(normalised) > 0 {
			findingsText = dumps(normalised)
		}
		currency := "current"
		if current != true {
			currency = pyjson.Text(state.Get("reason"))
			if currency == "" {
				currency = "unknown"
			}
		}
		tier, err := a.ackEvidenceTier(ctx, eventID)
		if err != nil {
			return err
		}
		headID, _ := state.Lookup("headEventId")
		headRev, _ := state.Lookup("headRevisionHash")
		if _, err := execSQL(ctx, a.Store, "INSERT INTO verdict_context (event_id, set_digest, coverage, findings, reason, currency, head_event_id, head_revision, ack_evidence, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_id) DO UPDATE SET set_digest = excluded.set_digest, coverage = excluded.coverage, findings = excluded.findings, reason = excluded.reason, currency = excluded.currency, head_event_id = excluded.head_event_id, head_revision = excluded.head_revision, ack_evidence = excluded.ack_evidence, recorded_at = excluded.recorded_at",
			eventID, setDigest, pyjson.Text(cover.Get("coverage")), findingsText, reason, currency, headID, headRev, tier, now); err != nil {
			return err
		}
		if a.Sync != nil {
			seen, err := one(ctx, a.Store, "SELECT COUNT(*) AS seen FROM journal WHERE kind = ? AND subject = ?", "verdict_superseded", eventID)
			if err != nil {
				return err
			}
			return a.Sync(ctx, relationship, event, verdict, normalised, record, setDigest, 1+seen.I("seen"))
		}
		return nil
	})
	if err == nil && replaced != nil {
		// an answer annotation, as _replay is: the stored record and the summary owed to the
		// coordination document stay the record's own fields
		record = append(record, F{Key: "_supersedes", Value: replaced})
	}
	return record, err
}

// queueToChild stores a relay-owned request to the child as a final event of the generation and queues its delivery, inside the
// caller's transaction. The correction a needs_changes ruling opens (outcome revision_request) and a decision reply (outcome
// decision_reply) are both written here, so the two cannot differ in how they reach the delivery engine.
func (a *Ack) queueToChild(ctx context.Context, eventID, rid string, generation int64, outcome, parentTask, turn, child string, payload Obj, now string) error {
	if _, err := execSQL(ctx, a.Store, "INSERT OR IGNORE INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, attempt, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at, observation_count) VALUES (?,?,?,?,?,?,NULL,?,?,?,?, 'final', ?,?,1)",
		eventID, rid, generation, store.NoDeliverable, outcome, "relay", parentTask, turn, "completed", dumps(payload), now, now); err != nil {
		return err
	}
	return a.Delivery.EnqueueIn(ctx, eventID, rid, Revision, child)
}

// OpenGenerationIn is registry.open_generation_in inside the caller's transaction.
func OpenGenerationIn(ctx context.Context, s *store.Store, clock Clock, rid, dispatchRequest, reason string, dispatchTurn any) (int64, error) {
	replay, err := one(ctx, s, "SELECT execution_generation FROM generations WHERE relationship_id = ? AND dispatch_request_id = ?", rid, dispatchRequest)
	if err != nil {
		return 0, err
	}
	if replay != nil {
		return replay.I("execution_generation"), store.RefuseWithdrawn(ctx, s.Q(ctx), rid, replay.I("execution_generation"))
	}
	current, err := one(ctx, s, "SELECT execution_generation, status, superseded_by FROM relationships WHERE relationship_id = ?", rid)
	if err != nil {
		return 0, err
	}
	if current == nil {
		return 0, refuse(UnregisteredRelationship, "no relationship %s", strconv.Quote(rid))
	}
	if current.S("status") != "active" || truthy(current.Opt("superseded_by")) {
		return 0, refuse(RelationshipNotActive, "relationship %s is not active", strconv.Quote(rid))
	}
	number, err := store.NextGeneration(ctx, s.Q(ctx), rid)
	if err != nil {
		return 0, err
	}
	now := clock.ISO()
	anchor, bound := "anchor_pending", any(nil)
	if dispatchTurn != nil {
		anchor, bound = "bound", now
	}
	if _, err := execSQL(ctx, s, "INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?,?,?,?,?,?,?,?)", rid, number, dispatchRequest, anchor, dispatchTurn, reason, now, bound); err != nil {
		return 0, err
	}
	if _, err := execSQL(ctx, s, "UPDATE relationships SET execution_generation = ?, updated_at = ? WHERE relationship_id = ?", number, now, rid); err != nil {
		return 0, err
	}
	if err := journal(ctx, s, "generation_opened", rid, Obj{{Key: "generation", Value: number}, {Key: "reason", Value: reason}}, now); err != nil {
		return 0, err
	}
	_, err = execSQL(ctx, s, "INSERT INTO delivery_supersession (event_id, reason, noted_at, applied) SELECT d.event_id, 'stale_generation', ?, 0 FROM deliveries d JOIN events e ON e.event_id = d.event_id WHERE d.relationship_id = ? AND e.execution_generation < ? AND e.outcome NOT IN ('merge_turn_grant') AND d.state IN ('queued','sending','held_uncertain','dispatched','deferred_busy','withheld_pre_send','inbox_only') ON CONFLICT(event_id) DO NOTHING", now, rid, number)
	return number, err
}

// RestorationOf is restoration_of.
func (a *Ack) RestorationOf(ctx context.Context, eventID string) (Obj, error) {
	row, err := one(ctx, a.Store, "SELECT detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq DESC LIMIT 1", "restoration_projected", eventID)
	if err != nil {
		return nil, err
	}
	if row == nil || row.S("detail") == "" {
		return Obj{{Key: "outcome", Value: "unmeasured"}, {Key: "basis", Value: nil}, {Key: "criterion", Value: nil}, {Key: "detail", Value: "this ruling was recorded before the relay measured restoration delivery"}}, nil
	}
	return loadsObj(row.S("detail")), nil
}

func pendingBackoff(attemptNo int64) float64 {
	return math.Min(900, 30*math.Pow(2, float64(max(0, attemptNo-1))))
}

func (a *Ack) pendingFingerprint(ctx context.Context, eventID string, delivery Row) (string, error) {
	event, err := a.Delivery.eventRow(ctx, eventID)
	if err != nil {
		return "", err
	}
	var rel Row
	if event != nil {
		if rel, err = one(ctx, a.Store, "SELECT status, execution_generation FROM relationships WHERE relationship_id = ?", event.S("relationship_id")); err != nil {
			return "", err
		}
	}
	parts := []string{"absent", "absent", "0", "0"}
	if delivery != nil {
		parts[0] = delivery.S("state")
	}
	if rel != nil {
		parts[1], parts[2] = rel.S("status"), fmt.Sprint(rel.I("execution_generation"))
	}
	if event != nil {
		parts[3] = fmt.Sprint(event.I("execution_generation"))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:]), nil
}

// pendingAcksSQL is the read VerifyPendingAcks makes, on every daemon tick: the acknowledgements still waiting for
// their turn to be verified, the ones due a look first. It takes now, DeliveryUnconfirmed, Dispatched, InboxOnly and
// the limit. Almost every acknowledgement is verified, so the statement reaches the few that are through the partial
// index acks_unverified (CRW-301), which SQLite can use only because the statement spells 'unverified_turn' as a
// literal: do not bind it.
const pendingAcksSQL = "SELECT a.event_id, a.ack_turn_id, a.ack_at, a.accepted, COALESCE(e.attempts, 0) AS attempts, e.last_reason, e.fingerprint, e.next_check_at FROM acks a LEFT JOIN ack_evidence e ON e.event_id = a.event_id LEFT JOIN deliveries d ON d.event_id = a.event_id WHERE a.verified = 'unverified_turn' AND (e.next_check_at IS NULL OR e.next_check_at <= ? OR (e.last_reason = ? AND d.state IN (?, ?))) ORDER BY COALESCE(e.next_check_at, 0), a.event_id LIMIT ?"

// VerifyPendingAcks is verify_pending_acks: complete acknowledgements authored without a host,
// re-checking disposition as acknowledge does.
func (a *Ack) VerifyPendingAcks(ctx context.Context, adapter Adapter, limit int, nowp *float64) ([]any, error) {
	now := a.Clock.Now()
	if nowp != nil {
		now = *nowp
	}
	if limit == 0 {
		limit = 8
	}
	pending, err := all(ctx, a.Store, pendingAcksSQL, now, DeliveryUnconfirmed, Dispatched, InboxOnly, limit)
	if err != nil {
		return nil, err
	}
	results := []any{}
	for _, p := range pending {
		eventID := p.S("event_id")
		row, err := a.Delivery.Find(ctx, eventID)
		if err != nil {
			return nil, err
		}
		if row == nil {
			continue
		}
		verification, err := a.verifyAckTurn(ctx, row, p.S("ack_turn_id"), adapter)
		if err != nil {
			if Reason(err) == "" {
				return nil, err
			}
			verification = Reason(err)
		}
		result, err := a.settlePendingAck(ctx, eventID, p, verification, now, row)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func (a *Ack) settlePendingAck(ctx context.Context, eventID string, pending Row, verification string, now float64, basisRow Row) (Obj, error) {
	stamp := a.Clock.ISO()
	var out Obj
	err := a.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		fresh, err := one(ctx, a.Store, "SELECT * FROM acks WHERE event_id = ?", eventID)
		if err != nil {
			return err
		}
		if fresh == nil || fresh.S("verified") == "verified" {
			out = Obj{{Key: "eventId", Value: eventID}, {Key: "outcome", Value: "already_settled"}}
			return nil
		}
		if fresh.S("ack_turn_id") != pending.S("ack_turn_id") || fresh.S("ack_at") != pending.S("ack_at") {
			out = Obj{{Key: "eventId", Value: eventID}, {Key: "outcome", Value: "replaced"}}
			return nil
		}
		delivery, err := a.Delivery.Find(ctx, eventID)
		if err != nil {
			return err
		}
		if basisRow != nil && delivery != nil && basis(delivery) != basis(basisRow) {
			out = Obj{{Key: "eventId", Value: eventID}, {Key: "outcome", Value: "changed"}}
			return nil
		}
		blocker := ""
		switch {
		case delivery != nil && slices.Contains(unconfirmed, delivery.S("state")):
			blocker = DeliveryUnconfirmed
		case delivery == nil || !slices.Contains([]string{Dispatched, InboxOnly, Acknowledged}, delivery.S("state")):
			blocker = "delivery_state_changed"
		default:
			computed, err := a.Evaluate(ctx, eventID)
			if Reason(err) != "" {
				computed = Reason(err)
			} else if err != nil {
				return err
			}
			if fresh.I("accepted") != 0 && computed != "" {
				blocker = computed
			}
		}
		if blocker == "" && delivery != nil {
			current, err := one(ctx, a.Store, "SELECT sent_at FROM attempts WHERE event_id = ? AND attempt_no = ?", eventID, delivery.I("attempt_count"))
			if err != nil {
				return err
			}
			if current != nil && !current.N("sent_at") && fresh.S("ack_at") < current.S("sent_at") {
				blocker = AckPredatesAttempt
			}
		}
		fingerprint, err := a.pendingFingerprint(ctx, eventID, delivery)
		if err != nil {
			return err
		}
		if blocker == "" && verification == "verified" {
			if _, err := execSQL(ctx, a.Store, "UPDATE acks SET verified = 'verified' WHERE event_id = ?", eventID); err != nil {
				return err
			}
			if _, err := execSQL(ctx, a.Store, "UPDATE deliveries SET state = ?, updated_at = ? WHERE event_id = ?", Acknowledged, stamp, eventID); err != nil {
				return err
			}
			if err := a.writeAckEvidence(ctx, eventID, "host_read", nil, nil, fingerprint, nil, stamp, true); err != nil {
				return err
			}
			out = Obj{{Key: "eventId", Value: eventID}, {Key: "outcome", Value: "verified"}}
			return journal(ctx, a.Store, "ack_verified", eventID, Obj{{Key: "tier", Value: "host_read"}}, stamp)
		}
		reason := blocker
		if reason == "" {
			reason = verification
		}
		unchanged := pending.Opt("last_reason") == reason && pending.Opt("fingerprint") == fingerprint
		if err := a.writeAckEvidence(ctx, eventID, "unverified", "the acknowledgement stands exactly as authored; it was not promoted", reason, fingerprint, now+pendingBackoff(pending.I("attempts")+1), stamp, true); err != nil {
			return err
		}
		out = Obj{{Key: "eventId", Value: eventID}, {Key: "outcome", Value: "withheld"}, {Key: "reason", Value: reason}}
		if !unchanged {
			return journal(ctx, a.Store, "ack_verification_withheld", eventID, Obj{{Key: "reason", Value: reason}}, stamp)
		}
		return nil
	})
	return out, err
}

// BindDispatchedRevision is bind_dispatched_revision.
func (a *Ack) BindDispatchedRevision(ctx context.Context, revisionEvent string) (Obj, error) {
	row, err := a.Delivery.Find(ctx, revisionEvent)
	if err != nil || row == nil || !slices.Contains([]string{Dispatched, Acknowledged}, row.S("state")) || !truthy(row.Opt("dispatch_turn_id")) {
		return nil, err
	}
	event, err := a.Delivery.eventRow(ctx, revisionEvent)
	if err != nil {
		return nil, err
	}
	if keepsAnchor(event) {
		return nil, nil
	}
	return BindAnchor(ctx, a.Store, a.Clock, event.S("relationship_id"), event.I("execution_generation"), row.S("dispatch_turn_id"))
}

// BindAnchor is registry.bind_anchor from a dispatch receipt: idempotent, never rebinding.
func BindAnchor(ctx context.Context, s *store.Store, clock Clock, rid string, number int64, turn string) (Obj, error) {
	if strings.TrimSpace(turn) == "" {
		return nil, refuse("unbound_generation", "an anchor needs an exact dispatch turn id")
	}
	var bound Obj
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, err := LoadRelationship(ctx, s, rid)
		if err != nil {
			return err
		}
		current := r.generation(number)
		if current == nil {
			return refuse(UnknownGeneration, "%s has no generation %d", strconv.Quote(rid), number)
		}
		if err := store.RefuseWithdrawn(ctx, s.Q(ctx), rid, number); err != nil {
			return err
		}
		if current.S("anchor_state") == "bound" {
			if current.S("dispatch_turn_id") == turn {
				bound = generationRecord(current)
				return nil
			}
			return refuse("anchor_already_bound", "generation %d is already bound to %s", number, quote.Value(current.Opt("dispatch_turn_id")))
		}
		now := clock.ISO()
		if _, err := execSQL(ctx, s, "UPDATE generations SET anchor_state = ?, dispatch_turn_id = ?, bound_at = ? WHERE relationship_id = ? AND execution_generation = ?", "bound", turn, now, rid, number); err != nil {
			return err
		}
		if err := journal(ctx, s, "anchor_bound", rid, Obj{{Key: "generation", Value: number}}, now); err != nil {
			return err
		}
		current, err = one(ctx, s, "SELECT * FROM generations WHERE relationship_id = ? AND execution_generation = ?", rid, number)
		if err == nil {
			bound = generationRecord(current)
		}
		return err
	})
	return bound, err
}

func generationRecord(g Row) Obj {
	return Obj{{Key: "relationshipId", Value: g.S("relationship_id")}, {Key: "executionGeneration", Value: g.I("execution_generation")}, {Key: "dispatchRequestId", Value: g.S("dispatch_request_id")}, {Key: "anchorState", Value: g.S("anchor_state")},
		{Key: "dispatchTurnId", Value: g.Opt("dispatch_turn_id")}, {Key: "openedAt", Value: g.S("opened_at")}, {Key: "boundAt", Value: g.Opt("bound_at")}, {Key: "reason", Value: g.Opt("reason")}}
}

// pendingAnchorsSQL reads the dispatched revision requests whose generation still waits for its anchor,
// oldest update first; its arguments are Revision, Dispatched, Acknowledged, "anchor_pending" and a limit.
//
// It starts from the generations (CROSS JOIN keeps the tables in the order written) and reaches the events
// of those that wait by events_relationship, then each delivery by its key. The product never runs ANALYZE,
// so left to choose, SQLite starts from the deliveries in state IN (dispatched, acknowledged), which is
// nearly every delivery the store has kept, and looks each one's generation up: on every tick, twice.
const pendingAnchorsSQL = "SELECT d.event_id FROM generations g CROSS JOIN events e ON e.relationship_id = g.relationship_id AND e.execution_generation = g.execution_generation CROSS JOIN deliveries d ON d.event_id = e.event_id WHERE d.kind = ? AND d.state IN (?,?) AND d.dispatch_turn_id IS NOT NULL AND g.anchor_state = ? ORDER BY d.updated_at LIMIT ?"

// pendingAnchorsLimit is how many anchors one recovery pass binds.
const pendingAnchorsLimit = 50

// BindPendingAnchors is bind_pending_anchors: recovery over state, never moving a bound anchor.
func (a *Ack) BindPendingAnchors(ctx context.Context) ([]any, error) {
	rows, err := a.pendingAnchors(ctx)
	if err != nil {
		return nil, err
	}
	bound := []any{}
	for _, r := range rows {
		result, err := a.BindDispatchedRevision(ctx, r.S("event_id"))
		var refused *Refused
		if errors.As(err, &refused) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if result != nil {
			bound = append(bound, r.S("event_id"))
		}
	}
	return bound, nil
}

// pendingAnchors is the read of BindPendingAnchors: the events of the revision requests it would bind, in order.
func (a *Ack) pendingAnchors(ctx context.Context) ([]Row, error) {
	return all(ctx, a.Store, pendingAnchorsSQL, Revision, Dispatched, Acknowledged, "anchor_pending", pendingAnchorsLimit)
}
