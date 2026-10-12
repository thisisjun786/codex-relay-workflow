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

// SettleFunc gives up the executions a cancelled run released and does not own any longer.
type SettleFunc func(ctx context.Context, st *store.Store, nodes []ops.ReleaseRef) (Settlement, error)

// SchedulerSettle settles through the relay's scheduler: AgentClose with the flow_cancel reason for each node (R07), which closes the
// execution and returns its slot in one transaction. A cancel that only wrote the run's finished row would hide the run from
// monitoring while its nodes kept their slots and their regions.
func SchedulerSettle(ctx context.Context, st *store.Store, nodes []ops.ReleaseRef) (Settlement, error) {
	var out Settlement
	if len(nodes) == 0 {
		return out, nil
	}
	list := make([]dagsched.RunNode, 0, len(nodes))
	for _, n := range nodes {
		if n.Plan == "" || n.Node == "" {
			return out, errors.New("a release row of the run names no plan or node")
		}
		list = append(list, dagsched.RunNode{Plan: n.Plan, Node: n.Node})
	}
	results, err := (&dagsched.Scheduler{Store: st}).CancelRunNodes(ctx, list)
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
