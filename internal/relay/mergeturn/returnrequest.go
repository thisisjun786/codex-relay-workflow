package mergeturn

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Delivering a return request to the holder (CRW-408).
//
// merge-turn-request-return records somebody asking for the target back, and now also queues a
// notice to the holder through the delivery engine, which wakes the holder's thread when it is
// idle. The notice travels the channel a grant uses: the same delivery kind, recipient, address
// and sender, told apart by the kind inside its receipt (ReturnRequestKind). Asking changes no
// state: only the holder releases a turn (or a stalled one is passed on, see pass.go).

// ReturnRequestKind is the receipt kind of a return request notice.
const ReturnRequestKind = "merge_turn_return_request"

// ReturnRequestLive is how status and the delivery snapshot name a return request notice that is
// still current, in the place a grant notice's supersession reading stands.
const ReturnRequestLive = "merge_turn_return_request_live"

// ReturnRequestID identifies one requester's request about one turn: a replay converges on it.
func ReturnRequestID(turn, requester string) string { return key("mtr", turn, requester) }

// IsReturnRequestReceipt reports whether a notice's receipt is a return request.
func IsReturnRequestReceipt(receipt string) bool {
	envelope := decode(receipt)
	kind, _ := envelope["kind"].(string)
	return kind == ReturnRequestKind
}

// RequestReturn records that actor asked for turn back and queues the notice to its holder, in
// one transaction: a notice that cannot be queued leaves no request behind. A replay (the same
// requester asking again about the same turn) keeps the original request and its notice.
func (s *Service) RequestReturn(ctx context.Context, turn, actor, evidence string) (map[string]any, error) {
	at := s.now()
	var notice map[string]any
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		identity := "return_requested:" + actor
		stated := evidence
		prior, err := s.Store.MergeLedgerEntry(tx, turn, identity)
		replay := err == nil
		switch {
		case replay:
			stated = prior.Evidence
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if err = s.ledger(tx, turn, "attestation", r.State, "", "return_requested", actor, stated, identity, at); err != nil {
			return err
		}
		if r.State != Holding && r.State != Merging && r.State != Unknown {
			notice = map[string]any{"state": "not_sent", "reason": "turn " + r.TurnID + " is " + r.State + ", so it holds nothing to give back and nobody is woken"}
			return nil
		}
		requestID := ReturnRequestID(turn, actor)
		// A notice already queued for this request is reported as it is, whatever state its
		// assignment is in now: the channel only decides whether a notice can be queued.
		if r.RelationshipID.Valid && r.RelationshipID.String != "" {
			existing := GrantEventID(r.RelationshipID.String, requestID)
			queued, err := s.Store.One(tx, "SELECT 1 FROM events WHERE event_id = ?", existing)
			if err != nil {
				return err
			}
			if queued != nil {
				notice = map[string]any{"state": "queued", "eventId": existing, "requestId": requestID}
				return nil
			}
		}
		refused, eventID := "no delivery channel is configured", ""
		if s.Delivery != nil {
			if refused, eventID, err = s.Delivery.Channel(tx, r.RelationshipID, r.HolderTaskID, requestID, r.ProjectKey); err != nil {
				return err
			}
		}
		if eventID == "" {
			notice = map[string]any{"state": "unaddressed", "reason": refused, "requestId": requestID}
			if replay {
				return nil
			}
			return s.journalOutcome(tx, "merge_turn_wake_unaddressed", turn, contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "recipientTaskId", Value: r.HolderTaskID}, {Key: "reason", Value: refused}}, at)
		}
		notice = map[string]any{"state": "queued", "eventId": eventID, "requestId": requestID}
		receipt := canonicalJSON(map[string]any{"kind": ReturnRequestKind, "requestId": requestID, "turnId": r.TurnID, "tenure": r.Tenure, "targetKey": r.TargetKey, "repository": r.Repository, "baseRef": r.BaseRef, "recipientTaskId": r.HolderTaskID, "candidateHead": r.CandidateHead, "requestedBy": actor, "evidence": stated, "holdingLimitSeconds": int64(HoldingLimitSeconds), "wake": map[string]any{"eventId": eventID}})
		if err = s.Delivery.Queue(tx, eventID, r.RelationshipID.String, r.HolderTaskID, receipt, requestID, at); err != nil {
			return err
		}
		return s.journalOutcome(tx, "merge_turn_wake_queued", turn, contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "eventId", Value: eventID}, {Key: "recipientTaskId", Value: r.HolderTaskID}}, at)
	})
	if err != nil {
		return nil, err
	}
	answer, err := s.Turn(ctx, turn)
	if err != nil {
		return nil, err
	}
	answer["returnNotice"] = notice
	return answer, nil
}
