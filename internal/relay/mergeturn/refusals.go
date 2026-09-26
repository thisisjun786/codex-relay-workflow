package mergeturn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// row is MergeTurn._row_in: the turn, or unregistered_scope when there is none.
func (s *Service) row(ctx context.Context, turn string) (store.MergeTurnsRow, error) {
	r, err := s.Store.MergeTurn(ctx, turn)
	if errors.Is(err, sql.ErrNoRows) {
		return r, &store.RefusedError{Reason: string(contract.RefusalUnregisteredScope), Detail: "no merge turn " + pyRepr(turn)}
	}
	return r, err
}

func coordination(r store.MergeTurnsRow, reason contract.RefusalReason, detail, incumbent, challenger string) *registry.CoordinationRefusal {
	return &registry.CoordinationRefusal{Reason: reason, Detail: detail, Domain: registry.DomainMergeTarget, Subject: r.TargetKey, Incumbent: incumbent, Challenger: challenger}
}

// notHolder is MergeTurn._not_holder.
func notHolder(r store.MergeTurnsRow, actor, what string) *registry.CoordinationRefusal {
	return coordination(r, contract.RefusalMergeTurnNotHeld, "task "+pyRepr(actor)+" does not hold turn "+pyRepr(r.TurnID)+", which belongs to "+pyRepr(r.HolderTaskID)+", so it cannot "+what+" it", r.HolderTaskID, actor)
}

// wrongState is MergeTurn._wrong_state.
func wrongState(r store.MergeTurnsRow, actor, what string) *registry.CoordinationRefusal {
	return coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyRepr(r.TurnID)+" is "+r.State+", which does not admit "+what, r.State, actor)
}

// staleOwner is MergeTurn._stale_owner; known is false for Python's None owner.
func staleOwner(r store.MergeTurnsRow, owner string, known bool, actor string) *registry.CoordinationRefusal {
	held := "None"
	if known {
		held = pyRepr(owner)
	}
	return coordination(r, contract.RefusalScopeRoleMismatch, "task "+pyRepr(actor)+" no longer owns project "+pyRepr(r.ProjectKey)+", which is held by "+held+"; the project changed hands after this claim was made", owner, actor)
}

// unresolved is MergeTurn._unresolved.
func unresolved(r store.MergeTurnsRow, actor string) *registry.CoordinationRefusal {
	return coordination(r, contract.RefusalMergeTurnUnresolved, "turn "+pyRepr(r.TurnID)+" is "+r.State+" on target "+pyRepr(r.TargetKey)+". Its holder may already have merged, so no amount of waiting and no cancellation releases it. Record the outcome with land or report_unknown, then resolve_unknown with an observation of the target", r.HolderTaskID, actor)
}

// paused is MergeTurn._paused.
func paused(r store.MergeTurnsRow, actor, what string) *registry.CoordinationRefusal {
	return coordination(r, contract.RefusalScopeRoleMismatch, "task "+pyRepr(actor)+" owns project "+pyRepr(r.ProjectKey)+" with a paused binding, so it keeps turn "+pyRepr(r.TurnID)+" and cannot "+what+" under it; resume the binding, or return the turn so a ready peer can proceed", r.HolderTaskID, actor)
}

// unreadableTarget is MergeTurn._unreadable.
func unreadableTarget(r store.MergeTurnsRow, actor, why, what string) *registry.CoordinationRefusal {
	return coordination(r, contract.RefusalMergeTargetUnreadable, "the base branch "+pyRepr(r.BaseRef)+" of "+pyRepr(r.Repository)+" was not read, so "+what+": "+why, r.TurnID, actor)
}

// mismatch is MergeTurn._mismatch.
func mismatch(r store.MergeTurnsRow, actor, stated string, tip Tip, what string, optional bool) *registry.CoordinationRefusal {
	tail := ""
	if optional {
		tail = ", or leave the statement out"
	}
	return coordination(r, contract.RefusalMergeBaseMismatch, "the base branch "+pyRepr(r.BaseRef)+" reads "+pyRepr(tip.SHA)+" ("+tip.Source+") and "+what+" states "+pyRepr(stated)+"; the relay records what the branch reads, so read it again and state it in full"+tail, tip.SHA, stated)
}

// parents is the task ids of the live parent bindings of a project, in owners() order.
func (s *Service) parents(ctx context.Context, project string) ([]string, error) {
	owners, err := s.Registry.Owners(ctx, "project", project)
	if err != nil {
		return nil, err
	}
	var held []string
	for _, o := range owners {
		if field(o, "role") == "parent" {
			held = append(held, fmt.Sprint(field(o, "taskId")))
		}
	}
	return held, nil
}

// ownerStatus is MergeTurn._owner_status: the single live parent's status, or "" for None.
func (s *Service) ownerStatus(ctx context.Context, project string) (string, error) {
	owners, err := s.Registry.Owners(ctx, "project", project)
	if err != nil {
		return "", err
	}
	var held []string
	for _, o := range owners {
		if field(o, "role") == "parent" {
			status, _ := field(o, "status").(string)
			held = append(held, status)
		}
	}
	if len(held) != 1 {
		return "", nil
	}
	return held[0], nil
}

// currentGrant is MergeTurn._current_grant_in: the newest recognised grant's id, or "".
func (s *Service) currentGrant(ctx context.Context, turn string, tenure int64) (string, error) {
	entries, err := s.Store.MergeLedger(ctx, turn)
	if err != nil {
		return "", err
	}
	current := ""
	var last int64
	for _, entry := range entries {
		if g := recognizedGrant(entry, turn, tenure); g != nil {
			if seq := grantSequence(g); current == "" || seq > last {
				last = seq
				current = g["grantId"].(string)
			}
		}
	}
	return current, nil
}

// unansweredGrant is MergeTurn._unanswered_grant: a grant nobody answered is a gate.
func (s *Service) unansweredGrant(ctx context.Context, r store.MergeTurnsRow, actor string) (*registry.CoordinationRefusal, error) {
	current, err := s.currentGrant(ctx, r.TurnID, r.Tenure)
	if err != nil || current == "" {
		return nil, err
	}
	_, err = s.Store.MergeLedgerEntry(ctx, r.TurnID, "grant_acknowledged:"+current)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return coordination(r, contract.RefusalMergeTurnNotHeld, "turn "+pyRepr(r.TurnID)+" was granted "+pyRepr(current)+" and has not acknowledged it, so nothing records that this candidate was re-checked against the store rather than against what its holder remembers. Acknowledge the grant, then merge", current, actor), nil
}

// latestLanding is MergeTurn._latest_landing_in: the landed turn whose recorded base the next
// candidate must restate, last by the order the closes were written.
func (s *Service) latestLanding(ctx context.Context, target string) (store.Row, error) {
	return s.Store.One(ctx, "SELECT m.turn_id, m.observed_base_sha, m.holder_task_id, m.project_key"+
		"  FROM merge_turns m LEFT JOIN journal j"+
		"    ON j.subject = m.turn_id AND j.kind = 'merge_turn_closed'"+
		"  WHERE m.target_key = ? AND m.state = 'landed' AND m.observed_base_sha IS NOT NULL"+
		"  ORDER BY COALESCE(j.seq, -1) DESC, m.closed_at DESC, m.turn_id DESC LIMIT 1", target)
}
