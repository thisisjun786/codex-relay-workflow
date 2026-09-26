package mergeturn

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (s *Service) close(ctx context.Context, r store.MergeTurnsRow, state, reason, actor, at string) error {
	return s.closeWith(ctx, r, state, reason, actor, at, sql.NullString{}, sql.NullString{})
}

// closeWith is MergeTurn._close_in.
func (s *Service) closeWith(ctx context.Context, r store.MergeTurnsRow, state, reason, actor, at string, landed, observedBase sql.NullString) error {
	if err := s.Store.CloseMergeTurn(ctx, r.TurnID, state, reason, at, landed, observedBase); err != nil {
		return err
	}
	if err := s.ledger(ctx, r.TurnID, "transition", r.State, state, "close", actor, reason, "close:"+state, at); err != nil {
		return err
	}
	return s.journalOutcome(ctx, "merge_turn_closed", r.TurnID, contract.OrderedObject{{Key: "state", Value: state}, {Key: "reason", Value: reason}, {Key: "actor", Value: actor}}, at)
}
func (s *Service) promote(ctx context.Context, target, at string) (string, error) {
	waiters, err := s.Store.ReadyMergeWaiters(ctx, target)
	if err != nil {
		return "", err
	}
	for _, candidate := range waiters {
		owner, refusal, err := s.ownership(ctx, candidate.ProjectKey, target, candidate.HolderTaskID)
		if err != nil {
			return "", err
		}
		if refusal == nil && owner == candidate.HolderTaskID {
			status, err := s.ownerStatus(ctx, candidate.ProjectKey)
			if err != nil {
				return "", err
			}
			if status != "active" {
				continue
			}
			if err = s.Store.PromoteMergeTurn(ctx, candidate.TurnID, Holding, at); err != nil {
				return "", err
			}
			if err = s.ledger(ctx, candidate.TurnID, "transition", Waiting, Holding, "promoted", candidate.HolderTaskID, "promoted when the target was released", fmt.Sprintf("promote:%d", candidate.Tenure), at); err != nil {
				return "", err
			}
			candidate.State = Holding
			if err = s.grant(ctx, candidate, "promotion", at); err != nil {
				return "", err
			}
			return candidate.TurnID, nil
		}
		stale := refusal
		if stale == nil {
			stale = staleOwner(candidate, owner, true, candidate.HolderTaskID)
		}
		if err = s.close(ctx, candidate, "withdrawn", "withdrawn at promotion: "+string(stale.Reason), candidate.HolderTaskID, at); err != nil {
			return "", err
		}
		if err = s.Registry.RecordCoordinationConflict(ctx, *stale, at); err != nil {
			return "", err
		}
	}
	return "", nil
}

func (s *Service) Release(ctx context.Context, turn, actor, disposition, reason, evidence string) (map[string]any, error) {
	if disposition != "returned" && disposition != "cancelled" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalLinkNotActive), Detail: "a disposition is returned or cancelled, not " + pyRepr(disposition) + "; a landing is recorded with land, not chosen here"}
	}
	if strings.TrimSpace(reason) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "a release states why"}
	}
	if disposition == "cancelled" && strings.TrimSpace(evidence) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "taking a turn away from its holder requires evidence, because the holder is not the one saying it is finished"}
	}
	at := s.now()
	var promoted string
	var refusal *registry.CoordinationRefusal
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		switch {
		case r.State == Merging || r.State == Unknown:
			refusal = unresolved(r, actor)
		case r.State == Waiting:
			refusal = wrongState(r, actor, "release; a waiting claim is withdrawn")
		case r.State != Holding:
			refusal = wrongState(r, actor, "release")
		case disposition == "returned" && r.HolderTaskID != actor:
			refusal = notHolder(r, actor, "return")
		case disposition == "cancelled":
			if refusal, err = s.authority(tx, r, actor, "take the turn away"); err != nil {
				return err
			}
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		if disposition == "cancelled" {
			reason += "; " + evidence
		}
		if err = s.close(tx, r, disposition, reason, actor, at); err != nil {
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
	var next any
	if promoted != "" {
		next, err = s.Turn(ctx, promoted)
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{"released": released, "promoted": next, "ledger": released["ledger"]}, nil
}

func (s *Service) Ready(ctx context.Context, turn, actor string, ready bool, head, cause string) (map[string]any, error) {
	at := s.now()
	var blocked any
	var refusal *registry.CoordinationRefusal
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		if r.HolderTaskID != actor {
			refusal = notHolder(r, actor, "declare readiness on")
		} else if r.State != Waiting && r.State != Holding {
			refusal = wrongState(r, actor, "declare readiness on")
		}
		if refusal == nil {
			owner, other, err := s.ownership(tx, r.ProjectKey, r.TargetKey, actor)
			if err != nil {
				return err
			}
			refusal = other
			if refusal == nil && owner != actor {
				refusal = staleOwner(r, owner, true, actor)
			}
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		if head == "" {
			head = r.CandidateHead
		}
		moved := head != r.CandidateHead
		flag := int64(0)
		if ready && !moved {
			flag = 1
		}
		state := r.State
		held := r.HeldAt
		if moved {
			if err = s.ledger(tx, turn, "transition", state, state, "candidate_head_changed", actor, r.CandidateHead+" -> "+head, "head:"+head, at); err != nil {
				return err
			}
			if state == Holding {
				r.CandidateHead = head
				if err = s.grant(tx, r, "candidate_restated", at); err != nil {
					return err
				}
			}
		}
		if flag != r.DeclaredReady {
			entries, err := s.Store.MergeLedger(tx, turn)
			if err != nil {
				return err
			}
			sequence := 1
			for _, entry := range entries {
				if entry.EvidenceKind == "readiness_declared" || entry.EvidenceKind == "readiness_withdrawn" {
					sequence++
				}
			}
			kind := "readiness_withdrawn"
			text := "readiness withdrawn on " + head
			if flag == 1 {
				kind = "readiness_declared"
				text = "declared ready on " + head
			}
			if moved {
				text = "the head moved to " + head
			}
			if cause != "" {
				text = cause
			}
			if err = s.ledger(tx, turn, "transition", state, state, kind, actor, text, fmt.Sprintf("ready:%d:%d", r.Tenure, sequence), at); err != nil {
				return err
			}
		}
		if state == Waiting && flag == 1 {
			occupant, err := s.Store.MergeTargetOccupant(tx, r.TargetKey)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if occupant.TurnID != "" {
				blocked = map[string]any{"state": occupant.State, "turnId": occupant.TurnID}
			} else {
				owners, err := s.Registry.Owners(tx, "project", r.ProjectKey)
				if err != nil {
					return err
				}
				paused := false
				for _, o := range owners {
					for _, f := range o {
						if f.Key == "status" && f.Value != "active" {
							paused = true
						}
					}
				}
				if paused {
					blocked = map[string]any{"state": "owner_paused", "turnId": nil}
				} else {
					state = Holding
					held = nullable(at)
					if err = s.ledger(tx, turn, "transition", Waiting, Holding, "took_free_target", actor, "declared ready while the target was free", fmt.Sprintf("take:%d", r.Tenure), at); err != nil {
						return err
					}
					r.State = Holding
					r.CandidateHead = head
					if err = s.grant(tx, r, "late_ready", at); err != nil {
						return err
					}
				}
			}
		}
		return s.Store.DeclareMergeReadiness(tx, turn, flag, head, state, held, at)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	answer, err := s.Turn(ctx, turn)
	if err != nil {
		return nil, err
	}
	answer["blockedBy"] = blocked
	return answer, nil
}

// supervisorOf is MergeTurn._supervisor_of: the initiative supervisor above a project, or ""
// when the walk cannot say (a handover, a contested scope or an unreadable store).
func (s *Service) supervisorOf(ctx context.Context, project string) (string, error) {
	owners, err := s.Registry.Owners(ctx, "project", project)
	if err != nil {
		return "", err
	}
	var parents []string
	for _, o := range owners {
		if field(o, "role") == "parent" {
			parents = append(parents, fmt.Sprint(field(o, "taskId")))
		}
	}
	if len(parents) != 1 {
		return "", nil
	}
	walk := s.Registry.Up(ctx, registry.UpSelector{Task: nullable(parents[0]), Scope: nullable(project)})
	if field(walk, "readable") != true || field(walk, "state") == "ambiguous" {
		return "", nil
	}
	levels, _ := field(walk, "levels").([]any)
	for _, item := range levels {
		level, _ := item.(contract.OrderedObject)
		if field(level, "scopeKind") == "initiative" {
			owner, _ := field(level, "owner").(contract.OrderedObject)
			task, _ := field(owner, "taskId").(string)
			return task, nil
		}
	}
	return "", nil
}

// authority is MergeTurn._authority_refusal: only the holder, or the supervisor above its
// project, may act on a held turn.
func (s *Service) authority(ctx context.Context, r store.MergeTurnsRow, actor, what string) (*registry.CoordinationRefusal, error) {
	if actor == r.HolderTaskID {
		return nil, nil
	}
	supervisor, err := s.supervisorOf(ctx, r.ProjectKey)
	if err != nil {
		return nil, err
	}
	if supervisor != "" && actor == supervisor {
		return nil, nil
	}
	which := ", and that project has no readable supervisor"
	if supervisor != "" {
		which = ", which is " + pyRepr(supervisor)
	}
	return &registry.CoordinationRefusal{Reason: contract.RefusalScopeRoleMismatch, Detail: "task " + pyRepr(actor) + " is neither the holder of turn " + pyRepr(r.TurnID) + " nor the supervisor above project " + pyRepr(r.ProjectKey) + which + ", so it cannot " + what, Domain: registry.DomainMergeTarget, Subject: r.TargetKey, Incumbent: r.HolderTaskID, Challenger: actor}, nil
}

func field(o contract.OrderedObject, key string) any {
	for _, f := range o {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// Withdraw is MergeTurn.withdraw: a waiting claim taken back by its claimant.
func (s *Service) Withdraw(ctx context.Context, turn, actor string) (map[string]any, error) {
	at := s.now()
	var refusal *registry.CoordinationRefusal
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, err := s.row(tx, turn)
		if err != nil {
			return err
		}
		if r.HolderTaskID != actor {
			refusal = notHolder(r, actor, "withdraw")
		} else if r.State != Waiting {
			refusal = wrongState(r, actor, "withdraw")
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		return s.close(tx, r, "withdrawn", "withdrawn by its claimant", actor, at)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	return s.Turn(ctx, turn)
}
