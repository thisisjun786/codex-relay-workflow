package dagsched

import (
	"context"
	"encoding/json"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The guards of the lifecycle steps that give up or run again the accepted result of an agent node (R07, CRW-1215): the landing guard (a result that landed or may have landed is kept) and the release
// admission of a correction attempt (the checks a release meets, with the node's own incumbent execution left out).

// agentLandingRefusal refuses a lifecycle step that would give up the accepted result of an agent node (a close revokes the acceptance, a correction supersedes it and runs the node again) when its head has
// landed or may have: the target holds it and an observation proved it (agentNodeIntegrated), or a batch of the acceptance moved the target, marked the head contained or planned the move and no observation
// made after the batch froze the acceptance says the target lacks the head (agentLandingOpen). IntegrateBatch moves the target and records the move before ObserveIntegration reads it, and the
// observation needs the acceptance to stay current, so the step waits for the observation: it settles a landing that happened, and shows the head absent when the target lost it (the step is then allowed).
func (s *Scheduler) agentLandingRefusal(ctx context.Context, q store.Querier, snap dag.Snapshot, node string, acc AgentAcceptance, what string) error {
	if acc.Head == "" {
		return nil
	}
	if landed, _, err := s.agentNodeIntegrated(ctx, q, snap, acc); err != nil {
		return err
	} else if landed {
		return refuse(contract.RefusalDispositionConflict, "%s landed (its accepted head %s is contained in every target it lands on): a node that landed is not %s, its value is in the target; what changed above it is carried by a successor node of a new plan revision (contract 8.4, E-20)", node, short(acc.Head), what)
	}
	open, batch, err := agentLandingOpen(ctx, q, acc)
	if err != nil {
		return err
	}
	if open {
		return refuse(contract.RefusalDispositionConflict, "the accepted head %s of %s may be in the target: batch %s moved it, marked it contained or planned to move it, and no observation made after the batch froze the node says the target lacks the head; observe the landing (dag-observe) first: a node whose head is in the target is not %s", short(acc.Head), node, batch, what)
	}
	return nil
}

// agentLandingOpen is whether a batch of an agent acceptance may have put its head in a target and no later reading settled it: the batch moved the ref (a ref_moved row), marked the acceptance (the head was
// already contained) or planned the move (the batch-level intent names the verified head, and a move that happened before its ref_moved row was written is only reconciled by the next batch), and the
// newest observation of the acceptance on that target is not a negative one made after the batch froze the acceptance (its intent row names the highest observation sequence number at that moment).
func agentLandingOpen(ctx context.Context, q store.Querier, acc AgentAcceptance) (bool, string, error) {
	has, err := tableExists(ctx, q, "dag_agent_integration_stages")
	if err != nil || !has {
		return false, "", err
	}
	type started struct {
		batch, ref, repository string
		before                 int64
	}
	var batches []started
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT c.batch_id, c.detail, b.detail, COALESCE(ib.repository, '') FROM dag_agent_integration_stages c"+
		" JOIN dag_integration_stages b ON b.batch_id = c.batch_id AND b.stage = 'intent' AND b.node_id = ''"+
		" LEFT JOIN dag_integration_batches ib ON ib.batch_id = c.batch_id"+
		" WHERE c.acceptance_id = ? AND c.stage = 'intent'"+
		" AND (EXISTS (SELECT 1 FROM dag_integration_stages m WHERE m.batch_id = c.batch_id AND m.stage = 'ref_moved')"+
		"  OR EXISTS (SELECT 1 FROM dag_agent_integration_stages k WHERE k.batch_id = c.batch_id AND k.acceptance_id = c.acceptance_id AND k.stage = 'marked')"+
		"  OR b.detail LIKE '%\"verified_head\"%')", acc.AcceptanceID)
	if err != nil {
		return false, "", err
	}
	for rows.Next() {
		var batch, nodeDetail, batchDetail, repository string
		if err := rows.Scan(&batch, &nodeDetail, &batchDetail, &repository); err != nil {
			_ = rows.Close()
			return false, "", err
		}
		var frozen struct {
			ObservedSeqBefore int64 `json:"observed_seq_before"`
		}
		_ = json.Unmarshal([]byte(nodeDetail), &frozen)
		var d map[string]string
		if json.Unmarshal([]byte(batchDetail), &d) != nil || d["integration_ref"] == "" {
			continue
		}
		batches = append(batches, started{batch: batch, ref: d["integration_ref"], repository: repository, before: frozen.ObservedSeqBefore})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, "", err
	}
	if err := rows.Close(); err != nil {
		return false, "", err
	}
	for _, b := range batches {
		var isAncestor, seq int64
		found, err := queryOne(ctx, q, "SELECT is_ancestor, observed_seq FROM dag_integration_observations WHERE acceptance_id = ? AND base_ref = ? AND (? = '' OR repository = ?) ORDER BY observed_seq DESC LIMIT 1",
			[]any{acc.AcceptanceID, b.ref, b.repository, b.repository}, &isAncestor, &seq)
		if err != nil {
			return false, "", err
		}
		if !found || isAncestor != 0 || seq <= b.before {
			return true, b.batch, nil
		}
	}
	return false, "", nil
}

// correctionAdmission is the release admission of a correction attempt: the checks a release meets before it is decided, without the readiness reading, which holds a node that is held by its own accepted
// execution (what that reading checks about the plan is judged by the route, agentCorrectionRoute). A host short of memory defers it (host: judged once, before the transaction, as a reading judges it once), a
// merge turn that moves a branch an incoming edge targets defers it (resourceHold: the project's one registered parent too), and so does an overlap of its edit regions with the other nodes that hold theirs under
// the release policy of the plan (judgeRelease). The refusals are those of a release (refusalOfReading).
func (s *Scheduler) correctionAdmission(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, n dag.SnapNode, host bool) error {
	if host && s.Host != nil && !storeOnly(ctx) {
		if verdict := s.Host.Judge(); verdict.State == HostMemoryDeferring {
			return refusalOfReading(NodeReading{NodeID: n.NodeID, Reason: DeferHostMemory, Detail: verdict.Detail()})
		}
	}
	reason, detail, err := s.resourceHold(ctx, q, plan, snap, n, incomingEdges(snap, n.NodeID), false)
	if err != nil {
		return err
	}
	if reason != "" {
		return refusalOfReading(NodeReading{NodeID: n.NodeID, Reason: reason, Detail: detail})
	}
	if n.Kind != dag.NodeImplementation {
		return nil
	}
	declarations, err := loadDeclarations(ctx, q, plan)
	if err != nil {
		return err
	}
	var holders []holder
	for _, other := range snap.Nodes {
		if other.NodeID == n.NodeID || other.Kind != dag.NodeImplementation || other.Lifecycle != "" {
			continue
		}
		state, err := s.stateOf(ctx, q, plan, snap, other)
		if err != nil {
			return err
		}
		if state.Holds {
			regions, declared := declarations[other.NodeID]
			holders = append(holders, holder{NodeID: other.NodeID, Regions: regions, Unknown: !declared})
		}
	}
	if len(holders) == 0 {
		return nil
	}
	regions, declared := declarations[n.NodeID]
	judged, err := judgeRelease(holder{NodeID: n.NodeID, Regions: regions, Unknown: !declared}, holders, &observations{ctx: ctx, q: q, plan: plan})
	if err != nil {
		return err
	}
	policy, err := s.releasePolicy(ctx, q, plan, snap)
	if err != nil {
		return err
	}
	judged.applyPolicy(policy)
	if judged.Rule == RuleDefer {
		return refusalOfReading(NodeReading{NodeID: n.NodeID, Reason: DeferEditOverlap, Detail: judged.deferDetail()})
	}
	return nil
}
