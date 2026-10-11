package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The life of an agent node after its release (R07, CRW-1215). The release (R04), the acceptance and the integration (R05) and the node-scoped readers (R06) are elsewhere; this file holds what a node goes
// through while it is held by an orchestrator and what the orchestrator does when the plan or the node moves:
//
//   - a handover: the plan's parent changed, so the new parent appends a parent_handover execution row for the node (Adopt, epoch.go; adoptAgent below). The same manifest, worktree and thread, no new
//     release and no operator: an agent node has no merge turn to hand over. An acceptance made before it stands, and whoever executionOf names integrates the node;
//   - a stale result: the route of an accepted agent node whose slice the plan changed is agent_correction (revalidation.go), not dag-correct. AgentCorrect closes the acceptance (superseded) and releases
//     the node again as a correction attempt of the SAME node: a row of kind correction that supersedes the current one, with a worktree of its own and a manifest rebuilt as the plan holds the node. The
//     node's lane runs again and accepts the reworked output as it accepts any output. A change of the criteria alone is no correction: the same head is judged again by a new reviewer call and accepted
//     again (acceptAgent), under the key (execution, head, criteria set) of R05, so the new acceptance row follows the old one and no execution row is written;
//   - dag-restart reports an agent node with its state (released, accepted, integrated, held), the orchestrator and run that hold it, and the action it needs (resumeOfAgent);
//   - giving a node up: a cancel (crw flow cancel) or an escalation (tiers_exhausted, blocked_scope, size_split) closes the execution (dag_agent_closures, state cancelled or escalated) and returns the slot in
//     the same transaction. A recovery hold (dispatch_unknown, a host failure) closes nothing and the node keeps its slot.
//
// The writer of these is one run process (R10); nothing here is a table of its own: the rows are the executions and closures of R04.

// The reasons AgentClose takes. AgentReasonFlowCancel is crw flow cancel; the three escalations give a node up; the two holds are recovery holds that keep the slot.
const (
	AgentReasonFlowCancel         = "flow_cancel"
	AgentEscalationTiersExhausted = "tiers_exhausted"
	AgentEscalationBlockedScope   = "blocked_scope"
	AgentEscalationSizeSplit      = "size_split"
	AgentHoldDispatchUnknown      = "dispatch_unknown"
	AgentHoldHostFailure          = "host_failure"
)

// The states of dag_agent_closures (the zone's CHECK).
const (
	AgentClosureCancelled  = "cancelled"
	AgentClosureEscalated  = "escalated"
	AgentClosureSuperseded = "superseded"
	AgentClosureRevoked    = "revoked"
)

// The states and actions dag-restart reports for an agent node.
const (
	AgentStateReleased   = "released"
	AgentStateAccepted   = "accepted"
	AgentStateIntegrated = "integrated"
	AgentStateHeld       = "held"

	AgentActionRunResume   = "run resume"
	AgentActionFlowRecover = "crw flow recover"
	AgentActionAdopt       = "dag-adopt"
	AgentActionIntegrate   = "dag-integrate"
	AgentActionCorrect     = "agent correction"
	AgentActionRereview    = "re-review"
)

// The reasons a slot goes back with when an execution is closed.
const (
	slotCancelledReason = "dag_agent_cancelled"
	slotEscalatedReason = "dag_agent_escalated"
)

// insertAgentClosure appends the closure of an execution or an acceptance.
func (s *Scheduler) insertAgentClosure(ctx context.Context, q store.Querier, subject, id, state string, slotReleased bool, actor, reason string) error {
	released := 0
	if slotReleased {
		released = 1
	}
	_, err := q.ExecContext(ctx, "INSERT INTO dag_agent_closures (subject_kind, subject_id, state, slot_released, actor_task_id, coordinator_epoch, reason, closed_at) VALUES (?,?,?,?,?,?,?,?)",
		subject, id, state, released, actor, s.ExpectedEpoch, reason, s.now())
	return err
}

// ---- handover ----

// AdoptOptions are the facts of an adoption that the store does not hold.
type AdoptOptions struct {
	// RunID is the flow run of the orchestrator that takes an agent node over; the run of the row it follows when empty.
	RunID string
}

// adoptAgent is Adopt for a node whose current execution is the agent execution cur: it appends the parent_handover row of the actor that replaced the orchestrator, in the transaction Adopt opened (the
// fence is checked). The actor has to be the registered parent of the plan's project. The row keeps the manifest, role, tier, pair, thread, worktree and base of the row it follows (the same work, held by
// another task), is the next attempt of the node and supersedes the current row; the slot, the acceptance and the closures are not touched. The actor that already holds the node adopts nothing (a replay).
func (s *Scheduler) adoptAgent(ctx context.Context, q store.Querier, plan, node, actor string, cur AgentExecution, opt AdoptOptions, out *AdoptResult) error {
	var project string
	if has, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &project); err != nil {
		return err
	} else if !has {
		return refuse(contract.RefusalUnregisteredScope, "plan %s has no revision", plan)
	}
	if err := requireProjectParent(ctx, q, project, actor); err != nil {
		return err
	}
	if opt.RunID != strings.TrimSpace(opt.RunID) || dag.HasControl(opt.RunID) {
		return refuse(contract.RefusalMalformedReceipt, "the run id %q has whitespace around it or a control character in it", opt.RunID)
	}
	out.ExecutionKind = ExecutionAgent
	if cur.Actor == actor {
		out.ExecutionID, out.Replayed = cur.ExecutionID, true
		return nil
	}
	row := cur
	row.Attempt, row.Kind, row.Actor, row.Supersedes = cur.Attempt+1, AgentParentHandoff, actor, cur.ExecutionID
	row.ExecutionID = AgentExecutionID(plan, node, cur.ManifestDigest, row.Attempt)
	row.Epoch, row.DecidedAt = s.ExpectedEpoch, s.now()
	if opt.RunID != "" {
		row.RunID = opt.RunID
	}
	if err := insertAgentExecution(ctx, q, row); err != nil {
		return err
	}
	out.ExecutionID = row.ExecutionID
	return nil
}

// ---- closing, escalation and hold ----

// AgentCloseInput says why an agent node's current execution is ended or held.
type AgentCloseInput struct {
	// Reason is one of AgentReasonFlowCancel (a cancel: state cancelled), AgentEscalationTiersExhausted, AgentEscalationBlockedScope and AgentEscalationSizeSplit (a node given up: state escalated),
	// AgentHoldDispatchUnknown and AgentHoldHostFailure (a recovery hold: nothing is closed and the slot stays).
	Reason string
}

// AgentCloseResult is what AgentClose answers.
type AgentCloseResult struct {
	PlanID, NodeID, ExecutionID, State, Reason string
	// RevokedAcceptance is the acceptance that was closed (revoked) with the execution, "" when the node had none standing.
	RevokedAcceptance string
	// Closed: the execution has a closure; Held: a recovery hold was recorded as the answer (nothing was written); SlotReleased: the closure returned the slot; Replayed: the closure was written before.
	Closed, Held, SlotReleased, Replayed bool
}

// closeKind is the class of a reason of AgentClose: the state of the closure and the reason the slot goes back with, or held.
func closeKind(reason string) (state, slotReason string, held, ok bool) {
	switch reason {
	case AgentReasonFlowCancel:
		return AgentClosureCancelled, slotCancelledReason, false, true
	case AgentEscalationTiersExhausted, AgentEscalationBlockedScope, AgentEscalationSizeSplit:
		return AgentClosureEscalated, slotEscalatedReason, false, true
	case AgentHoldDispatchUnknown, AgentHoldHostFailure:
		return "", "", true, true
	}
	return "", "", false, false
}

// AgentClose ends the current execution of an agent node, or records that it is held. A cancel and an escalation give the node up: the execution gets a closure row (cancelled or escalated, with the reason)
// and the slot goes back in the same transaction, as the task that holds it when the orchestrator is not the one that reserved it (releaseSlot). An accepted result that has not landed is revoked with it
// (a closure of the acceptance), so a node nobody runs does not keep satisfying the edges built on it; a result that landed is in the target and is not given up. A recovery hold closes nothing: the
// execution stays current, the slot stays held and the answer says Held. The actor is the registered parent of the plan's project that holds the execution. The same close again is a replay.
func (s *Scheduler) AgentClose(ctx context.Context, plan, node, actor string, in AgentCloseInput) (AgentCloseResult, error) {
	out := AgentCloseResult{PlanID: plan, NodeID: node, Reason: in.Reason}
	state, slotReason, held, ok := closeKind(in.Reason)
	if !ok {
		return out, refuse(contract.RefusalMalformedReceipt, "the reason of a close is %s, %s, %s, %s (a node given up) or %s, %s (a hold), not %q", AgentReasonFlowCancel, AgentEscalationTiersExhausted,
			AgentEscalationBlockedScope, AgentEscalationSizeSplit, AgentHoldDispatchUnknown, AgentHoldHostFailure, in.Reason)
	}
	q := s.Store.Q(ctx)
	if err := s.fence(ctx, q, plan, actor); err != nil {
		return out, err
	}
	var project string
	if has, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &project); err != nil {
		return out, err
	} else if !has {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no revision", plan)
	}
	if err := requireProjectParent(ctx, q, project, actor); err != nil {
		return out, err
	}
	end, found, err := agentChainEnd(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if !found {
		return out, refuse(contract.RefusalDispositionConflict, "node %s of plan %s has no agent execution to close", node, plan)
	}
	out.ExecutionID = end.ExecutionID
	if closure, closed, err := agentClosureOf(ctx, q, "execution", end.ExecutionID); err != nil {
		return out, err
	} else if closed {
		if held || closure.State != state || closure.Reason != in.Reason {
			return out, refuse(contract.RefusalDispositionConflict, "execution %s of %s was closed as %s for %s: it is not closed again, or held", end.ExecutionID, node, closure.State, closure.Reason)
		}
		out.Closed, out.Replayed, out.State, out.SlotReleased = true, true, closure.State, closure.SlotReleased
		return out, nil
	}
	if end.Actor != actor {
		return out, notOrchestrator(actor, end)
	}
	if held {
		out.Held = true
		return out, nil
	}
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		now, live, err := currentAgentExecution(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !live || now.ExecutionID != end.ExecutionID {
			return refuse(contract.RefusalStaleGeneration, "execution %s of %s is no longer the node's current execution: read the node again", end.ExecutionID, node)
		}
		if now.Actor != actor {
			return notOrchestrator(actor, now)
		}
		revoke, err := s.standingAgentAcceptance(txCtx, tx, plan, node, now)
		if err != nil {
			return err
		}
		out.RevokedAcceptance = ""
		if revoke != nil {
			if revoke.Head != "" {
				snap, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
				if err != nil {
					return err
				}
				if landed, _, err := s.agentNodeIntegrated(txCtx, tx, snap, *revoke); err != nil {
					return err
				} else if landed {
					return refuse(contract.RefusalDispositionConflict, "%s landed (its accepted head %s is contained in every target it lands on): a node that landed is not given up, its value is in the target", node, short(revoke.Head))
				}
			}
			out.RevokedAcceptance = revoke.AcceptanceID
		}
		if out.SlotReleased, err = s.releaseSlot(txCtx, tx, plan, node, actor, slotReason); err != nil {
			return err
		}
		if revoke != nil {
			if err := s.insertAgentClosure(txCtx, tx, "acceptance", revoke.AcceptanceID, AgentClosureRevoked, false, actor, in.Reason); err != nil {
				return err
			}
		}
		return s.insertAgentClosure(txCtx, tx, "execution", now.ExecutionID, state, out.SlotReleased, actor, in.Reason)
	})
	if err != nil {
		out.RevokedAcceptance, out.SlotReleased = "", false
		return out, err
	}
	out.Closed, out.State = true, state
	return out, nil
}

// standingAgentAcceptance is the node's current agent acceptance when it stands on the node's current execution (or one the current execution continues), nil when the node has none.
func (s *Scheduler) standingAgentAcceptance(ctx context.Context, q store.Querier, plan, node string, cur AgentExecution) (*AgentAcceptance, error) {
	acc, has, err := currentAgentAcceptance(ctx, q, plan, node)
	if err != nil || !has {
		return nil, err
	}
	stands, err := agentExecutionStands(ctx, q, cur, acc.ExecutionID)
	if err != nil || !stands {
		return nil, err
	}
	return &acc, nil
}

// ---- the correction attempt of a stale result ----

// AgentCorrectResult is what AgentCorrect answers: the release of the correction attempt, and the acceptance that was closed for it ("" on a replay).
type AgentCorrectResult struct {
	AgentReleaseResult
	SupersededAcceptance string
}

// agentCorrectionRoute is the refusal of a correction that is not owed: the node is not stale, its result landed, or its route is not agent_correction. It reads the node as the snapshot holds it.
func (s *Scheduler) agentCorrectionRoute(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, acc AgentAcceptance) (*Stale, error) {
	if acc.Head != "" {
		if landed, _, err := s.agentNodeIntegrated(ctx, q, snap, acc); err != nil {
			return nil, err
		} else if landed {
			return nil, refuse(contract.RefusalDispositionConflict, "%s landed (its accepted head %s is contained in every target it lands on): a node that landed is never run again, so no correction attempt is released for it; what changed above it is carried by a successor node of a new plan revision (contract 8.4, E-20)", n.NodeID, short(acc.Head))
		}
	}
	ctx, _ = memoFor(ctx, plan, snap)
	st, err := s.staleOf(ctx, q, plan, snap, n)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, refuse(contract.RefusalDispositionConflict, "the accepted result of %s is not stale: the plan did not move under it, so there is nothing to correct", n.NodeID)
	}
	action, detail, err := s.routeStale(ctx, q, plan, snap, n, st)
	if err != nil {
		return nil, err
	}
	if action != ActionAgentCorrection {
		return nil, refuse(contract.RefusalDispositionConflict, "the accepted result of %s is stale (%s) and its route is %s, not %s: %s", n.NodeID, st.Reason(), action, ActionAgentCorrection, detail)
	}
	return st, nil
}

// judgeCorrection is the judgement of the manifest of a correction attempt: the one judgeNodeRelease makes for a release (the criteria the request carries are the plan's, the freshness of pinned pull requests,
// the manifest built and verified, the base the attempt starts from) without its readiness reading. The node is held by its own accepted execution until the closure of the acceptance, so the reading would not
// call it ready; what the reading checks is judged by the route (agentCorrectionRoute): the plan lets the node run, nothing above it is stale and every input is there.
func (s *Scheduler) judgeCorrection(ctx context.Context, q store.Querier, plan, node, actor string, req ReleaseRequest) (releaseJudgement, error) {
	snap, n, err := liveNode(ctx, q, plan, node)
	if err != nil {
		return releaseJudgement{}, err
	}
	var base *BaseRef
	if n.Kind == dag.NodeImplementation {
		if req.Base == nil {
			return releaseJudgement{}, refuse(contract.RefusalMalformedReceipt, "an implementation node starts from a base: the request names its repository and ref")
		}
		if s.Tips == nil {
			return releaseJudgement{}, errors.New("this scheduler has no target reader, so it cannot read the tip the attempt starts from")
		}
		tip, err := s.Tips.Tip(ctx, req.Base.Repository, req.Base.Ref)
		if err != nil {
			return releaseJudgement{}, err
		}
		base = &BaseRef{Repository: req.Base.Repository, Ref: req.Base.Ref, SHA: tip.SHA}
	}
	if err := s.checkCriteria(n, req.Criteria); err != nil {
		return releaseJudgement{}, err
	}
	preds, err := s.pinnedPredecessors(ctx, q, plan, incomingEdges(snap, node))
	if err != nil {
		return releaseJudgement{}, err
	}
	if err := s.freshness(ctx, plan, actor, preds); err != nil {
		return releaseJudgement{}, err
	}
	opts := VerifyOptions{ArtifactRoots: req.ArtifactRoots}
	in := ManifestInput{Base: base, Volatile: req.Volatile, RuleVersion: req.RuleVersion, CreatedByTaskID: actor, CreatedAt: s.now()}
	body, blocked, err := s.BuildManifest(ctx, q, plan, snap, n, in, opts)
	if err != nil {
		return releaseJudgement{}, err
	}
	if len(blocked) > 0 {
		return releaseJudgement{}, refusalOfFinding(blocked[0])
	}
	if err := s.pinnedInputKeep(ctx, plan, body, opts); err != nil {
		return releaseJudgement{}, err
	}
	if findings, err := s.VerifyManifest(ctx, q, plan, snap, n, body, opts); err != nil {
		return releaseJudgement{}, err
	} else if len(findings) > 0 {
		return releaseJudgement{}, refusalOfFinding(findings[0])
	}
	checked := map[string]bool{}
	for _, p := range preds {
		checked[p.Acceptance.AcceptanceID] = true
	}
	for _, e := range incomingEdges(snap, node) {
		if !e.PinsCodeHead {
			continue
		}
		if acceptance := inputAcceptance(body, e.EdgeID); !checked[acceptance] {
			return releaseJudgement{}, refuse(contract.RefusalDispositionConflict, "the manifest rests on acceptance %s for the pinned edge %s, whose pull request the relay did not read: the predecessor's acceptance changed while the correction was judged; repeat it", acceptance, e.EdgeID)
		}
	}
	for _, p := range preds {
		if !restsOn(body, p.Acceptance.AcceptanceID) {
			return releaseJudgement{}, refuse(contract.RefusalDispositionConflict, "the acceptance of %s changed while its pull request %s#%d was being read; repeat the correction so the current head is checked", p.Acceptance.NodeID, p.Forge, p.Number)
		}
	}
	digest, _ := body["manifest_digest"].(string)
	return releaseJudgement{snap: snap, n: n, base: base, body: body, digest: digest, opts: opts}, nil
}

// rejudgeCorrection repeats under the lock what judgeCorrection said about the plan: the manifest still matches the node's slice, criteria and incoming edges, and every input still stands.
func (s *Scheduler) rejudgeCorrection(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, body map[string]any, opts VerifyOptions) error {
	edges := incomingEdges(snap, n.NodeID)
	ids := make([]string, len(edges))
	for i, e := range edges {
		ids[i] = e.EdgeID
	}
	var held []string
	inputs, _ := body["inputs"].([]any)
	for _, item := range inputs {
		in, _ := item.(map[string]any)
		held = append(held, textOf(in["edge_id"]))
	}
	sort.Strings(held)
	if body["node_slice_digest"] != n.SliceDigest || body["criteria_set_digest"] != n.CriteriaSetDigest || strings.Join(held, ",") != strings.Join(ids, ",") {
		return refuse(contract.RefusalDispositionConflict, "the plan changed between the judgement and the correction: slice %v is now %s", body["node_slice_digest"], n.SliceDigest)
	}
	findings, err := s.VerifyManifest(ctx, q, plan, snap, n, body, VerifyOptions{SkipFileBytes: true, ArtifactRoots: opts.ArtifactRoots})
	if err != nil {
		return err
	}
	if len(findings) > 0 {
		return refusalOfFinding(findings[0])
	}
	return nil
}

// AgentCorrect releases the correction attempt of an accepted agent node that the plan made stale and whose route is agent_correction (the result must be reworked; revalidation.go). The orchestrator that
// holds the execution closes the acceptance (superseded, with the stale reason) and appends a row of kind correction that supersedes the current execution: the next attempt of the same node, with the
// worktree and run the caller names (a worktree of its own), the role and tier it starts from, and a manifest rebuilt as the plan holds the node now, stored in the same transaction. The slot the node holds
// stays held; a node whose slot went back with its acceptance (a node without a head) reserves one as a release does. Nothing is written to a table of the relationship side and no child is made. The
// node's lane then runs again, and the reworked output is accepted as any output is: the acceptance follows the closed one.
//
// It is idempotent: a node whose current execution is a correction that no acceptance stands on answers that execution (Replayed). A node that is not stale, whose route is another, or whose head landed
// is refused disposition_conflict and nothing is written.
func (s *Scheduler) AgentCorrect(ctx context.Context, plan, node, actor string, req ReleaseRequest, opt AgentReleaseOptions) (AgentCorrectResult, error) {
	var out AgentCorrectResult
	out.PlanID, out.NodeID = plan, node
	opt, err := opt.resolved()
	if err != nil {
		return out, err
	}
	q := s.Store.Q(ctx)
	if err := s.fence(ctx, q, plan, actor); err != nil {
		return out, err
	}
	var project string
	if has, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &project); err != nil {
		return out, err
	} else if !has {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no revision", plan)
	}
	if err := requireProjectParent(ctx, q, project, actor); err != nil {
		return out, err
	}
	if _, _, err := executionOf(ctx, q, plan, node); err != nil {
		return out, err
	}
	cur, live, err := currentAgentExecution(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if !live {
		return out, refuse(contract.RefusalDispositionConflict, "node %s of plan %s has no current agent execution: a node whose execution a closure ended is released again (dag-release --surface agent)", node, plan)
	}
	if cur.Actor != actor {
		return out, notOrchestrator(actor, cur)
	}
	acc, err := s.standingAgentAcceptance(ctx, q, plan, node, cur)
	if err != nil {
		return out, err
	}
	if acc == nil {
		if cur.Kind == AgentCorrection {
			res, err := s.agentReplay(ctx, cur)
			out.AgentReleaseResult = res
			return out, err
		}
		return out, refuse(contract.RefusalDispositionConflict, "node %s has no accepted result that stands on its current execution %s: there is nothing to correct", node, cur.ExecutionID)
	}
	snap0, n0, err := liveNode(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if _, err := s.agentCorrectionRoute(ctx, q, plan, snap0, n0, *acc); err != nil {
		return out, err
	}
	j, err := s.judgeCorrection(ctx, q, plan, node, actor, req)
	if err != nil {
		return out, err
	}
	row := AgentExecution{ExecutionID: AgentExecutionID(plan, node, j.digest, cur.Attempt+1), PlanID: plan, NodeID: node, Attempt: cur.Attempt + 1, Kind: AgentCorrection, ManifestDigest: j.digest,
		Role: opt.Role, Tier: opt.Tier, Worktree: opt.Worktree, RunID: opt.RunID, Actor: actor, Epoch: s.ExpectedEpoch, Supersedes: cur.ExecutionID}
	if j.base != nil {
		row.BaseSHA = j.base.SHA
	}
	var stash error
	var concurrent *AgentExecution
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		now, live, err := currentAgentExecution(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		switch {
		case live && now.ExecutionID != cur.ExecutionID && now.Kind == AgentCorrection && now.Supersedes == cur.ExecutionID:
			// another call released this correction first: this call is its replay
			concurrent = &now
			return nil
		case !live || now.ExecutionID != cur.ExecutionID:
			stash = refuse(contract.RefusalDispositionConflict, "the executions of %s changed while the correction was judged; repeat it", node)
			return nil
		}
		standing, err := s.standingAgentAcceptance(txCtx, tx, plan, node, now)
		if err != nil {
			return err
		}
		if standing == nil || standing.AcceptanceID != acc.AcceptanceID {
			stash = refuse(contract.RefusalDispositionConflict, "the accepted result of %s changed while the correction was judged; repeat it", node)
			return nil
		}
		snap, n, err := stillLive(txCtx, tx, plan, node)
		if err != nil {
			if refused, ferr := asRefusal(err); ferr != nil {
				return ferr
			} else if refused != nil {
				stash = refused
				return nil
			}
		}
		st, err := s.agentCorrectionRoute(txCtx, tx, plan, snap, n, *acc)
		if refused, ferr := asRefusal(err); ferr != nil {
			return ferr
		} else if refused != nil {
			stash = refused
			return nil
		}
		if err := releaseGate(txCtx, tx, plan, node, j.digest); err != nil {
			if refused, ferr := asRefusal(err); ferr != nil {
				return ferr
			} else if refused != nil {
				stash = refused
				return nil
			}
		}
		if refused, err := asRefusal(s.rejudgeCorrection(txCtx, tx, plan, snap, n, j.body, j.opts)); err != nil {
			return err
		} else if refused != nil {
			stash = refused
			return nil
		}
		if refused, err := asRefusal(agentRegionOwners(txCtx, tx, plan, snap, n)); err != nil {
			return err
		} else if refused != nil {
			stash = refused
			return nil
		}
		// the slot: a node that holds one keeps it (an accepted head not landed holds its slot, and its holder may be the previous orchestrator); a node whose slot went back with its acceptance reserves
		// one from the frozen manifest, as a release does
		if state, _, err := s.slotState(txCtx, tx, plan, node); err != nil {
			return err
		} else if state != "held" {
			if refused, err := asRefusal(s.reserveFrozen(txCtx, tx, project, plan, node, j.digest, actor, j.body)); err != nil {
				return err
			} else if refused != nil {
				stash = refused
				return nil
			}
		}
		stored, err := json.Marshal(j.body)
		if err != nil {
			return err
		}
		if _, err := (&dag.Repo{Store: s.Store, Now: s.Now}).PutManifest(txCtx, stored); err != nil {
			return err
		}
		if _, err := frozenManifestNode(txCtx, tx, node, j.digest, nil); err != nil {
			return err
		}
		if err := s.insertAgentClosure(txCtx, tx, "acceptance", acc.AcceptanceID, AgentClosureSuperseded, false, actor, st.Reason()); err != nil {
			return err
		}
		row.DecidedAt = s.now()
		return insertAgentExecution(txCtx, tx, row)
	})
	if err != nil {
		return out, err
	}
	if stash != nil {
		return out, stash
	}
	if concurrent != nil {
		res, err := s.agentReplay(ctx, *concurrent)
		out.AgentReleaseResult = res
		return out, err
	}
	res, err := s.agentResult(ctx, s.Store.Q(ctx), row, false)
	out.AgentReleaseResult, out.SupersededAcceptance = res, acc.AcceptanceID
	return out, err
}

// ---- dag-restart ----

// RestartAgent is what dag-restart says of an agent node: the execution that is current (its kind and attempt), the state, the orchestrator and the run that hold it, and the action it needs.
type RestartAgent struct {
	ExecutionID, ExecutionKind, State, Actor, RunID, Action string
	Attempt                                                 int64
}

// agentWorkOf is the executions the current row continues: itself, the parent_handover rows before it and the row the handover started from (the same work held by other orchestrators). A call of any of them is
// a call of the work the current row holds.
func agentWorkOf(ctx context.Context, q store.Querier, cur AgentExecution) ([]string, error) {
	ids := []string{cur.ExecutionID}
	row := cur
	for row.Kind == AgentParentHandoff && row.Supersedes != "" {
		prev, err := scanAgentExecution(q.QueryRowContext(ctx, "SELECT "+agentExecutionColumns+" FROM dag_agent_executions WHERE execution_id = ?", row.Supersedes))
		if err != nil {
			return nil, err
		}
		ids = append(ids, prev.ExecutionID)
		row = prev
	}
	return ids, nil
}

// agentCallHeld is whether the newest call row of the work the execution holds says the run does not know where it stands: the call ended held or with an unknown outcome (a dispatch whose answer never came, a
// host failure), or never ended. The journal of a store that has none holds nothing.
func agentCallHeld(ctx context.Context, q store.Querier, plan, node string, cur AgentExecution) (bool, error) {
	has, err := tableExists(ctx, q, "flow_calls")
	if err != nil || !has {
		return false, err
	}
	ids, err := agentWorkOf(ctx, q, cur)
	if err != nil {
		return false, err
	}
	args := []any{plan, node}
	for _, id := range ids {
		args = append(args, id)
	}
	var event string
	found, err := queryOne(ctx, q, "SELECT event FROM flow_calls WHERE kind = 'agent' AND plan_id = ? AND node_id = ? AND execution_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+") ORDER BY rowid DESC LIMIT 1", args, &event)
	if err != nil || !found {
		return false, err
	}
	switch event {
	case "completed", "null_model", "refused", "superseded":
		return false, nil
	}
	return true, nil
}

// resumeOfAgent fills the report of a node whose current execution is an agent execution and answers whether it did (a node of the relationship side is the caller's). The state is released (the execution
// runs), accepted (an acceptance stands on it), integrated (the accepted head landed) or held (the newest call is held or of unknown outcome); the actor and the run are the ones the current row names. What is
// outstanding is resumed by the orchestrator that holds the node: the run resumes a released node, `crw flow recover` settles a held one, the accepted head is integrated and a stale result is corrected; a
// node another orchestrator holds is adopted first (dag-adopt). A result that landed, or an accepted one with no head to land, needs nothing.
func (s *Scheduler) resumeOfAgent(ctx context.Context, q store.Querier, plan, actor string, n NodeReading, out *RestartNode) (bool, error) {
	cur, live, err := currentAgentExecution(ctx, q, plan, n.NodeID)
	if err != nil || !live {
		return false, err
	}
	if _, _, err := executionOf(ctx, q, plan, n.NodeID); err != nil {
		if crossKindHeld(err) {
			return false, nil
		}
		return false, err
	}
	a := &RestartAgent{ExecutionID: cur.ExecutionID, ExecutionKind: cur.Kind, Attempt: cur.Attempt, Actor: cur.Actor, RunID: cur.RunID, State: AgentStateReleased}
	out.Agent, out.Resume = a, ResumeNone
	action := AgentActionRunResume
	outstanding := true
	switch n.State {
	case StateIntegrated:
		a.State, outstanding = AgentStateIntegrated, false
	case StateAccepted, StateStale:
		a.State, action = AgentStateAccepted, AgentActionIntegrate
		na, found, err := acceptanceOf(ctx, q, plan, n.NodeID)
		if err != nil {
			return true, err
		}
		switch {
		case n.State == StateStale && n.Stale != nil && n.Stale.Action == ActionAgentCorrection:
			action = AgentActionCorrect
		case n.State == StateStale && n.Stale != nil && n.Stale.Action == ActionRevalidate:
			action = AgentActionRereview
		case n.State == StateStale:
			outstanding = false // the route says hold: nothing to do on this node until what it waits for is repaired (the reading's detail names it)
		case !found || na.Agent.Head == "":
			outstanding = false // a result with no head to land: the acceptance is the node's value
		}
	default:
		held, err := agentCallHeld(ctx, q, plan, n.NodeID, cur)
		if err != nil {
			return true, err
		}
		if held {
			a.State, action = AgentStateHeld, AgentActionFlowRecover
		}
	}
	if !outstanding {
		return true, nil
	}
	switch {
	case cur.Actor != actor:
		a.Action = AgentActionAdopt
		out.Resume = ResumeAdoptNeeded
		out.ResumeDetail = "run dag-adopt: execution " + cur.ExecutionID + " is held by " + cur.Actor + " and the actor is the registered parent that replaced it; then " + action
	case a.State == AgentStateHeld:
		a.Action = action
		out.Resume, out.ResumeDetail = ResumeReconcile, "the newest call of execution "+cur.ExecutionID+" ended held or with an unknown outcome: settle it with "+action+" and never call it again"
	default:
		a.Action = action
		out.Resume, out.ResumeDetail = ResumeAdopt, "this actor holds execution "+cur.ExecutionID+": "+action
	}
	return true, nil
}

func (a RestartAgent) object() contract.OrderedObject {
	return contract.OrderedObject{{Key: "execution_id", Value: a.ExecutionID}, {Key: "execution_kind", Value: a.ExecutionKind}, {Key: "attempt", Value: a.Attempt}, {Key: "state", Value: a.State},
		{Key: "actor_task_id", Value: a.Actor}, {Key: "run_id", Value: optionalText(a.RunID)}, {Key: "action", Value: optionalText(a.Action)}}
}
