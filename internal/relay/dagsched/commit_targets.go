package dagsched

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// commitAccepted is whether an acceptance was made by the commit path: a verification row and no forge row (CRW-965).
// An agent acceptance is one (CRW-1213): the verification record is in its row and no forge is involved. A store that
// predates the verification table has no commit acceptances, so it is answered without a query that would fail on it.
func commitAccepted(ctx context.Context, q store.Querier, acceptanceID string) (bool, error) {
	if agent, found, err := loadAgentAcceptanceByID(ctx, q, acceptanceID); err != nil || found {
		return found && agent.Head != "", err
	}
	var name string
	exists, err := queryOne(ctx, q, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'dag_acceptance_verifications'", nil, &name)
	if err != nil || !exists {
		return false, err
	}
	var one int
	hasVerification, err := queryOne(ctx, q, "SELECT 1 FROM dag_acceptance_verifications WHERE acceptance_id = ?", []any{acceptanceID}, &one)
	if err != nil || !hasVerification {
		return false, err
	}
	hasForge, err := queryOne(ctx, q, "SELECT 1 FROM dag_acceptance_forge WHERE acceptance_id = ?", []any{acceptanceID}, &one)
	if err != nil {
		return false, err
	}
	return !hasForge, nil
}

// integrationRefsOf are the integration refs an acceptance completes on (CRW-965, parent decision D4 and its revision): the
// refs of the acceptance's own batches that moved the ref (a ref_moved row), and the ref of any of its batches that marked the
// acceptance as already contained on it (a marked row, which moves no ref). A batch that failed before its move, and a planned
// or abandoned intent, name no ref. The result is sorted and deduplicated, and nodeTargets and the observation both read it.
//
// An agent acceptance (CRW-1213) is read the same way from its own rows: the node-level intent in dag_agent_integration_stages is joined by
// batch_id to the batch-level intent in dag_integration_stages (which every batch writes, agent-only or mixed), and the ref counts when that batch
// moved the ref (a batch-level ref_moved row) or when the same acceptance has a marked row of the batch in dag_agent_integration_stages.
func integrationRefsOf(ctx context.Context, q store.Querier, acceptanceID string) ([]string, error) {
	seen := map[string]bool{}
	read := func(query string) error {
		rows, err := q.QueryContext(ctx, query, acceptanceID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var detail string
			if err := rows.Scan(&detail); err != nil {
				return err
			}
			var d map[string]string
			if json.Unmarshal([]byte(detail), &d) == nil && d["integration_ref"] != "" {
				seen[d["integration_ref"]] = true
			}
		}
		return rows.Err()
	}
	if err := read("SELECT DISTINCT b.detail FROM dag_integration_stages c JOIN dag_integration_stages b ON b.batch_id = c.batch_id AND b.stage = 'intent' AND b.node_id = '' WHERE c.acceptance_id = ? AND c.stage = 'intent' AND c.node_id <> '' AND (EXISTS (SELECT 1 FROM dag_integration_stages m WHERE m.batch_id = c.batch_id AND m.stage = 'ref_moved') OR EXISTS (SELECT 1 FROM dag_integration_stages k WHERE k.batch_id = c.batch_id AND k.acceptance_id = c.acceptance_id AND k.stage = 'marked'))"); err != nil {
		return nil, err
	}
	if has, err := tableExists(ctx, q, "dag_agent_integration_stages"); err != nil {
		return nil, err
	} else if has {
		if err := read("SELECT DISTINCT b.detail FROM dag_agent_integration_stages c JOIN dag_integration_stages b ON b.batch_id = c.batch_id AND b.stage = 'intent' AND b.node_id = '' WHERE c.acceptance_id = ? AND c.stage = 'intent' AND (EXISTS (SELECT 1 FROM dag_integration_stages m WHERE m.batch_id = c.batch_id AND m.stage = 'ref_moved') OR EXISTS (SELECT 1 FROM dag_agent_integration_stages k WHERE k.batch_id = c.batch_id AND k.acceptance_id = c.acceptance_id AND k.stage = 'marked'))"); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(seen))
	for ref := range seen {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out, nil
}

// commitIntegratedEdge judges an integrated edge out of a commit-accepted node: the node must be integrated on every
// integration branch its batches moved, and the edge's base ref is not consulted (CRW-965, parent decision D4).
func (s *Scheduler) commitIntegratedEdge(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, e dag.SnapEdge, from dag.SnapNode, a Acceptance) (EdgeStatus, error) {
	refs, err := integrationRefsOf(ctx, q, a.AcceptanceID)
	if err != nil {
		return EdgeStatus{}, err
	}
	if len(refs) == 0 {
		return wait(e, "the accepted head is not yet moved onto an integration branch"), nil
	}
	var last IntegratedAt
	for _, ref := range refs {
		at, err := s.integratedAt(ctx, q, plan, a, e.TargetRepository, ref)
		if err != nil {
			return EdgeStatus{}, err
		}
		if at.Unprovable {
			return blocked(BlockedIntegrationUnprovable, "a merge landed this head but the integration branch does not contain it"), nil
		}
		if !at.Satisfied {
			return wait(e, "the accepted head is not yet observed in "+e.TargetRepository+" "+ref+" with the merged mark"), nil
		}
		last = at
	}
	if st, err := s.stalePredecessor(ctx, q, plan, snap, e, from); err != nil || st != nil {
		return valueOf(st), err
	}
	return EdgeStatus{Satisfied: true, Since: last.Since, AcceptanceID: a.AcceptanceID, ObservationID: last.Observation}, nil
}
