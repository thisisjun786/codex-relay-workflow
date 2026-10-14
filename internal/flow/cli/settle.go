package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Settlement is what giving up a cancelled run's executions did.
type Settlement struct {
	// Closed are the executions closed, SlotsReturned the slots that went back with them.
	Closed, SlotsReturned int
}

// SettleFunc gives up the executions a cancelled run released and still owns: runID is the run, nodes the nodes its rows show released
// and not ended, each with the execution of its last release.
type SettleFunc func(ctx context.Context, st *store.Store, runID string, nodes []ops.ReleaseRef) (Settlement, error)

// SchedulerSettle settles through the relay's scheduler: AgentClose with the flow_cancel reason for each node (R07), which closes the
// execution and returns its slot in one transaction. A cancel that only wrote the run's finished row would hide the run from
// monitoring while its nodes kept their slots and their regions.
//
// The nodes are those the run's rows show and the nodes whose agent executions the relay records under the run (CRW-1397): a process
// killed between the relay's release and its release row leaves an execution of the run its journal does not name, which the cancel gives
// up too (CancelRunNodes closes only a node whose current execution is the run's).
func SchedulerSettle(ctx context.Context, st *store.Store, runID string, nodes []ops.ReleaseRef) (Settlement, error) {
	var out Settlement
	list := make([]dagsched.RunNode, 0, len(nodes))
	seen := map[[2]string]bool{}
	for _, n := range nodes {
		if n.Plan == "" || n.Node == "" {
			return out, errors.New("a release row of the run names no plan or node")
		}
		list = append(list, dagsched.RunNode{Plan: n.Plan, Node: n.Node, Execution: n.Execution})
		seen[[2]string{n.Plan, n.Node}] = true
	}
	recorded, err := relayRunNodes(ctx, st, runID)
	if err != nil {
		return out, err
	}
	for _, n := range recorded {
		if !seen[[2]string{n.Plan, n.Node}] {
			seen[[2]string{n.Plan, n.Node}] = true
			list = append(list, n)
		}
	}
	if len(list) == 0 {
		return out, nil
	}
	results, err := (&dagsched.Scheduler{Store: st}).CancelRunNodes(ctx, runID, list)
	for _, r := range results {
		if r.Closed && !r.Replayed {
			out.Closed++
		}
		if r.SlotReleased && !r.Replayed {
			out.SlotsReturned++
		}
	}
	if err != nil {
		return out, fmt.Errorf("the executions of the run were not all given up: %w", err)
	}
	return out, nil
}

// relayRunNodes are the nodes whose agent executions the relay records under the run, in the order the relay wrote them; a store with no
// agent executions has none.
func relayRunNodes(ctx context.Context, st *store.Store, runID string) ([]dagsched.RunNode, error) {
	var exists int
	if err := st.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'dag_agent_executions'").Scan(&exists); err != nil || exists == 0 {
		return nil, err
	}
	rows, err := st.DB.QueryContext(ctx, "SELECT plan_id, node_id FROM dag_agent_executions WHERE run_id = ? GROUP BY plan_id, node_id ORDER BY MIN(rowid)", runID)
	if err != nil {
		return nil, fmt.Errorf("the executions of run %s cannot be read: %w", runID, err)
	}
	defer rows.Close()
	var out []dagsched.RunNode
	for rows.Next() {
		var n dagsched.RunNode
		if err := rows.Scan(&n.Plan, &n.Node); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
