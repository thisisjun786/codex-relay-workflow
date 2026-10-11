package manage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1214 (R06): crw manage reads a node on both sides. The rows of an agent node (dag_agent_executions, dag_agent_acceptances and the agent stages of dag_agent_integration_stages, R04 and R05) are
// written here the way the agent's own path leaves them, in the shape the zone's guards accept, over a plan written through the DAG repository; every reader under test is the production reader.

// agentSeed writes the agent rows of one plan into a fixture's store. exec runs one statement on the fixture's store; prepare writes the plan the fixture described (an agent row names the plan and its
// nodes, and the manifest it rests on carries the node's own slice and criteria digests, so a reading that checks them finds the plan as it is).
type agentSeed struct {
	t       *testing.T
	plan    string
	store   func() *store.Store
	prepare func()
	exec    func(query string, args ...any)
	at      string
}

// agentRows is one execution and, when accepted, its acceptance.
type agentRows struct {
	execution, manifest, acceptance string
}

func (a agentSeed) node(node string) dag.SnapNode {
	a.t.Helper()
	a.prepare()
	ctx := context.Background()
	snap, _, err := dag.SnapshotAt(ctx, a.store().Q(ctx), a.plan, 0)
	if err != nil {
		a.t.Fatalf("snapshot of %s: %v", a.plan, err)
	}
	for _, n := range snap.Nodes {
		if n.NodeID == node {
			return n
		}
	}
	a.t.Fatalf("plan %s holds no node %s", a.plan, node)
	return dag.SnapNode{}
}

// manifest stores the input manifest an agent execution of the node is released under.
func (a agentSeed) manifest(node string) string {
	a.t.Helper()
	n := a.node(node)
	body := map[string]any{
		"schema": dag.SchemaManifest, "node_id": node, "issue_key": n.IssueKey, "node_slice_digest": n.SliceDigest,
		"criteria_set_digest": n.CriteriaSetDigest, "inputs": []any{}, "rule_version": map[string]any{"model": "m"},
		"plan_revision_no": 1, "coordinator_epoch": 0, "created_by_task_id": "task-parent", "created_at": a.at,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		a.t.Fatal(err)
	}
	digest, err := (&dag.Repo{Store: a.store()}).PutManifest(context.Background(), raw)
	if err != nil {
		a.t.Fatalf("manifest of %s: %v", node, err)
	}
	return digest
}

// execution appends one execution row to the node's chain (the first has no predecessor) and returns its id and manifest.
func (a agentSeed) execution(node, kind string, attempt int, supersedes string) agentRows {
	a.t.Helper()
	manifest := a.manifest(node)
	id := dagsched.AgentExecutionID(a.plan, node, manifest, int64(attempt))
	var predecessor any
	if supersedes != "" {
		predecessor = supersedes
	}
	a.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at, supersedes_execution_id) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		id, a.plan, node, attempt, kind, manifest, "executor", "executor", "task-parent", 0, a.at, predecessor)
	return agentRows{execution: id, manifest: manifest}
}

// accept writes the acceptance of an execution's head, under the criteria the plan fixes for the node.
func (a agentSeed) accept(node string, rows agentRows, head, repository string) agentRows {
	a.t.Helper()
	criteria := a.node(node).CriteriaSetDigest
	rows.acceptance = dagsched.AgentAcceptanceID(a.plan, node, rows.execution, head, criteria)
	a.exec("INSERT INTO dag_agent_acceptances (acceptance_id, plan_id, node_id, manifest_digest, execution_id, head, base, repository, premerge_digest, premerge_json, premerge_evaluated_head, verification_digest, verification_json, tree_sha,"+
		" criteria_set_digest, reviewer_call_id, reviewer_thread_id, reviewer_turn_id, reviewer_model, reviewer_effort, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at)"+
		" VALUES (?,?,?,?,?,?,'base',?,'p-digest','{\"schema\":\"premerge-record/1\"}',?,'v-digest','{\"schema\":\"verification-record/1\"}','tree',?,'rev-call','rev-thread','rev-turn','gpt-fake-sol','high','authors','task-parent',0,?)",
		rows.acceptance, a.plan, node, rows.manifest, rows.execution, head, repository, head, criteria, a.at)
	return rows
}

// integrate writes what a batch of the integration ref leaves for an accepted head: the batch-level intent and ref move, the node-level intent and the agent marked row; and the observation of the head in
// the ref, when observe is set.
func (a agentSeed) integrate(node string, rows agentRows, head, repository, ref string, observe bool) {
	a.t.Helper()
	batch := "batch-" + node
	for _, stage := range []struct{ id, kind, detail string }{{"i", "intent", `{"integration_ref":"` + ref + `"}`}, {"m", "ref_moved", "{}"}} {
		a.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			batch+"-batch-"+stage.id, batch, a.plan, stage.kind, "", "", "", "", 0, "", stage.detail, "task-parent", a.at)
	}
	for _, stage := range []string{"intent", "marked"} {
		a.exec("INSERT INTO dag_agent_integration_stages (stage_id, batch_id, plan_id, node_id, acceptance_id, execution_id, stage, head, detail, recorded_by, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			batch+"-"+stage, batch, a.plan, node, rows.acceptance, rows.execution, stage, head, "{}", "task-parent", a.at)
	}
	if observe {
		a.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, observed_seq, reverted_by, observed_at) VALUES (?,?,?,?,?,?,1,'ancestry',1,NULL,?)",
			"observation-"+node, rows.acceptance, repository, ref, head, "tip", a.at)
	}
}

func (f *branchFixture) agentSeed() agentSeed {
	return agentSeed{t: f.t, plan: branchTestPlan, store: func() *store.Store { return f.store }, prepare: f.writePlan, exec: f.exec, at: branchTestStamp(0)}
}

func (f *dagReviewFixture) agentSeed() agentSeed {
	return agentSeed{t: f.t, plan: "plan-1", store: func() *store.Store { return f.store }, prepare: f.writePlans, exec: f.exec, at: dagReviewAt(0)}
}

func (f *checkpointFixture) agentSeed(plan string) agentSeed {
	return agentSeed{t: f.t, plan: plan, store: func() *store.Store { return f.store }, prepare: func() {}, exec: f.exec, at: checkpointAt(1)}
}

// A node an agent is running is released as far as every reader of crw manage is concerned: the branch candidates leave it, and the bundle it is in, out; the review counts its executions (a correction after
// the first is an execution again) and reads it as released; the relay read shows it in the running stage of its plan.
func TestAgentReleaseVisibleToManage(t *testing.T) {
	t.Run("the branch candidates leave a released agent node and its bundle out", func(t *testing.T) {
		// B waits on A's edge, so A is the only ready node of its bundle. C is released to an agent, which takes C+D out.
		f := branchNewFixture(t, "CRW-1", "CRW-2")
		f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5")
		f.edge("e1", "A", "B").edge("e2", "C", "D")
		for _, node := range []string{"A", "B", "C", "D", "E"} {
			f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
		}
		f.agentSeed().execution("C", dagsched.AgentInitial, 1, "")
		got := branchSummaries(branchList(t, f.publish().run()))
		branchWant(t, got, "A+B pkg/A.go,pkg/B.go ready=1 edges=1")
	})

	t.Run("the review counts the executions of an agent node", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewOnePlan(t, f, "A")
		seed := f.agentSeed()
		first := seed.execution("A", dagsched.AgentInitial, 1, "")
		seed.execution("A", dagsched.AgentCorrection, 2, first.execution)
		f.close()

		review := dagReviewRunReview(t, f, nil, 0)
		found := dagReviewFind(review, dagReviewKindReleasedRepeatedly)
		if len(found) != 1 || found[0].Node != "A" || found[0].Detail != "executed 2 times (initial 1, correction 1)" {
			t.Fatalf("released_repeatedly = %+v, want one for A, executed twice", found)
		}
		if len(review.Plans) != 1 || review.Plans[0].Released != 1 {
			t.Fatalf("the review reads the plan as %+v, want the agent node released", review.Plans)
		}
	})

	t.Run("the relay read shows the agent node in the running stage", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewOnePlan(t, f, "A")
		f.agentSeed().execution("A", dagsched.AgentInitial, 1, "")
		f.close()

		relayReadWithStore(t, f.path, func(ctx context.Context, st *store.Store) {
			var projection RelayProjection
			plans := relayReadPlans(ctx, st, RelayReadOptions{Plans: []string{"plan-1"}}, &projection)
			if len(plans) != 1 || plans[0].Read.State != relayReadReadOK || plans[0].Stages["running"] != 1 {
				t.Fatalf("the plan item = %+v (failures %+v), want one running node", plans, projection.Failures)
			}
		})
	})
}

// An agent acceptance is the node's acceptance for crw manage: the integration of its head is the scheduler's own judgement (the agent marked row and an observation in every target), so a landed agent node
// leaves the branch graph and counts as an integration of its project's checkpoint, and an agent node that is accepted and not landed does neither.
func TestAgentAcceptanceVisibleToManage(t *testing.T) {
	t.Run("a landed agent node leaves the branch graph and an unlanded one stays", func(t *testing.T) {
		build := func(t *testing.T, landed bool) []string {
			t.Helper()
			// C joins D, which joins E: a C that stays live keeps D and E inside its component, and a C that landed leaves D+E a bundle of its own.
			f := branchNewFixture(t, "CRW-1", "CRW-2", "CRW-4", "CRW-5")
			f.node("A", "CRW-1").node("B", "CRW-2").node("C", "CRW-3").node("D", "CRW-4").node("E", "CRW-5")
			f.edge("e1", "A", "B").edge("e2", "C", "D").edge("e3", "D", "E")
			for _, node := range []string{"A", "B", "C", "D", "E"} {
				f.region(node, "pkg/"+node+".go", "file", "", "edit", false)
			}
			seed := f.agentSeed()
			rows := seed.accept("C", seed.execution("C", dagsched.AgentInitial, 1, ""), "head-C", branchTestRepo)
			seed.integrate("C", rows, "head-C", branchTestRepo, "dev", landed)
			return branchSummaries(branchList(t, f.publish().run()))
		}
		live := build(t, false)
		for _, summary := range live {
			if len(summary) >= 3 && summary[:3] == "D+E" {
				t.Fatalf("an agent node that is marked and not observed left the graph: %v", live)
			}
		}
		landed := build(t, true)
		var gotDE bool
		for _, summary := range landed {
			if len(summary) >= 3 && summary[:3] == "D+E" {
				gotDE = true
			}
		}
		if !gotDE {
			t.Fatalf("a landed agent node stayed in the graph: %v, want D+E to be a bundle of its own", landed)
		}
	})

	t.Run("a landed agent acceptance is an integration of the checkpoint", func(t *testing.T) {
		count := func(t *testing.T, observe bool) int {
			t.Helper()
			f := checkpointNewFixture(t)
			f.scope("relationship-1", "project-1")
			f.plan("plan-1", "project-1", "node", [2]string{"owner/repo", "dev"})
			seed := f.agentSeed("plan-1")
			rows := seed.accept("node", seed.execution("node", dagsched.AgentInitial, 1, ""), "head-1", "owner/repo")
			seed.integrate("node", rows, "head-1", "owner/repo", "dev", observe)
			f.close()
			report := checkpointReport(t, checkpointRead(t, f, CheckpointOptions{}, nil), "project-1")
			if report.Counts.IntegrationsSinceCheckpoint == nil {
				t.Fatalf("the integration count is unmeasured: %+v", report)
			}
			return *report.Counts.IntegrationsSinceCheckpoint
		}
		if got := count(t, false); got != 0 {
			t.Fatalf("an agent acceptance with a mark and no observation counted as %d integrations", got)
		}
		if got := count(t, true); got != 1 {
			t.Fatalf("a landed agent acceptance counted as %d integrations, want 1", got)
		}
	})

	t.Run("the review and the relay read show the accepted and the landed node", func(t *testing.T) {
		f := dagReviewNewFixture(t)
		dagReviewOnePlan(t, f, "A")
		seed := f.agentSeed()
		rows := seed.accept("A", seed.execution("A", dagsched.AgentInitial, 1, ""), "head-A", "owner/repo")
		seed.integrate("A", rows, "head-A", "owner/repo", "dev", true)
		f.close()

		review := dagReviewRunReview(t, f, nil, 0)
		if len(review.Plans) != 1 || review.Plans[0].Accepted != 1 || review.Plans[0].Integrated != 1 {
			t.Fatalf("the review reads the plan as %+v, want the agent node accepted and integrated", review.Plans)
		}
		relayReadWithStore(t, f.path, func(ctx context.Context, st *store.Store) {
			var projection RelayProjection
			plans := relayReadPlans(ctx, st, RelayReadOptions{Plans: []string{"plan-1"}}, &projection)
			if len(plans) != 1 || plans[0].Stages["integrated"] != 1 {
				t.Fatalf("the plan item = %+v (failures %+v), want one integrated node", plans, projection.Failures)
			}
		})
	})
}
