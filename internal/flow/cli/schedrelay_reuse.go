package cli

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// Integrated is the relay's read of a node for crw.reuseIntegrated (CRW-1597): whether the node's current agent acceptance was
// observed integrated on the plan's target and the target still holds the accepted head. It only reads - the acceptance chain,
// the real integration observations, the scheduler's snapshot and git; it claims nothing and writes nothing. A node with no
// integrated acceptance answers not-integrated plainly: reuse never preempts the release path's own judgment (a pending node's
// plan supersession is the release's to decide). Only for an actually integrated candidate is the plan's current node checked
// against the one the run read (plan_superseded when the latest snapshot's slice or criteria are empty or other), the
// acceptance's repository and identity proved, and the accepted head's ancestry on the target verified. A malformed or an
// orphaned acceptance - or a target that lost the head - is refused integration_reuse_unproven: that path never rebuilds.
var _ ops.IntegratedReader = (*SchedRelay)(nil)

func (r *SchedRelay) Integrated(ctx context.Context, plan string, node ops.PlanNode) (ops.IntegratedState, error) {
	checkout, ref, err := r.target(plan)
	if err != nil {
		return ops.IntegratedState{}, err
	}
	q := r.q(ctx)
	cur, found, err := dagsched.CurrentAcceptance(ctx, q, plan, node.ID)
	if err != nil {
		return ops.IntegratedState{}, err
	}
	if !found || cur.Kind != dagsched.AcceptanceAgent {
		return ops.IntegratedState{}, nil
	}
	if cur.Agent.CriteriaSetDigest != node.CriteriaDigest {
		// the current acceptance is of another criteria set than the node the run read: not reusable, and no fault
		return ops.IntegratedState{}, nil
	}
	landed, err := r.Sched.AgentAcceptanceIntegrated(ctx, q, cur.Agent)
	if err != nil {
		return ops.IntegratedState{}, err
	}
	if !landed {
		return ops.IntegratedState{}, nil
	}
	// only an actually integrated candidate is judged against the plan's latest snapshot: the node's slice and criteria as the
	// relay reads them now must be the ones the run's milestone row carries
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return ops.IntegratedState{}, err
	}
	var live *dag.SnapNode
	for i := range snap.Nodes {
		if snap.Nodes[i].NodeID == node.ID {
			live = &snap.Nodes[i]
		}
	}
	if live == nil || live.SliceDigest == "" || live.CriteriaSetDigest == "" || live.SliceDigest != node.SliceDigest || live.CriteriaSetDigest != node.CriteriaDigest {
		return ops.IntegratedState{}, &ops.Refusal{Reason: ops.ReasonPlanSuperseded, Detail: fmt.Sprintf("node %s is not the node the plan carries now: the run's slice %q and criteria %q do not match the plan's", node.ID, node.SliceDigest, node.CriteriaDigest)}
	}
	unproven := func(detail string) (ops.IntegratedState, error) {
		return ops.IntegratedState{}, &ops.Refusal{Reason: ops.ReasonIntegrationUnproven, Detail: fmt.Sprintf("node %s of plan %s: %s", node.ID, plan, detail)}
	}
	if cur.Agent.Repository == "" || !sameDirectory(cur.Agent.Repository, checkout) {
		return unproven(fmt.Sprintf("the current acceptance %s was made in repository %q, not the target checkout %s", cur.AcceptanceID, cur.Agent.Repository, checkout))
	}
	if cur.Agent.Head == "" || cur.AcceptanceID != dagsched.AgentAcceptanceID(cur.Agent.PlanID, cur.Agent.NodeID, cur.Agent.ExecutionID, cur.Agent.Head, cur.Agent.CriteriaSetDigest) {
		return unproven(fmt.Sprintf("the current acceptance %s does not name the accepted head of its chain", cur.AcceptanceID))
	}
	tip, err := r.IntegrationTip(ctx, plan)
	if err != nil {
		return ops.IntegratedState{}, err
	}
	anc, err := isAncestor(ctx, checkout, cur.Agent.Head, tip)
	if err != nil {
		return ops.IntegratedState{}, err
	}
	if !anc {
		return unproven(fmt.Sprintf("the integration target lost the accepted head %s (tip %s)", cur.Agent.Head, tip))
	}
	return ops.IntegratedState{Integrated: true, AcceptanceID: cur.AcceptanceID, Head: cur.Agent.Head, IntegrationRef: ref, Tip: tip}, nil
}
