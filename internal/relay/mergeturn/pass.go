package mergeturn

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Passing a stalled turn on (CRW-408).
//
// A holder that went silent keeps the target occupied until somebody takes it away. The route is
// narrow on purpose. Only a turn that is still holding can be passed: a holder that follows the
// protocol runs merge-turn-check, which moves the turn to merging, before it merges, so a holding
// turn has not begun a merge by the protocol. That is a statement about the relay's record, not
// proof that the pull request is unmerged, which is why a passer reads the pull request. A merging
// or unknown turn is never passed (its holder may already have merged, and elapsed time is not an
// observation); it leaves through land, or unknown and resolve. Only the supervisor above the
// holder's project, or a parent with a waiting claim on the same target, may pass a turn, and only
// once the relay's own ledger says the holder has been silent for the holding limit.

// passRefusal is nil when actor may pass a turn on: the supervisor above its project, or the
// parent of a live waiting claim on the same target that still owns its project.
func (s *Service) passRefusal(ctx context.Context, r store.MergeTurnsRow, actor string) (*registry.CoordinationRefusal, error) {
	supervisor, err := s.supervisorOf(ctx, r.ProjectKey)
	if err != nil {
		return nil, err
	}
	if supervisor != "" && actor == supervisor {
		return nil, nil
	}
	claims, err := s.Store.MergeTurnsForTarget(ctx, r.TargetKey)
	if err != nil {
		return nil, err
	}
	for _, claim := range claims {
		if claim.State != Waiting || claim.HolderTaskID != actor {
			continue
		}
		owner, refusal, err := s.ownership(ctx, claim.ProjectKey, r.TargetKey, actor)
		if err != nil {
			return nil, err
		}
		if refusal == nil && owner == actor {
			return nil, nil
		}
	}
	which := ", and that project has no readable supervisor"
	if supervisor != "" {
		which = ", which is " + pyvalue.StrRepr(supervisor)
	}
	return coordination(r, contract.RefusalScopeRoleMismatch, "task "+pyvalue.StrRepr(actor)+" is neither the supervisor above project "+pyvalue.StrRepr(r.ProjectKey)+which+" nor the parent of a waiting claim on target "+pyvalue.StrRepr(r.TargetKey)+" that still owns its project, so it cannot pass turn "+pyvalue.StrRepr(r.TurnID)+" on", r.HolderTaskID, actor), nil
}

// Pass takes a stalled holding turn from its silent holder and gives the target to the next
// ready waiter, keeping every value the turn had and recording who passed it and why.
func (s *Service) Pass(ctx context.Context, turn, actor, evidence string) (map[string]any, error) {
	if strings.TrimSpace(evidence) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "passing a turn on states what was observed: it takes the target from a holder that did not release it, so the reason is the record of why"}
	}
	at := s.now()
	var promoted string
	var refusal *registry.CoordinationRefusal
	var passed map[string]any
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		switch {
		case r.State == Merging || r.State == Unknown:
			refusal = unresolved(r, actor)
		case r.State != Holding:
			refusal = wrongState(r, actor, "being passed on")
		}
		if refusal == nil {
			if refusal, err = s.passRefusal(tx, r, actor); err != nil {
				return err
			}
		}
		var silence stall
		if refusal == nil {
			entries, err := s.Store.MergeLedger(tx, turn)
			if err != nil {
				return err
			}
			// Read inside the transaction: a progress record committed before it is seen here.
			silence = stallOf(r, entries, at)
			switch {
			case !silence.Readable:
				refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyvalue.StrRepr(turn)+" is holding but its last progress time "+pyvalue.StrRepr(silence.Last.At)+" cannot be read, so it cannot be judged stalled against the holding limit of "+fmt.Sprint(HoldingLimitSeconds)+" s and is not passed on", r.HolderTaskID, actor)
			case !silence.Stalled:
				refusal = coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyvalue.StrRepr(turn)+" is holding and its holder "+pyvalue.StrRepr(r.HolderTaskID)+" last made progress at "+silence.Last.At+" ("+silence.Last.describe()+"), "+fmt.Sprint(silence.ElapsedSeconds)+" s ago; the holding limit is "+fmt.Sprint(HoldingLimitSeconds)+" s, so the turn is not stalled and is not passed on before "+silence.StallsAt, r.HolderTaskID, actor)
			}
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		var heldAt, step any
		if r.HeldAt.Valid {
			heldAt = r.HeldAt.String
		}
		if silence.Last.Step != "" {
			step = silence.Last.Step
		}
		passed = map[string]any{"candidateHead": r.CandidateHead, "elapsedSeconds": silence.ElapsedSeconds, "evidence": evidence, "heldAt": heldAt, "holder": r.HolderTaskID, "lastProgressAt": silence.Last.At, "lastProgressKind": silence.Last.Kind, "lastProgressStep": step, "limitSeconds": int64(HoldingLimitSeconds), "passedBy": actor, "turnId": turn}
		if err = s.ledger(tx, turn, "attestation", Holding, "", "turn_passed", actor, canonicalJSON(passed), fmt.Sprintf("pass:%d", r.Tenure), at); err != nil {
			return err
		}
		reason := "passed on to the next waiter by " + actor + " after " + fmt.Sprint(silence.ElapsedSeconds) + " s without progress (holding limit " + fmt.Sprint(HoldingLimitSeconds) + " s; last progress " + silence.Last.describe() + " at " + silence.Last.At + "): " + evidence
		if err = s.close(tx, r, Passed, reason, actor, at); err != nil {
			return err
		}
		promoted, err = s.promote(tx, r.TargetKey, at)
		return err
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	released, err := s.Turn(ctx, turn)
	if err != nil {
		return nil, err
	}
	next, err := s.turnOrNil(ctx, promoted)
	if err != nil {
		return nil, err
	}
	return map[string]any{"released": released, "promoted": next, "passed": passed, "ledger": released["ledger"]}, nil
}
