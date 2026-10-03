package dagsched

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The start of a command that acts on one node of a plan: read the plan as it stands and find the live node in it, and refuse unregistered_scope when the plan does not hold it. The commands
// that read the plan again inside their transaction say "no longer has" (a plan that moved), which is why the two wordings are two functions; lifecycleOpen and releaseGate in lifecycle.go
// name what they refuse after the node and keep their own words.

// liveNode is the plan's current snapshot and the live node of it.
func liveNode(ctx context.Context, q store.Querier, plan, node string) (dag.Snapshot, dag.SnapNode, error) {
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return dag.Snapshot{}, dag.SnapNode{}, err
	}
	n, err := requireNode(snap, plan, node)
	return snap, n, err
}

// requireNode is the live node of a snapshot already read: for the commands that look at several nodes of one reading.
func requireNode(snap dag.Snapshot, plan, node string) (dag.SnapNode, error) {
	n, ok := nodeOf(snap, node)
	if !ok {
		return dag.SnapNode{}, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	return n, nil
}

// stillLive is liveNode for the transaction that repeats a judgement made on an earlier reading.
func stillLive(ctx context.Context, q store.Querier, plan, node string) (dag.Snapshot, dag.SnapNode, error) {
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return dag.Snapshot{}, dag.SnapNode{}, err
	}
	n, err := requireStillNode(snap, plan, node)
	return snap, n, err
}

// requireStillNode is requireNode for a plan that moved since the node was read.
func requireStillNode(snap dag.Snapshot, plan, node string) (dag.SnapNode, error) {
	n, ok := nodeOf(snap, node)
	if !ok {
		return dag.SnapNode{}, refuse(contract.RefusalUnregisteredScope, "plan %s no longer has the live node %s", plan, node)
	}
	return n, nil
}
