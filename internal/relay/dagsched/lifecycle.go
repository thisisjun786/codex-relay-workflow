package dagsched

import (
	"context"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// What the plan says of a node's life (CRW-281, internal/relay/dag/lifecycle.go): paused, cancelled or archived, or the whole plan paused. The scheduler reads it at the revision it
// reads the plan at, never from a table of its own, and obeys it in two places: the reading (a node the plan holds is not offered and says why) and the commands that would advance a node
// (release, accept, correct, judge and request a merge turn, observe an integration), which refuse with the closed reason in the detail and write nothing.
//
// A pause does not reach into the relay. The node's relationship stays as it is, its child keeps running and reports, and its slot stays held until an explicit slot-release (contract 7.4,
// 3.2); what the plan stops is the scheduler releasing the node, accepting its result or sending it on, so nothing downstream is released from a result that arrives while the node is paused.

// lifeView is what the plan says of one node's life.
type lifeView struct {
	planPaused bool
	node       string // dag.LifePaused, dag.LifeCancelled, dag.LifeArchived or "" for an active node
}

func lifeOf(snap dag.Snapshot, n dag.SnapNode) lifeView {
	return lifeView{planPaused: snap.PlanState == dag.LifePaused, node: n.Lifecycle}
}

func (l lifeView) active() bool { return !l.planPaused && l.node == "" }

// ended is a node that is cancelled or archived: final, and never released again.
func (l lifeView) ended() bool { return l.node == dag.LifeCancelled || l.node == dag.LifeArchived }

// reason is the closed reason the plan's life gives a node and the words that say why: an ended node first, then the plan's pause, then the node's own.
func (l lifeView) reason(id string) (reason, detail string) {
	switch {
	case l.node == dag.LifeCancelled:
		return SkipNodeCancelled, "the plan cancelled node " + id + ": it is never released again, what it produced is not counted as done, and a cancel is not reverted (to do the work again the plan replaces the node)"
	case l.node == dag.LifeArchived:
		return SkipNodeArchived, "the plan archived node " + id + ": it is never released again and nothing downstream is released from its result; a pull request it already had accepted may still land"
	case l.planPaused:
		detail = "the plan is paused: nothing is released, accepted, corrected or sent to the merge lane until a resume revision"
		if l.node == dag.LifePaused {
			detail += " (node " + id + " is also paused by its own change)"
		}
		return DeferPlanPaused, detail
	}
	return DeferNodePaused, "the plan paused node " + id + ": it is not released, accepted, corrected or sent to the merge lane until a resume revision, and its slot stays held (contract 7.4)"
}

// holdUnowned reads a node nobody owns while the plan holds it: before any edge is looked at, since a node the plan holds is not a candidate whatever its edges say. A node with a lifecycle
// of its own has that state; a node of a paused plan has no execution record to describe and reads planned.
func (l lifeView) holdUnowned(r *NodeReading) {
	r.Lifecycle = l.node
	switch l.node {
	case dag.LifeCancelled:
		r.State = StateCancelled
	case dag.LifeArchived:
		r.State = StateArchivedNode
	case dag.LifePaused:
		r.State = StatePausedNode
	default:
		r.State = StatePlanned
	}
	reason, detail := l.reason(r.NodeID)
	r.Disposition, r.Reason, r.Detail = dispositionOf(reason), reason, detail
}

// overlayOwned puts the plan's life on the reading of a node that has a release or an execution. What the relay says of the execution stays in its State and is added to the detail. Two
// facts are kept as they are: a landing (the plan cannot take back what is in the target) and a blocked reason (an operator has to act on it, a creation of unknown outcome among them). An
// ended node overrides the rest; a pause overrides only that the node is owned, because a pause does not invalidate an acceptance (contract 7.4).
func (l lifeView) overlayOwned(r *NodeReading, st nodeState) {
	r.Lifecycle = l.node
	if l.active() {
		return
	}
	reason, detail := l.reason(r.NodeID)
	switch {
	case st.Reason == DoneIntegrated:
		r.Detail += "; the plan has not reverted the landing: " + detail
	case strings.HasPrefix(st.Reason, "blocked:"):
		r.Detail += "; " + detail
		if st.Reason == BlockedCreationUnknown || st.Reason == BlockedReleaseAbandoned {
			r.Detail += "; a repeat of the release is refused until the plan lets the node run"
		}
	case l.ended() || st.Reason == SkipAlreadyOwned:
		r.Disposition, r.Reason = dispositionOf(reason), reason
		r.Detail = detail + "; its execution: " + st.Detail
	default:
		r.Detail += "; " + detail
	}
}

// lifecycleRefusal is the refusal of work on a node the plan holds: paused, cancelled, archived or in a paused plan. It is the existing reason disposition_conflict (D-02) with the closed reason in
// the detail. The landing of a pull request the node already had accepted (judging it, asking the merge lane for a turn, observing where it landed) is not refused for an archived node, as it is not for
// an archived relationship (mergeable, observableRelationship): the work ended and the pull request still has to land.
func lifecycleRefusal(snap dag.Snapshot, n dag.SnapNode, doing string, landing bool) error {
	l := lifeOf(snap, n)
	if landing && l.node == dag.LifeArchived {
		l.node = ""
	}
	if l.active() {
		return nil
	}
	reason, detail := l.reason(n.NodeID)
	return refuse(contract.RefusalDispositionConflict, "%s (%s): %s: %s is refused", n.NodeID, reason, detail, doing)
}

// lifecycleOpen reads the plan as the reader sees it now and refuses when the plan holds the node. It is called inside the transactions of the commands (the slice and criteria digests they compare
// do not move for a pause) and beside their other checks. A node the plan does not hold is the caller's business.
func lifecycleOpen(ctx context.Context, q store.Querier, plan, node, doing string, landing bool) error {
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return err
	}
	n, ok := nodeOf(snap, node)
	if !ok {
		return nil
	}
	return lifecycleRefusal(snap, n, doing, landing)
}

// endedInput is a node the plan cancelled or archived whose result an accepted node consumed.
type endedInput struct{ node, life string }

// endedInInputs follows what an accepted node consumed, through the manifest of its acceptance and then through the acceptances those inputs rest on, and returns the first node the plan cancelled or
// archived. A node accepted on the result of a node the plan then ended is not a base for what follows it: the descendants of an ended node are not released through the edge that leaves it
// and not through what they consumed either. A landing is the exception, as it is for the edge: what an integrated input handed over is in the target, so that input is not followed. This is a
// gate on release and nothing is revoked: no acceptance row changes (judging a result stale is a separate issue's).
func endedInInputs(ctx context.Context, q store.Querier, snap dag.Snapshot, a Acceptance, seen map[string]bool) (*endedInput, error) {
	if seen[a.AcceptanceID] {
		return nil, nil
	}
	seen[a.AcceptanceID] = true
	body, found, err := dag.ReadManifestOn(ctx, q, a.ManifestDigest)
	var corrupt *dag.CorruptError
	switch {
	case errors.As(err, &corrupt), err == nil && !found:
		return nil, nil // a manifest that is damaged or missing is the finding consumedStanding makes
	case err != nil:
		return nil, err
	}
	inputs, _ := body["inputs"].([]any)
	for _, item := range inputs {
		in, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := in["kind"].(string); kind == dag.EdgeIntegrated {
			continue
		}
		from, _ := in["from_node_id"].(string)
		if n, ok := nodeOf(snap, from); ok && (n.Lifecycle == dag.LifeCancelled || n.Lifecycle == dag.LifeArchived) {
			return &endedInput{node: from, life: n.Lifecycle}, nil
		}
		id, _ := in["acceptance_id"].(string)
		if id == "" {
			continue
		}
		next, err := loadAcceptanceByID(ctx, q, id)
		if errors.Is(err, errNoAcceptance) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if ended, err := endedInInputs(ctx, q, snap, next, seen); err != nil || ended != nil {
			return ended, err
		}
	}
	return nil, nil
}

// endedPredecessor is the edge status of an edge out of a node whose accepted result rests on a node the plan ended, or nil.
func (s *Scheduler) endedPredecessor(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, from dag.SnapNode) (*EdgeStatus, error) {
	a, found, err := loadActiveAcceptance(ctx, q, plan, from.NodeID)
	if err != nil || !found {
		return nil, err
	}
	ended, err := endedInInputs(ctx, q, snap, a, map[string]bool{})
	if err != nil || ended == nil {
		return nil, err
	}
	reason := BlockedPredecessorCancelled
	if ended.life == dag.LifeArchived {
		reason = BlockedPredecessorArchived
	}
	st := blocked(reason, "the accepted result of "+from.NodeID+" rests on the result of node "+ended.node+", which the plan "+ended.life+"; the plan has to be revised before anything is released from it")
	return &st, nil
}
