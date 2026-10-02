package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// EdgeStatus is whether one incoming edge is satisfied by what the store holds now (contract 2) and, when it is not, why,
// as a member of the closed vocabulary. Every predicate reads current rows and is scoped by plan_id: node and edge ids
// are plan-local, and the contract's SQL was written before the plan id existed.
type EdgeStatus struct {
	Satisfied    bool
	Reason       string
	Detail       string
	Since        string // the stored time the edge became satisfied; never a clock reading
	AcceptanceID string
	// The exact rows that satisfied the edge, so a manifest records the evidence the predicate used and not another row a looser query would pick.
	ObservationID    string // integrated: the observation integratedAt accepted
	DecisionID       string // decision: the decision or directive that settled it
	DecisionRevision int64
}

func wait(e dag.SnapEdge, detail string) EdgeStatus {
	return EdgeStatus{Reason: WaitEdge(e.EdgeID), Detail: detail}
}

func blocked(reason, detail string) EdgeStatus { return EdgeStatus{Reason: reason, Detail: detail} }

// queryOne scans one row; found is false when there is none.
func queryOne(ctx context.Context, q store.Querier, query string, args []any, dest ...any) (bool, error) {
	err := q.QueryRowContext(ctx, query, args...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

const acceptanceColumns = "acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, output_manifest_ref, evidence_digest, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, supersedes_acceptance_id, state"

func scanAcceptance(row interface{ Scan(...any) error }) (Acceptance, error) {
	var a Acceptance
	var head, repo, ref, evidence, supersedes sql.NullString
	var pr sql.NullInt64
	err := row.Scan(&a.AcceptanceID, &a.PlanID, &a.NodeID, &a.ManifestDigest, &a.RelationshipID, &a.ExecutionGeneration, &a.EventID, &a.RevisionHash,
		&a.CriteriaSetDigest, &a.Verdict, &head, &repo, &pr, &ref, &evidence, &a.AckTier, &a.VerdictTurnID, &a.RuleVersionJSON,
		&a.AcceptedByTask, &a.CoordinatorEpoch, &a.AcceptedAt, &supersedes, &a.State)
	a.HeadSHA, a.Repository, a.OutputManifestRef = head.String, repo.String, ref.String
	a.EvidenceDigest, a.SupersedesAcceptanceID, a.PRNumber = evidence.String, supersedes.String, pr.Int64
	return a, err
}

// loadActiveAcceptance is the active acceptance of a node in a plan: there is at most one (dag_acceptances_active).
func loadActiveAcceptance(ctx context.Context, q store.Querier, plan, node string) (Acceptance, bool, error) {
	a, err := scanAcceptance(q.QueryRowContext(ctx, "SELECT "+acceptanceColumns+" FROM dag_acceptances WHERE plan_id = ? AND node_id = ? AND state = 'active'", plan, node))
	if errors.Is(err, sql.ErrNoRows) {
		return Acceptance{}, false, nil
	}
	return a, err == nil, err
}

// effectiveCriteria is the criteria digest an acceptance stands on: the newest re-validation of the same accepted output,
// else the digest it was accepted with (contract E-11 against the shipped one-row-per-output index).
func effectiveCriteria(ctx context.Context, q store.Querier, a Acceptance) (string, error) {
	var digest string
	found, err := queryOne(ctx, q, "SELECT criteria_set_digest FROM dag_acceptance_revalidations WHERE acceptance_id = ? ORDER BY reval_seq DESC LIMIT 1", []any{a.AcceptanceID}, &digest)
	if err != nil || !found {
		return a.CriteriaSetDigest, err
	}
	return digest, nil
}

type relRow struct {
	ID, Status, IssueKey, ParentTaskID string
	Generation                         int64
	// Superseded is a relationship the registry replaced with another (relationships.superseded_by): its result is nobody's to accept.
	Superseded bool
}

func loadRelationship(ctx context.Context, q store.Querier, rid string) (relRow, bool, error) {
	var r relRow
	found, err := queryOne(ctx, q, "SELECT relationship_id, status, issue_key, parent_task_id, execution_generation, superseded_by IS NOT NULL FROM relationships WHERE relationship_id = ?", []any{rid}, &r.ID, &r.Status, &r.IssueKey, &r.ParentTaskID, &r.Generation, &r.Superseded)
	return r, found, err
}

// currentRelationshipOf is the relationship a node's executions currently stand on: a live one first, then the newest.
func currentRelationshipOf(ctx context.Context, q store.Querier, plan, node string) (relRow, bool, error) {
	var r relRow
	found, err := queryOne(ctx, q, "SELECT r.relationship_id, r.status, r.issue_key, r.parent_task_id, r.execution_generation, r.superseded_by IS NOT NULL"+
		" FROM dag_node_executions e JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" WHERE e.plan_id = ? AND e.node_id = ?"+
		" ORDER BY CASE r.status WHEN 'active' THEN 0 WHEN 'paused' THEN 1 ELSE 2 END, r.created_at DESC, r.relationship_id DESC LIMIT 1",
		[]any{plan, node}, &r.ID, &r.Status, &r.IssueKey, &r.ParentTaskID, &r.Generation, &r.Superseded)
	return r, found, err
}

func nodeOf(snap dag.Snapshot, id string) (dag.SnapNode, bool) {
	for _, n := range snap.Nodes {
		if n.NodeID == id {
			return n, true
		}
	}
	return dag.SnapNode{}, false
}

// edgeStatus decides one incoming edge (docs/relay/dag-scheduler.md, "Edge satisfaction").
func (s *Scheduler) edgeStatus(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, e dag.SnapEdge) (EdgeStatus, error) {
	from, ok := nodeOf(snap, e.FromNodeID)
	if !ok {
		return EdgeStatus{}, errors.New("edge " + e.EdgeID + " starts at a node the plan does not hold")
	}
	cancelled := false
	if rel, found, err := currentRelationshipOf(ctx, q, plan, e.FromNodeID); err != nil {
		return EdgeStatus{}, err
	} else if found && rel.Status == "cancelled" {
		cancelled = true
	}
	switch e.Kind {
	case dag.EdgeArtifactVerified:
		if cancelled {
			return blocked(BlockedPredecessorCancelled, "the predecessor was cancelled; its outgoing artifact edges stay unsatisfied until the plan is revised"), nil
		}
		return s.artifactVerified(ctx, q, plan, snap, e, from)
	case dag.EdgeIntegrated:
		st, err := s.integratedEdge(ctx, q, plan, e, from)
		if err != nil {
			return EdgeStatus{}, err
		}
		if !st.Satisfied && cancelled {
			return blocked(BlockedPredecessorCancelled, "the predecessor was cancelled before its merge landed"), nil
		}
		return st, nil
	case dag.EdgeDecision:
		if cancelled {
			return blocked(BlockedPredecessorCancelled, "the predecessor was cancelled; its outgoing decision edges stay unsatisfied until the plan is revised"), nil
		}
		return s.decisionEdge(ctx, q, plan, e)
	}
	return EdgeStatus{}, errors.New("edge " + e.EdgeID + " has an unknown kind " + e.Kind)
}

// standing is what every edge that rests on an accepted result needs of the acceptance, whatever the edge's kind: the row is what its digest says and whole, it belongs to an execution of
// the node, and the criteria it stands on are the plan's and the relationship's now. A blocked answer is returned for the first link that fails; nil means all hold.
func (s *Scheduler) standing(ctx context.Context, q store.Querier, plan string, from dag.SnapNode, a Acceptance) (*EdgeStatus, error) {
	// 2. the row is what its digest says, and whole.
	if AcceptanceDigest(a) != a.AcceptanceID {
		st := blocked(BlockedAcceptanceTampered, "the acceptance row no longer digests to its id")
		return &st, nil
	}
	// contract 4.3: every required field, the ones the identity leaves out included; an implementation node also needs its evidence digest.
	if a.ManifestDigest == "" || a.RelationshipID == "" || a.EventID == "" || a.RevisionHash == "" || a.CriteriaSetDigest == "" || a.VerdictTurnID == "" || a.AckTier == "" ||
		a.AcceptedByTask == "" || a.AcceptedAt == "" || a.RuleVersionJSON == "" || (from.Kind == dag.NodeImplementation && a.EvidenceDigest == "") {
		st := blocked(BlockedAcceptanceIncomplete, "a required column of the acceptance is blank")
		return &st, nil
	}
	// 3. the acceptance belongs to an execution of this node (a forged row on a foreign relationship opens nothing).
	var one int
	tied, err := queryOne(ctx, q, "SELECT 1 FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ? AND execution_generation = ?",
		[]any{plan, from.NodeID, a.RelationshipID, a.ExecutionGeneration}, &one)
	if err != nil {
		return nil, err
	}
	if !tied {
		st := blocked(BlockedInputUnaccepted, "the acceptance is not tied to an execution of the node")
		return &st, nil
	}
	// 4. the criteria it stands on are the plan's and the relationship's now.
	effective, err := effectiveCriteria(ctx, q, a)
	if err != nil {
		return nil, err
	}
	var rows, distinct int
	var canonical sql.NullString
	if _, err := queryOne(ctx, q, "SELECT COUNT(*), COUNT(DISTINCT set_digest), MIN(set_digest) FROM canonical_criteria WHERE relationship_id = ?", []any{a.RelationshipID}, &rows, &distinct, &canonical); err != nil {
		return nil, err
	}
	if effective != from.CriteriaSetDigest || rows == 0 || distinct != 1 || canonical.String != effective {
		st := blocked(BlockedStaleCriteria, "the acceptance's criteria digest is not the plan's and the registered criteria's")
		return &st, nil
	}
	return nil, nil
}

// consumedStanding is step 6: what the accepted node consumed is still what its predecessors' acceptances are (Frankenbuild, E-25).
func (s *Scheduler) consumedStanding(ctx context.Context, q store.Querier, plan string, a Acceptance) (*EdgeStatus, error) {
	body, mfound, err := dag.ReadManifestOn(ctx, q, a.ManifestDigest)
	var corrupt *dag.CorruptError
	switch {
	case errors.As(err, &corrupt):
		st := blocked(BlockedManifestTampered, corrupt.Detail)
		return &st, nil
	case err != nil:
		return nil, err
	case !mfound:
		st := blocked(BlockedAcceptanceIncomplete, "the manifest the acceptance consumed is not stored")
		return &st, nil
	}
	if inputs, ok := body["inputs"].([]any); ok {
		for _, item := range inputs {
			in, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, _ := in["acceptance_id"].(string)
			if id == "" {
				continue
			}
			// the consumed acceptance must be the active acceptance of the node the manifest names, in THIS plan (ids are plan-local).
			fromNode, _ := in["from_node_id"].(string)
			var one int
			active, err := queryOne(ctx, q, "SELECT 1 FROM dag_acceptances WHERE acceptance_id = ? AND plan_id = ? AND node_id = ? AND state = 'active'", []any{id, plan, fromNode}, &one)
			if err != nil {
				return nil, err
			}
			if !active {
				st := blocked(BlockedStalePredecessor, "an input of the accepted node, acceptance "+id+", is no longer active")
				return &st, nil
			}
		}
	}
	return nil, nil
}

// recordedEvidence is the part of step 8 every edge on an implementation node needs: the latest merge check the relay recorded still digests to what it recorded (B-13). It returns the head
// that check observed, if there is one.
func (s *Scheduler) recordedEvidence(ctx context.Context, q store.Querier, a Acceptance) (observed string, st *EdgeStatus, err error) {
	var digest, evidence string
	seen, err := queryOne(ctx, q, "SELECT checks_digest, evidence_json, observed_head_sha FROM dag_merge_checks WHERE acceptance_id = ? ORDER BY check_seq DESC LIMIT 1", []any{a.AcceptanceID}, &digest, &evidence, &observed)
	if err != nil || !seen {
		return "", nil, err
	}
	if recomputed, perr := RecomputeEvidenceDigest(evidence); perr != nil || recomputed != digest {
		blockedStatus := blocked(BlockedEvidenceMismatch, "the latest merge check no longer digests to what was recorded")
		return observed, &blockedStatus, nil
	}
	return observed, nil, nil
}

// artifactVerified is contract 2.1 as the reader applies it: the durable acceptance plus the currency checks that make a stale
// result open nothing.
func (s *Scheduler) artifactVerified(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, e dag.SnapEdge, from dag.SnapNode) (EdgeStatus, error) {
	a, found, err := loadActiveAcceptance(ctx, q, plan, e.FromNodeID)
	if err != nil {
		return EdgeStatus{}, err
	}
	if !found {
		return wait(e, "the predecessor has no active acceptance"), nil
	}
	if st, err := s.standing(ctx, q, plan, from, a); err != nil || st != nil {
		return valueOf(st), err
	}
	// 5. the accepted event is still the head of its generation while the relationship lives.
	rel, relFound, err := loadRelationship(ctx, q, a.RelationshipID)
	if err != nil {
		return EdgeStatus{}, err
	}
	if relFound && (rel.Status == "active" || rel.Status == "paused") {
		if rel.Generation != a.ExecutionGeneration {
			return blocked(BlockedStaleHead, "the relationship moved to generation "+itoa64(rel.Generation)), nil
		}
		head, err := delivery.HeadRevisionFrom(ctx, q, a.RelationshipID, a.ExecutionGeneration)
		if err != nil {
			return EdgeStatus{}, err
		}
		if id, _ := objString(head, "eventId"); id != a.EventID {
			return blocked(BlockedStaleHead, "the accepted revision is no longer the head of its generation"), nil
		}
	}
	// 6. what the accepted node consumed is still what its predecessors' acceptances are (Frankenbuild, E-25).
	if st, err := s.consumedStanding(ctx, q, plan, a); err != nil || st != nil {
		return valueOf(st), err
	}
	// 7. a code pin needs the accepted head, the target and the pull request to read it from.
	if e.PinsCodeHead {
		var forge int
		hasForge, err := queryOne(ctx, q, "SELECT 1 FROM dag_acceptance_forge WHERE acceptance_id = ?", []any{a.AcceptanceID}, &forge)
		if err != nil {
			return EdgeStatus{}, err
		}
		if a.HeadSHA == "" || a.PRNumber < 1 || !hasForge || a.Repository != e.TargetRepository {
			return blocked(BlockedAcceptanceIncomplete, "a pinned acceptance needs its head, its pull request and the edge's target"), nil
		}
	}
	// 8. the relay's latest observation of the pull request (the forge is read at release, judge and accept, never by a reader).
	if from.Kind == dag.NodeImplementation {
		observed, st, err := s.recordedEvidence(ctx, q, a)
		if err != nil || st != nil {
			return valueOf(st), err
		}
		if observed != "" && observed != a.HeadSHA {
			return blocked(BlockedStaleHead, "the pull request head was observed at "+observed+" after "+a.HeadSHA+" was accepted"), nil
		}
	}
	// 9. the accepted result still rests on the plan as it is now: a stale predecessor opens no edge, so nothing is released onto it (contract 8.2, E-25). This is the last check, so the reasons
	// above (criteria, head, consumed inputs) are the ones shown first when several hold; the judgement itself is asked directly and does not depend on them.
	if st, err := s.stalePredecessor(ctx, q, plan, snap, e, from); err != nil || st != nil {
		return valueOf(st), err
	}
	return EdgeStatus{Satisfied: true, Since: a.AcceptedAt, AcceptanceID: a.AcceptanceID}, nil
}

// valueOf is a blocked status as a value (the zero status when there is none, for a caller that returns it with an error).
func valueOf(st *EdgeStatus) EdgeStatus {
	if st == nil {
		return EdgeStatus{}
	}
	return *st
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func objString(o delivery.Obj, key string) (string, bool) {
	for _, f := range o {
		if f.Key == key {
			s, ok := f.Value.(string)
			return s, ok
		}
	}
	return "", false
}

// IntegratedAt is whether an accepted head is contained in a target branch and the parent recorded the merge.
type IntegratedAt struct {
	Satisfied   bool
	Unprovable  bool // observations exist, none says contained, and a merge turn landed this head: a squash or rebase landing
	Observation string
	Since       string
}

// integratedAt is P-INT (contract 2.2) for one acceptance and one target, not for an edge kind: a terminal node and a stacked
// predecessor have targets too. It needs a contained observation of the accepted head with no later one that says otherwise, the parent's
// merged mark on the same event, generation and revision, and, when a merge turn carried the observation, that turn landing the same head on the
// same target. The observation it returns is the EARLIEST such one: the first positive observation of the current containment run (after the last
// negative one). Ancestry is monotone, so a merge that moves the target changes no answer, and the landed commit and the time the edge became satisfied
// that consumers recorded must not follow every later observation of a moved tip (contract E-27).
func (s *Scheduler) integratedAt(ctx context.Context, q store.Querier, plan string, a Acceptance, repository, baseRef string) (IntegratedAt, error) {
	var out IntegratedAt
	found, err := queryOne(ctx, q, "SELECT o.observation_id, o.observed_at"+
		" FROM dag_acceptances a"+
		" JOIN dag_integration_observations o ON o.acceptance_id = a.acceptance_id AND o.repository = ? AND o.base_ref = ?"+
		"  AND o.subject_sha = a.head_sha AND o.is_ancestor = 1 AND o.reverted_by IS NULL"+
		" JOIN assignment_marks k ON k.relationship_id = a.relationship_id AND k.mark = 'merged' AND k.event_id = a.event_id"+
		"  AND k.execution_generation = a.execution_generation AND k.revision_hash = a.revision_hash"+
		" WHERE a.acceptance_id = ? AND a.plan_id = ? AND a.head_sha IS NOT NULL AND a.head_sha <> ''"+
		"  AND NOT EXISTS (SELECT 1 FROM dag_integration_observations o2 WHERE o2.acceptance_id = a.acceptance_id AND o2.repository = o.repository"+
		"   AND o2.base_ref = o.base_ref AND o2.observed_seq > o.observed_seq AND o2.is_ancestor = 0)"+
		"  AND (o.merge_turn_id IS NULL OR EXISTS (SELECT 1 FROM merge_turns m WHERE m.turn_id = o.merge_turn_id AND m.state = 'landed'"+
		"   AND m.candidate_head = a.head_sha AND m.repository = o.repository AND m.base_ref = o.base_ref))"+
		" ORDER BY o.observed_seq ASC LIMIT 1",
		[]any{repository, baseRef, a.AcceptanceID, plan}, &out.Observation, &out.Since)
	if err != nil {
		return IntegratedAt{}, err
	}
	if found {
		out.Satisfied = true
		return out, nil
	}
	// the diagnosis follows the CURRENT observation: an older negative followed by a positive one is a merge still waiting for its mark, not a squash.
	var one, latest int
	seen, err := queryOne(ctx, q, "SELECT is_ancestor FROM dag_integration_observations WHERE acceptance_id = ? AND repository = ? AND base_ref = ? ORDER BY observed_seq DESC LIMIT 1", []any{a.AcceptanceID, repository, baseRef}, &latest)
	if err != nil || !seen || latest != 0 || a.HeadSHA == "" {
		return out, err
	}
	landed, err := queryOne(ctx, q, "SELECT 1 FROM merge_turns WHERE repository = ? AND base_ref = ? AND candidate_head = ? AND state = 'landed' LIMIT 1", []any{repository, baseRef, a.HeadSHA}, &one)
	out.Unprovable = landed
	return out, err
}

func (s *Scheduler) integratedEdge(ctx context.Context, q store.Querier, plan string, e dag.SnapEdge, from dag.SnapNode) (EdgeStatus, error) {
	a, found, err := loadActiveAcceptance(ctx, q, plan, e.FromNodeID)
	if err != nil {
		return EdgeStatus{}, err
	}
	if !found {
		return wait(e, "the predecessor has no active acceptance"), nil
	}
	if a.HeadSHA == "" {
		return blocked(BlockedAcceptanceIncomplete, "an integrated edge needs the accepted head"), nil
	}
	// a landing does not make a result current: a result accepted against criteria the plan or the relationship has since replaced, a forged or altered row, an acceptance on a foreign
	// relationship and an input that is no longer what the node consumed open nothing, as they open nothing on an artifact edge. The head's currency is not asked: what landed is in the target.
	if st, err := s.standing(ctx, q, plan, from, a); err != nil || st != nil {
		return valueOf(st), err
	}
	if st, err := s.consumedStanding(ctx, q, plan, a); err != nil || st != nil {
		return valueOf(st), err
	}
	if _, st, err := s.recordedEvidence(ctx, q, a); err != nil || st != nil {
		return valueOf(st), err
	}
	at, err := s.integratedAt(ctx, q, plan, a, e.TargetRepository, e.TargetBaseRef)
	if err != nil {
		return EdgeStatus{}, err
	}
	switch {
	case at.Satisfied:
		return EdgeStatus{Satisfied: true, Since: at.Since, AcceptanceID: a.AcceptanceID, ObservationID: at.Observation}, nil
	case at.Unprovable:
		return blocked(BlockedIntegrationUnprovable, "a merge landed this head but the target does not contain it (a squash or rebase landing)"), nil
	}
	return wait(e, "the accepted head is not yet observed in "+e.TargetRepository+" "+e.TargetBaseRef+" with the merged mark"), nil
}

// decisionEdge is contract 2.3: a recorded decision (P-DEC-2) or a settled supervisor directive (P-DEC-1) carrying the digest the plan fixed,
// by an authority the edge names. Authority text is opaque (D-09): it is compared, never read.
func (s *Scheduler) decisionEdge(ctx context.Context, q store.Querier, plan string, e dag.SnapEdge) (EdgeStatus, error) {
	authority, err := json.Marshal(e.RequiredAuthority)
	if err != nil {
		return EdgeStatus{}, err
	}
	var id, at string
	var revision int64
	found, err := queryOne(ctx, q, "SELECT x.decision_id, x.recorded_at, x.revision FROM dag_decisions x WHERE x.plan_id = ? AND x.subject = ? AND x.digest = ?"+
		" AND x.disposition = 'approved' AND x.state = 'active' AND x.authority_kind IN (SELECT value FROM json_each(?)) ORDER BY x.revision DESC LIMIT 1",
		[]any{plan, e.DecisionSubject, e.DecisionDigest, string(authority)}, &id, &at, &revision)
	if err != nil {
		return EdgeStatus{}, err
	}
	if found {
		return EdgeStatus{Satisfied: true, Since: at, DecisionID: id, DecisionRevision: revision}, nil
	}
	var project string
	if ok, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &project); err != nil {
		return EdgeStatus{}, err
	} else if ok {
		found, err = queryOne(ctx, q, "SELECT d.directive_id, d.decided_at FROM scope_directives d"+
			" JOIN scope_links l ON l.link_id = d.link_id AND l.link_kind = 'execution' AND l.status = 'active' AND l.superseded_by IS NULL"+
			" JOIN scope_bindings sup ON sup.scope_kind = l.upper_kind AND sup.scope_key = l.upper_key AND sup.task_id = d.from_task_id AND sup.status = 'active' AND sup.superseded_by IS NULL"+
			" JOIN scope_bindings own ON own.scope_kind = d.scope_kind AND own.scope_key = d.scope_key AND own.task_id = d.decided_by AND own.role = 'parent'"+
			"  AND own.created_at <= d.decided_at AND (own.superseded_by IS NULL OR own.updated_at >= d.decided_at)"+
			" WHERE d.scope_kind = 'project' AND d.scope_key = ? AND d.digest = ? AND d.disposition = 'chosen' LIMIT 1",
			[]any{project, e.DecisionDigest}, &id, &at)
		if err != nil {
			return EdgeStatus{}, err
		}
		if found {
			return EdgeStatus{Satisfied: true, Since: at, DecisionID: id}, nil
		}
	}
	var one int
	// an APPROVED decision of this subject that does not carry the digest or the authority the edge names; a rejected or withdrawn one is no approval at all.
	mismatch, err := queryOne(ctx, q, "SELECT 1 FROM dag_decisions WHERE plan_id = ? AND subject = ? AND state = 'active' AND disposition = 'approved' LIMIT 1", []any{plan, e.DecisionSubject}, &one)
	if err != nil {
		return EdgeStatus{}, err
	}
	if mismatch {
		return blocked(BlockedDecisionMismatch, "an approved decision of this subject is recorded, but not with this digest or an authority the edge names"), nil
	}
	return EdgeStatus{Reason: DeferAuthorityPending, Detail: "no recorded decision carries the digest the plan fixed"}, nil
}
