package dagsched

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// commitAccepted is whether an acceptance was made by the commit path: a verification row and no forge row (CRW-965).
// A store that predates the verification table has no commit acceptances, so it is answered without a query that
// would fail on it.
func commitAccepted(ctx context.Context, q store.Querier, acceptanceID string) (bool, error) {
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

// integrationRefsOf are the integration refs the batches that moved an acceptance named, read from the batch intent
// rows that carry them (CRW-965, parent decision D4). The result is sorted and deduplicated.
func integrationRefsOf(ctx context.Context, q store.Querier, acceptanceID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT b.detail FROM dag_integration_stages c JOIN dag_integration_stages b ON b.batch_id = c.batch_id AND b.stage = 'intent' AND b.node_id = '' WHERE c.acceptance_id = ? AND c.stage = 'intent' AND c.node_id <> ''", acceptanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return nil, err
		}
		var d map[string]string
		if json.Unmarshal([]byte(detail), &d) == nil && d["integration_ref"] != "" {
			seen[d["integration_ref"]] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	moved, err := movedIntegrationRefs(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for ref := range seen {
		if moved[ref] {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out, nil
}

// movedIntegrationRefs are the integration refs a batch with a ref_moved row has moved. A planned, abandoned, deferred or
// failed intent names no moved ref, so it adds no completion target (CRW-965, parent decision D4). It is the one definition
// nodeTargets and the observation read.
func movedIntegrationRefs(ctx context.Context, q store.Querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT b.detail FROM dag_integration_stages m JOIN dag_integration_stages b ON b.batch_id = m.batch_id AND b.stage = 'intent' AND b.node_id = '' WHERE m.stage = 'ref_moved'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	moved := map[string]bool{}
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			return nil, err
		}
		var d map[string]string
		if json.Unmarshal([]byte(detail), &d) == nil && d["integration_ref"] != "" {
			moved[d["integration_ref"]] = true
		}
	}
	return moved, rows.Err()
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
