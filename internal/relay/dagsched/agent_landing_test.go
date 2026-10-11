package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-1213 (R05): the integration of an agent node's acceptance. dag-integrate takes its candidates from the agent acceptance rows, writes the node-level stage rows to dag_agent_integration_stages, marks the
// merge with an agent marked row and observes the landing with agentIntegratedAt. Every candidate is a real commit in a real repository, the batch merges and verifies it for real and moves a real branch;
// nothing is seeded into a stage or an acceptance table, and nothing is pushed anywhere but to a temporary bare remote.

func fileOf(name string) map[string]string { return map[string]string{name + ".txt": name + "\n"} }

// handover appends the row an orchestrator that took the node over writes (R07 writes it; the row is the same): the same work under the same manifest, held by another task.
func (k *agentKit) handover(node, actor string) string {
	k.t.Helper()
	cur, live, err := currentAgentExecution(context.Background(), k.s.Q(context.Background()), "g", node)
	if err != nil || !live {
		k.t.Fatalf("node %s has no current agent execution: %v", node, err)
	}
	id := AgentExecutionID("g", node, cur.ManifestDigest, cur.Attempt+1)
	k.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, author_model, author_effort, thread_id, worktree, base_sha, run_id, actor_task_id, coordinator_epoch, decided_at, supersedes_execution_id)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", id, "g", node, cur.Attempt+1, AgentParentHandoff, cur.ManifestDigest, cur.Role, cur.Tier, "", "", "", cur.Worktree, cur.BaseSHA, cur.RunID, actor, 0, k.clock(), cur.ExecutionID)
	return id
}

// The agent candidate's owner is the orchestrator that holds the node's current execution, read with executionOf: a task that is not that orchestrator, and the previous orchestrator after a handover, may not
// integrate the node ('a parent integrates its own nodes only', integration_batch.go), and the new orchestrator may. The batch row is written as before and its merged entries name the execution kind.
func TestAgentIntegrateActorIsOrchestrator(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "r", files: fileOf("r"), relationship: true}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	ctx := context.Background()
	deps := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}

	in := k.batchIn()
	in.Actor, in.Nodes = "stranger", []string{"a"}
	_, err := k.sched.IntegrateBatch(ctx, in, deps)
	wantAgentRefusal(t, "a task that holds none of the nodes", err, contract.RefusalScopeRoleMismatch, AgentNotOrchestrator)

	// the orchestrator of a handover row owns the node from then on; the acceptance made before it stands
	k.handover("a", "parent-2")
	in.Actor = "parent"
	_, err = k.sched.IntegrateBatch(ctx, in, deps)
	wantAgentRefusal(t, "the previous orchestrator after a handover", err, contract.RefusalScopeRoleMismatch, AgentNotOrchestrator)
	if n := k.count("SELECT COUNT(*) FROM dag_integration_batches"); n != 0 {
		t.Fatalf("%d batches after the refusals", n)
	}
	in.Actor = "parent-2"
	res, err := k.sched.IntegrateBatch(ctx, in, deps)
	if err != nil || len(res.Merged) != 1 || res.Merged[0].NodeID != "a" || res.Merged[0].ExecutionKind != ExecutionAgent {
		t.Fatalf("batch by the new orchestrator: %+v, %v", res, err)
	}
	// the relationship node is its parent's, and is integrated by the parent
	in.Actor, in.Nodes = "parent", []string{"r"}
	res2, err := k.sched.IntegrateBatch(ctx, in, deps)
	if err != nil || len(res2.Merged) != 1 || res2.Merged[0].NodeID != "r" || res2.Merged[0].ExecutionKind != "" {
		t.Fatalf("batch of the relationship node: %+v, %v", res2, err)
	}
	// the batch rows are written as they were; only an agent entry of merged_json names its execution kind
	var merged1, merged2 string
	if err := k.s.DB.QueryRow("SELECT merged_json FROM dag_integration_batches WHERE batch_id = ?", res.BatchID).Scan(&merged1); err != nil {
		t.Fatal(err)
	}
	if err := k.s.DB.QueryRow("SELECT merged_json FROM dag_integration_batches WHERE batch_id = ?", res2.BatchID).Scan(&merged2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(merged1, `"execution_kind":"agent"`) || strings.Contains(merged2, "execution_kind") {
		t.Fatalf("merged_json = %s and %s", merged1, merged2)
	}
	// the new orchestrator observes the landing; the previous one does not
	if _, err := k.observe("a"); refusalReason(err) != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("observe by the previous orchestrator: %v", err)
	}
	k.sched.Tips, k.sched.Ancestry = newLegacyLocalTipReader(t), GitAncestry{}.Ancestry
	obs, err := k.sched.ObserveIntegration(ctx, "g", "a", "parent-2", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
	if err != nil || !obs.Integrated {
		t.Fatalf("observe by the new orchestrator: %+v, %v", obs, err)
	}
}

// An agent acceptance never enters a merge turn: its observation is carried by none even where a turn landed the same head on the same branch, and an observation that names a turn that never landed does
// not keep the head from being integrated, which the relationship kind's integratedAt would refuse.
func TestAgentNodeNeverEntersMergeTurn(t *testing.T) {
	t.Parallel()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	if _, err := k.integrate(t); err != nil {
		t.Fatalf("batch: %v", err)
	}
	obs, err := k.observe("a")
	if err != nil || !obs.Integrated || len(obs.Observations) != 1 || obs.Observations[0].MergeTurnID != "" {
		t.Fatalf("observe: %+v, %v", obs, err)
	}
	var turn *string
	if err := k.s.DB.QueryRow("SELECT merge_turn_id FROM dag_integration_observations WHERE acceptance_id = ?", k.currentAcceptance("a").AcceptanceID).Scan(&turn); err != nil || turn != nil {
		t.Fatalf("the observation names merge turn %v (%v)", turn, err)
	}
	a := k.currentAcceptance("a").Agent
	// an observation that names a turn that never landed: the relationship kind requires the turn to have landed the head, the agent kind has no turn
	k.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, merge_turn_id, observed_seq, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"obs-turn", a.AcceptanceID, k.repo.path, "dev-turn", a.Head, a.Head, 1, "test", "turn-that-never-landed", 1, k.clock())
	at, err := agentIntegratedAt(context.Background(), k.s.Q(context.Background()), a, k.repo.path, "dev-turn")
	if err != nil || !at.Satisfied || at.Unprovable {
		t.Fatalf("agentIntegratedAt with an observation naming a turn: %+v, %v", at, err)
	}
	if n := k.count("SELECT COUNT(*) FROM merge_turns"); n != 0 {
		t.Fatalf("%d merge turns for an agent node", n)
	}
}

// MarkFrozen reads the actor from executionOf and the acceptance from acceptanceOf, applies the same staleOf and writes the agent marked row (acceptance, execution, head, batch) in the batch's fenced
// transaction: nothing goes to assignment_marks, nothing to the frozen stage table, and after a handover the actor is the new orchestrator's.
func TestAgentMarkFrozenWritesAgentMark(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	ctx := context.Background()
	cands, err := k.sched.AcceptedCandidates(ctx, "g")
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates: %+v, %v", cands, err)
	}
	a := cands[0]
	if a.NodeID != "a" || a.ExecutionKind != ExecutionAgent || a.ExecutionID != k.execution("a") || a.RelationshipID != "" || a.EventID != "" {
		t.Fatalf("the agent candidate = %+v", a)
	}
	// the mark names its batch
	if _, err := k.sched.MarkFrozen(ctx, a, "parent", "evidence"); refusalReason(err) != string(contract.RefusalMalformedReceipt) {
		t.Fatalf("MarkFrozen of a candidate with no batch: %v", err)
	}
	a.BatchID = "batch-1"
	if _, err := k.sched.MarkFrozen(ctx, a, "stranger", "evidence"); refusalReason(err) != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("MarkFrozen by a task that does not hold the node: %v", err)
	}
	id, err := k.sched.MarkFrozen(ctx, a, "parent", "evidence")
	if err != nil || id == "" {
		t.Fatalf("MarkFrozen: %q, %v", id, err)
	}
	rows := k.agentStages()
	if len(rows) != 1 || rows[0].StageID != id || rows[0].Stage != "marked" || rows[0].BatchID != "batch-1" || rows[0].AcceptanceID != a.AcceptanceID || rows[0].ExecutionID != a.ExecutionID ||
		rows[0].Head != a.HeadSHA || rows[0].NodeID != "a" || rows[0].PlanID != "g" || rows[0].RecordedBy != "parent" || rows[0].Detail != "evidence" {
		t.Fatalf("the agent stage rows = %+v", rows)
	}
	if n := k.count("SELECT COUNT(*) FROM assignment_marks") + k.count("SELECT COUNT(*) FROM dag_integration_stages"); n != 0 {
		t.Fatalf("%d rows in assignment_marks and dag_integration_stages for an agent mark", n)
	}
	// the same mark again is the same fact
	if again, err := k.sched.MarkFrozen(ctx, a, "parent", "evidence"); err != nil || again != id || len(k.agentStages()) != 1 {
		t.Fatalf("MarkFrozen again: %q, %v, %d rows", again, err, len(k.agentStages()))
	}
	// a handover moves the node to another orchestrator: the actor is read from the node's current execution
	k.handover("a", "parent-2")
	if _, err := k.sched.MarkFrozen(ctx, a, "parent", "evidence-2"); refusalReason(err) != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("MarkFrozen by the previous orchestrator: %v", err)
	}
	if id2, err := k.sched.MarkFrozen(ctx, a, "parent-2", "evidence-2"); err != nil || id2 == id || len(k.agentStages()) != 2 {
		t.Fatalf("MarkFrozen by the new orchestrator: %q, %v, %d rows", id2, err, len(k.agentStages()))
	}
	// the frozen acceptance has to be the node's current one
	b := cands[1]
	b.BatchID = "batch-1"
	wt := k.worktrees["b"]
	writeFile(t, wt, "b2.txt", "two\n")
	k.repo.git("-C", wt, "add", "b2.txt")
	k.repo.git("-C", wt, "commit", "-q", "-m", "two")
	k.heads["b"] = k.repo.git("-C", wt, "rev-parse", "HEAD")
	k.review("b", reviewerModel, "verified", releaseCriteriaDigest())
	in := k.input("b")
	in.Supersedes = k.currentAcceptance("b").AcceptanceID
	if _, err := k.sched.Accept(ctx, "g", "b", "parent", in); err != nil {
		t.Fatalf("supersede b: %v", err)
	}
	if _, err := k.sched.MarkFrozen(ctx, b, "parent", "evidence"); refusalReason(err) != string(contract.RefusalStaleMarkContext) {
		t.Fatalf("MarkFrozen of a superseded acceptance: %v", err)
	}
}

// dag-integrate pushes only what it verified and moved: VerifiedMoveOnto reads the batch-level rows alone, which a batch of agent candidates writes to dag_integration_stages as any batch does, so it is true
// for the head of an agent-only batch and of a batch that mixes both kinds, and false for a commit the relay did not move the branch onto. The code of VerifiedMoveOnto did not change.
func TestAgentVerifiedMoveOnto(t *testing.T) {
	t.Parallel()
	only := newAgentKit(t, agentSpec{name: "a", files: fileOf("a")}, agentSpec{name: "b", files: fileOf("b")})
	only.acceptAll(agentSpec{name: "a"}, agentSpec{name: "b"})
	mixedSpecs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "r", files: fileOf("r"), relationship: true}}
	mixed := newAgentKit(t, mixedSpecs...)
	mixed.acceptAll(mixedSpecs...)
	ctx := context.Background()
	for what, k := range map[string]*agentKit{"agent-only": only, "mixed": mixed} {
		res, err := k.integrate(t)
		if err != nil || len(res.Merged) != 2 {
			t.Fatalf("%s batch: %+v, %v", what, res, err)
		}
		if ok, err := k.sched.VerifiedMoveOnto(ctx, "dev-int", res.NewHead); err != nil || !ok {
			t.Errorf("%s: VerifiedMoveOnto(%s) = %v, %v; want true", what, res.NewHead, ok, err)
		}
		for name, commit := range map[string]string{"the base": k.base, "a node head": k.heads["a"]} {
			if ok, err := k.sched.VerifiedMoveOnto(ctx, "dev-int", commit); err != nil || ok {
				t.Errorf("%s: VerifiedMoveOnto(%s) = %v, %v; want false", what, name, ok, err)
			}
		}
		if ok, err := k.sched.VerifiedMoveOnto(ctx, "another-ref", res.NewHead); err != nil || ok {
			t.Errorf("%s: VerifiedMoveOnto on another ref = %v, %v; want false", what, ok, err)
		}
		// the batch-level rows are the frozen table's, whichever kinds the batch held
		if n := k.count("SELECT COUNT(*) FROM dag_integration_stages WHERE node_id = '' AND batch_id = ? AND stage IN ('intent', 'ref_moved')", res.BatchID); n != 3 {
			t.Errorf("%s: %d batch-level rows, want the planned and the frozen intent and ref_moved", what, n)
		}
		// the push of the branch is allowed, and it is the same fast-forward as ever
		remote := bareRemote(t)
		k.repo.git("remote", "add", "origin", remote)
		if pushed, err := PushIntegration(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil); err != nil || pushed.Outcome != PushPushed {
			t.Errorf("%s: push: %+v, %v", what, pushed, err)
		}
	}
}

// integrationRefsOf reads an agent acceptance from its own rows: the node-level intent of dag_agent_integration_stages joined by batch id to the batch-level intent, counted when the batch moved the ref or the
// same acceptance has a marked row of it (a head the branch already contained is marked and moves no ref). A batch that failed before its move, and a node that was never frozen, name no ref; the union holds
// the relationship kind's refs as before.
func TestAgentIntegrationRefsUnion(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "bad", files: map[string]string{"bad.txt": "bad\n"}}, {name: "r", files: fileOf("r"), relationship: true},
		{name: "twin", headOf: "a", unreleased: true}}
	k := newAgentKit(t, specs...)
	ctx := context.Background()
	q := k.s.Q(ctx)
	refs := func(node string) []string {
		t.Helper()
		var id string
		if na, found, err := acceptanceOf(ctx, q, "g", node); err != nil || !found {
			t.Fatalf("acceptance of %s: %v", node, err)
		} else {
			id = na.AcceptanceID
		}
		out, err := integrationRefsOf(ctx, q, id)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	// an acceptance that was never frozen in a batch names no ref
	k.acceptAll(specs[0])
	if got := refs("a"); len(got) != 0 {
		t.Fatalf("refs before any batch = %v", got)
	}
	// a batch whose merged tree fails verification moves no ref: the failing node names none
	k.acceptAll(specs[1])
	if res, err := k.sched.IntegrateBatch(ctx, IntegrationBatchInput{Plan: "g", Actor: "parent", Checkout: k.repo.path, IntegrationRef: "dev-int", BaseRef: "dev", Nodes: []string{"bad"}},
		IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil || len(res.Merged) != 0 || len(res.Split) != 1 {
		t.Fatalf("failing batch: %+v, %v", res, err)
	}
	if got := refs("bad"); len(got) != 0 {
		t.Fatalf("refs of a node of a batch that moved nothing = %v", got)
	}
	k.acceptAll(specs[2])
	res, err := k.sched.IntegrateBatch(ctx, IntegrationBatchInput{Plan: "g", Actor: "parent", Checkout: k.repo.path, IntegrationRef: "dev-int", BaseRef: "dev", Nodes: []string{"a", "r"}},
		IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(res.Merged) != 2 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	for _, node := range []string{"a", "r"} {
		if got := refs(node); len(got) != 1 || got[0] != "dev-int" {
			t.Errorf("refs of %s = %v, want [dev-int]", node, got)
		}
	}
	// a node whose head the branch already contains is marked and moves no ref: its ref is the one of the batch that marked it
	k.release("twin")
	k.judged("twin")
	k.mustAccept("twin")
	res2, err := k.sched.IntegrateBatch(ctx, IntegrationBatchInput{Plan: "g", Actor: "parent", Checkout: k.repo.path, IntegrationRef: "dev-int", BaseRef: "dev", Nodes: []string{"twin"}},
		IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(res2.Merged) != 0 || len(res2.AlreadyContained) != 1 || res2.AlreadyContained[0].NodeID != "twin" || res2.AlreadyContained[0].MarkedEvent == "" {
		t.Fatalf("batch of the contained node: %+v, %v", res2, err)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_integration_stages WHERE stage = 'ref_moved' AND batch_id = ?", res2.BatchID); n != 0 {
		t.Fatalf("the contained batch moved the ref (%d rows)", n)
	}
	if got := refs("twin"); len(got) != 1 || got[0] != "dev-int" {
		t.Errorf("refs of the contained node = %v, want [dev-int]", got)
	}
	// a terminal node has no edge that names a target: it arrives with the first observation
	if targets, err := k.sched.CandidateTargets(ctx, "g", "twin"); err != nil || len(targets) != 0 {
		t.Errorf("targets before any observation = %v, %v; a terminal node's target arrives with its first observation", targets, err)
	}
}

// An agent acceptance is integrated only when the head is contained by an observation no later one contradicts AND the merged mark of the same acceptance and execution was written: either alone, a mark of
// another execution, and a negative observation after the positive one all leave it unintegrated.
func TestAgentIntegratedAtNeedsMarkAndObservation(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	ctx := context.Background()
	q := k.s.Q(ctx)
	repo, ref := k.repo.path, "dev-int"
	observe := func(a AgentAcceptance, seq int, ancestor int, subject string) {
		t.Helper()
		k.exec("INSERT INTO dag_integration_observations (observation_id, acceptance_id, repository, base_ref, subject_sha, tip_sha, is_ancestor, method, merge_turn_id, observed_seq, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			fmt.Sprintf("obs-%s-%d", a.NodeID, seq), a.AcceptanceID, repo, ref, subject, subject, ancestor, "test", nil, seq, k.clock())
	}
	integratedAt := func(a AgentAcceptance) bool {
		t.Helper()
		at, err := agentIntegratedAt(ctx, q, a, repo, ref)
		if err != nil {
			t.Fatal(err)
		}
		return at.Satisfied
	}
	a, b := k.currentAcceptance("a").Agent, k.currentAcceptance("b").Agent
	if integratedAt(a) {
		t.Fatal("integrated with neither a mark nor an observation")
	}
	// an observation without the mark
	observe(a, 1, 1, a.Head)
	if integratedAt(a) {
		t.Fatal("integrated by an observation alone")
	}
	// the mark without an observation
	mark := func(node string) {
		t.Helper()
		c, err := k.sched.AcceptedCandidates(ctx, "g")
		if err != nil {
			t.Fatal(err)
		}
		for _, cand := range c {
			if cand.NodeID == node {
				cand.BatchID = "batch-x"
				if _, err := k.sched.MarkFrozen(ctx, cand, "parent", "evidence"); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
		t.Fatalf("no candidate %s", node)
	}
	mark("b")
	if integratedAt(b) {
		t.Fatal("integrated by a mark alone")
	}
	// both: integrated, and an observation of another head does not count
	mark("a")
	if !integratedAt(a) {
		t.Fatal("not integrated by a mark and a containing observation")
	}
	observe(b, 1, 1, a.Head)
	if integratedAt(b) {
		t.Fatal("integrated by an observation of another head")
	}
	// a later negative observation withdraws it, and a positive one after that restores it
	observe(a, 2, 0, a.Head)
	if integratedAt(a) {
		t.Fatal("integrated after a later negative observation")
	}
	observe(a, 3, 1, a.Head)
	if !integratedAt(a) {
		t.Fatal("not integrated after the head was observed contained again")
	}
	// only the target observed counts
	if at, err := agentIntegratedAt(ctx, q, a, repo, "dev-other"); err != nil || at.Satisfied {
		t.Fatalf("integrated on a target nobody observed: %+v, %v", at, err)
	}
	// the mark names the execution of the acceptance: a marked row of another execution is no mark of this one
	c := a
	c.ExecutionID = "an-execution-of-another-run"
	if at, err := agentIntegratedAt(ctx, q, c, repo, ref); err != nil || at.Satisfied {
		t.Fatalf("integrated by the mark of another execution: %+v, %v", at, err)
	}
}

// The node-level stage rows of an agent acceptance are rows of dag_agent_integration_stages and never of dag_integration_stages, which keeps the batch-level rows of every batch; the id of an agent
// acceptance is written into no frozen table but dag_integration_observations (keyed by an acceptance id alone), and the id of its execution into none.
func TestAgentStageRowsStayOutOfFrozenTables(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "r", files: fileOf("r"), relationship: true}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	if _, err := k.integrate(t); err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, node := range []string{"a", "r"} {
		if obs, err := k.observe(node); err != nil || !obs.Integrated {
			t.Fatalf("observe %s: %+v, %v", node, obs, err)
		}
	}
	if n := k.count("SELECT COUNT(*) FROM dag_integration_stages WHERE node_id = 'a'"); n != 0 {
		t.Fatalf("%d node-level rows of the agent node in dag_integration_stages", n)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_integration_stages WHERE node_id = 'r'"); n != 2 {
		t.Fatalf("%d node-level rows of the relationship node in dag_integration_stages, want its intent and marked rows", n)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_integration_stages WHERE node_id = ''"); n < 2 {
		t.Fatalf("%d batch-level rows in dag_integration_stages", n)
	}
	rows := k.agentStages()
	if countAgentStage(rows, "intent", "a") != 1 || countAgentStage(rows, "marked", "a") != 1 || len(rows) != 2 {
		t.Fatalf("the agent stage rows = %+v", rows)
	}
	acceptance, execution := k.currentAcceptance("a").AcceptanceID, k.execution("a")
	frozen := map[string]bool{}
	for _, table := range frozenTables {
		frozen[table] = true
	}
	holds := map[string]bool{}
	execHolds := map[string]bool{}
	for table, tableRows := range allRows(t, k.s.DB) {
		for _, row := range tableRows {
			for _, value := range row {
				if text, ok := value.(string); ok && text == acceptance {
					holds[table] = true
				}
				if text, ok := value.(string); ok && text == execution {
					execHolds[table] = true
				}
			}
		}
	}
	for table := range holds {
		if frozen[table] {
			t.Errorf("the frozen table %s holds the id of an agent acceptance", table)
		}
	}
	if !holds["dag_integration_observations"] || !holds["dag_agent_acceptances"] || !holds["dag_agent_integration_stages"] {
		t.Errorf("the agent acceptance id is held by %v", holds)
	}
	for table := range execHolds {
		if frozen[table] || table == "dag_integration_observations" {
			t.Errorf("%s holds the id of an agent execution", table)
		}
	}
	// the one frozen table that holds an agent acceptance id is the observations: named, so a second one is a failure of this test
	var named []string
	for table := range holds {
		if !strings.HasPrefix(table, "dag_agent_") && table != "flow_calls" {
			named = append(named, table)
		}
	}
	if len(named) != 1 || named[0] != "dag_integration_observations" {
		t.Errorf("tables outside the agent zone that hold an agent acceptance id: %v, want [dag_integration_observations]", named)
	}
}

// The relationship path is what it was: a managed child's node is accepted, frozen, merged, marked in assignment_marks, observed and its slot returned exactly as before, nothing is written to an agent table,
// the batch row names no execution kind, and acceptanceOf reads it as a relationship acceptance.
func TestRelationshipPathUnchanged(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "r1", files: fileOf("r1"), relationship: true}, {name: "r2", files: fileOf("r2"), relationship: true}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	ctx := context.Background()
	for _, node := range []string{"r1", "r2"} {
		if na := k.currentAcceptance(node); na.Kind != AcceptanceRelationship || na.Relationship.HeadSHA != k.heads[node] || na.AcceptanceID != na.Relationship.AcceptanceID {
			t.Fatalf("acceptanceOf %s = %+v", node, na)
		}
	}
	res, err := k.integrate(t)
	if err != nil || len(res.Merged) != 2 || len(res.MarkedEvents) != 2 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	var merged string
	if err := k.s.DB.QueryRow("SELECT merged_json FROM dag_integration_batches WHERE batch_id = ?", res.BatchID).Scan(&merged); err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(merged), &entries); err != nil || len(entries) != 2 {
		t.Fatalf("merged_json = %s (%v)", merged, err)
	}
	for _, entry := range entries {
		if len(entry) != 4 || entry["NodeID"] == nil || entry["AcceptanceID"] == nil || entry["HeadSHA"] == nil || entry["MergeCommit"] == nil {
			t.Fatalf("a merged entry of the relationship kind = %v; want its four members as before", entry)
		}
	}
	if k.count("SELECT COUNT(*) FROM assignment_marks WHERE mark = 'merged'") != 2 || k.count("SELECT COUNT(*) FROM dag_integration_stages WHERE stage = 'marked'") != 2 {
		t.Fatal("the merged marks of the relationship kind are not where they were")
	}
	for _, node := range []string{"r1", "r2"} {
		obs, err := k.observe(node)
		if err != nil || !obs.Integrated || !obs.MarkPresent {
			t.Fatalf("observe %s: %+v, %v", node, obs, err)
		}
	}
	rows := allRows(t, k.s.DB)
	for _, table := range []string{"dag_agent_executions", "dag_agent_acceptances", "dag_agent_integration_stages", "dag_agent_closures"} {
		if len(rows[table]) != 0 {
			t.Errorf("%s holds %d rows of a relationship plan", table, len(rows[table]))
		}
	}
	// a repeated acceptance of a relationship node is the replay it was, and names no execution kind
	again, err := k.sched.Accept(ctx, "g", "r1", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"},
		Commit: &CommitRef{Head: k.heads["r1"], Base: k.base, Checkout: k.repo.path, Record: writeSealedRecord(t, k.repo.path, k.heads["r1"], k.base, k.treeOf(k.heads["r1"]), "pass")}})
	if err != nil || !again.Replayed || again.ExecutionKind != "" || again.ExecutionID != "" || again.RelationshipID == "" {
		t.Fatalf("replay of a relationship acceptance: %+v, %v", again, err)
	}
}

// A node never holds a current acceptance on both sides: acceptanceOf refuses a store that does (cross_kind_acceptance, a disposition_conflict) and returns neither; candidate selection holds such a node out,
// the explicit request for it is refused by name, and once a closure ends the agent side the relationship acceptance is read alone.
func TestAcceptanceOfAtMostOneCurrentAcrossKinds(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "r", files: fileOf("r"), relationship: true}, {name: "a", files: fileOf("a")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	ctx := context.Background()
	q := k.s.Q(ctx)
	if na, found, err := acceptanceOf(ctx, q, "g", "r"); err != nil || !found || na.Kind != AcceptanceRelationship {
		t.Fatalf("relationship alone: %+v %v %v", na, found, err)
	}
	if na, found, err := acceptanceOf(ctx, q, "g", "a"); err != nil || !found || na.Kind != AcceptanceAgent {
		t.Fatalf("agent alone: %+v %v %v", na, found, err)
	}
	if _, found, err := acceptanceOf(ctx, q, "g", "nobody"); err != nil || found {
		t.Fatalf("a node with none: %v %v", found, err)
	}
	// a store written another way: an agent execution and acceptance beside the relationship acceptance of r
	var manifest string
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions WHERE plan_id = 'g' AND node_id = 'r' LIMIT 1").Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	k.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"e-forced", "g", "r", 1, AgentInitial, manifest, "executor", "executor", "parent", 0, k.clock())
	k.exec("INSERT INTO dag_agent_acceptances (acceptance_id, plan_id, node_id, manifest_digest, execution_id, criteria_set_digest, reviewer_call_id, reviewer_model, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		"acc-forced", "g", "r", manifest, "e-forced", releaseCriteriaDigest(), "call", reviewerModel, "authors", "parent", 0, k.clock())
	_, found, err := acceptanceOf(ctx, q, "g", "r")
	wantAgentRefusal(t, "an acceptance on both sides", err, contract.RefusalDispositionConflict, CrossKindAcceptance)
	if found {
		t.Error("acceptanceOf returned a record for a node that holds both")
	}
	cands, err := k.sched.AcceptedCandidates(ctx, "g")
	if err != nil || len(cands) != 1 || cands[0].NodeID != "a" {
		t.Fatalf("candidates = %+v, %v; want the agent node alone", cands, err)
	}
	in := k.batchIn()
	in.Nodes = []string{"r"}
	_, err = k.sched.IntegrateBatch(ctx, in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	wantAgentRefusal(t, "the explicit request for a node that holds both", err, contract.RefusalDispositionConflict, CrossKindAcceptance)
	// a closure of the acceptance ends the agent side; the relationship acceptance is the node's alone
	k.exec("INSERT INTO dag_agent_closures (subject_kind, subject_id, state, slot_released, actor_task_id, coordinator_epoch, reason, closed_at) VALUES ('acceptance','acc-forced','revoked',0,'parent',0,'closed by the test',?)", k.clock())
	if na, found, err := acceptanceOf(ctx, q, "g", "r"); err != nil || !found || na.Kind != AcceptanceRelationship {
		t.Fatalf("after the closure: %+v %v %v", na, found, err)
	}
}

// An agent node's slot goes back with the reason dag_integrated in the transaction of the observation that completes its integration, and only then: while any target it has to land on is unsatisfied the slot is
// held.
func TestAgentIntegratedReleasesSlot(t *testing.T) {
	t.Parallel()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	ctx := context.Background()
	res, err := k.integrate(t)
	if err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	if state, _ := k.slotOf("a"); state != "held" {
		t.Fatalf("slot = %s after the batch and before any observation, want held", state)
	}
	k.sched.Tips, k.sched.Ancestry = newLegacyLocalTipReader(t), GitAncestry{}.Ancestry
	k.repo.git("branch", "dev-other", k.base)
	// two targets: the integration branch, which holds the head, and another branch the observation names, which does not
	obs, err := k.sched.ObserveIntegration(ctx, "g", "a", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}, {Repository: k.repo.path, BaseRef: "dev-other"}})
	if err != nil || obs.Integrated || obs.SlotReleased || !obs.MarkPresent || len(obs.Observations) != 2 {
		t.Fatalf("observe with one target unsatisfied: %+v, %v", obs, err)
	}
	if state, _ := k.slotOf("a"); state != "held" {
		t.Fatalf("slot = %s while a target is unsatisfied, want held", state)
	}
	// the second target takes the head: the observation that satisfies every target returns the slot in its own transaction
	k.repo.git("update-ref", "refs/heads/dev-other", res.NewHead)
	obs, err = k.sched.ObserveIntegration(ctx, "g", "a", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-other"}})
	if err != nil || !obs.Integrated || !obs.SlotReleased {
		t.Fatalf("observe with every target satisfied: %+v, %v", obs, err)
	}
	if state, reason := k.slotOf("a"); state != "released" || reason != "dag_integrated" {
		t.Fatalf("slot = %s (%s), want released for dag_integrated", state, reason)
	}
}

// Two projects that integrate into the same target are serialised by the compare-and-swap of the ref move: the push of a branch is allowed only for a head the relay verified and moved the branch onto. The
// project that loses the race moves nothing and cannot push, and it merges its candidate on the new tip and verifies that tree again before it lands. Each project's orchestrator integrates its own plan's nodes
// and has no authority over the other's.
func TestTwoPlansSameTargetCASLoserReverifies(t *testing.T) {
	t.Parallel()
	a := newAgentKit(t, agentSpec{name: "a", files: fileOf("a")})
	b := newAgentKitOn(t, a.repo, agentSpec{name: "b", files: fileOf("b")})
	a.acceptAll(agentSpec{name: "a"})
	b.acceptAll(agentSpec{name: "b"})
	ctx := context.Background()
	remote := bareRemote(t)
	a.repo.git("remote", "add", "origin", remote)
	verify := stubVerifier(writeStubVerifier(t))

	// b wins the race while a is verifying its merged tree: the branch is b's when a tries to move it
	var won IntegrationBatchResult
	raced := false
	loser := IntegrationBatchDeps{Update: updateIntegrationRef, Verify: func(ctx context.Context, dir string, env []string) error {
		if !raced {
			raced = true
			var err error
			if won, err = b.integrate(t); err != nil {
				t.Errorf("the winner's batch: %v", err)
			}
		}
		return verify(ctx, dir, env)
	}}
	_, err := a.sched.IntegrateBatch(ctx, a.batchIn(), loser)
	wantAgentRefusal(t, "the loser's move", err, contract.RefusalStaleMarkContext, "moved while the merged tree was being verified")
	tip := a.repo.git("rev-parse", "dev-int")
	if won.NewHead == "" || tip != won.NewHead {
		t.Fatalf("the integration branch is %s; want the winner's %s", tip, won.NewHead)
	}
	if n := countAgentStage(a.agentStages(), "marked", ""); n != 0 {
		t.Fatalf("the loser marked %d nodes of a ref it did not move", n)
	}
	// the loser has no verification record of this tip, so it cannot push it; the winner can
	for _, commit := range []string{tip, a.heads["a"]} {
		if ok, err := a.sched.VerifiedMoveOnto(ctx, "dev-int", commit); err != nil || ok {
			t.Errorf("the loser's VerifiedMoveOnto(%s) = %v, %v; want false", commit, ok, err)
		}
	}
	_, err = PushIntegration(ctx, a.repo.path, "origin", "dev", "dev-int", a.sched.VerifiedMoveOnto, nil)
	wantAgentRefusal(t, "the loser's push", err, contract.RefusalMergeBaseMismatch, "did not verify and move the branch onto")
	if got := remoteBranch(t, remote, "dev"); got != "" {
		t.Fatalf("the remote holds %s after the loser's push", got)
	}
	if pushed, err := PushIntegration(ctx, b.repo.path, "origin", "dev", "dev-int", b.sched.VerifiedMoveOnto, nil); err != nil || pushed.Outcome != PushPushed {
		t.Fatalf("the winner's push: %+v, %v", pushed, err)
	}

	// the loser runs again: it merges on the new tip, verifies that merged tree, moves the branch by the compare-and-swap and then pushes by a fast-forward
	again, err := a.sched.IntegrateBatch(ctx, a.batchIn(), IntegrationBatchDeps{Update: updateIntegrationRef, Verify: verify})
	if err != nil || len(again.Merged) != 1 || again.OldHead != won.NewHead {
		t.Fatalf("the loser's second batch: %+v, %v; want a merge on the winner's tip %s", again, err, won.NewHead)
	}
	if again.Verification.TreeHash != a.treeOf(again.NewHead) || again.Verification.Result != "pass" || again.VerificationDigest == "" {
		t.Fatalf("the verification of the loser's second batch = %+v (%s), want a pass on the tree of %s", again.Verification, again.VerificationDigest, again.NewHead)
	}
	if !strings.Contains(a.repo.git("ls-tree", "-r", "--name-only", again.NewHead), "b.txt") {
		t.Fatal("the new tip does not hold the winner's work under the loser's")
	}
	if ok, err := a.sched.VerifiedMoveOnto(ctx, "dev-int", again.NewHead); err != nil || !ok {
		t.Fatalf("VerifiedMoveOnto after the second batch = %v, %v", ok, err)
	}
	if pushed, err := PushIntegration(ctx, a.repo.path, "origin", "dev", "dev-int", a.sched.VerifiedMoveOnto, nil); err != nil || pushed.Outcome != PushPushed {
		t.Fatalf("the loser's push after verifying: %+v, %v", pushed, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != again.NewHead {
		t.Fatalf("the remote holds %s, want %s", got, again.NewHead)
	}
	// no cross-plan authority: another project's orchestrator integrates none of this plan's nodes
	in := a.batchIn()
	in.Actor, in.Nodes = "parent-of-the-other-project", []string{"a"}
	_, err = a.sched.IntegrateBatch(ctx, in, IntegrationBatchDeps{Update: updateIntegrationRef, Verify: verify})
	if err == nil {
		t.Fatal("another project's orchestrator integrated a node of this plan")
	}
}

// landed_sha of an agent landing is the merge commit the relay recorded for the acceptance, not the head the acceptance stands on and not the tip the observation happened to read (the cancelled CRW-1003, d3):
// a batch that merges the head makes a merge commit that differs from the head, the batch row keeps it with the observation, and a later merge that moves the tip changes nothing about it.
func TestAgentLandedShaIsRecordedMergeCommit(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	ctx := context.Background()
	q := k.s.Q(ctx)
	k.acceptAll(specs[0])
	first, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(first.Merged) != 1 {
		t.Fatalf("batch of a: %+v, %v", first, err)
	}
	mergeCommit, head := first.Merged[0].MergeCommit, first.Merged[0].HeadSHA
	if mergeCommit == "" || mergeCommit == head || head != k.heads["a"] {
		t.Fatalf("merged entry %+v: the merge commit must differ from the accepted head %s", first.Merged[0], k.heads["a"])
	}
	// another node lands after it: the tip the observation of a reads is no longer the merge commit of a
	k.acceptAll(specs[1])
	second, err := k.integrate(t)
	if err != nil || len(second.Merged) != 1 || second.NewHead == mergeCommit {
		t.Fatalf("batch of b: %+v, %v", second, err)
	}
	obs, err := k.observe("a")
	if err != nil || !obs.Integrated || len(obs.Observations) != 1 {
		t.Fatalf("observe: %+v, %v", obs, err)
	}
	if obs.Observations[0].TipSHA != second.NewHead || obs.Observations[0].SubjectSHA != head {
		t.Fatalf("observation = %+v", obs.Observations[0])
	}
	acceptance := k.currentAcceptance("a").AcceptanceID
	if got, err := agentLandedSHA(ctx, q, acceptance); err != nil || got != mergeCommit {
		t.Fatalf("agentLandedSHA = %q, %v; want the recorded merge commit %s", got, err, mergeCommit)
	}
	if got, err := landedOf(ctx, q, obs.Observations[0].ObservationID); err != nil || got != mergeCommit || got == head || got == second.NewHead {
		t.Fatalf("landedOf = %q, %v; want %s (not the head %s, not the tip %s)", got, err, mergeCommit, head, second.NewHead)
	}
	// the merge commit recorded with the batch is still there: the batch row holds it and the marked row names the batch
	var merged string
	if err := k.s.DB.QueryRow("SELECT merged_json FROM dag_integration_batches WHERE batch_id = ?", first.BatchID).Scan(&merged); err != nil || !strings.Contains(merged, mergeCommit) {
		t.Fatalf("batch row merged_json = %s (%v), want the merge commit %s", merged, err, mergeCommit)
	}
	var batch string
	if err := k.s.DB.QueryRow("SELECT batch_id FROM dag_agent_integration_stages WHERE acceptance_id = ? AND stage = 'marked'", acceptance).Scan(&batch); err != nil || batch != first.BatchID {
		t.Fatalf("the marked row names batch %s (%v), want %s", batch, err, first.BatchID)
	}
	if k.repo.git("rev-parse", mergeCommit+"^{commit}") != mergeCommit {
		t.Fatal("the merge commit is not in the repository")
	}
}

// A mark that cannot be written when the branch has moved is a mark_pending row of the agent table, and the next run completes it from the frozen row, as the relationship kind does: here the node is handed
// over between the move and the mark, so the previous orchestrator's mark is refused and the new orchestrator's run writes it.
func TestAgentMarkPendingIsCompletedByTheNextBatch(t *testing.T) {
	t.Parallel()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	ctx := context.Background()
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef,
		AfterMove: func(context.Context) error { k.handover("a", "parent-2"); return nil }})
	if err != nil || len(res.Merged) != 1 || len(res.Pending) != 1 || res.Pending[0] != "a" || len(res.MarkedEvents) != 0 {
		t.Fatalf("batch: %+v, %v; want the mark pending", res, err)
	}
	rows := k.agentStages()
	if countAgentStage(rows, "intent", "a") != 1 || countAgentStage(rows, "mark_pending", "a") != 1 || countAgentStage(rows, "marked", "a") != 0 {
		t.Fatalf("agent stage rows after the batch = %+v", rows)
	}
	if n := k.count("SELECT COUNT(*) FROM assignment_marks") + k.count("SELECT COUNT(*) FROM dag_integration_stages WHERE node_id <> ''"); n != 0 {
		t.Fatalf("%d node-level rows in the frozen tables", n)
	}
	// the next run is the new orchestrator's: it completes the mark of the earlier batch from the frozen row
	in := k.batchIn()
	in.Actor = "parent-2"
	if _, err := k.sched.IntegrateBatch(ctx, in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil && refusalReason(err) != string(contract.RefusalDispositionConflict) {
		t.Fatalf("next batch: %v", err)
	}
	var marked int
	for _, r := range k.agentStages() {
		if r.Stage == "marked" && r.BatchID == res.BatchID && r.NodeID == "a" && r.AcceptanceID == k.currentAcceptance("a").AcceptanceID {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("the earlier batch has %d marked rows of a after the next run, want 1: %+v", marked, k.agentStages())
	}
	k.sched.Tips, k.sched.Ancestry = newLegacyLocalTipReader(t), GitAncestry{}.Ancestry
	if obs, err := k.sched.ObserveIntegration(ctx, "g", "a", "parent-2", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}}); err != nil || !obs.Integrated {
		t.Fatalf("observe: %+v, %v", obs, err)
	}
}
