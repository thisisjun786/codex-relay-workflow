package dagsched

import (
	"context"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// twoTargetPlan: impl-a has an integrated edge to each of two branches of one repository; ship is a terminal node.
func twoTargetPlan(f *fixture, plan string) {
	f.t.Helper()
	f.putPlan(plan, 0, plan+"-r1",
		addNode("impl-a", dag.NodeImplementation), addNode("join1", dag.NodeNonPR), addNode("join2", dag.NodeNonPR), addNode("ship", dag.NodeImplementation),
		addEdge("x1", "impl-a", "join1", dag.EdgeIntegrated, nil),
		addEdge("x2", "impl-a", "join2", dag.EdgeIntegrated, doc{"target_base_ref": "release"}))
}

func (f *fixture) integratedNode(plan string, a accepted) (bool, []Target) {
	f.t.Helper()
	ctx := context.Background()
	ok, targets, err := f.sched.nodeIntegrated(ctx, f.s.Q(ctx), plan, f.snapshot(plan), a.Acceptance)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok, targets
}

var pinnedOpts = acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: "owner/repo"}

func TestNodeIntegratedNeedsEveryTarget(t *testing.T) {
	dev, release := Target{"owner/repo", "dev"}, Target{"owner/repo", "release"}
	t.Run("partial, then complete", func(t *testing.T) {
		f := newFixture(t)
		twoTargetPlan(f, "p1")
		a := f.acceptNode("p1", "impl-a", pinnedOpts)
		if ok, targets := f.integratedNode("p1", a); ok || !reflect.DeepEqual(targets, []Target{dev, release}) {
			t.Fatalf("before anything landed: integrated=%v targets=%v", ok, targets)
		}
		f.integrate(a, "owner/repo", "dev", true, true)
		if ok, _ := f.integratedNode("p1", a); ok {
			t.Fatal("one of two targets landed: the node must not read integrated")
		}
		f.integrate(a, "owner/repo", "release", true, true)
		if ok, _ := f.integratedNode("p1", a); !ok {
			t.Fatal("both targets landed: the node must read integrated")
		}
	})
	t.Run("duplicate targets of an integrated edge and a pinned edge are one", func(t *testing.T) {
		f := newFixture(t)
		forkJoinPlan(f, "p1") // impl-a: e3 integrated and e8 pinned, both owner/repo@dev
		a := f.acceptNode("p1", "impl-a", pinnedOpts)
		if _, targets := f.integratedNode("p1", a); !reflect.DeepEqual(targets, []Target{dev}) {
			t.Fatalf("targets = %v, want the one shared target", targets)
		}
		f.integrate(a, "owner/repo", "dev", true, true)
		if ok, _ := f.integratedNode("p1", a); !ok {
			t.Fatal("the shared target landed: integrated")
		}
	})
	t.Run("a stacked successor's pinned edge makes its target required too", func(t *testing.T) {
		f := newFixture(t)
		pin := doc{"pins_code_head": true, "target_repository": "owner/repo", "target_base_ref": "dev"}
		f.putPlan("p1", 0, "p1-r1", addNode("impl-a", dag.NodeImplementation), addNode("stack", dag.NodeImplementation), addNode("join", dag.NodeNonPR),
			addEdge("s1", "impl-a", "stack", dag.EdgeArtifactVerified, pin), addEdge("s2", "impl-a", "join", dag.EdgeIntegrated, doc{"target_base_ref": "release"}))
		a := f.acceptNode("p1", "impl-a", pinnedOpts)
		f.integrate(a, "owner/repo", "release", true, true)
		ok, targets := f.integratedNode("p1", a)
		if ok || !reflect.DeepEqual(targets, []Target{dev, release}) {
			t.Fatalf("the integrated target landed but the pinned one did not: integrated=%v targets=%v", ok, targets)
		}
		f.integrate(a, "owner/repo", "dev", true, true)
		if ok, _ := f.integratedNode("p1", a); !ok {
			t.Fatal("both landed: integrated")
		}
	})
	t.Run("a target once observed stays required after its edge is retired", func(t *testing.T) {
		f := newFixture(t)
		twoTargetPlan(f, "p1")
		a := f.acceptNode("p1", "impl-a", pinnedOpts)
		f.integrate(a, "owner/repo", "release", false, false)
		f.putPlan("p1", 1, "p1-r2", doc{"op": dag.OpRetireEdge, "edge_id": "x2"})
		f.integrate(a, "owner/repo", "dev", true, true)
		ok, targets := f.integratedNode("p1", a)
		if ok || !reflect.DeepEqual(targets, []Target{dev, release}) {
			t.Fatalf("a negatively observed target must stay required: integrated=%v targets=%v", ok, targets)
		}
	})
	t.Run("a retired edge's target that was never observed is no longer required", func(t *testing.T) {
		f := newFixture(t)
		twoTargetPlan(f, "p1")
		a := f.acceptNode("p1", "impl-a", pinnedOpts)
		f.putPlan("p1", 1, "p1-r2", doc{"op": dag.OpRetireEdge, "edge_id": "x2"})
		f.integrate(a, "owner/repo", "dev", true, true)
		ok, targets := f.integratedNode("p1", a)
		if !ok || !reflect.DeepEqual(targets, []Target{dev}) {
			t.Fatalf("integrated=%v targets=%v, want integrated at the one remaining target", ok, targets)
		}
	})
	t.Run("a terminal node's targets are the ones it was observed at", func(t *testing.T) {
		f := newFixture(t)
		twoTargetPlan(f, "p1")
		a := f.acceptNode("p1", "ship", pinnedOpts)
		if ok, targets := f.integratedNode("p1", a); ok || len(targets) != 0 {
			t.Fatalf("no edge and no observation: integrated=%v targets=%v, want none", ok, targets)
		}
		f.integrate(a, "owner/repo", "main", true, true)
		ok, targets := f.integratedNode("p1", a)
		if !ok || !reflect.DeepEqual(targets, []Target{{"owner/repo", "main"}}) {
			t.Fatalf("integrated=%v targets=%v", ok, targets)
		}
	})
	t.Run("a node without a code head is never integrated", func(t *testing.T) {
		f := newFixture(t)
		forkJoinPlan(f, "p1")
		a := f.acceptNode("p1", "design", acceptOpts{})
		if ok, _ := f.integratedNode("p1", a); ok {
			t.Fatal("a non_pr acceptance has no head to land")
		}
	})
	t.Run("another plan's observations do not count", func(t *testing.T) {
		f := newFixture(t)
		twoTargetPlan(f, "p1")
		twoTargetPlan(f, "p2")
		one := f.acceptNode("p1", "impl-a", pinnedOpts)
		two := f.acceptNode("p2", "impl-a", pinnedOpts)
		f.integrate(two, "owner/repo", "dev", true, true)
		f.integrate(two, "owner/repo", "release", true, true)
		f.integrate(two, "elsewhere/repo", "main", true, true)
		ok, targets := f.integratedNode("p1", one)
		if ok || !reflect.DeepEqual(targets, []Target{dev, release}) {
			t.Fatalf("plan p1 saw plan p2's rows: integrated=%v targets=%v", ok, targets)
		}
	})
	t.Run("a superseded acceptance's observations belong to it, not to its successor", func(t *testing.T) {
		f := newFixture(t)
		twoTargetPlan(f, "p1")
		old := f.acceptNode("p1", "impl-a", pinnedOpts)
		f.integrate(old, "elsewhere/repo", "main", true, true)
		f.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", old.Acceptance.AcceptanceID)
		next := old.Acceptance
		next.EventID, next.RevisionHash, next.State = "evt-next", dig("next revision"), "active"
		next.AcceptanceID = AcceptanceDigest(next)
		f.insertAcceptance(next)
		ok, targets := f.integratedNode("p1", accepted{Acceptance: next})
		if ok || !reflect.DeepEqual(targets, []Target{dev, release}) {
			t.Fatalf("the successor inherited the old acceptance's targets: integrated=%v targets=%v", ok, targets)
		}
	})
	t.Run("a negative observation at an unlisted target keeps the node unintegrated", func(t *testing.T) {
		f := newFixture(t)
		forkJoinPlan(f, "p1")
		a := f.acceptNode("p1", "impl-a", pinnedOpts)
		f.integrate(a, "owner/repo", "dev", true, true)
		f.integrate(a, "owner/repo", "hotfix", false, true)
		ok, targets := f.integratedNode("p1", a)
		if ok || !reflect.DeepEqual(targets, []Target{dev, {"owner/repo", "hotfix"}}) {
			t.Fatalf("integrated=%v targets=%v", ok, targets)
		}
	})
}
