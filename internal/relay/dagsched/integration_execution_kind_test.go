package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1036 (pre-merge evaluation d1 of CRW-1031, CRW-1033 and CRW-1036): the merged mark is the integration of an accepted result that has no head, and of nothing else. A revision that turns an
// implementation node into a non_pr one does not make its accepted code head headless: the head still has to be observed on its targets before the child of that node is cleaned up.
func TestExecutionIntegratedJudgesTheAcceptedHeadNotTheKindTheNodeBecame(t *testing.T) {
	t.Parallel()
	k := newLegacyLocalIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature-d")
	head := repo.commit("d.txt", "d")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "D", "d.txt")
	a := k.acceptNode("g", "D", acceptOpts{HeadSHA: head, PR: 6, Forge: "owner/repo", Repository: repo.path})
	k.holdSlotsFor("g", "D")
	k.mark(a)
	judge := func(want bool, when string) {
		t.Helper()
		applicable, integrated, err := k.sched.ExecutionIntegrated(context.Background(), a.Acceptance.RelationshipID, a.Event, a.Acceptance.ExecutionGeneration, a.Acceptance.RevisionHash)
		if err != nil || !applicable || integrated != want {
			t.Fatalf("%s: applicable=%v integrated=%v err=%v, want integrated=%v", when, applicable, integrated, err, want)
		}
	}
	judge(false, "an accepted code head with the merged mark and no observation")
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", doc{"op": dag.OpUpdateNode, "node": relNode("D", dag.NodeNonPR)})
	if n, _ := nodeOf(k.snapshot("g"), "D"); n.Kind != dag.NodeNonPR {
		t.Fatalf("the revision did not change the kind of D: %+v", n)
	}
	judge(false, "the same acceptance after the plan turned D into a non_pr node")
}

// A non_pr node that left the plan after its decision document was accepted and its merged mark recorded: the acceptance has no head and the plan no longer names the node, and the child's
// cleanup still goes ahead on the mark (pre-merge evaluation d3 of CRW-1036). A retired node whose acceptance holds a head is not integrated by the mark.
func TestExecutionIntegratedOfANonPRNodeThatLeftThePlan(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	b := accepted["B"]
	var event, revision string
	var generation int64
	if err := k.s.DB.QueryRow("SELECT event_id, revision_hash, execution_generation FROM dag_acceptances WHERE acceptance_id = ?", b.AcceptanceID).Scan(&event, &revision, &generation); err != nil {
		t.Fatal(err)
	}
	k.exec("INSERT OR IGNORE INTO assignment_marks (relationship_id, mark, event_id, execution_generation, revision_hash, evidence, actor, marked_at) VALUES (?, 'merged', ?, ?, ?, 'merged', 'parent', ?)",
		b.RelationshipID, event, generation, revision, k.clock())
	judge := func(want bool, when string) {
		t.Helper()
		applicable, integrated, err := k.sched.ExecutionIntegrated(context.Background(), b.RelationshipID, event, generation, revision)
		if err != nil || !applicable || integrated != want {
			t.Fatalf("%s: applicable=%v integrated=%v err=%v, want integrated=%v", when, applicable, integrated, err, want)
		}
	}
	judge(true, "B is in the plan")
	k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", doc{"op": dag.OpRetireEdge, "edge_id": "rb"}, doc{"op": dag.OpRetireNode, "node_id": "B"})
	if _, ok := nodeOf(k.snapshot("sr"), "B"); ok {
		t.Fatal("B is still in the plan")
	}
	judge(true, "B left the plan")
}
