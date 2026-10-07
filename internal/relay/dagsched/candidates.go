package dagsched

import (
	"context"
	"sort"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The read facade the integration command needs from the scheduler (CRW-965). internal/relay/integrate
// imports this package and never the other way, so the scheduler keeps owning the predicates and the
// integration command reads them instead of copying them. The functions here add no behaviour of their
// own: they name what the scheduler already computes, for a caller outside the package.

// Candidate is one accepted implementation node the integration command may merge: the node, the
// acceptance that stands for its result, and the head, relationship, event, revision and generation the
// acceptance stands on. It is what the batch merges and what the merged mark is keyed on.
type Candidate struct {
	PlanID, NodeID, AcceptanceID string
	RelationshipID               string
	EventID, RevisionHash        string
	Generation                   int64
	HeadSHA, Repository          string
}

// AcceptedCandidates is the plan's live implementation nodes that hold an active acceptance with a head
// and have not landed on every target yet, in node-id order so two calls agree. A node that already
// integrated is left out (there is nothing to merge), and so is a node the plan holds: the integration
// command advances no node the plan paused or ended.
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
		stand, err := s.standOf(ctx, q, acc)
		if err != nil {
			return nil, err
		}
		out = append(out, Candidate{PlanID: plan, NodeID: n.NodeID, AcceptanceID: acc.AcceptanceID, RelationshipID: stand.RelationshipID,
			EventID: stand.EventID, RevisionHash: stand.RevisionHash, Generation: stand.Generation, HeadSHA: stand.Head, Repository: acc.Repository})
	}
	return out, nil
}

// MarkMerged records the parent's merged mark for the revision an accepted head stands on, exactly as
// the parent's own assignment-mark does: the same table, the same columns and the same actor, so every
// reader of integratedAt cannot tell the two apart. The integration batch records it once it has
// verified the merged tree and moved the local integration branch. It returns the event the mark names.
func (s *Scheduler) MarkMerged(ctx context.Context, plan, node, actor, evidence string) (string, error) {
	q := s.Store.Q(ctx)
	acc, found, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return "", err
	}
	if !found || acc.HeadSHA == "" {
		return "", refuse(contract.RefusalDispositionConflict, "node %s has no accepted head to mark merged", node)
	}
	stand, err := s.standOf(ctx, q, acc)
	if err != nil {
		return "", err
	}
	view := s.assignmentView(ctx)
	if _, err := view.Mark(ctx, stand.RelationshipID, "merged", evidence, actor, stand.EventID); err != nil {
		return "", err
	}
	return stand.EventID, nil
}

// CandidateTargets are the targets a node's active acceptance has to land on, the same set the scheduler
// judges for integration. A node with no active acceptance has none.
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
