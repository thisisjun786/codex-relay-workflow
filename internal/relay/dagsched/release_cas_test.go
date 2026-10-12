package dagsched

import (
	"context"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// TestReleaseComparesThePlanSliceAtTheRelayCommit (CRW-1288): a release that names the slice of the plan its caller compared
// (expected_slice_digest) releases nothing when the node's slice is another - a plan revision made before the relay judged the
// release, or one made right before the transaction that reserves the slot and records the release - and answers
// plan_revision_conflict with no row written, on both surfaces. A release that names the slice the plan has then is released.
func TestReleaseComparesThePlanSliceAtTheRelayCommit(t *testing.T) {
	t.Parallel()
	for _, surface := range []string{"agent", "managed"} {
		for _, moment := range []string{"before the judgement", "before the commit"} {
			t.Run(surface+", the plan changes "+moment, func(t *testing.T) {
				t.Parallel()
				k := newReleaseKit(t)
				releasePlan(k.fixture, "rp")
				compared := k.sliceOf("rp", "A")
				var before map[string][]map[string]any
				change := func() {
					n := relNode("A", dag.NodeNonPR)
					n["title"] = "A, retitled by a plan revision"
					k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-retitle", doc{"op": dag.OpUpdateNode, "node": n})
					before = allRows(t, k.s.DB)
				}
				if moment == "before the judgement" {
					change()
				} else {
					k.sched.testBetweenReadAndIntent = func() {
						k.sched.testBetweenReadAndIntent = nil
						change()
					}
				}
				if err := k.releaseExpecting(surface, "A", compared); refusalReason(err) != "plan_revision_conflict" {
					t.Fatalf("the release under slice %s after the plan changed: %v, want plan_revision_conflict", compared, err)
				}
				if before == nil {
					t.Fatal("the plan did not change")
				}
				if after := allRows(t, k.s.DB); !reflect.DeepEqual(before, after) {
					t.Fatal("the refused release wrote rows")
				}
				now := k.sliceOf("rp", "A")
				if now == compared {
					t.Fatal("the plan revision did not change the node's slice")
				}
				if err := k.releaseExpecting(surface, "A", now); err != nil {
					t.Fatalf("the release under the slice the plan has: %v", err)
				}
				if got := k.count("SELECT COUNT(*) FROM dag_input_manifests WHERE node_id = 'A'"); got != 1 {
					t.Fatalf("%d manifests of A", got)
				}
			})
		}
	}
}

// sliceOf is the node's slice of the plan's head.
func (k *releaseKit) sliceOf(plan, node string) string {
	k.t.Helper()
	n, ok := nodeOf(k.snapshot(plan), node)
	if !ok {
		k.t.Fatalf("plan %s has no node %s", plan, node)
	}
	return n.SliceDigest
}

// releaseExpecting releases a non_pr node on a surface with the slice its caller compared.
func (k *releaseKit) releaseExpecting(surface, node, slice string) error {
	k.t.Helper()
	var err error
	if surface == "agent" {
		req := k.agentRequest(false)
		req.ExpectedSliceDigest = slice
		_, err = k.sched.ReleaseAgent(context.Background(), "rp", node, "parent", req, AgentReleaseOptions{RunID: "run-1", Worktree: "/wt/" + node})
	} else {
		req := k.request(false)
		req.ExpectedSliceDigest = slice
		_, err = k.sched.Release(context.Background(), "rp", node, "parent", req)
	}
	return err
}
