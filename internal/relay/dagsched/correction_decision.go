package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A decision reply that advances the generation (split_approval, scope_change: docs/relay/README.md, "Replying to a blocked receipt") opens generation g+1 of the SAME relationship in the relay's own
// transaction, under the reason decision_reply and the dispatch request id decision-<decision event id>. Neither a ruling nor a coordinator opened it, so dag-correct binds it by a third rule. What the child
// was told is the decision, which names the criteria set it continues under and nothing else: the manifest the child was dispatched with stays what it consumed, and only what the plan changed (the
// criteria, and with them the slice digest) is brought up to date.

// decisionRecord is what the decision event keeps that the binding reads.
type decisionRecord struct {
	Decision                string `json:"decision"`
	NextExecutionGeneration int64  `json:"nextExecutionGeneration"`
	CriteriaDigest          string `json:"criteriaDigest"`
}

// recordDecisionOpened binds the current generation of the node's relationship, which a decision reply opened, to the manifest of the node as the plan holds it now (dag_node_executions, kind correction,
// managed_request_id the decision request id). It is bounded so that it cannot record a generation the child was not told about or a node the child was not dispatched as:
//
//   - the generation names a decision of this relationship that advanced to it (a split approval or a scope change);
//   - the node differs from what the child was dispatched with by its criteria alone (criteriaOnly: the predicate the stale reading uses), because the decision tells the child its criteria and nothing
//     else, and those criteria are the plan's now (criteria_set_changed otherwise);
//   - the decision was dispatched to the child: its delivery is dispatched or acknowledged and the generation is bound to the turn it was dispatched into (a queued decision, or a generation that is not bound
//     yet, is retried after the daemon's pass; a generation bound by hand to another turn is never rebound and stops the node here);
//   - the manifest the child was dispatched with, brought up to date, still rests on what the node's edges are satisfied by (VerifyManifest without the file bytes, as the hand-opened route does): a
//     predecessor accepted again since leaves the child unaware of the input that replaced it.
//
// A decision that leaves the node as the manifest recorded it (the plan did not change it) binds that manifest as it is (CarriedOver). The previous acceptance, if any, stays active until the result of
// this generation is accepted with a supersede, as for a ruling.
func (s *Scheduler) recordDecisionOpened(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, rel relRow, actor, suppliedDigest string, out *CorrectionResult) error {
	var request, anchor string
	var anchorTurn sql.NullString
	if _, err := queryOne(ctx, q, "SELECT dispatch_request_id, anchor_state, dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", []any{rel.ID, rel.Generation}, &request, &anchor, &anchorTurn); err != nil {
		return err
	}
	event, named := strings.CutPrefix(request, "decision-")
	var receipt string
	var record decisionRecord
	found, err := queryOne(ctx, q, "SELECT receipt FROM events WHERE event_id = ? AND relationship_id = ? AND outcome = ? AND producer = 'relay'", []any{event, rel.ID, delivery.DecisionReply}, &receipt)
	if err != nil {
		return err
	}
	advanced := named && found && json.Unmarshal([]byte(receipt), &record) == nil && record.NextExecutionGeneration == rel.Generation &&
		(record.Decision == delivery.DecisionSplitApproval || record.Decision == delivery.DecisionScopeChange)
	if !advanced {
		return refuse(contract.RefusalDispositionConflict, "generation %d of %s carries the reason decision_reply (request %q) and no split_approval or scope_change of this relationship opened it, so it is not recorded for node %s", rel.Generation, rel.ID, request, n.NodeID)
	}

	// what the child was dispatched with is the manifest of the previous generation, which RecordCorrection already required to be recorded
	var previous string
	if _, err := queryOne(ctx, q, "SELECT manifest_digest FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? AND execution_generation = ?", []any{plan, n.NodeID, rel.ID, rel.Generation - 1}, &previous); err != nil {
		return err
	}
	was, stored, err := dag.ReadManifestOn(ctx, q, previous)
	if err != nil || !stored {
		return refuse(contract.RefusalRevisionMismatch, "the manifest %s that generation %d of %s was dispatched with cannot be read back: %v", previous, rel.Generation-1, rel.ID, err)
	}
	if !criteriaOnly(n, snap, was) {
		return refuse(contract.RefusalDispositionConflict, "node %s changed beyond its criteria since its child was dispatched with manifest %s: a decision tells the child its criteria and nothing else, so generation %d of %s is not recorded as consuming the changed node; "+
			"restore the node (the same slice digest, its edges included) or redefine it", n.NodeID, short(previous), rel.Generation, rel.ID)
	}
	if record.CriteriaDigest != n.CriteriaSetDigest {
		return refuse(contract.RefusalCriteriaSetChanged, "the decision continued the child under criteria %s and the plan holds %s for %s: revise the plan so the node holds the set the child was given, then record the generation", record.CriteriaDigest, n.CriteriaSetDigest, n.NodeID)
	}

	// the child was told: the decision itself was dispatched into the turn the generation is bound to
	var delivered string
	var dispatched sql.NullString
	if _, err := queryOne(ctx, q, "SELECT state, dispatch_turn_id FROM deliveries WHERE event_id = ?", []any{event}, &delivered, &dispatched); err != nil {
		return err
	}
	switch {
	case (delivered != delivery.Dispatched && delivered != delivery.Acknowledged) || strings.TrimSpace(dispatched.String) == "":
		return refuse(contract.RefusalDispositionConflict, "the decision %s has not been dispatched to the child of %s (its delivery is %q): decision-show --relationship %s prints where it stands; record the generation once it is dispatched", short(event), n.NodeID, delivered, rel.ID)
	case anchor != "bound" || anchorTurn.String == "":
		return refuse(contract.RefusalDispositionConflict, "the decision %s was dispatched into turn %s and generation %d of %s is not bound to it yet: the relay binds it on its next pass, record the generation then", short(event), dispatched.String, rel.Generation, rel.ID)
	case anchorTurn.String != dispatched.String:
		return refuse(contract.RefusalDispositionConflict, "the decision %s was dispatched into turn %s and generation %d of %s is bound to turn %s (a generation bound by hand is never rebound), so it is not recorded for %s: report this refusal and open no further generation",
			short(event), dispatched.String, rel.Generation, rel.ID, anchorTurn.String, n.NodeID)
	}

	// the manifest brought up to date: the dispatch manifest with the node's slice and criteria as the plan holds them and the revision, epoch, author and time of this recording; the digest is recomputed
	roots, err := relationshipRoots(ctx, q, rel.ID)
	if err != nil {
		return err
	}
	body := make(map[string]any, len(was)+1)
	for key, value := range was {
		body[key] = value
	}
	body["node_slice_digest"], body["criteria_set_digest"] = n.SliceDigest, n.CriteriaSetDigest
	body["plan_revision_no"], body["coordinator_epoch"] = snap.Revision, s.ExpectedEpoch
	body["created_by_task_id"], body["created_at"] = actor, s.now()
	delete(body, "manifest_digest")
	digest := dag.ManifestDigest(body)
	body["manifest_digest"] = digest
	if findings, err := s.VerifyManifest(ctx, q, plan, snap, n, body, VerifyOptions{SkipFileBytes: true, ArtifactRoots: roots}); err != nil {
		return err
	} else if len(findings) > 0 {
		return refuse(contract.RefusalDispositionConflict, "the manifest %s that the child was dispatched with does not rest on the inputs of node %s as they stand now (%s %s: %s): the child was not told what moved, so generation %d of %s is not recorded",
			short(previous), n.NodeID, findings[0].Code, findings[0].Reason, findings[0].Detail, rel.Generation, rel.ID)
	}
	if suppliedDigest != "" && suppliedDigest != digest {
		return refuse(contract.RefusalDispositionConflict, "the digest given (%s) is not the manifest of the decision (%s)", suppliedDigest, digest)
	}
	if digest != previous {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if _, err := (&dag.Repo{Store: s.Store, Now: s.Now}).PutManifest(ctx, raw); err != nil {
			return err
		}
	}
	if _, err := q.ExecContext(ctx, "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,?,?,'correction',?)",
		plan, n.NodeID, rel.ID, rel.Generation, digest, request); err != nil {
		return err
	}
	out.ManifestDigest, out.CarriedOver = digest, digest == previous
	out.OpenedBy, out.DispatchRequestID, out.DispatchTurnID = OpenedByDecisionReply, request, anchorTurn.String
	return nil
}
