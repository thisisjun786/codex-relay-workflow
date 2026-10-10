package dagsched

import (
	"context"
	"encoding/json"
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
// in plan, node and acceptance order. An acceptance is an integrated one of this head when a batch of integrationRef marked it
// merged (the batches that came before the last one count) and the head it integrated is an ancestor of head in the checkout.
// A candidate a batch split out has no mark and is not read, and a batch of another integration ref is not read either.
func (s *Scheduler) StaleIntegratedNodes(ctx context.Context, checkout, integrationRef, head string) ([]StaleNode, error) {
	q := s.Store.Q(ctx)
	marked, err := integratedAcceptances(ctx, q, integrationRef)
	if err != nil {
		return nil, err
	}
	snaps := map[string]dag.Snapshot{}
	var out []StaleNode
	for _, m := range marked {
		if !hasCommitIn(ctx, checkout, m.head) {
			continue
		}
		included, err := isAncestorIn(ctx, checkout, m.head, head)
		if err != nil {
			return nil, err
		}
		if !included {
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

// markedAcceptance is the newest acceptance of one node that a batch of the ref marked merged, with the head it integrated.
type markedAcceptance struct{ plan, node, acceptance, head string }

// integratedAcceptances are, per plan and node, the newest acceptance a batch of integrationRef marked merged (a batch that
// merged it and a batch that found it already contained both write the mark). An older acceptance of the same node that a
// later one replaced in the ref is not read: the later one is what the ref holds for the node.
func integratedAcceptances(ctx context.Context, q store.Querier, integrationRef string) ([]markedAcceptance, error) {
	rows, err := q.QueryContext(ctx, "SELECT c.plan_id, c.node_id, c.acceptance_id, c.head_sha, b.detail FROM dag_integration_stages c JOIN dag_integration_stages b ON b.batch_id = c.batch_id AND b.stage = 'intent' AND b.node_id = '' WHERE c.stage = 'marked' AND c.node_id <> '' AND c.acceptance_id <> '' ORDER BY c.rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	newest := map[[2]string]markedAcceptance{}
	var keys [][2]string
	for rows.Next() {
		var m markedAcceptance
		var detail string
		if err := rows.Scan(&m.plan, &m.node, &m.acceptance, &m.head, &detail); err != nil {
			return nil, err
		}
		var d map[string]string
		if json.Unmarshal([]byte(detail), &d) != nil || d["integration_ref"] != integrationRef {
			continue
		}
		key := [2]string{m.plan, m.node}
		if _, seen := newest[key]; !seen {
			keys = append(keys, key)
		}
		newest[key] = m
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]markedAcceptance, 0, len(keys))
	for _, key := range keys {
		out = append(out, newest[key])
	}
	return out, nil
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
