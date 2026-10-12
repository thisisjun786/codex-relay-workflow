package dagsched

import (
	"context"
	"errors"
	"fmt"
)

// RunNode names a node of a plan that a flow run released to an executor.
type RunNode struct{ Plan, Node string }

// CancelRunNodes gives up the agent executions that a cancelled flow run still owns: for each node whose current execution is an
// agent execution it calls AgentClose with the flow_cancel reason, which writes the closure and returns the slot in one transaction,
// so a cancel of a run that no process holds leaves no execution open and no slot reserved. The actor and the epoch are those the
// execution was released under. A node with no current agent execution (closed, never released, or on the relationship side) is
// skipped; the same call again is a replay. It goes on after a node that fails and returns every failure joined, with the results
// of the nodes it closed.
func (s *Scheduler) CancelRunNodes(ctx context.Context, nodes []RunNode) ([]AgentCloseResult, error) {
	var results []AgentCloseResult
	var failures []error
	for _, n := range nodes {
		cur, found, err := CurrentExecution(ctx, s.Store.Q(ctx), n.Plan, n.Node)
		if err != nil {
			failures = append(failures, fmt.Errorf("node %s of plan %s: %w", n.Node, n.Plan, err))
			continue
		}
		if !found || cur.Kind != ExecutionAgent {
			continue
		}
		scoped := *s
		scoped.ExpectedEpoch = cur.Agent.Epoch
		res, err := scoped.AgentClose(ctx, n.Plan, n.Node, cur.Agent.Actor, AgentCloseInput{Reason: AgentReasonFlowCancel})
		if err != nil {
			failures = append(failures, fmt.Errorf("node %s of plan %s: %w", n.Node, n.Plan, err))
			continue
		}
		results = append(results, res)
	}
	return results, errors.Join(failures...)
}
