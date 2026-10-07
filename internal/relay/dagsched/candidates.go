package dagsched

import (
	"context"
	"database/sql"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The read and write facade of the integration batch (CRW-965). The batch lives in this package so its tests can
// use the same plan, acceptance and mark fixtures as the scheduler's own tests. A candidate is an accepted
// implementation node whose active acceptance is current: the node is live, its relationship is active and on the
// generation the acceptance stands on, and the scheduler's staleness judgment (spec, consumed inputs and criteria)
// finds nothing wrong. Every judgment is the scheduler's own; the batch reads them and never restates them.

// Candidate is one accepted implementation node a batch may merge, and the identity its merged mark is keyed on:
// the acceptance, its event, its revision hash, its generation and the head it stands on. A batch freezes these
// before it touches git, and the mark names the frozen acceptance and event, never whatever is current later.
type Candidate struct {
	PlanID, NodeID, AcceptanceID string
	RelationshipID               string
	EventID, RevisionHash        string
	Generation                   int64
	HeadSHA, Repository          string
	// CriteriaSetDigest is the criteria set the acceptance stands on: a frozen batch row names it (CRW-965, parent decision d2).
	CriteriaSetDigest string
}

// alreadyIntegratedNode is whether a node's active acceptance already landed on an integration target (CRW-965,
// parent decision d2): such a node is never a candidate again, so an explicit request for it is refused by name.
func (s *Scheduler) alreadyIntegratedNode(ctx context.Context, plan, node string) (bool, error) {
	q := s.Store.Q(ctx)
	acc, found, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil || !found || acc.HeadSHA == "" {
		return false, err
	}
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return false, err
	}
	landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, acc)
	return landed, err
}

// AcceptedCandidates is the plan's ready accepted implementation candidates, in node-id order so two calls agree.
// A node that already integrated is left out, and so is one that is stale, whose relationship is not active or is
// not on the generation its acceptance stands on, or that the plan holds (paused, cancelled or archived).
func (s *Scheduler) AcceptedCandidates(ctx context.Context, plan string) ([]Candidate, error) {
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return nil, err
	}
	nodes := append([]dag.SnapNode(nil), snap.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	var out []Candidate
	for _, n := range nodes {
		if n.Kind != dag.NodeImplementation {
			continue
		}
		if err := lifecycleRefusal(snap, n, "integrating its result", true); err != nil {
			continue
		}
		acc, found, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
		if err != nil {
			return nil, err
		}
		if !found || acc.HeadSHA == "" {
			continue
		}
		landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, acc)
		if err != nil {
			return nil, err
		}
		if landed {
			continue
		}
		stale, err := s.staleOf(ctx, q, plan, snap, n)
		if err != nil {
			return nil, err
		}
		if stale != nil {
			continue
		}
		rel, found, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
		if err != nil {
			return nil, err
		}
		if !found || rel.Status != "active" || rel.Superseded {
			continue
		}
		stand, err := s.standOf(ctx, q, acc)
		if err != nil {
			return nil, err
		}
		criteria, err := currentCriteriaDigest(ctx, q, acc)
		if err != nil {
			return nil, err
		}
		if stand.Generation != rel.Generation {
			continue
		}
		out = append(out, Candidate{PlanID: plan, NodeID: n.NodeID, AcceptanceID: acc.AcceptanceID, RelationshipID: stand.RelationshipID,
			EventID: stand.EventID, RevisionHash: stand.RevisionHash, Generation: stand.Generation, HeadSHA: stand.Head, Repository: acc.Repository,
			CriteriaSetDigest: criteria})
	}
	return out, nil
}

// currentCriteriaDigest is the criteria set a candidate is verified under: the one its latest revalidation names, else
// the one its acceptance stands on (CRW-965, parent decision d2).
func currentCriteriaDigest(ctx context.Context, q store.Querier, acc Acceptance) (string, error) {
	var digest string
	found, err := queryOne(ctx, q, "SELECT criteria_set_digest FROM dag_acceptance_revalidations WHERE acceptance_id = ? ORDER BY reval_seq DESC LIMIT 1", []any{acc.AcceptanceID}, &digest)
	if err != nil {
		return "", err
	}
	if found {
		return digest, nil
	}
	return acc.CriteriaSetDigest, nil
}

// MarkFrozen writes the parent's merged mark for one frozen candidate. The actor must be the parent of the node's
// relationship (notParentHeldBy, the check dag-accept makes), the acceptance must still be the frozen one and not
// stale, and the mark names the frozen event: a mark that cannot be written is refused, and the batch records it as
// pending.
func (s *Scheduler) MarkFrozen(ctx context.Context, f Candidate, actor, evidence string) (string, error) {
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, f.PlanID, 0)
	if err != nil {
		return "", err
	}
	n, ok := nodeOf(snap, f.NodeID)
	if !ok {
		return "", refuse(contract.RefusalStaleMarkContext, "node %s is no longer in plan %s", f.NodeID, f.PlanID)
	}
	rel, found, err := currentRelationshipOf(ctx, q, f.PlanID, f.NodeID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", refuse(contract.RefusalUnregisteredRelationship, "node %s has no execution to mark", f.NodeID)
	}
	if rel.ParentTaskID != actor {
		return "", notParentHeldBy(actor, rel)
	}
	acc, found, err := loadActiveAcceptance(ctx, q, f.PlanID, f.NodeID)
	if err != nil {
		return "", err
	}
	if !found || acc.AcceptanceID != f.AcceptanceID {
		return "", refuse(contract.RefusalStaleMarkContext, "the active acceptance of %s is no longer the one the batch merged (%s)", f.NodeID, f.AcceptanceID)
	}
	stale, err := s.staleOf(ctx, q, f.PlanID, snap, n)
	if err != nil {
		return "", err
	}
	if stale != nil {
		return "", refuse(contract.RefusalStaleMarkContext, "node %s is stale: its merged mark waits for the node to be current", f.NodeID)
	}
	view := s.assignmentView(ctx)
	if _, err := view.Mark(ctx, rel.ID, "merged", evidence, actor, f.EventID); err != nil {
		return "", err
	}
	return f.EventID, nil
}

// IntegrationWrite runs the write of an integration stage inside one transaction after the coordinator-epoch fence
// dag-accept uses (fence, the same check): a stale epoch is refused before anything is written. The write sees the
// transaction's context.
func (s *Scheduler) IntegrationWrite(ctx context.Context, plan, actor string, write func(txCtx context.Context) error) error {
	return s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		if err := s.fence(txCtx, s.Store.Q(txCtx), plan, actor); err != nil {
			return err
		}
		return write(txCtx)
	})
}

// Successors maps each node of a plan to the nodes that depend on it through the plan's edges, sorted. The batch
// uses it to leave a failing candidate's dependants out with it.
func (s *Scheduler) Successors(ctx context.Context, plan string) (map[string][]string, error) {
	snap, _, err := dag.SnapshotAt(ctx, s.Store.Q(ctx), plan, 0)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, e := range snap.Edges {
		out[e.FromNodeID] = append(out[e.FromNodeID], e.ToNodeID)
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out, nil
}

// CandidateTargets are the targets a node's active acceptance has to land on, the same set the scheduler judges for
// integration. A node with no active acceptance has none.
func (s *Scheduler) CandidateTargets(ctx context.Context, plan, node string) ([]Target, error) {
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return nil, err
	}
	acc, found, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil || !found {
		return nil, err
	}
	return s.nodeTargets(ctx, q, snap, acc)
}
