package dagsched

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

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

// How a correction generation was opened (CorrectionResult.OpenedBy).
const (
	OpenedByRuling         = "ruling"
	OpenedByGenerationOpen = "generation_open"
)

// CorrectionRequestID is the dispatch request id a correction generation opened by hand carries (generation-open --dispatch-request-id): derived from the plan, the node, the manifest and the generation, so
// the generation row itself names the one manifest it was opened for and a generation opened under any other id is not bound to a manifest by dag-correct.
func CorrectionRequestID(plan, node, manifestDigest string, generation int64) string {
	return "dag-correct-" + shaOf([]byte(strings.Join([]string{plan, node, manifestDigest, itoa64(generation)}, "|")))[:40]
}

// describeOpening says how the recorded generation of a node was opened, from the row dag-correct wrote and the generation the relay holds.
func (s *Scheduler) describeOpening(ctx context.Context, q store.Querier, plan, node string, rel relRow, out *CorrectionResult) error {
	var kind string
	var request sql.NullString
	found, err := queryOne(ctx, q, "SELECT kind, managed_request_id FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? AND execution_generation = ?", []any{plan, node, rel.ID, rel.Generation}, &kind, &request)
	if err != nil || !found || kind != "correction" {
		return err
	}
	out.OpenedBy = OpenedByRuling
	if !request.Valid {
		return nil
	}
	var turn sql.NullString
	if _, err := queryOne(ctx, q, "SELECT dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", []any{rel.ID, rel.Generation}, &turn); err != nil {
		return err
	}
	out.OpenedBy, out.DispatchRequestID, out.DispatchTurnID = OpenedByGenerationOpen, request.String, turn.String
	return nil
}

// recordHandOpened binds a generation the coordinator opened by hand (the relay's generation-open, store-only, then generation-bind to the turn that carried the instruction line to the child) to the manifest
// dag-correct --prepare made for it. It is the way a correction reaches the same child when no needs_changes ruling can open the generation: the relay's verdict writer refuses a second ruling on
// an accepted head it ruled verified (disposition_conflict, naming this route) unless the criteria registered for the relationship changed (a node whose consumed input was replaced, its criteria untouched). The
// route is bounded so that it cannot make a rerun or bind a manifest other than the one the generation was opened for:
//
//   - the node's accepted result is stale and its route, without this generation, is a correction: a result that is not accepted yet is corrected by a ruling, an accepted result that is current has no recorded correction route in this build, a change of the criteria alone is a revalidation and
//     opens no generation, and a node that landed was refused before this;
//   - the manifest is named (--manifest-digest) and is the one stored for this node at its current slice and criteria, as on the ruling route, and its inputs are still the ones the node's edges are satisfied
//     by now (VerifyManifest without the file bytes, as release checks its own intent): a predecessor accepted again after the prepare leaves another input than the one the child was told to consume;
//   - the generation was opened under the dispatch request id derived from that manifest (CorrectionRequestID), so it was opened for this manifest and no other;
//   - the generation is bound to a dispatch turn: the instruction line reached the child in it (a child reports in a bound generation only).
//
// What is recorded of how the instruction reached the child is that request id (dag_node_executions.managed_request_id, which a ruling leaves NULL) and the bound dispatch turn. It is the coordinator's
// statement: the relay does not read the child's thread, so unlike the ruling route (which compares the restoration note with the instruction line) nothing here compares the dispatching message with the
// line; the child checks the manifest file against the digest and hash the line names and answers blocked_needs_input on a mismatch.
func (s *Scheduler) recordHandOpened(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, rel relRow, suppliedDigest string, out *CorrectionResult) error {
	notRuled := fmt.Sprintf("no needs_changes ruling on generation %d of %s opened generation %d", rel.Generation-1, rel.ID, rel.Generation)
	st, err := s.staleOf(ctx, q, plan, snap, n)
	if err != nil {
		return err
	}
	if st == nil {
		return refuse(contract.RefusalDispositionConflict, "%s, and the accepted result of %s is not stale, so it is not corrected by a generation opened by hand either: a result that is not accepted yet is corrected by a ruling, and an accepted result that is current has no recorded correction route in this build. A generation that only merged the base into the branch is not a correction: when the generation was ruled verified, dag-base-refresh records it, after proving from git that its head is the accepted head plus merges of the base", notRuled, n.NodeID)
	}
	// the same route a stale node has without the generation: a change of the criteria alone is ruled again (a revalidation, no generation), and what rests on a stale predecessor or an input that is not
	// there waits; only a result that must be reworked is corrected
	if action, detail, err := s.routeOf(ctx, q, plan, snap, n, st, true); err != nil {
		return err
	} else if action != ActionCorrect {
		return refuse(contract.RefusalDispositionConflict, "%s, and the route of %s is %s, not a correction by hand: %s", notRuled, n.NodeID, action, detail)
	}
	// the generation recorded now is the one right after the generation the acceptance stands on: a correction that is already open and recorded is not skipped by opening another beside it
	if acc, has, err := loadActiveAcceptance(ctx, q, plan, n.NodeID); err != nil {
		return err
	} else if has {
		stand, err := s.standOf(ctx, q, acc)
		if err != nil {
			return err
		}
		if stand.Generation != rel.Generation-1 {
			return refuse(contract.RefusalDispositionConflict, "%s, and the accepted result of %s stands on generation %d, so a correction of it is already open (generation %d is recorded for it): report this refusal and open no further generation, because opening one moved the relationship past the generation that correction is accepted on",
				notRuled, n.NodeID, stand.Generation, rel.Generation-1)
		}
	}
	if suppliedDigest == "" {
		return refuse(contract.RefusalDispositionConflict, "%s: a generation opened by hand is bound to the manifest it was opened for, so name it with --manifest-digest", notRuled)
	}
	body, stored, err := dag.ReadManifestOn(ctx, q, suppliedDigest)
	if err != nil {
		return refuse(contract.RefusalRevisionMismatch, "the manifest %s does not digest to its name: %v", suppliedDigest, err)
	}
	if !stored || body["node_id"] != n.NodeID || body["issue_key"] != n.IssueKey {
		return refuse(contract.RefusalDispositionConflict, "the manifest %s is not stored for node %s of %s", suppliedDigest, n.NodeID, n.IssueKey)
	}
	if body["node_slice_digest"] != n.SliceDigest || body["criteria_set_digest"] != n.CriteriaSetDigest {
		return refuse(contract.RefusalDispositionConflict, "the manifest %s was prepared for another version of node %s than the plan holds now, so generation %d of %s cannot be bound to it: report this refusal and open no further generation", suppliedDigest, n.NodeID, rel.Generation, rel.ID)
	}
	// the manifest was built when the inputs of the node were as they were then: a predecessor accepted again, or a decision settled again, between the prepare and now leaves the slice and the criteria of this node
	// as they were and the inputs the child is told to consume superseded. It is judged against the store as it stands, as release does when it records its own intent (the bytes of the files aside: the child
	// verifies those against the digest and hash in the instruction line)
	roots, err := relationshipRoots(ctx, q, rel.ID)
	if err != nil {
		return err
	}
	if findings, err := s.VerifyManifest(ctx, q, plan, snap, n, body, VerifyOptions{SkipFileBytes: true, ArtifactRoots: roots}); err != nil {
		return err
	} else if len(findings) > 0 {
		return refuse(contract.RefusalDispositionConflict, "the manifest %s does not rest on the inputs of node %s as they stand now (%s %s: %s), so generation %d of %s cannot be bound to it: report this refusal and open no further generation",
			suppliedDigest, n.NodeID, findings[0].Code, findings[0].Reason, findings[0].Detail, rel.Generation, rel.ID)
	}
	var request, anchor string
	var turn sql.NullString
	if _, err := queryOne(ctx, q, "SELECT dispatch_request_id, anchor_state, dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", []any{rel.ID, rel.Generation}, &request, &anchor, &turn); err != nil {
		return err
	}
	want := CorrectionRequestID(plan, n.NodeID, suppliedDigest, rel.Generation)
	if request != want {
		return refuse(contract.RefusalDispositionConflict, "generation %d of %s was opened under dispatch request %q: the request id for manifest %s is %q, so the generation was not opened for this manifest (open it with the id dag-correct --prepare printed)",
			rel.Generation, rel.ID, request, suppliedDigest, want)
	}
	if anchor != "bound" || strings.TrimSpace(turn.String) == "" {
		return refuse(contract.RefusalDispositionConflict, "generation %d of %s has no dispatch turn: send the instruction line to the child and bind the turn that carried it (generation-bind --dispatch-turn-id) before the generation is recorded", rel.Generation, rel.ID)
	}
	if _, err := q.ExecContext(ctx, "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,?,?,'correction',?)",
		plan, n.NodeID, rel.ID, rel.Generation, suppliedDigest, want); err != nil {
		return err
	}
	out.ManifestDigest, out.OpenedBy, out.DispatchRequestID, out.DispatchTurnID = suppliedDigest, OpenedByGenerationOpen, want, turn.String
	return nil
}

// consumedChange is what is no longer as an accepted node consumed it, the criteria aside. It is nil, and the text empty, only when no acceptance the node consumed was replaced and the manifest rebuilt
// from the store as the node consumed it (its criteria taken as the consumed ones) is complete and digests to the one it consumed: that is the one case in which a change of the criteria leaves the node
// able to be ruled again as it is. Otherwise it is the change that explains the difference, or, when none does (a value an edge reports on its own), the sentence that says so. An input that is not there
// at all is unavailableInput's, asked before this.
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
	return nil, fmt.Sprintf("a value %s consumed changed without a stale result behind it: the edges and the merge lane report which", n.NodeID), nil
}

// reviewOpen is whether the relay's review of an accepted head is open, which is the one thing that lets the verdict writer take another ruling on an accepted head it ruled verified (it refuses with disposition_conflict otherwise,
// and opens no generation): the accepted event is still the head of the current generation, it is ruled verified, and the criteria registered for the relationship are no longer the set it was ruled under.
func (s *Scheduler) reviewOpen(ctx context.Context, q store.Querier, rel relRow, a Acceptance) (bool, error) {
	// the review of what the acceptance stands on: its own event, or the head event of the later generation a recorded base refresh carried it to (baserefresh.go)
	stand, err := s.standOf(ctx, q, a)
	if err != nil {
		return false, err
	}
	if rel.Generation != stand.Generation {
		return false, nil
	}
	head, err := delivery.HeadRevisionFrom(ctx, q, rel.ID, rel.Generation)
	if err != nil {
		return false, err
	}
	if event, _ := objString(head, "eventId"); event != stand.EventID {
		return false, nil
	}
	var verdict, ruled string
	if found, err := queryOne(ctx, q, "SELECT v.verdict, c.set_digest FROM verdicts v JOIN verdict_context c ON c.event_id = v.event_id WHERE v.event_id = ?", []any{stand.EventID}, &verdict, &ruled); err != nil || !found || verdict != "verified" {
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

// unavailableInput is the first incoming edge, by id, that is not satisfied now, as a sentence, or "" when every one is: a node cannot be rebuilt, ruled again or corrected on an input that is not there
// (its manifest cannot be built), whatever changed it, so there is nothing to do on it until the edge is satisfied.
func (s *Scheduler) unavailableInput(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode) (string, error) {
	for _, e := range incomingEdges(snap, n.NodeID) {
		status, err := s.edgeStatus(ctx, q, plan, snap, e)
		if err != nil {
			return "", err
		}
		if !status.Satisfied {
			return fmt.Sprintf("an input of %s is not there: edge %s from %s reads %s", n.NodeID, e.EdgeID, e.FromNodeID, status.Reason), nil
		}
	}
	return "", nil
}

// holdOnPredecessor is the route of a node that rests on a stale predecessor: nothing to do on it until that is repaired.
func holdOnPredecessor(n dag.SnapNode, st *Stale) (string, string) {
	return ActionHold, fmt.Sprintf("nothing to do on %s yet: over edge %s it rests on %s, whose accepted result is stale (it stems from %s); repair that first, and the route of this node follows from what it then consumes",
		n.NodeID, st.EdgeID, st.Predecessor, st.Seed)
}

// routeStale is what is done about the stale result of a node: its route and the sentence that says why. Nothing is written.
func (s *Scheduler) routeStale(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, st *Stale) (action, detail string, err error) {
	return s.routeOf(ctx, q, plan, snap, n, st, false)
}

// routeOf is routeStale; ignoreOpen leaves out the generation a correction already opened, which routeStale reports as hold: dag-correct asks what the route of the node is without the generation it is
// about to record.
func (s *Scheduler) routeOf(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, st *Stale, ignoreOpen bool) (action, detail string, err error) {
	rel, found, err := currentRelationshipOf(ctx, q, plan, n.NodeID)
	if err != nil {
		return "", "", err
	}
	acc, hasAcc, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
	if err != nil {
		return "", "", err
	}
	// the generation the accepted result stands on: its own, or the later one a recorded base refresh carried it to (baserefresh.go)
	standGeneration := acc.ExecutionGeneration
	if hasAcc {
		stand, err := s.standOf(ctx, q, acc)
		if err != nil {
			return "", "", err
		}
		standGeneration = stand.Generation
	}
	above := st
	if st.Cause != CausePredecessorStale {
		if above, err = s.restsOnStale(ctx, q, plan, snap, n); err != nil {
			return "", "", err
		}
	}
	missing, err := s.unavailableInput(ctx, q, plan, snap, n)
	if err != nil {
		return "", "", err
	}
	life := lifeOf(snap, n)
	switch {
	case !life.active():
		// the plan holds the node (paused, or the whole plan paused; an ended node is not judged stale at all): ruling, accepting and correcting it are refused until the plan lets it run (lifecycle.go)
		reason, why := life.reason(n.NodeID)
		return ActionHold, reason + ": " + why, nil
	case above != nil:
		action, detail = holdOnPredecessor(n, above)
		return action, detail, nil
	case missing != "":
		return ActionHold, missing + ": the output cannot be ruled again or corrected as it stands, and a manifest cannot be built, until the edge is satisfied", nil
	case !found || rel.Superseded || rel.Status == "archived" || rel.Status == "cancelled":
		return ActionRedefine, fmt.Sprintf("the relationship of %s has ended, so there is no child to send a correction to: reworking this output is a redefinition of the node, a new relationship registered with supersedes (contract 5, decision D-08), "+
			"which the parent decides and which makes a new child", n.NodeID), nil
	case rel.Status != "active":
		return ActionHold, fmt.Sprintf("relationship %s of %s is %s: resume it (relationship-resume) before a ruling or a correction can reach its child", short(rel.ID), n.NodeID, rel.Status), nil
	case !ignoreOpen && hasAcc && rel.Generation > standGeneration:
		return ActionHold, fmt.Sprintf("generation %d of relationship %s is open, a correction of %s that goes to its child: record it with dag-correct if that is not done yet, wait for its report, then accept it with dag-accept --supersedes %s",
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
	ruling := "the accepted head is not open to another ruling, so the relay's verdict writer refuses one (disposition_conflict) and opens no generation: open the generation by hand instead (generation-open --relationship " + rel.ID +
		" --dispatch-request-id <the id dag-correct --prepare prints> --reason needs_changes_revision), send the instruction line to the child in the dispatching message, bind that turn (generation-bind --dispatch-turn-id), and record it with dag-correct --manifest-digest <the digest>; the review also reopens when the criteria registered for the relationship change"
	if hasAcc {
		open, err := s.reviewOpen(ctx, q, rel, acc)
		if err != nil {
			return "", "", err
		}
		if open {
			ruling = "the relay's review of the accepted head is open (the criteria registered for the relationship are not the set it was ruled under), so a needs_changes ruling opens the next generation; a generation opened by hand with the id dag-correct --prepare prints works as well"
		}
	}
	return ActionCorrect, fmt.Sprintf("the output of %s must be reworked (%s). It goes to the same child as generation %d of relationship %s: dag-correct --prepare, then a needs_changes ruling carrying its instruction as the restoration "+
		"finding or a generation opened by hand, then dag-correct; accept the reworked result with dag-accept --supersedes %s. %s", n.NodeID, why, rel.Generation+1, short(rel.ID), short(acc.AcceptanceID), ruling), nil
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
