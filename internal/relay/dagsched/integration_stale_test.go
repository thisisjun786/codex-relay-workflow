package dagsched

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1026 (d2): dag-integrate-push lists the integrated nodes of the pushed head whose acceptance the plan no longer stands
// behind. These tests run real batches with an in-process verifier (no subprocess) and read the list through its own
// predicate; the list only informs.

// inProcessVerifier is the verify command of a batch without a subprocess: it fails when bad.txt is in the worktree and
// otherwise seals the record of the worktree's HEAD for the batch's base, naming the pins the commit declares.
func inProcessVerifier(t *testing.T) IntegrationBatchVerifier {
	return func(ctx context.Context, dir string, env []string) error {
		get := func(name string) string {
			for _, kv := range env {
				if v, ok := strings.CutPrefix(kv, name+"="); ok {
					return v
				}
			}
			return ""
		}
		if _, err := os.Stat(filepath.Join(dir, "bad.txt")); err == nil {
			return fmt.Errorf("bad.txt is present: the merged tree fails")
		}
		head, err := runGit(ctx, dir, nil, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		head = strings.TrimSpace(head)
		keys, err := CommitVerificationKeys(ctx, dir, head)
		if err != nil {
			return err
		}
		raw, err := SealVerificationRecord(VerificationRecord{Runner: "local", Repository: "owner/repo", BaseCommit: get("CRW_VERIFY_BASE"), HeadCommit: head,
			TreeHash: keys.Tree, CiDigest: keys.CiDigest, Tools: copyPins(keys.Pins), Pins: copyPins(keys.Pins), PinMismatch: []string{},
			Dependencies: keys.Dependencies, OS: runtime.GOOS, Arch: runtime.GOARCH, Result: "pass"})
		if err != nil {
			return err
		}
		return os.WriteFile(get("CRW_VERIFY_RECORD"), raw, 0o600)
	}
}

// integrateNodes accepts the nodes and integrates them in one batch of the ref; it answers the branch head.
func (k *batchKit) integrateNodes(t *testing.T, ref string, nodes ...string) string {
	t.Helper()
	for _, n := range nodes {
		k.acceptByCommit(n)
	}
	in := k.batchIn()
	in.IntegrationRef, in.Nodes = ref, nodes
	res, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: inProcessVerifier(t), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch of %v on %s: %v", nodes, ref, err)
	}
	if len(res.Merged) != len(nodes) {
		t.Fatalf("batch of %v on %s merged %+v, split %+v", nodes, ref, res.Merged, res.Split)
	}
	return res.NewHead
}

// staleNodes reads the stale nodes of a ref at a head.
func (k *batchKit) staleNodes(t *testing.T, ref, head string) []StaleNode {
	t.Helper()
	got, err := k.sched.StaleIntegratedNodes(context.Background(), k.repo.path, ref, head)
	if err != nil {
		t.Fatalf("stale nodes of %s at %s: %v", ref, head, err)
	}
	return got
}

// staleSummary is the nodes as "node:reason".
func staleSummary(nodes []StaleNode) []string {
	out := []string{}
	for _, n := range nodes {
		out = append(out, n.NodeID+":"+n.Reason)
	}
	return out
}

func (k *batchKit) changeCriteria(node string) string {
	digest := dig("changed " + node)
	k.invRevise("g", node, "g-change-"+node, func(n doc) { n["criteria_set_digest"] = digest })
	return digest
}

// The plan's criteria for an integrated node change after the integration: the node is listed with the digests, and a node
// whose criteria did not change is not.
func TestStaleNodesListAnIntegratedNodeWhoseCriteriaChangedAfterTheIntegration(t *testing.T) {
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	head := k.integrateNodes(t, "dev-int", "a", "b")
	if got := k.staleNodes(t, "dev-int", head); len(got) != 0 {
		t.Fatalf("nothing changed after the integration, got %v", staleSummary(got))
	}
	changed := k.changeCriteria("a")
	got := k.staleNodes(t, "dev-int", head)
	if len(got) != 1 || got[0].NodeID != "a" || got[0].Reason != StaleNodeCriteriaChanged || got[0].PlanID != "g" || got[0].AcceptanceID == "" ||
		got[0].CurrentCriteria != changed || got[0].AcceptedCriteria == "" || got[0].AcceptedCriteria == changed {
		t.Fatalf("a must be the one stale node, by its criteria: %+v", got)
	}
}

// A successful revalidation of the same output under the plan's criteria clears the node.
func TestStaleNodesClearAfterARevalidationOfTheSameOutput(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	head := k.integrateNodes(t, "dev-int", "a")
	changed := k.changeCriteria("a")
	if got := k.staleNodes(t, "dev-int", head); len(got) != 1 {
		t.Fatalf("the criteria changed: want a listed, got %v", staleSummary(got))
	}
	k.revalidateCriteria(t, "a", changed)
	if got := k.staleNodes(t, "dev-int", head); len(got) != 0 {
		t.Fatalf("a revalidation of the same output under the new criteria clears the list, got %v", staleSummary(got))
	}
	// the plan moves again: the revalidation stood on the previous criteria
	k.invRevise("g", "a", "g-change-again", func(n doc) { n["criteria_set_digest"] = dig("changed again") })
	if got := k.staleNodes(t, "dev-int", head); len(got) != 1 || got[0].AcceptedCriteria != changed {
		t.Fatalf("the node is stale again against the newer criteria, accepted under the revalidated set: %+v", got)
	}
}

// A node integrated by an earlier batch is in the pushed head of a later batch and is listed from there.
func TestStaleNodesReadIntegratedNodesOfEarlierBatches(t *testing.T) {
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	first := k.integrateNodes(t, "dev-int", "a")
	second := k.integrateNodes(t, "dev-int", "b")
	if first == second {
		t.Fatal("the second batch must move the branch")
	}
	k.changeCriteria("a")
	if got := k.staleNodes(t, "dev-int", second); !reflect.DeepEqual(staleSummary(got), []string{"a:criteria_changed"}) {
		t.Fatalf("the stale node of the earlier batch is listed at the later head, got %v", staleSummary(got))
	}
	// the earlier head does not hold b, and b has not changed: the same node, read at the head of its own batch
	if got := k.staleNodes(t, "dev-int", first); !reflect.DeepEqual(staleSummary(got), []string{"a:criteria_changed"}) {
		t.Fatalf("got %v", staleSummary(got))
	}
}

// A candidate the batch split out was never integrated, and a node integrated on another ref (or not contained in the
// pushed head) is not read for this one.
func TestStaleNodesExcludeSplitCandidatesAndOtherRefs(t *testing.T) {
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}},
		batchNode{name: "c", files: map[string]string{"c.txt": "c\n", "bad.txt": "bad\n"}})
	// c fails verification alone and is split out of the batch that integrates a
	k.acceptByCommit("a")
	k.acceptByCommit("c")
	in := k.batchIn()
	in.Nodes = []string{"a", "c"}
	res, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: inProcessVerifier(t), Update: updateIntegrationRef})
	if err != nil || len(res.Merged) != 1 || res.Merged[0].NodeID != "a" || len(res.Split) != 1 || res.Split[0].NodeID != "c" {
		t.Fatalf("a must merge and c must be split out: %+v, %v", res, err)
	}
	devHead := res.NewHead
	otherHead := k.integrateNodes(t, "other-int", "b")
	for _, n := range []string{"a", "b", "c"} {
		k.changeCriteria(n)
	}
	for _, c := range []struct {
		ref, head string
		want      []string
	}{
		{"dev-int", devHead, []string{"a:criteria_changed"}},
		{"other-int", otherHead, []string{"b:criteria_changed"}},
		{"dev-int", otherHead, []string{}}, // a is on the ref but not in that head
		{"other-int", devHead, []string{}}, // b is in no batch of this ref's head
		{"elsewhere", devHead, []string{}}, // no batch of that ref
	} {
		if got := staleSummary(k.staleNodes(t, c.ref, c.head)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s at %.8s: got %v, want %v (c was split out and is never listed)", c.ref, c.head, got, c.want)
		}
	}
}

// An integrated acceptance that is no longer the node's active one is listed under its identity.
func TestStaleNodesListASupersededAcceptance(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	head := k.integrateNodes(t, "dev-int", "a")
	k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE plan_id = 'g' AND node_id = 'a' AND state = 'active'")
	got := k.staleNodes(t, "dev-int", head)
	if len(got) != 1 || got[0].Reason != StaleNodeAcceptanceSuperseded || got[0].AcceptedCriteria != "" || got[0].CurrentCriteria != "" {
		t.Fatalf("a superseded acceptance is listed by identity, got %+v", got)
	}
}

// A node the plan retired after its integration is listed as removed.
func TestStaleNodesListARetiredNode(t *testing.T) {
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}},
		batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	head := k.integrateNodes(t, "dev-int", "a")
	k.putPlan("g", int(k.snapshot("g").Revision), "g-retire-a", doc{"op": dag.OpRetireNode, "node_id": "a"})
	got := k.staleNodes(t, "dev-int", head)
	if len(got) != 1 || got[0].NodeID != "a" || got[0].Reason != StaleNodeRemoved {
		t.Fatalf("a retired node is listed as removed, got %+v", got)
	}
}

// The list only informs: the push goes through with stale nodes, writes nothing to the store, and a list that cannot be read
// is reported next to the push.
func TestPushWithStaleNodesStillPushesAndWritesNothing(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	head := k.integrateNodes(t, "dev-int", "a")
	k.changeCriteria("a")
	tables := []string{"dag_integration_stages", "dag_integration_batches", "dag_acceptances", "dag_acceptance_revalidations", "assignment_marks", "dag_integration_observations"}
	count := func() map[string]int {
		out := map[string]int{}
		for _, table := range tables {
			var n int
			if err := k.s.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[table] = n
		}
		return out
	}
	before := count()
	res, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, k.sched.StaleIntegratedNodes)
	if err != nil || res.Outcome != PushPushed {
		t.Fatalf("a stale node must not block the push: %+v, %v", res, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != head {
		t.Fatalf("the remote holds %s, want %s", got, head)
	}
	if len(res.StaleNodes) != 1 || res.StaleNodes[0].NodeID != "a" || res.StaleNodesError != "" {
		t.Fatalf("the push result carries the stale node: %+v", res)
	}
	if after := count(); !reflect.DeepEqual(before, after) {
		t.Fatalf("the push wrote to the store: before %v, after %v", before, after)
	}
	// up to date: the list is still read; and a list that fails does not change the outcome
	failing := func(context.Context, string, string, string) ([]StaleNode, error) {
		return nil, fmt.Errorf("the store is busy")
	}
	again, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, failing)
	if err != nil || again.Outcome != PushUpToDate || again.StaleNodesError != "the store is busy" || len(again.StaleNodes) != 0 {
		t.Fatalf("a list that cannot be read is reported and does not block: %+v, %v", again, err)
	}
}

// A merge whose mark the criteria change refused (the branch moved, the mark stayed pending) is in the pushed head all the
// same: the push lists it, still pushes, and a split candidate of the same batch stays out of the list.
func TestPushListsAMergedNodeWhoseMarkStayedPending(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.acceptByCommit("a")
	in := k.batchIn()
	deps := IntegrationBatchDeps{Verify: inProcessVerifier(t), Update: updateIntegrationRef, AfterMove: func(context.Context) error {
		k.changeCriteria("a")
		return nil
	}}
	res, err := k.sched.IntegrateBatch(context.Background(), in, deps)
	if err != nil || len(res.Pending) != 1 || len(res.MarkedEvents) != 0 {
		t.Fatalf("want a merged candidate whose mark is pending: %+v, %v", res, err)
	}
	push, err := PushIntegration(context.Background(), k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, k.sched.StaleIntegratedNodes)
	if err != nil || push.Outcome != PushPushed {
		t.Fatalf("the pending mark must not block the push: %+v, %v", push, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != res.NewHead {
		t.Fatalf("the remote holds %s, want %s", got, res.NewHead)
	}
	if len(push.StaleNodes) != 1 || push.StaleNodes[0].NodeID != "a" || push.StaleNodes[0].Reason != StaleNodeCriteriaChanged || push.StaleNodesError != "" {
		t.Fatalf("a pushed merge with a pending mark and changed criteria is listed: %+v", push)
	}
}
