package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
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
//   - on the relationship side, an open release intent, or an execution on a relationship that has not ended (relationshipExecutionCurrent).
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

// relationshipExecutionCurrent is whether the node's relationship side holds a current execution, and the record that says so. It is current while a release intent is open (latestRelease: a managed
// start that dag-release-close did not end), or while its executions stand on a relationship that has not ended: active or paused and not replaced, or, when a result of it is accepted, anything but cancelled.
// A relationship the store does not hold counts as current: the question is whether the node can be given to another kind of execution, and an unreadable record is no reason to say yes.
func relationshipExecutionCurrent(ctx context.Context, q store.Querier, plan, node string) (NodeExecution, bool, error) {
	out := NodeExecution{Kind: ExecutionRelationship, PlanID: plan, NodeID: node}
	open, intent, err := latestRelease(ctx, q, plan, node)
	if err != nil {
		return out, false, err
	}
	if intent {
		out.ManifestDigest, out.ManagedRequestID = open.Digest, open.Request
		if _, err := queryOne(ctx, q, "SELECT coordinator_epoch, decided_at FROM dag_releases WHERE plan_id = ? AND node_id = ? AND manifest_digest = ?", []any{plan, node, open.Digest}, &out.Epoch, &out.DecidedAt); err != nil {
			return out, false, err
		}
	}
	var count int
	if _, err := queryOne(ctx, q, "SELECT COUNT(*) FROM dag_node_executions WHERE plan_id = ? AND node_id = ?", []any{plan, node}, &count); err != nil {
		return out, false, err
	}
	if count == 0 {
		return out, intent, nil
	}
	// the newest execution of the relationship the node stands on
	rel, found, err := currentRelationshipOf(ctx, q, plan, node)
	if err != nil {
		return out, false, err
	}
	var generation int64
	var manifest, request string
	var ok bool
	if found {
		ok, err = queryOne(ctx, q, "SELECT execution_generation, manifest_digest, COALESCE(managed_request_id, '') FROM dag_node_executions WHERE plan_id = ? AND node_id = ? AND relationship_id = ?"+
			" ORDER BY execution_generation DESC LIMIT 1", []any{plan, node, rel.ID}, &generation, &manifest, &request)
		if err != nil {
			return out, false, err
		}
	}
	if !found || !ok {
		return out, true, nil
	}
	if !intent {
		out.ManifestDigest, out.ManagedRequestID = manifest, request
	}
	out.RelationshipID, out.Generation = rel.ID, generation
	if intent {
		return out, true, nil
	}
	_, accepted, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return out, false, err
	}
	switch {
	case accepted && rel.Status != "cancelled":
		return out, true, nil
	case !rel.Superseded && (rel.Status == "active" || rel.Status == "paused"):
		return out, true, nil
	}
	return out, false, nil
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
// on the relationship side the open intent for the manifest. A release that a closure ended, or that a later release of another manifest followed, is not current.
func releaseOf(ctx context.Context, q store.Querier, plan, node, manifest string) (NodeExecution, bool, error) {
	cur, hasAgent, err := currentAgentExecution(ctx, q, plan, node)
	if err != nil {
		return NodeExecution{}, false, err
	}
	var out NodeExecution
	if hasAgent {
		// the release the current execution stands on is the nearest row of kind initial or correction at or before it in the chain
		row := cur
		for row.Kind != AgentInitial && row.Kind != AgentCorrection && row.Supersedes != "" {
			if row, err = scanAgentExecution(q.QueryRowContext(ctx, "SELECT "+agentExecutionColumns+" FROM dag_agent_executions WHERE execution_id = ?", row.Supersedes)); err != nil {
				return NodeExecution{}, false, err
			}
		}
		hasAgent = (row.Kind == AgentInitial || row.Kind == AgentCorrection) && row.ManifestDigest == manifest
		if hasAgent {
			out = NodeExecution{Kind: ExecutionAgent, PlanID: plan, NodeID: node, ManifestDigest: manifest, Epoch: row.Epoch, DecidedAt: row.DecidedAt, Agent: row}
		}
	}
	intent, hasRel, err := latestRelease(ctx, q, plan, node)
	if err != nil {
		return NodeExecution{}, false, err
	}
	hasRel = hasRel && intent.Digest == manifest
	switch {
	case hasAgent && hasRel:
		return NodeExecution{}, false, refuse(contract.RefusalDispositionConflict, "%s: node %s of plan %s holds a release of manifest %s on both sides", CrossKindExecution, node, plan, manifest)
	case hasAgent:
		return out, true, nil
	case hasRel:
		rel := NodeExecution{Kind: ExecutionRelationship, PlanID: plan, NodeID: node, ManifestDigest: intent.Digest, ManagedRequestID: intent.Request}
		if _, err := queryOne(ctx, q, "SELECT coordinator_epoch, decided_at FROM dag_releases WHERE plan_id = ? AND node_id = ? AND manifest_digest = ?", []any{plan, node, intent.Digest}, &rel.Epoch, &rel.DecidedAt); err != nil {
			return NodeExecution{}, false, err
		}
		return rel, true, nil
	}
	return NodeExecution{}, false, nil
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
