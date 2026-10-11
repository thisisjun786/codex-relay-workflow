package dagsched

import (
	"context"
	"path/filepath"
	"testing"
)

// CRW-1214 (R06), correction round: a node that holds a current execution or acceptance on both sides is read as neither, by every reader that names its acceptance, not only by the state and the edges that
// notice the acceptance conflict. Every fact is made by the production path of the agent; only the forbidden half (a relationship-side execution of the same node) is a seeded row, the way a store written
// another way would hold it (TestExecutionOfFailsClosedAcrossKinds).

// holdRelationshipExecution gives a node a current execution on the relationship side, beside whatever the agent side holds.
func (k *agentKit) holdRelationshipExecution(node string) {
	k.t.Helper()
	k.foreignRelationship("CRW-" + node)
	k.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('g',?,?,1,?,'initial','req-x')",
		node, "rel-foreign-CRW-"+node, dig("a manifest of the relationship side"))
}

// An agent node that holds one valid acceptance and a current relationship-side execution is ambiguous (stateOf): the artifact edge out of it is blocked, dag-progress shows no acceptance for it, and the
// acceptances the measurements and the base refresh read do not include it.
func TestAgentExecutionOnlyCrossKindOpensNothing(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "c", files: fileOf("c")}, {name: "b", files: fileOf("b"), unreleased: true}}
	k := newAgentKit(t, specs...)
	k.pinnedEdge("ab", "a", "b")
	k.acceptAll(specs[0], specs[1])
	if st := k.edgeStatusOf("ab"); !st.Satisfied {
		t.Fatalf("the edge out of the accepted agent node = %+v, want it satisfied before the other side is held", st)
	}
	k.holdRelationshipExecution("a")
	if st := k.edgeStatusOf("ab"); st.Satisfied || st.Reason != BlockedAmbiguousHead {
		t.Fatalf("the edge out of a node with an execution on both sides = %+v, want blocked:ambiguous_head", st)
	}
	p := k.readProgress()
	if a := progressNode(t, p, "a"); a.Stage != StageAmbiguous || a.AcceptanceID != "" {
		t.Fatalf("the node reads stage %s acceptance %q, want ambiguous with none", a.Stage, a.AcceptanceID)
	}
	if c := progressNode(t, p, "c"); c.Stage != StageAccepted || c.AcceptanceID == "" {
		t.Fatalf("c, which holds one kind, reads %+v: the rest of the plan is read", c)
	}
	ctx := context.Background()
	for _, headOnly := range []bool{false, true} {
		got, err := activeAcceptancesOf(ctx, k.s.Q(ctx), "g", headOnly)
		if err != nil {
			t.Fatalf("activeAcceptancesOf(%v): %v", headOnly, err)
		}
		if len(got) != 1 || got[0].NodeID != "c" {
			t.Fatalf("activeAcceptancesOf(headOnly %v) = %d acceptances %+v, want only c's", headOnly, len(got), got)
		}
	}
}

// A node whose two sides both hold a current acceptance contributes none to the list the landings and the base refresh read: it is counted neither once nor twice.
func TestAgentAcceptanceListLeavesOutAmbiguousNodes(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "r", files: fileOf("r"), relationship: true}, {name: "a", files: fileOf("a")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	var manifest string
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions WHERE plan_id = 'g' AND node_id = 'r' LIMIT 1").Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	q := k.s.Q(ctx)
	if got, err := activeAcceptancesOf(ctx, q, "g", false); err != nil || len(got) != 2 {
		t.Fatalf("before the conflict: %d acceptances, %v, want both nodes'", len(got), err)
	}
	k.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"e-forced", "g", "r", 1, AgentInitial, manifest, "executor", "executor", "parent", 0, k.clock())
	k.exec("INSERT INTO dag_agent_acceptances (acceptance_id, plan_id, node_id, manifest_digest, execution_id, criteria_set_digest, reviewer_call_id, reviewer_model, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		"acc-forced", "g", "r", manifest, "e-forced", releaseCriteriaDigest(), "call", reviewerModel, "authors", "parent", 0, k.clock())
	got, err := activeAcceptancesOf(ctx, q, "g", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].NodeID != "a" {
		t.Fatalf("activeAcceptancesOf = %+v, want only a's: r holds an acceptance on both sides", got)
	}
}

// The default manual conflict sweep of a plan whose nodes run on the agent surface finds its checkout from the worktrees the running executions record, before any acceptance exists: it needs no
// --repository, and measures the pair of the running heads.
func TestAgentOnlyPlanSweepFindsItsCheckoutBeforeTheFirstAcceptance(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	if rows := k.agentAcceptanceRows(); rows != 0 {
		t.Fatalf("%d agent acceptances exist before the sweep", rows)
	}
	k.sched.Checkout = ""
	res, err := k.sched.ObserveLive(context.Background(), "g", "parent", SweepInput{Trigger: TriggerManual, TriggerNode: "a", Tips: []SweepTip{{Repository: k.repo.path, Ref: "dev", SHA: k.base}}})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.State != SweepRecorded {
		t.Fatalf("the sweep is %s (%s), want it recorded in the checkout of the running worktrees", res.State, res.Reason)
	}
	want, _ := filepath.EvalSymlinks(k.repo.path)
	if got, _ := filepath.EvalSymlinks(res.Repository); got != want {
		t.Fatalf("the sweep measured in %q, want the repository %q the worktrees belong to", res.Repository, want)
	}
	if m := memberOf(res, MemberPair, "a", "b"); m.Status != MemberObserved || m.LeftHead != k.heads["a"] || m.RightHead != k.heads["b"] {
		t.Fatalf("the pair of the running heads = %+v, want it observed at %s and %s", m, k.heads["a"], k.heads["b"])
	}
}
