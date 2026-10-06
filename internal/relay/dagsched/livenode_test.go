package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The four ways a command finds the node it acts on: the same reason (unregistered_scope), and the words of the command that asks, which are the relay's output and so do not move.
// The activation is a node id the plan does not hold; the node it does hold comes back as the plan's own.
func TestAnAbsentNodeIsRefusedInTheWordsOfTheCommandThatAsked(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	forkJoinPlan(f, "p1")
	ctx := context.Background()
	q := f.s.Q(ctx)
	snap := f.snapshot("p1")
	const absent, moved = "unregistered_scope: plan p1 has no live node ghost", "unregistered_scope: plan p1 no longer has the live node ghost"
	rows := []struct {
		name string
		find func(node string) (dag.SnapNode, error)
		want string
	}{
		{"liveNode", func(node string) (dag.SnapNode, error) { _, n, err := liveNode(ctx, q, "p1", node); return n, err }, absent},
		{"requireNode", func(node string) (dag.SnapNode, error) { return requireNode(snap, "p1", node) }, absent},
		{"stillLive", func(node string) (dag.SnapNode, error) { _, n, err := stillLive(ctx, q, "p1", node); return n, err }, moved},
		{"requireStillNode", func(node string) (dag.SnapNode, error) { return requireStillNode(snap, "p1", node) }, moved},
	}
	for _, row := range rows {
		if n, err := row.find("design"); err != nil || n.NodeID != "design" {
			t.Errorf("%s of a live node = (%q, %v), want the node", row.name, n.NodeID, err)
		}
		if _, err := row.find("ghost"); err == nil || err.Error() != row.want {
			t.Errorf("%s of an absent node: %v, want %q", row.name, err, row.want)
		}
	}
}
