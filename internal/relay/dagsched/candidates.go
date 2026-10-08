package dagsched

import (
	"context"
	"database/sql"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
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
	// AcceptedCriteriaDigest is the criteria set the acceptance was made under, before any revalidation (CRW-965, D1).
	AcceptedCriteriaDigest string
	// PremergeDigest is the digest of the pre-merge record the acceptance stores, judged again for this candidate (CRW-952 answer 3).
	PremergeDigest string
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
		c, ok, err := s.currentCandidate(ctx, q, plan, snap, n)
		if _, held := premergeHeld(err); held {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// currentCandidate is the one currency predicate of an implementation node's candidate (CRW-965, parent decisions D-D and
// D3): the node is live, its active acceptance is not integrated, not stale, its relationship is active on the generation
// the acceptance stands on, and the accepted event is still the head of that generation. Candidate selection and the
// batch's check inside its fenced transaction both call it, so they never disagree.
func (s *Scheduler) currentCandidate(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (Candidate, bool, error) {
	if err := lifecycleRefusal(snap, n, "integrating its result", true); err != nil {
		return Candidate{}, false, nil
	}
	acc, found, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
	if err != nil {
		return Candidate{}, false, err
	}
	if !found || acc.HeadSHA == "" {
		return Candidate{}, false, nil
	}
	landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, acc)
	if err != nil {
		return Candidate{}, false, err
	}
	if landed {
		return Candidate{}, false, nil
	}
	stale, err := s.staleOf(ctx, q, plan, snap, n)
	if err != nil {
		return Candidate{}, false, err
	}
	if stale != nil {
		return Candidate{}, false, nil
	}
	rel, found, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
	if err != nil {
		return Candidate{}, false, err
	}
	if !found || rel.Status != "active" || rel.Superseded {
		return Candidate{}, false, nil
	}
	stand, err := s.standOf(ctx, q, acc)
	if err != nil {
		return Candidate{}, false, err
	}
	criteria, err := currentCriteriaDigest(ctx, q, acc)
	if err != nil {
		return Candidate{}, false, err
	}
	if stand.Generation != rel.Generation {
		return Candidate{}, false, nil
	}
	// the accepted event must still be the head of its generation: a newer receipt makes this acceptance stale (CRW-965 review)
	head, err := delivery.HeadRevisionFrom(ctx, q, stand.RelationshipID, stand.Generation)
	if err != nil {
		return Candidate{}, false, err
	}
	if id, _ := objString(head, "eventId"); id != stand.EventID {
		return Candidate{}, false, nil
	}
	// the stored pre-merge record is judged again on every selection: a refusal holds the candidate with its premerge_* name
	judged, err := s.premergeOfAcceptance(ctx, q, acc, n)
	if err != nil {
		return Candidate{}, false, err
	}
	return Candidate{PlanID: plan, NodeID: n.NodeID, AcceptanceID: acc.AcceptanceID, RelationshipID: stand.RelationshipID,
		EventID: stand.EventID, RevisionHash: stand.RevisionHash, Generation: stand.Generation, HeadSHA: stand.Head, Repository: acc.Repository,
		CriteriaSetDigest: criteria, AcceptedCriteriaDigest: acc.CriteriaSetDigest, PremergeDigest: judged.digest}, true, nil
}

// frozenCandidateStillCurrent is whether a frozen candidate is still the current candidate of its node under the same
// predicate candidate selection uses, with the same acceptance, event, revision, generation and head (CRW-965, D3). The
// batch asks it inside the transaction that moves the branch.
func (s *Scheduler) frozenCandidateStillCurrent(ctx context.Context, f Candidate) (bool, error) {
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, f.PlanID, 0)
	if err != nil {
		return false, err
	}
	n, ok := nodeOf(snap, f.NodeID)
	if !ok {
		return false, nil
	}
	c, ok, err := s.currentCandidate(ctx, q, f.PlanID, snap, n)
	if _, held := premergeHeld(err); held {
		return false, nil
	}
	if err != nil || !ok {
		return false, err
	}
	return c.AcceptanceID == f.AcceptanceID && c.EventID == f.EventID && c.RevisionHash == f.RevisionHash &&
		c.Generation == f.Generation && c.HeadSHA == f.HeadSHA && c.CriteriaSetDigest == f.CriteriaSetDigest && c.PremergeDigest == f.PremergeDigest, nil
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
