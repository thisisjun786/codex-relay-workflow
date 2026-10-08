package mergeturn

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ucValidStandAcceptances are the active acceptances that stand on head through a base refresh the relay validly
// recorded. A refresh row keeps the head only when it names the acceptance's relationship and a generation no older
// than the acceptance's, and its id is the digest acceptance.Stands recomputes. A row that fails that check does not
// keep a head the acceptance no longer stands on, so the replaced-head refusal still applies to it.
func ucValidStandAcceptances(ctx context.Context, q store.Querier, head string) ([]string, error) {
	present, err := ucZoneTable(ctx, q, "dag_base_refreshes")
	if err != nil || !present {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, "SELECT f.refresh_id, f.acceptance_id, a.relationship_id, a.execution_generation, f.relationship_id, f.execution_generation, f.event_id, f.revision_hash, f.head_sha, f.base_repository, f.base_ref, f.base_tip_sha, f.proof_json, f.resolved_paths_json"+
		" FROM dag_base_refreshes f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id"+
		" WHERE a.state = 'active' AND crw_same_commit(f.head_sha, ?)", head)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var refreshID, acceptanceID, accRelationship, rowRelationship, event, revision, refreshedHead, baseRepo, baseRef, baseTip, proof, resolved string
		var accGeneration, rowGeneration int64
		if err := rows.Scan(&refreshID, &acceptanceID, &accRelationship, &accGeneration, &rowRelationship, &rowGeneration, &event, &revision, &refreshedHead, &baseRepo, &baseRef, &baseTip, &proof, &resolved); err != nil {
			return nil, err
		}
		if rowRelationship == accRelationship && rowGeneration >= accGeneration &&
			refreshID == acceptance.RefreshDigest(acceptanceID, rowRelationship, rowGeneration, event, revision, refreshedHead, baseRepo, baseRef, baseTip, proof, resolved) {
			out = append(out, acceptanceID)
		}
	}
	return out, rows.Err()
}
