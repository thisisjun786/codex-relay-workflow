package dagsched

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Observation is one reading of whether an accepted head is contained in a target branch.
type Observation struct {
	ObservationID, Repository, BaseRef, SubjectSHA, TipSHA, Method, MergeTurnID string
	IsAncestor, Replayed                                                        bool
	Seq                                                                         int64
}

// IntegrationResult is the answer of ObserveIntegration: what was observed, whether the node is now integrated (every target it has to land on contains its head and the parent marked the
// merge), whether the mark is present, and whether the node's slot was returned.
type IntegrationResult struct {
	PlanID, NodeID, AcceptanceID string
	Observations                 []Observation
	Integrated, MarkPresent      bool
	SlotReleased                 bool
}

// ObserveIntegration records, for each target, whether the head the parent accepted is contained in the branch now, as a fact the relay read itself (git merge-base --is-ancestor for a local
// checkout, the compare API for a forge repository). Targets are two sets: the ones observed now (the explicit list, else every required one) and the REQUIRED ones that decide integration:
// the targets of the node's outgoing integrated and code-pinned edges, every target the acceptance already has an observation for, and the ones observed now. Observing a subset therefore never
// completes a node that still has an unobserved target. The parent's order is accept, merge, assignment-mark merged, observe; integration needs the mark on the same revision.
//
// The observation is refused for a paused or cancelled relationship (no new integration while paused, contract 3.2); observations already written are never removed. The preconditions are read again
// inside the transaction, because a pause can land while the tip and the ancestry are being read.
func (s *Scheduler) ObserveIntegration(ctx context.Context, plan, node, actor string, explicit []Target) (IntegrationResult, error) {
	out := IntegrationResult{PlanID: plan, NodeID: node}
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return out, err
	}
	n, ok := nodeOf(snap, node)
	if !ok {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	if n.Kind != dag.NodeImplementation {
		return out, refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it has no head to observe", node, n.Kind)
	}
	acc, hasAcc, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if !hasAcc || acc.HeadSHA == "" {
		return out, refuse(contract.RefusalDispositionConflict, "node %s has no accepted head to observe", node)
	}
	out.AcceptanceID = acc.AcceptanceID
	if err := s.observableRelationship(ctx, q, acc, actor); err != nil {
		return out, err
	}
	known, err := s.nodeTargets(ctx, q, snap, acc)
	if err != nil {
		return out, err
	}
	observeNow := explicit
	if len(observeNow) == 0 {
		observeNow = known
	}
	if len(observeNow) == 0 {
		return out, refuse(contract.RefusalMalformedReceipt, "node %s has no outgoing edge that names where its head lands: name the target to observe", node)
	}
	if s.Tips == nil || s.Ancestry == nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "this scheduler has no target reader or ancestry check, so it cannot observe where %s landed", node)
	}
	type reading struct {
		target Target
		tip    string
		anc    bool
		method string
	}
	var readings []reading
	for _, t := range observeNow {
		tip, err := s.Tips.Tip(ctx, t.Repository, t.BaseRef)
		if err != nil {
			return out, err
		}
		anc, method, err := s.Ancestry(ctx, t.Repository, acc.HeadSHA, tip.SHA)
		if err != nil {
			return out, err
		}
		readings = append(readings, reading{t, tip.SHA, anc, method})
	}
	if s.testBeforeObserveTx != nil {
		s.testBeforeObserveTx()
	}
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		// the targets are judged against the plan as it is now: an edge added while the tips were being read adds a target nobody has observed
		now, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		if _, ok := nodeOf(now, node); !ok {
			return refuse(contract.RefusalUnregisteredScope, "plan %s no longer has the live node %s", plan, node)
		}
		current, found, err := loadActiveAcceptance(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !found || current.AcceptanceID != acc.AcceptanceID || current.HeadSHA != acc.HeadSHA {
			return refuse(contract.RefusalDispositionConflict, "the accepted head of %s changed while it was being observed", node)
		}
		if err := s.observableRelationship(txCtx, tx, acc, actor); err != nil {
			return err
		}
		for _, r := range readings {
			var subject, tip sql.NullString
			var lastSeq, lastAnc sql.NullInt64
			if err := tx.QueryRowContext(txCtx, "SELECT MAX(observed_seq) FROM dag_integration_observations WHERE acceptance_id = ? AND repository = ? AND base_ref = ?", acc.AcceptanceID, r.target.Repository, r.target.BaseRef).Scan(&lastSeq); err != nil {
				return err
			}
			obs := Observation{Repository: r.target.Repository, BaseRef: r.target.BaseRef, SubjectSHA: acc.HeadSHA, TipSHA: r.tip, IsAncestor: r.anc, Method: r.method}
			if lastSeq.Valid {
				_ = tx.QueryRowContext(txCtx, "SELECT subject_sha, tip_sha, is_ancestor FROM dag_integration_observations WHERE acceptance_id = ? AND repository = ? AND base_ref = ? AND observed_seq = ?",
					acc.AcceptanceID, r.target.Repository, r.target.BaseRef, lastSeq.Int64).Scan(&subject, &tip, &lastAnc)
				if subject.String == acc.HeadSHA && tip.String == r.tip && (lastAnc.Int64 == 1) == r.anc {
					obs.Replayed, obs.Seq = true, lastSeq.Int64
					var id string
					if err := tx.QueryRowContext(txCtx, "SELECT observation_id FROM dag_integration_observations WHERE acceptance_id = ? AND repository = ? AND base_ref = ? AND observed_seq = ?",
						acc.AcceptanceID, r.target.Repository, r.target.BaseRef, lastSeq.Int64).Scan(&id); err != nil {
						return err
					}
					obs.ObservationID = id
					out.Observations = append(out.Observations, obs)
					continue
				}
			}
			obs.Seq = lastSeq.Int64 + 1
			sum := sha256.Sum256([]byte(acc.AcceptanceID + "|" + r.target.Repository + "|" + r.target.BaseRef + "|" + strconv.FormatInt(obs.Seq, 10)))
			obs.ObservationID = "dio-" + hex.EncodeToString(sum[:])[:32]
			// a merge turn that landed this head on this branch carries the observation
			var turn sql.NullString
			if err := tx.QueryRowContext(txCtx, "SELECT turn_id FROM merge_turns WHERE repository = ? AND base_ref = ? AND candidate_head = ? AND state = 'landed' ORDER BY turn_id LIMIT 1",
				r.target.Repository, r.target.BaseRef, acc.HeadSHA).Scan(&turn); err != nil && err != sql.ErrNoRows {
				return err
			}
			var carrier any
			if turn.Valid {
				carrier, obs.MergeTurnID = turn.String, turn.String
			}
			anc := 0
			if r.anc {
				anc = 1
			}
			if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, merge_turn_id, observed_seq, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
				obs.ObservationID, acc.AcceptanceID, obs.Repository, obs.BaseRef, obs.SubjectSHA, obs.TipSHA, anc, obs.Method, carrier, obs.Seq, s.now()); err != nil {
				return err
			}
			out.Observations = append(out.Observations, obs)
		}
		// integrated: every REQUIRED target contains the head and the parent marked the merge
		required, err := s.nodeTargets(txCtx, tx, now, acc)
		if err != nil {
			return err
		}
		complete := len(required) > 0
		for _, t := range required {
			at, err := s.integratedAt(txCtx, tx, plan, acc, t.Repository, t.BaseRef)
			if err != nil {
				return err
			}
			complete = complete && at.Satisfied
		}
		var one int
		out.MarkPresent, err = queryOne(txCtx, tx, "SELECT 1 FROM assignment_marks WHERE relationship_id = ? AND mark = 'merged' AND event_id = ? AND execution_generation = ? AND revision_hash = ?",
			[]any{acc.RelationshipID, acc.EventID, acc.ExecutionGeneration, acc.RevisionHash}, &one)
		if err != nil {
			return err
		}
		out.Integrated = complete
		if complete {
			out.SlotReleased, err = s.releaseSlot(txCtx, tx, plan, node, actor, "dag_integrated")
		}
		return err
	})
	return out, err
}

// observableRelationship refuses an observation for a relationship that is not the parent's to advance: paused or cancelled (contract 3.2), or another parent's. An archived
// relationship is fine: the node's work ended and its landing is still to be observed.
func (s *Scheduler) observableRelationship(ctx context.Context, q store.Querier, acc Acceptance, actor string) error {
	rel, found, err := loadRelationship(ctx, q, acc.RelationshipID)
	if err != nil {
		return err
	}
	if !found {
		return refuse(contract.RefusalUnregisteredRelationship, "relationship %s of the accepted result is not in the store", acc.RelationshipID)
	}
	if rel.Status == "paused" || rel.Status == "cancelled" {
		return refuse(contract.RefusalRelationshipNotActive, "the relationship %s is %s: no integration is recorded for it", rel.ID, rel.Status)
	}
	if rel.ParentTaskID != actor {
		return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the parent of relationship %s, which is held by %s", actor, rel.ID, rel.ParentTaskID)
	}
	return nil
}
