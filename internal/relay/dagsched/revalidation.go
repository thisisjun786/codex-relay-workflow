package dagsched

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// What is done about a stale node (contract 3.2, 5, 8.4: E-11, E-19, E-20, E-21, E-25). Invalidation (invalidation.go) says which accepted results no longer match the plan and why; this file says what
// each of them is owed, from that reading and from the relationship the node stands on, and refuses the two moves the contract forbids:
//
//   - a change of the criteria alone leaves the consumed inputs and the output as they were, so the same output is ruled again under the new criteria and its acceptance is re-verified
//     (dag_acceptance_revalidations): no new generation and no new child. Anything else that is stale cannot be made current by ruling the old output again, so a re-validation of such a node is refused
//     before it writes a row that would only look like progress;
//   - a result that must be reworked goes back to the SAME child as the next generation of the same relationship, through dag-correct, and never to a new child. Redefining the node, which does make a new
//     child, is a new relationship that supersedes the old one (contract 5, decision D-08): the parent's decision, never the scheduler's, so a node whose relationship has ended is routed to it and not corrected;
//   - a node that already landed is never run again (E-20). It is not stale, so nothing above it marks it; and no correction is prepared for it or recorded as its own, because what its upstream changed is
//     carried by a successor node of a new plan revision.
//
// The route is derived with the rest of the reading and never stored.

// The routes of a stale node, as the reading prints them in the action of its stale object.
const (
	// ActionRevalidate: only the criteria changed. Rule the same output again under the plan's criteria and accept it again: the acceptance is re-verified.
	ActionRevalidate = "revalidate"
	// ActionCorrect: the output must be reworked. The next generation of the same relationship goes to the same child (dag-correct).
	ActionCorrect = "correct"
	// ActionHold: nothing to do on this node now: it rests on a stale predecessor or on an input that is not there, its relationship is paused, or a correction of it is already under way.
	ActionHold = "hold"
	// ActionRedefine: the relationship has ended, so there is no child to correct. Reworking the output is a redefinition: a new relationship that supersedes the old one.
	ActionRedefine = "redefine"
)

// StaleActions are the routes of a stale node, in the order the documentation lists them.
func StaleActions() []string {
	return []string{ActionRevalidate, ActionCorrect, ActionHold, ActionRedefine}
}

// consumedChange is what is no longer as an accepted node consumed it, the criteria aside. It is nil, and the text empty, only when no acceptance the node consumed was replaced and the manifest rebuilt
// from the store as the node consumed it (its criteria taken as the consumed ones) is complete and digests to the one it consumed: that is the one case in which a change of the criteria leaves the node
// able to be ruled again as it is. Otherwise it is the change that explains the difference, or, when none does (an input that is not there, a value an edge reports on its own), the sentence that says so.
func (s *Scheduler) consumedChange(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (*Stale, string, error) {
	ctx, m := memoFor(ctx, plan, snap)
	if err := s.prepare(ctx, q, plan, snap, m); err != nil {
		return nil, "", err
	}
	c := m.consumed[n.NodeID]
	if c == nil {
		return nil, "", nil
	}
	if replaced, err := s.staleInput(ctx, q, plan, c); err != nil || replaced != nil {
		return replaced, "", err
	}
	rebuilt, complete, err := s.rebuildAsConsumed(ctx, q, plan, snap, n, c.body)
	if err != nil {
		return nil, "", err
	}
	if complete && rebuilt == c.acc.ManifestDigest {
		return nil, "", nil
	}
	if st, err := s.explain(ctx, q, plan, snap, n, c); err != nil || st != nil {
		return st, "", err
	}
	for _, e := range incomingEdges(snap, n.NodeID) {
		status, err := s.edgeStatus(ctx, q, plan, snap, e)
		if err != nil {
			return nil, "", err
		}
		if !status.Satisfied {
			return nil, fmt.Sprintf("an input of %s is not there as it consumed it: edge %s reads %s", n.NodeID, e.EdgeID, status.Reason), nil
		}
	}
	return nil, fmt.Sprintf("a value %s consumed changed without a stale result behind it: the edges and the merge lane report which", n.NodeID), nil
}

// reviewOpen is whether the relay's review of an accepted head is open, which is the one thing that lets the verdict writer take another ruling on a head it ruled verified (it answers a replay otherwise,
// and opens no generation): the accepted event is still the head of the current generation, it is ruled verified, and the criteria registered for the relationship are no longer the set it was ruled under.
func (s *Scheduler) reviewOpen(ctx context.Context, q store.Querier, rel relRow, a Acceptance) (bool, error) {
	if rel.Generation != a.ExecutionGeneration {
		return false, nil
	}
	head, err := delivery.HeadRevisionFrom(ctx, q, rel.ID, rel.Generation)
	if err != nil {
		return false, err
	}
	if event, _ := objString(head, "eventId"); event != a.EventID {
		return false, nil
	}
	var verdict, ruled string
	if found, err := queryOne(ctx, q, "SELECT v.verdict, c.set_digest FROM verdicts v JOIN verdict_context c ON c.event_id = v.event_id WHERE v.event_id = ?", []any{a.EventID}, &verdict, &ruled); err != nil || !found || verdict != "verified" {
		return false, err
	}
	var rows, distinct int
	var registered *string
	if _, err := queryOne(ctx, q, "SELECT COUNT(*), COUNT(DISTINCT set_digest), MIN(set_digest) FROM canonical_criteria WHERE relationship_id = ?", []any{a.RelationshipID}, &rows, &distinct, &registered); err != nil {
		return false, err
	}
	return rows > 0 && distinct == 1 && registered != nil && *registered != ruled, nil
}

// restsOnStale is the stale result an incoming artifact or integrated edge of the node rests on, when there is one: the first such edge, by id, whose predecessor's accepted result is itself stale. It is
// asked for every cause, because what a node consumed having been replaced says nothing of whether what replaced it is current, and a node cannot be rebuilt, corrected or ruled again on a stale result.
func (s *Scheduler) restsOnStale(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (*Stale, error) {
	ctx, m := memoFor(ctx, plan, snap)
	if err := s.prepare(ctx, q, plan, snap, m); err != nil {
		return nil, err
	}
	var inputs map[string]map[string]any
	if c := m.consumed[n.NodeID]; c != nil {
		inputs = consumedInputs(c.body)
	}
	for _, e := range incomingEdges(snap, n.NodeID) {
		if e.Kind != dag.EdgeArtifactVerified && e.Kind != dag.EdgeIntegrated {
			continue
		}
		pred, ok := nodeOf(snap, e.FromNodeID)
		if !ok {
			continue
		}
		above, err := s.staleOf(ctx, q, plan, snap, pred)
		if err != nil {
			return nil, err
		}
		if above != nil {
			return s.stalePredecessorReading(ctx, q, plan, snap, e, textOf(inputs[e.EdgeID]["acceptance_id"]), above)
		}
	}
	return nil, nil
}

// holdOnPredecessor is the route of a node that rests on a stale predecessor: nothing to do on it until that is repaired.
func holdOnPredecessor(n dag.SnapNode, st *Stale) (string, string) {
	return ActionHold, fmt.Sprintf("nothing to do on %s yet: over edge %s it rests on %s, whose accepted result is stale (it stems from %s); repair that first, and the route of this node follows from what it then consumes",
		n.NodeID, st.EdgeID, st.Predecessor, st.Seed)
}

// routeStale is what is done about the stale result of a node: its route and the sentence that says why. Nothing is written.
func (s *Scheduler) routeStale(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, st *Stale) (action, detail string, err error) {
	rel, found, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
	if err != nil {
		return "", "", err
	}
	acc, hasAcc, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
	if err != nil {
		return "", "", err
	}
	above := st
	if st.Cause != CausePredecessorStale {
		if above, err = s.restsOnStale(ctx, q, plan, snap, n); err != nil {
			return "", "", err
		}
	}
	switch {
	case above != nil:
		action, detail = holdOnPredecessor(n, above)
		return action, detail, nil
	case !found || rel.Superseded || rel.Status == "archived" || rel.Status == "cancelled":
		return ActionRedefine, fmt.Sprintf("the relationship of %s has ended, so there is no child to send a correction to: reworking this output is a redefinition of the node, a new relationship registered with supersedes (contract 5, decision D-08), "+
			"which the parent decides and which makes a new child", n.NodeID), nil
	case rel.Status != "active":
		return ActionHold, fmt.Sprintf("relationship %s of %s is %s: resume it (relationship-resume) before a ruling or a correction can reach its child", short(rel.ID), n.NodeID, rel.Status), nil
	case hasAcc && rel.Generation > acc.ExecutionGeneration:
		return ActionHold, fmt.Sprintf("generation %d of relationship %s is open, a correction of %s that goes to its child: wait for its report, then accept it with dag-accept --supersedes %s",
			rel.Generation, short(rel.ID), n.NodeID, short(acc.AcceptanceID)), nil
	}
	why := st.Cause
	if st.Cause == CauseCriteriaChanged {
		changed, unavailable, err := s.consumedChange(ctx, q, plan, snap, n)
		if err != nil {
			return "", "", err
		}
		switch {
		case changed == nil && unavailable == "":
			return ActionRevalidate, fmt.Sprintf("only the criteria of %s changed: what it consumed and its output are as they were. Rule the same output again under the plan's criteria and accept it again with dag-accept: "+
				"the acceptance is re-verified, with no new generation and no new child", n.NodeID), nil
		case changed == nil:
			return ActionHold, unavailable + ": the output cannot be ruled again as it stands, and a correction cannot be prepared until the inputs are there again", nil
		}
		why = st.Cause + " and " + changed.Cause
	}
	ruling := "the accepted head is not open to another ruling, so the relay's verdict writer answers one with a replay and opens no generation: the review reopens when the criteria registered for the relationship change"
	if hasAcc {
		open, err := s.reviewOpen(ctx, q, rel, acc)
		if err != nil {
			return "", "", err
		}
		if open {
			ruling = "the relay's review of the accepted head is open (the criteria registered for the relationship are not the set it was ruled under), so a needs_changes ruling opens the next generation"
		}
	}
	return ActionCorrect, fmt.Sprintf("the output of %s must be reworked (%s). It goes to the same child as generation %d of relationship %s: dag-correct --prepare, the needs_changes ruling carrying its instruction as the restoration "+
		"finding, then dag-correct; accept the reworked result with dag-accept --supersedes %s. %s", n.NodeID, why, rel.Generation+1, short(rel.ID), short(acc.AcceptanceID), ruling), nil
}

// withRoute is the stale reading with its route: a copy, because the verdict the memo holds is shared by every question asked of one pass.
func (s *Scheduler) withRoute(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, st *Stale) (*Stale, error) {
	action, detail, err := s.routeStale(ctx, q, plan, snap, n, st)
	if err != nil {
		return nil, err
	}
	routed := *st
	routed.Action, routed.ActionDetail = action, detail
	return &routed, nil
}

// refuseRevalidation is the guard of a re-validation (contract 3.2, E-11, E-21): ruling the same output again under the plan's criteria resolves a change of the criteria and nothing else, so a node that
// is stale for another reason is refused with disposition_conflict, naming where it goes instead, and nothing is written. A node that is not stale (one that landed, for example) is not refused.
func (s *Scheduler) refuseRevalidation(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) error {
	ctx, _ = memoFor(ctx, plan, snap)
	st, err := s.staleOf(ctx, q, plan, snap, n)
	if err != nil || st == nil {
		return err
	}
	action, detail, err := s.routeStale(ctx, q, plan, snap, n, st)
	if err != nil || action == ActionRevalidate {
		return err
	}
	return refuse(contract.RefusalDispositionConflict, "the accepted result of %s is stale (%s) and ruling the same output again would not make it current; its route is %s: %s", n.NodeID, st.Reason(), action, detail)
}

// refuseLanded is the guard of a correction (contract 8.4, E-20): a node whose accepted head landed in every target it lands on is never run again, whatever changed above it and whatever the plan now
// says the node is: what landed is judged from the acceptance, so a revision that changes the kind of a landed node does not lift it. The change is carried by a successor node of a new plan revision,
// which is released like any other node.
func (s *Scheduler) refuseLanded(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) error {
	acc, has, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
	if err != nil || !has || acc.HeadSHA == "" {
		return err
	}
	landed, _, err := s.nodeIntegrated(ctx, q, plan, snap, acc)
	if err != nil || !landed {
		return err
	}
	return refuse(contract.RefusalDispositionConflict, "%s landed (its accepted head %s is contained in every target it lands on and the parent marked it merged): a node that landed is never run again, so no correction is "+
		"prepared or recorded for it; what changed above it is carried by a successor node of a new plan revision (contract 8.4, E-20)", n.NodeID, short(acc.HeadSHA))
}
