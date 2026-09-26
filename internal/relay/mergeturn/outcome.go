package mergeturn

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// baseWasRead is MergeTurn._base_was_read_in: whether the checked base is a relay reading.
func (s *Service) baseWasRead(ctx context.Context, r store.MergeTurnsRow) (bool, error) {
	entry, err := s.Store.MergeLedgerEntry(ctx, r.TurnID, "merging:"+r.CandidateHead)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if entry.EvidenceKind != "currency_confirmed" || !r.CheckedBaseSHA.Valid || entry.Kind != "transition" || entry.FromState.String != Holding || entry.ToState.String != Merging {
		return false, nil
	}
	var payload map[string]any
	if json.Unmarshal([]byte(entry.Evidence), &payload) != nil || payload == nil {
		return false, nil
	}
	return payload["baseRead"] == r.CheckedBaseSHA.String, nil
}

func observation(tip Tip) any {
	if tip.SHA == "" {
		return nil
	}
	return map[string]any{"sha": tip.SHA, "source": tip.Source, "reference": tip.Reference, "repository": tip.Repository}
}

// observed is dict(reading, at=where), json.dumps'd into the journal in the reading's order.
func observed(tip Tip, where string) contract.OrderedObject {
	return contract.OrderedObject{{Key: "sha", Value: tip.SHA}, {Key: "source", Value: tip.Source}, {Key: "reference", Value: tip.Reference}, {Key: "repository", Value: tip.Repository}, {Key: "at", Value: where}}
}

// Land is MergeTurn.land: confirm the merge landed and close the tenure in one transaction.
func (s *Service) Land(ctx context.Context, turn, actor, landed, stated, evidence string, reader Reader) (map[string]any, error) {
	if strings.TrimSpace(evidence) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "a landing states what was observed; the relay reads only where the base branch points, so the evidence is what makes the merge itself a fact"}
	}
	if _, err := registry.CoordinationExact(landed, "a landed sha"); err != nil {
		return nil, err
	}
	if stated != "" {
		if _, err := registry.CoordinationExact(stated, "an observed base sha"); err != nil {
			return nil, err
		}
	}
	early, err := s.Store.MergeTurn(ctx, turn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	why := "the turn was not merging under this holder when the call began, and changed during it; call again"
	tip := Tip{}
	if err == nil && early.HolderTaskID == actor && early.State == Merging {
		tip, why = readTarget(ctx, reader, early.Repository, early.BaseRef)
	}
	at := s.now()
	var refusal *registry.CoordinationRefusal
	var promoted string
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, e := s.row(tx, turn)
		if e != nil {
			return e
		}
		read := false
		if r.HolderTaskID == actor && r.State == Merging {
			if read, e = s.baseWasRead(tx, r); e != nil {
				return e
			}
		}
		switch {
		case r.HolderTaskID != actor:
			refusal = notHolder(r, actor, "record a landing on")
		case r.State != Merging:
			refusal = wrongState(r, actor, "recording a landing")
		case !read:
			refusal = coordination(r, contract.RefusalMergeEvidenceRequired, "turn "+pyRepr(turn)+" entered merging before the relay read its base, so its checked base "+reprOrNone(r.CheckedBaseSHA)+" was typed rather than read and the branch cannot show whether this merge landed. Report the outcome with merge-turn-unknown and resolve it from the pull request's state with merge-turn-resolve, which reads the branch", r.CheckedBaseSHA.String, actor)
		case tip.SHA == "":
			refusal = unreadableTarget(r, actor, why, "the base this landing leaves behind cannot be recorded; the turn stays merging")
		case r.CheckedBaseSHA.Valid && SameCommit(tip.SHA, r.CheckedBaseSHA.String) && !SameCommit(r.CheckedBaseSHA.String, r.CandidateHead):
			refusal = coordination(r, contract.RefusalMergeBaseNotAdvanced, "the base branch "+pyRepr(r.BaseRef)+" still reads "+pyRepr(tip.SHA)+", the base the currency check read before merging, so the merge of "+pyRepr(r.CandidateHead)+" is not on it. The turn stays merging: merge and land again, or read again if the forge has not caught up. If the merge changed nothing because the base already contained the candidate, record that with merge-turn-unknown and merge-turn-resolve --pr-state merged", r.CheckedBaseSHA.String, tip.SHA)
		case stated != "" && !SameCommit(stated, tip.SHA):
			refusal = mismatch(r, actor, stated, tip, "--observed-base-sha", true)
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		if e = s.closeWith(tx, r, "landed", evidence, actor, at, nullable(landed), nullable(tip.SHA)); e != nil {
			return e
		}
		if e = s.journalOutcome(tx, "merge_turn_base_observed", turn, observed(tip, "landing"), at); e != nil {
			return e
		}
		promoted, e = s.promote(tx, r.TargetKey, at)
		return e
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
	return map[string]any{"released": released, "promoted": next, "baseObservation": observation(tip), "ledger": released["ledger"]}, nil
}

func reprOrNone(v sql.NullString) string {
	if !v.Valid {
		return "None"
	}
	return pyRepr(v.String)
}

func (s *Service) turnOrNil(ctx context.Context, turn string) (any, error) {
	if turn == "" {
		return nil, nil
	}
	return s.Turn(ctx, turn)
}

// Resolve is MergeTurn.resolve_unknown: the only key to an unknown target is an observation.
func (s *Service) Resolve(ctx context.Context, turn, actor, stated, prState, evidence string, reader Reader) (map[string]any, error) {
	if strings.TrimSpace(evidence) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "resolving an unknown outcome requires the observation that resolves it; elapsed time is not one and never becomes one"}
	}
	if _, err := registry.CoordinationExact(stated, "an observed base sha"); err != nil {
		return nil, err
	}
	early, err := s.Store.MergeTurn(ctx, turn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	tip := Tip{}
	why := "the turn was not unknown when the call began, or the caller could not resolve it then, and it changed during the call; call again"
	if err == nil && early.State == Unknown && (prState == "merged" || prState == "open" || prState == "closed") {
		if refusal, e := s.authority(ctx, early, actor, "resolve its outcome"); e != nil {
			return nil, e
		} else if refusal == nil {
			tip, why = readTarget(ctx, reader, early.Repository, early.BaseRef)
		}
	}
	at := s.now()
	var refusal *registry.CoordinationRefusal
	var promoted string
	landed := false
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, e := s.row(tx, turn)
		if e != nil {
			return e
		}
		if r.State != Unknown {
			refusal = wrongState(r, actor, "resolving an unknown outcome")
		} else if refusal, e = s.authority(tx, r, actor, "resolve its outcome"); e != nil {
			return e
		}
		if refusal == nil {
			switch prState {
			case "merged":
				landed = true
			case "open", "closed":
			default:
				moved := ""
				if r.CheckedBaseSHA.Valid && stated != r.CheckedBaseSHA.String {
					moved = "; the base moved from " + pyRepr(r.CheckedBaseSHA.String) + " to " + pyRepr(stated) + ", which any unrelated commit also does"
				}
				// Raised inside the transaction, as Python does: it rolls back and records no contest.
				return &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: pyRepr(prState) + " does not say whether this candidate merged. Read the pull request and resolve again with merged, open or closed" + moved}
			}
			if tip.SHA == "" {
				if landed {
					refusal = unreadableTarget(r, actor, why, "a merged outcome has no base to record; the turn stays unknown")
				}
			} else if !SameCommit(stated, tip.SHA) {
				refusal = mismatch(r, actor, stated, tip, "--observed-base-sha", false)
			}
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		state := "returned"
		landedSHA := sql.NullString{}
		if landed {
			state = "landed"
			landedSHA = nullable(r.CandidateHead)
		}
		if e = s.closeWith(tx, r, state, "resolved from an observation: pr_state="+prState+"; "+evidence, actor, at, landedSHA, nullable(tip.SHA)); e != nil {
			return e
		}
		if tip.SHA != "" {
			if e = s.journalOutcome(tx, "merge_turn_base_observed", turn, observed(tip, "resolution"), at); e != nil {
				return e
			}
		}
		promoted, e = s.promote(tx, r.TargetKey, at)
		return e
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
	outcome := "returned"
	if landed {
		outcome = "landed"
	}
	return map[string]any{"released": released, "outcome": outcome, "promoted": next, "baseObservation": observation(tip), "ledger": released["ledger"]}, nil
}

// journalOutcome is store.journal(kind, subject, detail): detail json.dumps'd in its own order.
func (s *Service) journalOutcome(ctx context.Context, kind, turn string, payload any, at string) error {
	_, err := s.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, kind, turn, pythonJSON(payload))
	return err
}

// Unknown is MergeTurn.report_unknown: the holder or its supervisor stating the outcome
// cannot be established. It keeps the target occupied.
func (s *Service) Unknown(ctx context.Context, turn, actor, reason string) (map[string]any, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, &store.RefusedError{Reason: string(contract.RefusalMergeEvidenceRequired), Detail: "an unknown outcome states why"}
	}
	at := s.now()
	var refusal *registry.CoordinationRefusal
	err := s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		r, e := s.row(tx, turn)
		if e != nil {
			return e
		}
		if r.State != Merging {
			refusal = wrongState(r, actor, "reporting an unknown outcome")
		} else if refusal, e = s.authority(tx, r, actor, "report its outcome unknown"); e != nil {
			return e
		}
		if refusal != nil {
			return s.Registry.RecordCoordinationConflict(tx, *refusal, at)
		}
		if _, e = s.Store.Querier(tx).ExecContext(tx, "UPDATE merge_turns SET state = ?, close_reason = ?, updated_at = ? WHERE turn_id = ?", Unknown, reason, at, turn); e != nil {
			return e
		}
		return s.ledger(tx, turn, "transition", Merging, Unknown, "outcome_unknown", actor, reason, "unknown:"+actor, at)
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal.Error()
	}
	return s.Turn(ctx, turn)
}
