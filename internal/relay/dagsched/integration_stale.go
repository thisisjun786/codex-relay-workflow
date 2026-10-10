package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The nodes of a pushed integration head whose acceptance no longer stands (CRW-1026, d2). A node that already integrated is
// never a candidate again, so a batch does not verify it again when its criteria change afterwards; the push is the last
// place the relay can say that the head it is about to publish holds such a node. dag-integrate-push reports them as
// stale_nodes. The list only informs: it never blocks the push and writes nothing, and a failure to compute it is reported
// next to the push, not instead of it.
//
// The predicate here is its own. It does not ask whether the node is a candidate (currentCandidate), which answers for a
// node that has not landed; it asks of an acceptance that is in the head whether the plan still stands behind it.

// Reasons a node of the pushed head is stale.
const (
	// StaleNodeCriteriaChanged: the plan's criteria for the node are not the ones its integrated acceptance stands on (the
	// acceptance's own, or the newest revalidation of the same output), so the output was not re-ruled under them.
	StaleNodeCriteriaChanged = "criteria_changed"
	// StaleNodeAcceptanceSuperseded: the integrated acceptance is not the node's active one any more, and no integrated
	// acceptance of the node replaced it.
	StaleNodeAcceptanceSuperseded = "acceptance_superseded"
	// StaleNodeRemoved: the plan no longer has the node.
	StaleNodeRemoved = "node_removed"
)

// StaleNode is one integrated acceptance of a pushed head that the plan no longer stands behind. The identity is the plan,
// the node and the acceptance; the checkout and the integration ref the push names are not part of it.
type StaleNode struct {
	PlanID, NodeID, AcceptanceID string
	Reason                       string
	// AcceptedCriteria is the criteria set the integrated acceptance stands on (its newest revalidation, else its own);
	// CurrentCriteria is the plan's now. Both are empty when the reason is not about the criteria.
	AcceptedCriteria, CurrentCriteria string
}

// StaleNodesCheck answers the stale nodes of an integration head. checkout and integrationRef locate the head; they do not
// identify a node.
type StaleNodesCheck func(ctx context.Context, checkout, integrationRef, head string) ([]StaleNode, error)

// StaleIntegratedNodes reads the integrated acceptances the head contains and answers those the plan no longer stands behind,
// in plan, node and acceptance order. An acceptance is an integrated one of this head when a batch of integrationRef put it in
// the branch (integratedAcceptances: a durable move that merged it, whether or not its mark was written, or its mark; the
// batches that came before the last one count) and the head it integrated is an ancestor of head in the checkout. A candidate
// a batch split out is not read, and a batch of another integration ref is not read either.
func (s *Scheduler) StaleIntegratedNodes(ctx context.Context, checkout, integrationRef, head string) ([]StaleNode, error) {
	q := s.Store.Q(ctx)
	histories, err := integratedAcceptances(ctx, q, integrationRef)
	if err != nil {
		return nil, err
	}
	snaps := map[string]dag.Snapshot{}
	var out []StaleNode
	for _, history := range histories {
		// the acceptance of the node this head holds: the newest of the ref's integrated acceptances whose head is an ancestor
		// of head. A newer one the head does not contain (another checkout of the store integrated it on a branch of the same
		// name) is not what this head publishes, and must not hide the one it does.
		var m markedAcceptance
		held := false
		for _, candidate := range history {
			if !hasCommitIn(ctx, checkout, candidate.head) {
				continue
			}
			included, err := isAncestorIn(ctx, checkout, candidate.head, head)
			if err != nil {
				return nil, err
			}
			if included {
				m, held = candidate, true
				break
			}
		}
		if !held {
			continue
		}
		snap, ok := snaps[m.plan]
		if !ok {
			if snap, _, err = dag.SnapshotAt(ctx, q, m.plan, 0); err != nil {
				return nil, err
			}
			snaps[m.plan] = snap
		}
		stale, isStale, err := s.integratedAcceptanceStale(ctx, q, snap, m.plan, m.node, m.acceptance)
		if err != nil {
			return nil, err
		}
		if isStale {
			out = append(out, stale)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.PlanID != b.PlanID {
			return a.PlanID < b.PlanID
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		return a.AcceptanceID < b.AcceptanceID
	})
	return out, nil
}

// markedAcceptance is one acceptance of a node that a batch of the ref merged, with the head it integrated.
type markedAcceptance struct{ plan, node, acceptance, head string }

// integratedAcceptances are, per plan and node, the acceptances a batch of integrationRef put in the branch, newest first: the
// history of the node on the ref. A batch puts a candidate in the branch when it moved the branch onto a head that merged it,
// and when it marked it as already contained:
//   - a batch whose ref_moved row is durable merged the candidates its batch row lists (merged_json); a move the next run
//     reconciled (the batch died after the swap, so it has no batch row) merged the candidates its verified-head intent
//     covers, the one that names the head the branch moved to. Neither depends on a mark, so a batch that stopped after the
//     move and before its first mark, or between two marks, still counts;
//   - a mark or a mark_pending row records a merge (a pending mark is a merge whose mark the criteria change refused) or a
//     candidate the branch already contained.
//
// A candidate a batch split out is in neither its merged list nor its verified head's covered set and has no mark; an
// intent whose batch never moved the branch has no ref_moved row; a batch of another ref is not read. The history keeps every
// acceptance, newest first, because which of them a pushed head holds is decided against that head, not here: a ref name can
// belong to more than one checkout of the store, and the newest acceptance of a node may sit in a head the pushed one does not
// contain.
func integratedAcceptances(ctx context.Context, q store.Querier, integrationRef string) ([][]markedAcceptance, error) {
	byKey := map[[2]string][]orderedAcceptance{}
	var keys [][2]string
	add := func(r orderedAcceptance) {
		if r.node == "" || r.acceptance == "" {
			return
		}
		key := [2]string{r.plan, r.node}
		if _, seen := byKey[key]; !seen {
			keys = append(keys, key)
		}
		byKey[key] = append(byKey[key], r)
	}
	marks, err := markedStageRows(ctx, q, integrationRef)
	if err != nil {
		return nil, err
	}
	for _, r := range marks {
		add(r)
	}
	moved, err := movedBatchMerges(ctx, q, integrationRef)
	if err != nil {
		return nil, err
	}
	for _, r := range moved {
		add(r)
	}
	out := make([][]markedAcceptance, 0, len(keys))
	for _, key := range keys {
		rows := byKey[key]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].at > rows[j].at })
		var history []markedAcceptance
		seen := map[string]bool{}
		for _, r := range rows {
			if !seen[r.acceptance+"@"+r.head] {
				seen[r.acceptance+"@"+r.head] = true
				history = append(history, r.markedAcceptance)
			}
		}
		out = append(out, history)
	}
	return out, nil
}

// orderedAcceptance is an integrated acceptance with the rowid of the stage row that records it, so the newest wins.
type orderedAcceptance struct {
	markedAcceptance
	at int64
}

// markedStageRows are the marked and mark_pending rows of the ref's batches.
func markedStageRows(ctx context.Context, q store.Querier, integrationRef string) ([]orderedAcceptance, error) {
	rows, err := q.QueryContext(ctx, "SELECT c.rowid, c.plan_id, c.node_id, c.acceptance_id, c.head_sha, b.detail FROM dag_integration_stages c JOIN dag_integration_stages b ON b.batch_id = c.batch_id AND b.stage = 'intent' AND b.node_id = '' WHERE c.stage IN ('marked', 'mark_pending') AND c.node_id <> '' AND c.acceptance_id <> '' ORDER BY c.rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orderedAcceptance
	for rows.Next() {
		var r orderedAcceptance
		var detail string
		if err := rows.Scan(&r.at, &r.plan, &r.node, &r.acceptance, &r.head, &detail); err != nil {
			return nil, err
		}
		if intentRef(detail) == integrationRef {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// movedBatchMerges are the candidates the ref's moved batches merged, each at the rowid of its batch's ref_moved row.
func movedBatchMerges(ctx context.Context, q store.Querier, integrationRef string) ([]orderedAcceptance, error) {
	type movedRow struct {
		at                int64
		batch, plan, head string
	}
	rows, err := q.QueryContext(ctx, "SELECT rowid, batch_id, plan_id, detail FROM dag_integration_stages WHERE stage = 'ref_moved' ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	var moves []movedRow
	for rows.Next() {
		var m movedRow
		if err := rows.Scan(&m.at, &m.batch, &m.plan, &m.head); err != nil {
			rows.Close()
			return nil, err
		}
		moves = append(moves, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []orderedAcceptance
	for _, m := range moves {
		var ref, mergedJSON string
		err := q.QueryRowContext(ctx, "SELECT integration_ref, merged_json FROM dag_integration_batches WHERE batch_id = ?", m.batch).Scan(&ref, &mergedJSON)
		switch {
		case err == nil:
			// the batch committed its move with its batch row: the merged list is what the move put in the branch
			if ref != integrationRef {
				continue
			}
			var merged []IntegrationBatchMerged
			if err := json.Unmarshal([]byte(mergedJSON), &merged); err != nil {
				return nil, err
			}
			for _, c := range merged {
				out = append(out, orderedAcceptance{markedAcceptance{plan: m.plan, node: c.NodeID, acceptance: c.AcceptanceID, head: c.HeadSHA}, m.at})
			}
		case errors.Is(err, sql.ErrNoRows):
			// a reconciled move: the candidates its verified head covers, read from the batch's own intent rows
			covered, err := reconciledMoveCandidates(ctx, q, m.batch, m.head, integrationRef)
			if err != nil {
				return nil, err
			}
			for _, c := range covered {
				out = append(out, orderedAcceptance{markedAcceptance{plan: m.plan, node: c.node, acceptance: c.acceptance, head: c.head}, m.at})
			}
		default:
			return nil, err
		}
	}
	return out, nil
}

// reconciledMoveCandidates are the frozen candidates of a batch that the verified-head intent of head covers on the ref, in
// node order. The covered set is the criteria map that intent names; a frozen candidate it does not name (split out) is not
// read.
func reconciledMoveCandidates(ctx context.Context, q store.Querier, batch, head, integrationRef string) ([]markedAcceptance, error) {
	rows, err := q.QueryContext(ctx, "SELECT node_id, acceptance_id, head_sha, detail FROM dag_integration_stages WHERE batch_id = ? AND stage = 'intent' ORDER BY rowid", batch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	frozen := map[string]markedAcceptance{}
	covered := map[string]bool{}
	for rows.Next() {
		var node, acceptance, sha, detail string
		if err := rows.Scan(&node, &acceptance, &sha, &detail); err != nil {
			return nil, err
		}
		if node != "" {
			frozen[node] = markedAcceptance{node: node, acceptance: acceptance, head: sha}
			continue
		}
		var d map[string]string
		if json.Unmarshal([]byte(detail), &d) != nil || d["verified_head"] != head || d["integration_ref"] != integrationRef {
			continue
		}
		criteria := map[string]string{}
		if json.Unmarshal([]byte(d["criteria"]), &criteria) != nil {
			continue
		}
		for node := range criteria {
			covered[node] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []markedAcceptance
	for node := range covered {
		if c, ok := frozen[node]; ok {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].node < out[j].node })
	return out, nil
}

// intentRef is the integration ref a batch-level intent row names, "" when it names none.
func intentRef(detail string) string {
	var d map[string]string
	if json.Unmarshal([]byte(detail), &d) != nil {
		return ""
	}
	return d["integration_ref"]
}

// integratedAcceptanceStale is the predicate: whether the plan still stands behind an integrated acceptance. It reads the
// acceptance's identity (it must still be the node's active acceptance), the plan's current criteria for the node and the
// acceptance's revalidations (a revalidation of the same output under the plan's criteria clears the node).
func (s *Scheduler) integratedAcceptanceStale(ctx context.Context, q store.Querier, snap dag.Snapshot, plan, node, acceptance string) (StaleNode, bool, error) {
	stale := StaleNode{PlanID: plan, NodeID: node, AcceptanceID: acceptance}
	n, ok := nodeOf(snap, node)
	if !ok {
		stale.Reason = StaleNodeRemoved
		return stale, true, nil
	}
	active, found, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return stale, false, err
	}
	if !found || active.AcceptanceID != acceptance {
		stale.Reason = StaleNodeAcceptanceSuperseded
		return stale, true, nil
	}
	effective, err := effectiveCriteria(ctx, q, active)
	if err != nil {
		return stale, false, err
	}
	// a blank criteria digest is an incomplete acceptance, which the edges report; it is not a change of the criteria
	if effective != "" && effective != n.CriteriaSetDigest {
		stale.Reason, stale.AcceptedCriteria, stale.CurrentCriteria = StaleNodeCriteriaChanged, effective, n.CriteriaSetDigest
		return stale, true, nil
	}
	return stale, false, nil
}
