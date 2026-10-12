package dagsched

import (
	"context"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// RunNode names a node of a plan that a flow run released to an executor, and Execution the execution that release opened ("" when the
// run's rows do not name it).
type RunNode struct{ Plan, Node, Execution string }

// CancelRunNodes gives up the agent executions that the cancelled flow run runID still owns: for each node whose current execution is an
// agent execution of that run it calls AgentClose with the flow_cancel reason, which writes the closure and returns the slot in one
// transaction, so a cancel of a run that no process holds leaves no execution open and no slot reserved. The actor and the epoch are
// those the execution was released under.
//
// An execution is the run's when its run id is runID and it is the execution the run released (RunNode.Execution) or follows it in the
// node's chain (a restart or a correction of the same run). A node the run released and that another run released again after the run's
// cancel (its current execution has another run id, or does not follow the released one) is left alone, so a second cancel of an
// already cancelled run never takes the execution or the slot of a newer run. A node with no current agent execution (closed, never
// released, or on the relationship side) is skipped; the same call again is a replay. It goes on after a node that fails and returns
// every failure joined, with the results of the nodes it closed.
func (s *Scheduler) CancelRunNodes(ctx context.Context, runID string, nodes []RunNode) ([]AgentCloseResult, error) {
	if runID == "" {
		return nil, errors.New("a cancel of a run's nodes names the run")
	}
	var results []AgentCloseResult
	var failures []error
	for _, n := range nodes {
		q := s.Store.Q(ctx)
		cur, found, err := CurrentExecution(ctx, q, n.Plan, n.Node)
		if err != nil {
			failures = append(failures, fmt.Errorf("node %s of plan %s: %w", n.Node, n.Plan, err))
			continue
		}
		if !found || cur.Kind != ExecutionAgent {
			continue
		}
		owned, err := ownsExecution(ctx, q, runID, n.Execution, cur.Agent)
		if err != nil {
			failures = append(failures, fmt.Errorf("node %s of plan %s: %w", n.Node, n.Plan, err))
			continue
		}
		if !owned {
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

// ownsExecution is whether cur, the current execution of a node, belongs to the run: it carries the run's id, and it is the released
// execution or has it in its chain of superseded executions. A released execution that is not named (released "") is matched by the run id alone.
func ownsExecution(ctx context.Context, q store.Querier, runID, released string, cur AgentExecution) (bool, error) {
	if cur.RunID != runID {
		return false, nil
	}
	if released == "" {
		return true, nil
	}
	row := cur
	for hops := 0; row.ExecutionID != released; hops++ {
		if row.Supersedes == "" || hops > maxExecutionChain {
			return false, nil
		}
		var err error
		if row, err = scanAgentExecution(q.QueryRowContext(ctx, "SELECT "+agentExecutionColumns+" FROM dag_agent_executions WHERE execution_id = ?", row.Supersedes)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// maxExecutionChain bounds the walk of a chain of superseded executions.
const maxExecutionChain = 10000
