package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The union readers of the node's execution (R04, CRW-1212). A node is run on one of two surfaces: by a managed Codex child, whose release, executions and relationship are rows of the frozen tables
// (dag_releases, dag_node_executions, relationships), or by an agent of the flow engine, whose release and executions are rows of dag_agent_executions with the closures of dag_agent_closures. Nothing of
// an agent node is ever written into a frozen table, so a reader that wants "the node's current execution" asks executionOf, which looks on both sides and answers the one that is current, and a
// reader that wants "the release of this manifest" asks releaseOf. The readers of the frozen tables that need the node-scoped answer move to these in R06 (docs/relay/agent-execution-readers.md lists them);
// the readers that are relationship-scoped stay as they are.
//
// What is current:
//
//   - on the agent side, the last row of the node's supersedes chain, unless a closure row closes it. The chain cannot branch (the root and the successor of a row are unique keys of the table), so
//     there is one last row; a closed last row leaves the node with no current agent execution, and an earlier row of the chain, superseded, is never current again;
//   - on the relationship side, an open release intent (a release no child is bound to yet), or the newest execution of a relationship that has not ended (relationshipExecutionCurrent): a release that
//     a child was bound to is history, and ends with its relationship.
//
// A node has at most one current execution of one kind: the agent release refuses while a relationship-side execution is current (live_relationship_execution), the relationship release refuses while an
// agent execution without a closure is current (live_agent_execution), and executionOf fails closed on a store that nevertheless holds both (cross_kind_execution).

// The side of the node's current execution.
const (
	ExecutionRelationship = "relationship"
	ExecutionAgent        = "agent"
)

// The closed reasons of the cross-kind exclusion. D-02: this build registers no new refusal reason, so each travels in the detail of a disposition_conflict.
const (
	LiveRelationshipExecution = "live_relationship_execution"
	LiveAgentExecution        = "live_agent_execution"
	CrossKindExecution        = "cross_kind_execution"
)

// The kinds of an agent execution row. A row of kind initial or correction is also the release record of its manifest.
const (
	AgentInitial       = "initial"
	AgentCorrection    = "correction"
	AgentTierStep      = "tier_step"
	AgentParentHandoff = "parent_handover"
)

// AgentExecution is one row of dag_agent_executions.
type AgentExecution struct {
	ExecutionID, PlanID, NodeID string
	Attempt                     int64
	Kind, ManifestDigest        string
	Role, Tier                  string
	AuthorModel, AuthorEffort   string
	ThreadID, Worktree, BaseSHA string
	RunID, Actor                string
	Epoch                       int64
	DecidedAt                   string
	// Supersedes is the execution this one follows in the node's chain, "" for the root.
	Supersedes string
}

const agentExecutionColumns = "execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, author_model, author_effort, thread_id, worktree, base_sha, run_id, actor_task_id, coordinator_epoch, decided_at, COALESCE(supersedes_execution_id, '')"

func scanAgentExecution(row interface{ Scan(...any) error }) (AgentExecution, error) {
	var e AgentExecution
	err := row.Scan(&e.ExecutionID, &e.PlanID, &e.NodeID, &e.Attempt, &e.Kind, &e.ManifestDigest, &e.Role, &e.Tier, &e.AuthorModel, &e.AuthorEffort, &e.ThreadID, &e.Worktree, &e.BaseSHA,
		&e.RunID, &e.Actor, &e.Epoch, &e.DecidedAt, &e.Supersedes)
	return e, err
}

// AgentExecutionID is the id of the execution row a release of a manifest writes: a pure function of the node, the manifest and the attempt, so a replay is the same row.
func AgentExecutionID(plan, node, manifest string, attempt int64) string {
	return requestID("agent-execution", plan, node, manifest, strconv.FormatInt(attempt, 10))
}

// agentChainEnd is the last row of a node's supersedes chain, closed or not: the row no other row follows. There is at most one (the successor key of the table), and none for a node that has no agent
// row. A store whose zone predates the agent tables has none either: a read-only open installs nothing, and the zone arrives with the first write open.
func agentChainEnd(ctx context.Context, q store.Querier, plan, node string) (AgentExecution, bool, error) {
	has, err := tableExists(ctx, q, "dag_agent_executions")
	if err != nil || !has {
		return AgentExecution{}, false, err
	}
	e, err := scanAgentExecution(q.QueryRowContext(ctx, "SELECT "+agentExecutionColumns+" FROM dag_agent_executions e WHERE e.plan_id = ? AND e.node_id = ?"+
		" AND NOT EXISTS (SELECT 1 FROM dag_agent_executions n WHERE n.supersedes_execution_id = e.execution_id)"+
		" ORDER BY e.attempt DESC, e.decided_at DESC, e.execution_id DESC LIMIT 1", plan, node))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentExecution{}, false, nil
	}
	return e, err == nil, err
}

// agentClosure is the closure row of a subject, with its state.
type agentClosure struct {
	Subject, ID, State, Reason string
	SlotReleased               bool
}

// agentClosureOf is the closure of an execution or an acceptance, if one was written.
func agentClosureOf(ctx context.Context, q store.Querier, subject, id string) (agentClosure, bool, error) {
	has, err := tableExists(ctx, q, "dag_agent_closures")
	if err != nil || !has {
		return agentClosure{}, false, err
	}
	var c agentClosure
	var released int
	found, err := queryOne(ctx, q, "SELECT subject_kind, subject_id, state, reason, slot_released FROM dag_agent_closures WHERE subject_kind = ? AND subject_id = ?", []any{subject, id}, &c.Subject, &c.ID, &c.State, &c.Reason, &released)
	c.SlotReleased = released == 1
	return c, found, err
}

// currentAgentExecution is the node's current agent execution: the last row of its chain when no closure ends it.
func currentAgentExecution(ctx context.Context, q store.Querier, plan, node string) (AgentExecution, bool, error) {
	end, found, err := agentChainEnd(ctx, q, plan, node)
	if err != nil || !found {
		return AgentExecution{}, false, err
	}
	if _, closed, err := agentClosureOf(ctx, q, "execution", end.ExecutionID); err != nil || closed {
		return AgentExecution{}, false, err
	}
	return end, true, nil
}

// NodeExecution is the current execution record of a node on either side: the kind says which fields are filled.
type NodeExecution struct {
	Kind           string // ExecutionRelationship or ExecutionAgent
	PlanID, NodeID string
	ManifestDigest string
	// Epoch and DecidedAt are the coordinator epoch and the time of the release the execution stands on (when the side keeps them).
	Epoch     int64
	DecidedAt string
	// The relationship side: the managed request of the release, and the relationship and generation of the execution once a child is bound (empty and 0 for an open intent that has none yet).
	ManagedRequestID, RelationshipID string
	Generation                       int64
	// The agent side.
	Agent AgentExecution
}

// relationshipExecutionCurrent is whether the node's relationship side holds a current execution, and the record that says so.
//
// A release is an open INTENT only until a child is bound to it: the managed start of the release (latestRelease: a dag_releases or rereleased row that dag-release-close did not end) has no
// dag_node_executions row of its managed request yet. An open intent holds the node, whatever the history under it. A bound release is history: dag-release-close never ends it, so what is current is
// decided by the executions and the relationship they stand on: the newest execution of the relationship the node stands on (a correction's generation and manifest, not the first release's), current
// while the relationship has not ended: never once the registry replaced it (superseded_by), else active or paused, or, when an acceptance of that relationship's result stands, anything but cancelled. A relationship the store does not hold counts as current: the
// question is whether the node can be given to another kind of execution, and an unreadable record is no reason to say yes.
func relationshipExecutionCurrent(ctx context.Context, q store.Querier, plan, node string) (NodeExecution, bool, error) {
	out := NodeExecution{Kind: ExecutionRelationship, PlanID: plan, NodeID: node}
	open, intent, err := latestRelease(ctx, q, plan, node)
	if err != nil {
		return out, false, err
	}
	if intent {
		var bound int
		if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND managed_request_id = ?", []any{plan, node, open.Request}, &bound); err != nil {
			return out, false, err
		}
		if bound == 0 {
			out.ManifestDigest, out.ManagedRequestID = open.Digest, open.Request
			if _, err := queryOne(ctx, q, "SELECT coordinator_epoch, decided_at FROM dag_releases WHERE plan_id = ? AND node_id = ? AND manifest_digest = ?", []any{plan, node, open.Digest}, &out.Epoch, &out.DecidedAt); err != nil {
				return out, false, err
			}
			return out, true, nil
		}
	}
	var count int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = ? AND node_id = ?", []any{plan, node}, &count); err != nil || count == 0 {
		return out, false, err
	}
	// the newest execution of the relationship the node stands on
	rel, found, err := currentRelationshipOf(ctx, q, plan, node)
	if err != nil {
		return out, false, err
	}
	if !found {
		return out, true, nil
	}
	ok, err := relationshipExecutionRow(ctx, q, plan, node, rel.ID, false, &out)
	if err != nil {
		return out, false, err
	}
	if !ok {
		return out, true, nil
	}
	// a relationship the registry replaced has ended, whatever was accepted of it: its result is nobody's to accept
	if rel.Superseded {
		return out, false, nil
	}
	if rel.Status == "active" || rel.Status == "paused" {
		return out, true, nil
	}
	// an ended relationship is current only while an acceptance of ITS result stands: another relationship's acceptance does not keep it
	acceptance, accepted, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return out, false, err
	}
	return out, accepted && acceptance.RelationshipID == rel.ID && rel.Status != "cancelled", nil
}

// relationshipExecutionRow fills out from the newest execution of a relationship (the newest release row, kind initial or correction, when release is set): its generation, manifest and managed request,
// and the epoch and time of the release the relationship was started by. It reports whether the relationship has such an execution.
func relationshipExecutionRow(ctx context.Context, q store.Querier, plan, node, relationship string, release bool, out *NodeExecution) (bool, error) {
	kinds := ""
	if release {
		kinds = " AND kind IN ('initial', 'correction')"
	}
	ok, err := queryOne(ctx, q, "SELECT execution_generation, manifest_digest, COALESCE(managed_request_id, '') FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ?"+kinds+
		" ORDER BY execution_generation DESC LIMIT 1", []any{plan, node, relationship}, &out.Generation, &out.ManifestDigest, &out.ManagedRequestID)
	if err != nil || !ok {
		return false, err
	}
	out.RelationshipID = relationship
	// the epoch and the time of the release the relationship was started by: the dag_releases row of its first execution's manifest
	_, err = queryOne(ctx, q, "SELECT d.coordinator_epoch, d.decided_at FROM dag_node_executions e JOIN dag_releases d ON d.plan_id = e.plan_id AND d.node_id = e.node_id AND d.manifest_digest = e.manifest_digest"+
		" WHERE e.plan_id = ? AND e.node_id = ? AND e.relationship_id = ? AND e.kind = 'initial' ORDER BY e.execution_generation LIMIT 1", []any{plan, node, relationship}, &out.Epoch, &out.DecidedAt)
	return true, err
}

// executionOf is the node's current execution, whichever side holds it. A node with no current execution answers false. A store in which both sides hold a current one answers a refusal and no record
// (cross_kind_execution): the exclusion of the two releases makes that impossible for a store this build wrote, and a store written another way is not read as either.
func executionOf(ctx context.Context, q store.Querier, plan, node string) (NodeExecution, bool, error) {
	agent, hasAgent, err := currentAgentExecution(ctx, q, plan, node)
	if err != nil {
		return NodeExecution{}, false, err
	}
	rel, hasRel, err := relationshipExecutionCurrent(ctx, q, plan, node)
	if err != nil {
		return NodeExecution{}, false, err
	}
	switch {
	case hasAgent && hasRel:
		return NodeExecution{}, false, refuse(contract.RefusalDispositionConflict, "%s: node %s of plan %s has a current execution on the relationship side (%s) and a current agent execution (%s); neither is read", CrossKindExecution, node, plan, orDash(rel.RelationshipID, rel.ManagedRequestID), agent.ExecutionID)
	case hasAgent:
		return NodeExecution{Kind: ExecutionAgent, PlanID: plan, NodeID: node, ManifestDigest: agent.ManifestDigest, Epoch: agent.Epoch, DecidedAt: agent.DecidedAt, Agent: agent}, true, nil
	case hasRel:
		return rel, true, nil
	}
	return NodeExecution{}, false, nil
}

func orDash(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return "-"
}

// releaseOf is the current release of a node under a manifest, whichever side holds it: on the agent side the row of kind initial or correction (the release fact) of the current chain whose manifest it is,
// on the relationship side the open intent, or else the newest release row (initial or correction) of the relationship the current execution stands on. A release that a closure ended, that a later release of another manifest followed, or whose relationship ended, is not current.
func releaseOf(ctx context.Context, q store.Querier, plan, node, manifest string) (NodeExecution, bool, error) {
	cur, hasAgent, err := currentAgentExecution(ctx, q, plan, node)
	if err != nil {
		return NodeExecution{}, false, err
	}
	rel, hasRel, err := relationshipReleaseOf(ctx, q, plan, node)
	if err != nil {
		return NodeExecution{}, false, err
	}
	// a store in which both sides hold a current execution is read as neither, whatever manifest is asked for (the same refusal as executionOf)
	if hasAgent && hasRel {
		return NodeExecution{}, false, refuse(contract.RefusalDispositionConflict, "%s: node %s of plan %s has a current execution on the relationship side (%s) and a current agent execution (%s); neither is read", CrossKindExecution, node, plan, orDash(rel.RelationshipID, rel.ManagedRequestID), cur.ExecutionID)
	}
	if hasAgent {
		// the release the current execution stands on is the nearest row of kind initial or correction at or before it in the chain
		row := cur
		for row.Kind != AgentInitial && row.Kind != AgentCorrection && row.Supersedes != "" {
			if row, err = scanAgentExecution(q.QueryRowContext(ctx, "SELECT "+agentExecutionColumns+" FROM dag_agent_executions WHERE execution_id = ?", row.Supersedes)); err != nil {
				return NodeExecution{}, false, err
			}
		}
		if (row.Kind == AgentInitial || row.Kind == AgentCorrection) && row.ManifestDigest == manifest {
			return NodeExecution{Kind: ExecutionAgent, PlanID: plan, NodeID: node, ManifestDigest: manifest, Epoch: row.Epoch, DecidedAt: row.DecidedAt, Agent: row}, true, nil
		}
		return NodeExecution{}, false, nil
	}
	if hasRel && rel.ManifestDigest == manifest {
		return rel, true, nil
	}
	return NodeExecution{}, false, nil
}

// relationshipReleaseOf is the release the node's current relationship-side execution stands on: the open intent while no child is bound to it, else the newest release row (initial or correction) of the
// relationship the execution stands on. A relationship that has ended, or whose result no acceptance keeps, has no current release.
func relationshipReleaseOf(ctx context.Context, q store.Querier, plan, node string) (NodeExecution, bool, error) {
	cur, current, err := relationshipExecutionCurrent(ctx, q, plan, node)
	if err != nil || !current {
		return NodeExecution{}, false, err
	}
	if cur.RelationshipID == "" {
		return cur, true, nil // the open intent
	}
	if _, err := relationshipExecutionRow(ctx, q, plan, node, cur.RelationshipID, true, &cur); err != nil {
		return NodeExecution{}, false, err
	}
	return cur, true, nil
}

// refuseLiveAgentExecution is the refusal of a relationship release of a node whose agent execution is current: the node is not given to a managed child while an agent holds it.
func refuseLiveAgentExecution(ctx context.Context, q store.Querier, plan, node string) error {
	cur, live, err := currentAgentExecution(ctx, q, plan, node)
	if err != nil || !live {
		return err
	}
	return refuse(contract.RefusalDispositionConflict, "%s: node %s of plan %s is held by agent execution %s (attempt %d, released under manifest %s) and no closure ends it, so a managed child is not started for it", LiveAgentExecution, node, plan, cur.ExecutionID, cur.Attempt, short(cur.ManifestDigest))
}

// refuseLiveRelationshipExecution is the refusal of an agent release of a node whose relationship side holds a current execution, also after the node was resized (update_node) and is released again.
func refuseLiveRelationshipExecution(ctx context.Context, q store.Querier, plan, node string) error {
	rel, live, err := relationshipExecutionCurrent(ctx, q, plan, node)
	if err != nil || !live {
		return err
	}
	return refuse(contract.RefusalDispositionConflict, "%s: node %s of plan %s has a current execution on the relationship side (%s), so an agent is not given the node", LiveRelationshipExecution, node, plan, orDash(rel.RelationshipID, rel.ManagedRequestID))
}

// The union readers of the node's acceptance (R05, CRW-1213). An agent node's accepted result is a row of dag_agent_acceptances (an acceptance of the frozen dag_acceptances is a relationship's), so a reader that
// wants "the node's current acceptance" asks acceptanceOf, which looks on both sides. A node has at most one current acceptance of one kind: the agent side is the last row of the node's acceptance chain unless a
// dag_agent_closures row of kind acceptance ends it, the relationship side is the active row of dag_acceptances, and acceptanceOf fails closed on a store that nevertheless holds both (cross_kind_acceptance).
// The readers that are node-scoped but still read the relationship side only move to acceptanceOf in R06 (docs/relay/agent-execution-readers.md).

// The kinds of a node's acceptance, named like the kinds of its execution.
const (
	AcceptanceRelationship = ExecutionRelationship
	AcceptanceAgent        = ExecutionAgent
)

// CrossKindAcceptance is the closed reason of the cross-kind exclusion of acceptances (D-02: it travels in the detail of a disposition_conflict, like the reasons of the executions).
const CrossKindAcceptance = "cross_kind_acceptance"

// AgentAcceptance is one row of dag_agent_acceptances: the accepted result of an agent execution. Head, Base and everything the head carries are ” for a node without a head.
type AgentAcceptance struct {
	AcceptanceID, PlanID, NodeID, ManifestDigest, ExecutionID string
	// Head is the commit the relay read from git; Base the commit it descends from; Repository the checkout both were proved in and the integration target is judged against.
	Head, Base, Repository string
	// The pre-merge record and the verification record the acceptance stands on, with their digests and bodies.
	PremergeDigest, PremergeJSON, PremergeEvaluatedHead string
	VerificationDigest, VerificationJSON, TreeSHA       string
	// CriteriaSetDigest is the criteria set the reviewer saw and the plan fixed.
	CriteriaSetDigest string
	// The reviewer call the run recorded, its thread and turn, and the pair it ran on; AuthorPairsDigest is the digest of the author pairs observed when the acceptance was made.
	ReviewerCallID, ReviewerThreadID, ReviewerTurnID, ReviewerModel, ReviewerEffort string
	AuthorPairsDigest                                                               string
	AcceptedBy                                                                      string
	Epoch                                                                           int64
	AcceptedAt                                                                      string
	// Supersedes is the acceptance this one follows in the node's chain, "" for the root.
	Supersedes string
}

const agentAcceptanceColumns = "acceptance_id, plan_id, node_id, manifest_digest, execution_id, head, base, repository, premerge_digest, premerge_json, premerge_evaluated_head, verification_digest, verification_json, tree_sha, criteria_set_digest, reviewer_call_id, reviewer_thread_id, reviewer_turn_id, reviewer_model, reviewer_effort, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at, COALESCE(supersedes_acceptance_id, '')"

// agentAcceptanceInsertColumns are the columns an INSERT names: agentAcceptanceColumns with the NULL of the root read as ”.
const agentAcceptanceInsertColumns = "acceptance_id, plan_id, node_id, manifest_digest, execution_id, head, base, repository, premerge_digest, premerge_json, premerge_evaluated_head, verification_digest, verification_json, tree_sha, criteria_set_digest, reviewer_call_id, reviewer_thread_id, reviewer_turn_id, reviewer_model, reviewer_effort, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at, supersedes_acceptance_id"

func scanAgentAcceptance(row interface{ Scan(...any) error }) (AgentAcceptance, error) {
	var a AgentAcceptance
	err := row.Scan(&a.AcceptanceID, &a.PlanID, &a.NodeID, &a.ManifestDigest, &a.ExecutionID, &a.Head, &a.Base, &a.Repository, &a.PremergeDigest, &a.PremergeJSON, &a.PremergeEvaluatedHead,
		&a.VerificationDigest, &a.VerificationJSON, &a.TreeSHA, &a.CriteriaSetDigest, &a.ReviewerCallID, &a.ReviewerThreadID, &a.ReviewerTurnID, &a.ReviewerModel, &a.ReviewerEffort,
		&a.AuthorPairsDigest, &a.AcceptedBy, &a.Epoch, &a.AcceptedAt, &a.Supersedes)
	return a, err
}

// AgentAcceptanceID is the id of the acceptance row of an execution's head under a criteria set: a pure function of the key of the table, so a replay is the same row.
func AgentAcceptanceID(plan, node, execution, head, criteria string) string {
	return requestID("agent-acceptance", plan, node, execution, head, criteria)
}

// agentAcceptanceChainEnd is the last row of a node's acceptance chain, ended or not: the row no other row follows. There is at most one (the successor key of the table), and none for a node that has no
// agent acceptance. A store whose zone predates the table has none either: a read-only open installs nothing.
func agentAcceptanceChainEnd(ctx context.Context, q store.Querier, plan, node string) (AgentAcceptance, bool, error) {
	has, err := tableExists(ctx, q, "dag_agent_acceptances")
	if err != nil || !has {
		return AgentAcceptance{}, false, err
	}
	a, err := scanAgentAcceptance(q.QueryRowContext(ctx, "SELECT "+agentAcceptanceColumns+" FROM dag_agent_acceptances a WHERE a.plan_id = ? AND a.node_id = ?"+
		" AND NOT EXISTS (SELECT 1 FROM dag_agent_acceptances n WHERE n.supersedes_acceptance_id = a.acceptance_id)"+
		" ORDER BY a.accepted_at DESC, a.acceptance_id DESC LIMIT 1", plan, node))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentAcceptance{}, false, nil
	}
	return a, err == nil, err
}

// currentAgentAcceptance is the node's current agent acceptance: the last row of its chain when no closure of kind acceptance ends it. A superseded row is never current again.
func currentAgentAcceptance(ctx context.Context, q store.Querier, plan, node string) (AgentAcceptance, bool, error) {
	end, found, err := agentAcceptanceChainEnd(ctx, q, plan, node)
	if err != nil || !found {
		return AgentAcceptance{}, false, err
	}
	if _, closed, err := agentClosureOf(ctx, q, "acceptance", end.AcceptanceID); err != nil || closed {
		return AgentAcceptance{}, false, err
	}
	return end, true, nil
}

// loadAgentAcceptanceByID is the agent acceptance with an id, current or not.
func loadAgentAcceptanceByID(ctx context.Context, q store.Querier, id string) (AgentAcceptance, bool, error) {
	has, err := tableExists(ctx, q, "dag_agent_acceptances")
	if err != nil || !has {
		return AgentAcceptance{}, false, err
	}
	a, err := scanAgentAcceptance(q.QueryRowContext(ctx, "SELECT "+agentAcceptanceColumns+" FROM dag_agent_acceptances WHERE acceptance_id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return AgentAcceptance{}, false, nil
	}
	return a, err == nil, err
}

// agentExecutionStands is whether the execution an acceptance stands on (accepted) is still the work the node's current execution holds: it is the current execution, or one the current execution continues. A
// parent_handover row continues the execution it follows (the same work under the same manifest, held by another orchestrator); every other kind starts new work, so an acceptance of an earlier
// execution does not stand on it.
func agentExecutionStands(ctx context.Context, q store.Querier, current AgentExecution, accepted string) (bool, error) {
	row := current
	for {
		if row.ExecutionID == accepted {
			return true, nil
		}
		if row.Kind != AgentParentHandoff || row.Supersedes == "" {
			return false, nil
		}
		prev, err := scanAgentExecution(q.QueryRowContext(ctx, "SELECT "+agentExecutionColumns+" FROM dag_agent_executions WHERE execution_id = ?", row.Supersedes))
		if err != nil {
			return false, err
		}
		row = prev
	}
}

// NodeAcceptance is the current acceptance of a node on either side: the kind says which field is filled.
type NodeAcceptance struct {
	Kind           string // AcceptanceRelationship or AcceptanceAgent
	PlanID, NodeID string
	AcceptanceID   string
	Relationship   Acceptance
	Agent          AgentAcceptance
}

// acceptanceOf is the node's current acceptance, whichever side holds it. A node with none answers false. A store in which both sides hold a current acceptance answers a refusal and no record
// (cross_kind_acceptance): the exclusion of the two executions makes that impossible for a store this build wrote, and a store written another way is not read as either.
func acceptanceOf(ctx context.Context, q store.Querier, plan, node string) (NodeAcceptance, bool, error) {
	agent, hasAgent, err := currentAgentAcceptance(ctx, q, plan, node)
	if err != nil {
		return NodeAcceptance{}, false, err
	}
	rel, hasRel, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return NodeAcceptance{}, false, err
	}
	switch {
	case hasAgent && hasRel:
		return NodeAcceptance{}, false, refuse(contract.RefusalDispositionConflict, "%s: node %s of plan %s has the active acceptance %s on the relationship side and the current agent acceptance %s; neither is read", CrossKindAcceptance, node, plan, rel.AcceptanceID, agent.AcceptanceID)
	case hasAgent:
		return NodeAcceptance{Kind: AcceptanceAgent, PlanID: plan, NodeID: node, AcceptanceID: agent.AcceptanceID, Agent: agent}, true, nil
	case hasRel:
		return NodeAcceptance{Kind: AcceptanceRelationship, PlanID: plan, NodeID: node, AcceptanceID: rel.AcceptanceID, Relationship: rel}, true, nil
	}
	return NodeAcceptance{}, false, nil
}

// agentMarked is whether the merged mark of an agent acceptance was written: a marked row of the acceptance and its execution in dag_agent_integration_stages (the agent's counterpart of the parent's
// assignment mark, which an agent node never has).
func agentMarked(ctx context.Context, q store.Querier, a AgentAcceptance) (bool, error) {
	has, err := tableExists(ctx, q, "dag_agent_integration_stages")
	if err != nil || !has {
		return false, err
	}
	var one int
	return queryOne(ctx, q, "SELECT 1 FROM dag_agent_integration_stages WHERE acceptance_id = ? AND execution_id = ? AND stage = 'marked' LIMIT 1", []any{a.AcceptanceID, a.ExecutionID}, &one)
}

// agentIntegratedAt is P-INT for one agent acceptance and one target: the head the relay read from git is contained in the target branch by an observation that no later observation contradicts, and the
// merged mark of the same acceptance and execution was written (agentMarked). A merge turn is no part of it: an agent node never enters one, so an observation carried by a turn does not count differently
// and a diagnosis of a squash landing does not exist. The observation it returns is the earliest of the current containment run, as integratedAt returns it. dag_integration_observations is keyed by the
// acceptance id alone, with no relationship column and no foreign key, so the agent acceptance id is stored there as it is.
func agentIntegratedAt(ctx context.Context, q store.Querier, a AgentAcceptance, repository, baseRef string) (IntegratedAt, error) {
	var out IntegratedAt
	if a.Head == "" {
		return out, nil
	}
	marked, err := agentMarked(ctx, q, a)
	if err != nil || !marked {
		return out, err
	}
	found, err := queryOne(ctx, q, "SELECT o.observation_id, o.observed_at FROM dag_integration_observations o"+
		" WHERE o.acceptance_id = ? AND o.repository = ? AND o.base_ref = ? AND o.subject_sha = ? AND o.is_ancestor = 1 AND o.reverted_by IS NULL"+
		"  AND NOT EXISTS (SELECT 1 FROM dag_integration_observations o2 WHERE o2.acceptance_id = o.acceptance_id AND o2.repository = o.repository"+
		"   AND o2.base_ref = o.base_ref AND o2.observed_seq > o.observed_seq AND o2.is_ancestor = 0)"+
		" ORDER BY o.observed_seq ASC LIMIT 1", []any{a.AcceptanceID, repository, baseRef, a.Head}, &out.Observation, &out.Since)
	out.Satisfied = found
	return out, err
}

// agentTargets are the targets an agent acceptance has to land on: the integration branches its batches moved, on the repositories of the node's integrated edges, and every target it already has an
// observation for (nodeTargets, which judges an agent acceptance as a commit acceptance).
func (s *Scheduler) agentTargets(ctx context.Context, q store.Querier, snap dag.Snapshot, a AgentAcceptance) ([]Target, error) {
	return s.nodeTargets(ctx, q, snap, Acceptance{AcceptanceID: a.AcceptanceID, PlanID: a.PlanID, NodeID: a.NodeID, HeadSHA: a.Head, Repository: a.Repository})
}

// agentNodeIntegrated is whether an agent node's accepted head has landed everywhere it has to: it has a head, at least one target, and every target satisfies agentIntegratedAt.
func (s *Scheduler) agentNodeIntegrated(ctx context.Context, q store.Querier, snap dag.Snapshot, a AgentAcceptance) (bool, []Target, error) {
	if a.Head == "" {
		return false, nil, nil
	}
	targets, err := s.agentTargets(ctx, q, snap, a)
	if err != nil || len(targets) == 0 {
		return false, targets, err
	}
	for _, t := range targets {
		at, err := agentIntegratedAt(ctx, q, a, t.Repository, t.BaseRef)
		if err != nil || !at.Satisfied {
			return false, targets, err
		}
	}
	return true, targets, nil
}

// agentIntentDetail is the detail of the node-level intent row an agent candidate is frozen with: the criteria set the batch verifies it under (as a relationship row names it) and the highest observed_seq of
// the acceptance's observations of the batch's target (the checkout and integration ref the batch row will name) at the moment the batch froze it, 0 when there is none. Observation sequence numbers grow
// per acceptance and target, so every observation the batch could have landed the head for has a higher one: that is how agentLandedSHA binds an observation to the landing it proved.
func agentIntentDetail(ctx context.Context, q store.Querier, c Candidate, repository, integrationRef string) (string, error) {
	var seq sql.NullInt64
	if err := q.QueryRowContext(ctx, "SELECT MAX(observed_seq) FROM dag_integration_observations WHERE acceptance_id = ? AND repository = ? AND base_ref = ?", c.AcceptanceID, repository, integrationRef).Scan(&seq); err != nil {
		return "", err
	}
	raw, err := json.Marshal(agentIntent{CriteriaSetDigest: c.CriteriaSetDigest, ObservedSeqBefore: seq.Int64})
	return string(raw), err
}

// agentIntent is the detail of an agent intent row (agentIntentDetail).
type agentIntent struct {
	CriteriaSetDigest string `json:"criteria_set_digest"`
	ObservedSeqBefore int64  `json:"observed_seq_before"`
}

// agentLandedSHA is the commit an agent acceptance landed as on one target, for the observation with sequence number seq on that target: the merge commit a batch that moved that target recorded for it
// (dag_integration_batches.merged_json, the batch whose repository and integration_ref are the observation's repository and base_ref) when the batch merged it, else "" (a head the branch already contained
// was marked, not merged). The accepted head is not the landed commit when the relay merged the head into the branch: the merge commit is (CRW-1003 d3, the landed_sha attribution). An acceptance
// integrated into several targets has one merge commit per target, so the target selects it. Of several marked batches of the same target, the one that decides is the latest batch frozen BEFORE the
// observation: its intent row names the highest sequence number of the acceptance's observations of that target when it froze the acceptance (agentIntentDetail), and only a batch whose number is below seq
// can be the landing the observation proved. A batch frozen later (the target lost the merge and the acceptance was integrated again) has a number at or above seq, so it never changes the answer for an
// observation already made; the observation made after it answers the new merge commit (CRW-1213, evaluation d4). An intent row whose detail names no number (none of this build) does not bound the batch.
func agentLandedSHA(ctx context.Context, q store.Querier, acceptanceID, repository, baseRef string, seq int64) (string, error) {
	has, err := tableExists(ctx, q, "dag_agent_integration_stages")
	if err != nil || !has {
		return "", err
	}
	rows, err := q.QueryContext(ctx, "SELECT b.merged_json, COALESCE((SELECT i.detail FROM dag_agent_integration_stages i WHERE i.batch_id = k.batch_id AND i.acceptance_id = k.acceptance_id AND i.stage = 'intent' ORDER BY i.rowid LIMIT 1), '')"+
		" FROM dag_agent_integration_stages k JOIN dag_integration_batches b ON b.batch_id = k.batch_id"+
		" WHERE k.acceptance_id = ? AND k.stage = 'marked' AND b.repository = ? AND b.integration_ref = ? ORDER BY k.rowid DESC", acceptanceID, repository, baseRef)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	for rows.Next() {
		var raw, intent string
		if err := rows.Scan(&raw, &intent); err != nil {
			return "", err
		}
		var frozen struct {
			ObservedSeqBefore *int64 `json:"observed_seq_before"`
		}
		if json.Unmarshal([]byte(intent), &frozen) == nil && frozen.ObservedSeqBefore != nil && *frozen.ObservedSeqBefore >= seq {
			// the batch froze the acceptance after this observation was made: it is a later landing
			continue
		}
		var merged []struct{ AcceptanceID, MergeCommit string }
		if json.Unmarshal([]byte(raw), &merged) != nil {
			continue
		}
		for _, m := range merged {
			if m.AcceptanceID == acceptanceID && m.MergeCommit != "" {
				return m.MergeCommit, nil
			}
		}
	}
	return "", rows.Err()
}
