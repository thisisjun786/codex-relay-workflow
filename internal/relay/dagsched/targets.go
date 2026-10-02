package dagsched

import (
	"context"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Target is a branch an accepted head has to be contained in before the node counts as integrated.
type Target struct{ Repository, BaseRef string }

// nodeTargets are the targets of a node's ACTIVE acceptance: the targets of its live outgoing edges that wait for a landing (integrated, and artifact_verified
// with a code pin, whose successors build on a landed head) and every target the acceptance already has an observation for, a negative one included (a
// target once observed stays required: an edge retired afterwards does not excuse it). The set is deduplicated and sorted, so two reads of one store agree.
// A terminal node has no outgoing edge: its target arrives with its first observation (dag-integration-observe --target).
func (s *Scheduler) nodeTargets(ctx context.Context, q store.Querier, snap dag.Snapshot, a Acceptance) ([]Target, error) {
	seen := map[Target]bool{}
	for _, e := range snap.Edges {
		if e.FromNodeID != a.NodeID || e.TargetRepository == "" || e.TargetBaseRef == "" {
			continue
		}
		if e.Kind == dag.EdgeIntegrated || (e.Kind == dag.EdgeArtifactVerified && e.PinsCodeHead) {
			seen[Target{e.TargetRepository, e.TargetBaseRef}] = true
		}
	}
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT repository, base_ref FROM dag_integration_observations WHERE acceptance_id = ?", a.AcceptanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Target
		if err := rows.Scan(&t.Repository, &t.BaseRef); err != nil {
			return nil, err
		}
		seen[t] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]Target, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repository != out[j].Repository {
			return out[i].Repository < out[j].Repository
		}
		return out[i].BaseRef < out[j].BaseRef
	})
	return out, nil
}

// nodeIntegrated is whether a node's accepted head has landed everywhere it has to: it has an accepted code head, at least one target, and every target
// satisfies integratedAt. It returns the targets it judged.
func (s *Scheduler) nodeIntegrated(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, a Acceptance) (bool, []Target, error) {
	if a.HeadSHA == "" {
		return false, nil, nil
	}
	targets, err := s.nodeTargets(ctx, q, snap, a)
	if err != nil || len(targets) == 0 {
		return false, targets, err
	}
	for _, t := range targets {
		at, err := s.integratedAt(ctx, q, plan, a, t.Repository, t.BaseRef)
		if err != nil || !at.Satisfied {
			return false, targets, err
		}
	}
	return true, targets, nil
}
