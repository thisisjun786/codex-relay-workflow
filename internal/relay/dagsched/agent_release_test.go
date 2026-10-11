package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1212 (R04): the agent surface of dag-release, the union readers executionOf and releaseOf, the cross-kind exclusion, and the cases of the cancelled CRW-1003 that concern it (d1, d2, d4, P3).

// frozenTables are the tables of the relationship kind an agent node never writes (R04: no agent row and no sentinel relationship or event id in any of them).
var frozenTables = []string{"dag_releases", "dag_node_executions", "dag_acceptances", "dag_verified_heads", "dag_integration_stages", "dag_acceptance_premerge", "dag_acceptance_verifications"}

// agentRequest is the release request of the agent surface: no endpoints, no managed start.
func (k *releaseKit) agentRequest(base bool) ReleaseRequest {
	req := ReleaseRequest{Schema: SchemaReleaseRequest, Criteria: releaseCriteria,
		RuleVersion:   RuleVersion{SkillsDigest: dig("skills"), Model: "gpt-5", Effort: "medium", PromptTemplate: "template-1", RelayBuild: "test-build"},
		ArtifactRoots: []string{k.root}}
	if base {
		req.Base = &BaseSpec{Repository: "owner/repo", Ref: "dev"}
	}
	return req
}

func (k *releaseKit) releaseAgent(plan, node string) (AgentReleaseResult, error) {
	k.t.Helper()
	return k.releaseAgentBy(k.sched, plan, node)
}

func (k *releaseKit) releaseAgentBy(s *Scheduler, plan, node string) (AgentReleaseResult, error) {
	k.t.Helper()
	n, _ := nodeOf(k.snapshot(plan), node)
	return s.ReleaseAgent(context.Background(), plan, node, "parent", k.agentRequest(n.Kind == dag.NodeImplementation), AgentReleaseOptions{RunID: "run-1", Worktree: "/wt/" + node})
}

func (k *releaseKit) mustReleaseAgent(plan, node string) AgentReleaseResult {
	k.t.Helper()
	res, err := k.releaseAgent(plan, node)
	if err != nil {
		k.t.Fatalf("release agent %s: %v", node, err)
	}
	return res
}

// agentCloses ends an execution the way the lifecycle of R07 will: a closure row, and the slot returned in the same step.
func (k *releaseKit) agentCloses(plan, node, execution, state string) {
	k.t.Helper()
	k.exec("INSERT INTO dag_agent_closures (subject_kind, subject_id, state, slot_released, actor_task_id, coordinator_epoch, reason, closed_at) VALUES ('execution',?,?,1,'parent',0,'closed by the test',?)", execution, state, k.clock())
	if _, err := k.sched.releaseSlot(context.Background(), k.s.Q(context.Background()), plan, node, "parent", "dag_agent_closed"); err != nil {
		k.t.Fatal(err)
	}
}

// issueNode is a node of a plan that shares its issue with others.
func issueNode(id, kind, issue string) doc {
	n := relNode(id, kind)
	n["issue_key"] = issue
	return doc{"op": dag.OpAddNode, "node": n}
}

func (k *releaseKit) agentRowCounts() map[string]int {
	out := map[string]int{}
	for _, table := range []string{"dag_agent_executions", "dag_agent_closures", "dag_input_manifests", "managed_start_requests", "relationships", "dag_release_requests", "dag_release_recoveries", "scope_bindings"} {
		out[table] = k.count("SELECT COUNT(*) FROM " + table)
	}
	out["slots"] = k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node'")
	return out
}

// The agent release records ONE row of dag_agent_executions (kind initial: the release fact, the actor, the epoch, the time), the input manifest and a held slot, and nothing of a managed start: no
// managed start request, no relationship, no standby, no dag_releases row. The host is never asked for a child.
func TestAgentReleaseReservesSlotNoManagedStart(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	baseline := k.agentRowCounts()
	res := k.mustReleaseAgent("rp", "A")
	if res.Replayed || res.Kind != AgentInitial || res.Attempt != 1 || res.ExecutionID == "" || res.SlotID == "" || res.ManifestDigest == "" || res.Role != "executor" || res.Tier != "executor" || res.DecidedAt == "" {
		t.Fatalf("result = %+v", res)
	}
	added := map[string]int{"dag_agent_executions": 1, "dag_input_manifests": 1, "slots": 1}
	for table, n := range k.agentRowCounts() {
		if n != baseline[table]+added[table] {
			t.Errorf("%s holds %d rows, want %d (%d before)", table, n, baseline[table]+added[table], baseline[table])
		}
	}
	for _, table := range frozenTables {
		if n := k.count("SELECT COUNT(*) FROM " + table); n != 0 {
			t.Errorf("%s holds %d rows of an agent release", table, n)
		}
	}
	if created, sent := k.host.counts(); created != 0 || sent != 0 {
		t.Fatalf("the host created %d threads and sent %d messages", created, sent)
	}
	var kind, manifest, actor, run, worktree, supersedes string
	var epoch int64
	if err := k.s.DB.QueryRow("SELECT kind, manifest_digest, actor_task_id, coordinator_epoch, run_id, worktree, COALESCE(supersedes_execution_id, '') FROM dag_agent_executions WHERE execution_id = ?", res.ExecutionID).
		Scan(&kind, &manifest, &actor, &epoch, &run, &worktree, &supersedes); err != nil {
		t.Fatal(err)
	}
	if kind != "initial" || manifest != res.ManifestDigest || actor != "parent" || epoch != 0 || run != "run-1" || worktree != "/wt/A" || supersedes != "" {
		t.Fatalf("the row = %s %s %s %d %s %s %q", kind, manifest, actor, epoch, run, worktree, supersedes)
	}
	if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = 'rp/A' AND state = 'held'") != 1 {
		t.Fatal("the slot is not held under the node's subject")
	}
	// the manifest is the frozen record the release stands on
	body, found, err := k.repo.ReadManifest(context.Background(), res.ManifestDigest)
	if err != nil || !found || body["node_id"] != "A" {
		t.Fatalf("manifest = %v %v %v", body, found, err)
	}
	// repeating the release answers the same execution and writes nothing
	before := allRows(t, k.s.DB)
	again := k.mustReleaseAgent("rp", "A")
	if !again.Replayed || again.ExecutionID != res.ExecutionID || again.SlotID != res.SlotID || again.DecidedAt != res.DecidedAt {
		t.Fatalf("repeat = %+v", again)
	}
	if !reflect.DeepEqual(before, allRows(t, k.s.DB)) {
		t.Fatal("a repeated release wrote rows")
	}
}

// An agent row of a node is written into none of the frozen tables of the relationship kind, whatever the node: not the release, not the execution, not an acceptance-side row; and the
// dag_integration_observations table, the one frozen table that is keyed by an acceptance id alone, holds nothing of it either.
func TestNoAgentRowInFrozenTables(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.mustReleaseAgent("rp", "A")
	k.mustReleaseAgent("rp", "I")
	rows := allRows(t, k.s.DB)
	for _, table := range append(append([]string{}, frozenTables...), "dag_integration_observations", "dag_release_requests", "dag_release_recoveries") {
		if got := rows[table]; len(got) != 0 {
			t.Errorf("%s holds %v", table, got)
		}
	}
	// no sentinel relationship or event id: the id of an execution is written nowhere but the agent tables
	executions := map[string]bool{}
	for _, row := range rows["dag_agent_executions"] {
		executions[row["execution_id"].(string)] = true
	}
	if len(executions) != 2 {
		t.Fatalf("executions = %v", executions)
	}
	for table, tableRows := range rows {
		if table == "dag_agent_executions" {
			continue
		}
		for _, row := range tableRows {
			for column, value := range row {
				if text, ok := value.(string); ok && executions[text] {
					t.Errorf("%s.%s holds the id of an agent execution: %q", table, column, text)
				}
			}
		}
	}
}

// The release is fenced by the coordinator epoch: a session that does not hold the plan's epoch, from the start or by a claim that lands between the judgement and the write, is refused
// stale_coordinator_epoch and writes nothing.
func TestAgentReleaseEpochFenced(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	stale := k.sched
	k.claim(stale, "rp", "parent", "session-1")
	stale.testBetweenReadAndIntent = func() { k.session("rp", "parent", "session-2") }
	before := allRows(t, k.s.DB)
	if _, err := k.releaseAgentBy(stale, "rp", "A"); !isStale(err) {
		t.Fatalf("release = %v, want stale_coordinator_epoch", err)
	}
	after := allRows(t, k.s.DB)
	delete(before, "dag_coordinator_claims")
	delete(after, "dag_coordinator_claims")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the stale release wrote something")
	}
	// a session that never held the epoch is refused before the node is read
	stale.testBetweenReadAndIntent = nil
	if _, err := k.releaseAgentBy(stale, "rp", "A"); !isStale(err) {
		t.Fatalf("release by the replaced session = %v, want stale_coordinator_epoch", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_agent_executions") != 0 || k.count("SELECT COUNT(*) FROM execution_slots") != 0 {
		t.Fatal("a fenced release left a row")
	}
	// the session that holds the epoch releases the node and records the epoch it held
	fresh := k.session("rp", "parent", "session-3")
	res, err := k.releaseAgentBy(fresh, "rp", "A")
	if err != nil || res.Epoch != fresh.ExpectedEpoch || res.Epoch == 0 {
		t.Fatalf("release by the holder: %+v %v", res, err)
	}
}

// The actor of an agent release is the registered parent of the plan's project: another task releases nothing and reserves nothing.
func TestAgentReleaseNeedsTheRegisteredParent(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	before := allRows(t, k.s.DB)
	_, err := k.sched.ReleaseAgent(context.Background(), "rp", "A", "stranger", k.agentRequest(false), AgentReleaseOptions{})
	if refusalReason(err) != "scope_role_mismatch" {
		t.Fatalf("a stranger released a node: %v", err)
	}
	if !reflect.DeepEqual(before, allRows(t, k.s.DB)) {
		t.Fatal("the refused release wrote rows")
	}
	if _, err := k.sched.ReleaseAgent(context.Background(), "nobody", "A", "parent", k.agentRequest(false), AgentReleaseOptions{}); refusalReason(err) != "unregistered_scope" {
		t.Fatalf("a plan that does not exist: %v", err)
	}
	if _, err := k.sched.ReleaseAgent(context.Background(), "rp", "A", "parent", k.agentRequest(false), AgentReleaseOptions{Role: " executor"}); refusalReason(err) != "malformed_receipt" {
		t.Fatalf("a role with whitespace: %v", err)
	}
}

// A node whose relationship side is current is not given to an agent: a managed child holds it. This also holds after the node was resized (update_node) and is released again.
func TestAgentReleaseRefusesLiveRelationship(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.mustRelease("rp", "A")
	before := allRows(t, k.s.DB)
	_, err := k.releaseAgent("rp", "A")
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), LiveRelationshipExecution) {
		t.Fatalf("agent release of a node a managed child runs: %v", err)
	}
	if !reflect.DeepEqual(before, allRows(t, k.s.DB)) {
		t.Fatal("the refused release wrote rows")
	}
	// size_fit changes the node (update_node) and the parent releases it again as an agent: the relationship execution still stands
	k.putPlan("sz", 0, "sz-r1", addRelNode("S", dag.NodeNonPR))
	k.mustRelease("sz", "S")
	k.putPlan("sz", int(k.snapshot("sz").Revision), "sz-resize", doc{"op": dag.OpUpdateNode, "node": relNode("S", dag.NodeImplementation)})
	_, err = k.releaseAgent("sz", "S")
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), LiveRelationshipExecution) {
		t.Fatalf("agent release after update_node: %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_agent_executions") != 0 {
		t.Fatal("an agent row was written beside a live relationship")
	}
	// an open intent counts as well: a managed release whose start did not reach a child holds the node
	real := k.sched.Start
	k.sched.Start = func(context.Context, []byte) (StartAnswer, error) { return StartAnswer{}, errCrash }
	if _, err := k.release("rp", "I"); !errors.Is(err, errCrash) {
		t.Fatalf("the intent was not written: %v", err)
	}
	k.sched.Start = real
	if _, err := k.releaseAgent("rp", "I"); err == nil || !strings.Contains(err.Error(), LiveRelationshipExecution) {
		t.Fatalf("agent release over an open managed intent: %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_agent_executions") != 0 {
		t.Fatal("an agent row was written beside a live relationship")
	}
}

// A relationship release is refused live_agent_execution while an agent execution without a closure is current, and starts nothing; once a closure ends the execution the node is released as a
// managed child like any other.
func TestRelationshipReleaseRefusesLiveAgentExecution(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	agent := k.mustReleaseAgent("rp", "A")
	before := allRows(t, k.s.DB)
	_, err := k.release("rp", "A")
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), LiveAgentExecution) || !strings.Contains(err.Error(), agent.ExecutionID) {
		t.Fatalf("managed release of a node an agent holds: %v", err)
	}
	if created, _ := k.host.counts(); created != 0 {
		t.Fatalf("a child was created (%d)", created)
	}
	if !reflect.DeepEqual(before, allRows(t, k.s.DB)) {
		t.Fatal("the refused release wrote rows")
	}
	// the other node of the plan is released as before
	k.mustRelease("rp", "I")
	// the closure ends the agent's hold and returns its slot; the node can be given to a managed child
	k.agentCloses("rp", "A", agent.ExecutionID, "cancelled")
	res, err := k.release("rp", "A")
	if err != nil || !res.Bound || res.RelationshipID == "" {
		t.Fatalf("managed release after the closure: %+v %v", res, err)
	}
	// and now the relationship side is current, so the agent surface is refused again
	if _, err := k.releaseAgent("rp", "A"); err == nil || !strings.Contains(err.Error(), LiveRelationshipExecution) {
		t.Fatalf("agent release over a managed child: %v", err)
	}
}

// executionOf and releaseOf answer the current record of either side: a closed execution is not current, and the last row of the chain is.
func TestExecutionOfPicksCurrent(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	ctx := context.Background()
	q := k.s.Q(ctx)
	if _, found, err := executionOf(ctx, q, "rp", "A"); err != nil || found {
		t.Fatalf("a node nobody holds: found=%v %v", found, err)
	}
	// the relationship side
	rel := k.mustRelease("rp", "I")
	cur, found, err := executionOf(ctx, q, "rp", "I")
	if err != nil || !found || cur.Kind != ExecutionRelationship || cur.RelationshipID != rel.RelationshipID || cur.ManifestDigest != rel.ManifestDigest || cur.Generation != 1 {
		t.Fatalf("relationship side: %+v %v %v", cur, found, err)
	}
	if r, found, err := releaseOf(ctx, q, "rp", "I", rel.ManifestDigest); err != nil || !found || r.Kind != ExecutionRelationship || r.ManagedRequestID != rel.RequestID || r.DecidedAt == "" {
		t.Fatalf("release of the relationship side: %+v %v %v", r, found, err)
	}
	if _, found, _ := releaseOf(ctx, q, "rp", "I", dig("another manifest")); found {
		t.Fatal("a release of another manifest was found")
	}
	// the agent side: the release row is current, then a later row of the chain is
	agent := k.mustReleaseAgent("rp", "A")
	cur, found, err = executionOf(ctx, q, "rp", "A")
	if err != nil || !found || cur.Kind != ExecutionAgent || cur.Agent.ExecutionID != agent.ExecutionID || cur.ManifestDigest != agent.ManifestDigest || cur.Epoch != 0 {
		t.Fatalf("agent side: %+v %v %v", cur, found, err)
	}
	rf, found, err := releaseOf(ctx, q, "rp", "A", agent.ManifestDigest)
	if err != nil || !found || rf.Kind != ExecutionAgent || rf.Agent.ExecutionID != agent.ExecutionID || rf.DecidedAt != agent.DecidedAt {
		t.Fatalf("release of the agent side: %+v %v %v", rf, found, err)
	}
	k.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at, supersedes_execution_id) VALUES ('tier-1','rp','A',2,'tier_step',?,'executor','executor-t1','parent',0,?,?)",
		agent.ManifestDigest, k.clock(), agent.ExecutionID)
	cur, found, err = executionOf(ctx, q, "rp", "A")
	if err != nil || !found || cur.Agent.ExecutionID != "tier-1" || cur.Agent.Tier != "executor-t1" {
		t.Fatalf("after a tier step the last row is current: %+v %v %v", cur, found, err)
	}
	// the release record stays the initial row
	if rf, found, err = releaseOf(ctx, q, "rp", "A", agent.ManifestDigest); err != nil || !found || rf.Agent.ExecutionID != agent.ExecutionID {
		t.Fatalf("release of the chain: %+v %v %v", rf, found, err)
	}
	// a closure ends the last row: the node has no current execution, and the superseded row is not current again
	k.exec("INSERT INTO dag_agent_closures (subject_kind, subject_id, state, slot_released, actor_task_id, coordinator_epoch, reason, closed_at) VALUES ('execution','tier-1','escalated',0,'parent',0,'tiers exhausted',?)", k.clock())
	if cur, found, err = executionOf(ctx, q, "rp", "A"); err != nil || found {
		t.Fatalf("a closed execution is current: %+v %v %v", cur, found, err)
	}
	if _, found, err := releaseOf(ctx, q, "rp", "A", agent.ManifestDigest); err != nil || found {
		t.Fatalf("the release of a closed execution is current: %v %v", found, err)
	}
	// a store that holds both kinds as current is read as neither
	k.foreignRelationship("CRW-A")
	k.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('rp','A','rel-foreign-CRW-A',1,?,'initial','req-x')", agent.ManifestDigest)
	k.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at, supersedes_execution_id) VALUES ('again-1','rp','A',3,'correction',?,'executor','executor','parent',0,?,'tier-1')",
		agent.ManifestDigest, k.clock())
	if _, found, err := executionOf(ctx, q, "rp", "A"); found || err == nil || !strings.Contains(err.Error(), CrossKindExecution) {
		t.Fatalf("both kinds current: found=%v %v", found, err)
	}
}

// A closed execution leaves the node to be released again: the new row supersedes the closed one, with the next attempt, and the chain stays one line.
func TestAgentReleaseAfterClosureChainsTheNextAttempt(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	first := k.mustReleaseAgent("rp", "A")
	k.agentCloses("rp", "A", first.ExecutionID, "cancelled")
	second := k.mustReleaseAgent("rp", "A")
	if second.Replayed || second.Attempt != 2 || second.ExecutionID == first.ExecutionID || second.SlotID == first.SlotID {
		t.Fatalf("second release = %+v (first %+v)", second, first)
	}
	var supersedes string
	if err := k.s.DB.QueryRow("SELECT supersedes_execution_id FROM dag_agent_executions WHERE execution_id = ?", second.ExecutionID).Scan(&supersedes); err != nil || supersedes != first.ExecutionID {
		t.Fatalf("supersedes = %q (%v), want %s", supersedes, err, first.ExecutionID)
	}
	cur, found, err := executionOf(context.Background(), k.s.Q(context.Background()), "rp", "A")
	if err != nil || !found || cur.Agent.ExecutionID != second.ExecutionID {
		t.Fatalf("current = %+v %v %v", cur, found, err)
	}
}

// d1 (CRW-1003): two agent nodes of one issue scope are both released. The registry refuses a second owner of an issue scope for a relationship (duplicate_scope_owner, and
// scope_bindings_one_live_owner and scope_links_one_live_edge under it); the agent surface creates no relationship and no scope binding, so nothing is refused and no scope row appears.
func TestAgentSecondPacketNoScopeOwnerRefusal(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	k.putPlan("rp", 0, "rp-r1", issueNode("P1", dag.NodeNonPR, "CRW-F"), issueNode("P2", dag.NodeNonPR, "CRW-F"), issueNode("Q", dag.NodeNonPR, "CRW-G"))
	// the constraints a managed second owner of the issue meets are the shipped schema's, and they are there
	for _, index := range []string{"scope_bindings_one_live_owner", "scope_links_one_live_edge", "managed_start_one_pending_issue"} {
		if k.count("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", index) != 1 {
			t.Fatalf("the shipped index %s is not in the store", index)
		}
	}
	scopes := k.count("SELECT COUNT(*) FROM scope_bindings")
	links := k.count("SELECT COUNT(*) FROM scope_links")
	first := k.mustReleaseAgent("rp", "P1")
	second, err := k.releaseAgent("rp", "P2")
	if err != nil {
		t.Fatalf("the second node of the issue: %v", err)
	}
	if first.ExecutionID == second.ExecutionID || first.SlotID == second.SlotID || first.SlotID == "" || second.SlotID == "" {
		t.Fatalf("first %+v second %+v", first, second)
	}
	if got := k.count("SELECT COUNT(*) FROM scope_bindings"); got != scopes {
		t.Fatalf("scope_bindings %d -> %d", scopes, got)
	}
	if got := k.count("SELECT COUNT(*) FROM scope_links"); got != links {
		t.Fatalf("scope_links %d -> %d", links, got)
	}
	if k.count("SELECT COUNT(*) FROM relationships") != 0 || k.count("SELECT COUNT(*) FROM dag_agent_executions") != 2 {
		t.Fatalf("rows: %v", k.agentRowCounts())
	}
	// and a repeat of either answers its own execution
	if again := k.mustReleaseAgent("rp", "P2"); !again.Replayed || again.ExecutionID != second.ExecutionID {
		t.Fatalf("repeat = %+v", again)
	}
}

// d2 (CRW-1003): two agent nodes of one issue are released in any order. managed_start_one_pending_issue allows one pending managed start per issue, so managed packets start in order; the agent
// surface writes no managed start request, so the limit cannot bite, and the second node does not wait for the first.
func TestAgentSecondPacketNoPendingManagedStartLimit(t *testing.T) {
	t.Parallel()
	for _, order := range [][]string{{"P1", "P2", "P3"}, {"P3", "P1", "P2"}, {"P2", "P3", "P1"}} {
		k := newReleaseKit(t)
		k.putPlan("rp", 0, "rp-r1", issueNode("P1", dag.NodeNonPR, "CRW-F"), issueNode("P2", dag.NodeNonPR, "CRW-F"), issueNode("P3", dag.NodeNonPR, "CRW-F"))
		for _, node := range order {
			if _, err := k.releaseAgent("rp", node); err != nil {
				t.Fatalf("order %v: release of %s: %v", order, node, err)
			}
		}
		if k.count("SELECT COUNT(*) FROM managed_start_requests") != 0 {
			t.Fatalf("order %v: a managed start request was written", order)
		}
		if k.count("SELECT COUNT(*) FROM dag_agent_executions WHERE kind = 'initial'") != 3 || k.count("SELECT COUNT(*) FROM execution_slots WHERE state = 'held'") != 3 {
			t.Fatalf("order %v: rows %v", order, k.agentRowCounts())
		}
		// the limit itself is the schema's: a second pending managed start of one issue is refused by the shipped index, which is why the agent surface must not write one
		k.managedRow("m-1", "CRW-F", "reserved", "")
		if err := tryExec(k.s.DB, "INSERT INTO managed_start_requests (request_id, issue_key, request_fingerprint, fingerprint_version, workspace, marker_root, socket_identity, create_request_id, dispatch_request_id, state, revision, receipt_status, created_at, updated_at) VALUES ('m-2','CRW-F','fp','1','ws','mr','sock','c2','d2','reserved',0,NULL,'t','t')"); err == nil {
			t.Fatalf("order %v: a second pending managed start of one issue was accepted", order)
		}
	}
}

// Fix round 2: an agent node is judged without the issue-level ownership of a managed start. A managed start in flight for the issue that no release of this node made, or an open relationship of
// another node of the issue, holds that other node, not this one: the node-level exclusion (live_relationship_execution) is what keeps a node to one kind of execution. The judgement before the
// transaction and the one inside it apply the same rule.
func TestAgentReleaseBesideManagedRecordsOfTheIssue(t *testing.T) {
	t.Parallel()
	plan := func(k *releaseKit) {
		k.putPlan("rp", 0, "rp-r1", issueNode("P1", dag.NodeNonPR, "CRW-F"), issueNode("P2", dag.NodeNonPR, "CRW-F"), issueNode("P3", dag.NodeNonPR, "CRW-F"))
	}
	t.Run("a pending managed start of the issue", func(t *testing.T) {
		k := newReleaseKit(t)
		plan(k)
		k.mustReleaseAgent("rp", "P1")
		k.managedRow("m-unrelated", "CRW-F", "reserved", "")
		if _, err := k.releaseAgent("rp", "P2"); err != nil {
			t.Fatalf("agent P2 beside a pending managed start of the issue: %v", err)
		}
		// the managed surface still meets the limit
		if r, ok := k.read("rp").find("P3"); !ok || r.Reason != SkipAlreadyOwned {
			t.Fatalf("the reading of P3 = %+v, want %s", r, SkipAlreadyOwned)
		}
	})
	t.Run("an open relationship of another node of the issue", func(t *testing.T) {
		k := newReleaseKit(t)
		plan(k)
		if res := k.mustRelease("rp", "P1"); !res.Bound || res.RelationshipID == "" {
			t.Fatalf("managed P1 = %+v", res)
		}
		res, err := k.releaseAgent("rp", "P2")
		if err != nil {
			t.Fatalf("agent P2 beside the relationship of P1: %v", err)
		}
		if res.Replayed || res.ExecutionID == "" || res.SlotID == "" {
			t.Fatalf("agent P2 = %+v", res)
		}
		// P1 itself is still not given to an agent
		if _, err := k.releaseAgent("rp", "P1"); err == nil || !strings.Contains(err.Error(), LiveRelationshipExecution) {
			t.Fatalf("agent P1 over its relationship: %v", err)
		}
		// and a managed release of P3 is still refused for the issue's open relationship
		if _, err := k.release("rp", "P3"); err == nil {
			t.Fatal("managed P3 was released beside the open relationship of P1")
		}
	})
}

// Fix round 2: a release refused at the reservation leaves nothing of the release: no input manifest, no execution row, no slot. The conflict row the refusal records is the refusal's record and
// stays. Both refusals of the reservation are covered: the ceilings are full, and another parent holds the node's slot subject.
func TestAgentReleaseRefusedReservationWritesNoRelease(t *testing.T) {
	t.Parallel()
	t.Run("capacity", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.holdSlots(6)
		before := k.rows()
		_, err := k.releaseAgent("rp", "A")
		if refusalReason(err) != "capacity_exhausted" {
			t.Fatalf("release with the ceilings full: %v", err)
		}
		after := k.rows()
		if after.manifests != before.manifests || after.slots != before.slots || k.count("SELECT COUNT(*) FROM dag_agent_executions") != 0 {
			t.Fatalf("a refused release left rows: before %+v after %+v, %d executions", before, after, k.count("SELECT COUNT(*) FROM dag_agent_executions"))
		}
		if after.conflicts != before.conflicts+1 {
			t.Fatalf("the refusal recorded %d conflict rows", after.conflicts-before.conflicts)
		}
	})
	t.Run("a foreign slot", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		ctx := context.Background()
		now := k.clock()
		if err := storeseed.InsertScopeBinding(ctx, k.s, store.ScopeBindingsRow{BindingID: "bind-other", Role: "parent", ScopeKind: "project", ScopeKey: "OTHER", TaskID: "other-parent", HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := (&capacity.Capacity{Store: k.s, Now: k.clock}).Reserve(ctx, capacity.Reservation{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey("rp", "A"), ParentTask: "other-parent", Project: "OTHER", ReservedBy: "other-parent"}); err != nil {
			t.Fatal(err)
		}
		before := k.rows()
		_, err := k.releaseAgent("rp", "A")
		var refused *store.RefusedError
		if !errors.As(err, &refused) {
			t.Fatalf("release under another parent's slot: %v", err)
		}
		after := k.rows()
		if after.manifests != before.manifests || after.slots != before.slots || k.count("SELECT COUNT(*) FROM dag_agent_executions") != 0 {
			t.Fatalf("a refused release left rows: before %+v after %+v", before, after)
		}
	})
}

func tryExec(db *sql.DB, statement string) error {
	_, err := db.Exec(statement)
	return err
}

// regionPlan is a plan of three implementation nodes of one issue: the nodes are released side by side, and the places they take are settled by their declarations.
func regionPlan(k *releaseKit, plan string) {
	k.t.Helper()
	k.putPlan(plan, 0, plan+"-r1", issueNode("X1", dag.NodeImplementation, "CRW-Z"), issueNode("X2", dag.NodeImplementation, "CRW-Z"), issueNode("X3", dag.NodeImplementation, "CRW-Z"))
}

func (k *releaseKit) declareRegions(plan, node string, regions ...Region) {
	k.t.Helper()
	for i := range regions {
		regions[i].Repository = "owner/repo"
	}
	if _, err := k.sched.DeclareRegions(context.Background(), plan, node, "parent", regions); err != nil {
		k.t.Fatalf("declare %s: %v", node, err)
	}
}

// d4 (CRW-1003): the owner of a place is judged per place and not per pair of nodes. Two nodes edit one file; a third holds the directory over it exclusive. The pair has no owner among
// themselves, but the directory's owner owns the place: the broad owner recorded by a tree covers the file key the pair made, and the release is not refused as an overlap with no owner. Without the
// third node the same pair is refused.
func TestAgentRegionOwnerBroadTreeKeyCoversPairFileKeys(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	regionPlan(k, "owned")
	regionPlan(k, "bare")
	for _, plan := range []string{"owned", "bare"} {
		k.declareRegions(plan, "X1", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeLocal})
		k.declareRegions(plan, "X2", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeLocal})
	}
	k.declareRegions("owned", "X3", Region{Path: "a", Kind: "tree", Change: "edit", Grade: GradeExclusive})
	if res, err := k.releaseAgent("owned", "X1"); err != nil || res.ExecutionID == "" {
		t.Fatalf("a place under a directory its owner holds: %+v %v", res, err)
	}
	// the other member of the pair is released too (the owner itself waits behind the nodes that overlap it: an exclusive overlap defers the later candidate, which is the scheduler's rule and not the owner rule's)
	if _, err := k.releaseAgent("owned", "X2"); err != nil {
		t.Fatalf("release of X2: %v", err)
	}
	// no node holds the place: the pair is refused, naming the place and the closed reason
	before := allRows(t, k.s.DB)
	_, err := k.releaseAgent("bare", "X1")
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), RegionOwnerMissing) || !strings.Contains(err.Error(), "a/x.go") {
		t.Fatalf("a shared place with no owner: %v", err)
	}
	if !reflect.DeepEqual(before, allRows(t, k.s.DB)) {
		t.Fatal("the refused release wrote rows")
	}
	// the first declaration of a node that owns the place settles it
	k.declareRegions("bare", "X3", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeExclusive})
	if _, err := k.releaseAgent("bare", "X1"); err != nil {
		t.Fatalf("after an owner declared the place: %v", err)
	}
	// two owners are refused as well: one place has one owner (judged on the declarations, apart from the scheduler's own deferral of overlapping candidates)
	k.declareRegions("bare", "X2", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeExclusive})
	snap := k.snapshot("bare")
	x2, _ := nodeOf(snap, "X2")
	err = agentRegionOwners(context.Background(), k.s.Q(context.Background()), "bare", snap, x2)
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), RegionOwnerAmbiguous) || !strings.Contains(err.Error(), "X2") || !strings.Contains(err.Error(), "X3") {
		t.Fatalf("a place with two owners: %v", err)
	}
	// nodes of another issue are not judged against each other
	k.putPlan("other", 0, "other-r1", issueNode("Y1", dag.NodeImplementation, "CRW-Y1"), issueNode("Y2", dag.NodeImplementation, "CRW-Y2"))
	k.declareRegions("other", "Y1", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeLocal})
	k.declareRegions("other", "Y2", Region{Path: "a/x.go", Kind: "file", Change: "edit", Grade: GradeLocal})
	if _, err := k.releaseAgent("other", "Y1"); err != nil {
		t.Fatalf("nodes of two issues: %v", err)
	}
}

// P3 (CRW-1003): the key of a whole-repository place carries a kind suffix, so a place the declarer stated as the whole repository, a directory and a file are three places, whatever their paths read like,
// and a hold of the whole repository is an owner of every place of it.
func TestAgentRegionKeyWholeRepoCarriesKindSuffix(t *testing.T) {
	t.Parallel()
	whole := Region{Repository: "owner/repo", Path: ".", Kind: "tree", Exclusive: true}
	other := Region{Repository: "owner/repo", Path: "*", Kind: "tree"}
	both, ok := sharedPlace(whole, Region{Repository: "owner/repo", Path: ".", Kind: "tree", Exclusive: true})
	if !ok || both.Kind != placeRepo || !strings.HasSuffix(both.key(), "\x00"+placeRepo) {
		t.Fatalf("the place two whole-repository holds share: %+v key %q", both, both.key())
	}
	tree := regionPlace{Repository: "owner/repo", Path: "*", Kind: placeTree}
	file := regionPlace{Repository: "owner/repo", Path: "*", Kind: placeFile}
	if both.key() == tree.key() || both.key() == file.key() || tree.key() == file.key() {
		t.Fatalf("keys collide: %q %q %q", both.key(), tree.key(), file.key())
	}
	for _, p := range []regionPlace{both, tree, file, {Repository: "owner/repo", Path: "a.go", Kind: placeSymbol, Symbol: "F"}} {
		if !strings.Contains(p.key(), "\x00"+p.Kind) {
			t.Errorf("key %q of %+v carries no kind", p.key(), p)
		}
	}
	// a hold of the whole repository owns every place of it, so a file shared with another node has an owner
	findings := regionOwnerFindings("N1", []Region{whole}, map[string][]Region{"N2": {{Repository: "owner/repo", Path: "a.go", Kind: "file"}}})
	if len(findings) != 0 {
		t.Fatalf("a whole-repository hold is an owner of the file: %+v", findings)
	}
	// two holds of the whole repository are two owners of one place, and the finding carries the suffixed key
	findings = regionOwnerFindings("N1", []Region{whole}, map[string][]Region{"N2": {{Repository: "owner/repo", Path: ".", Kind: "tree", Exclusive: true}}})
	if len(findings) != 1 || findings[0].Reason != RegionOwnerAmbiguous || findings[0].Place.Kind != placeRepo || !strings.HasSuffix(findings[0].Place.key(), "\x00repo") {
		t.Fatalf("two whole-repository holds: %+v", findings)
	}
	// a directory spelled "*" is not the whole repository
	findings = regionOwnerFindings("N1", []Region{other}, map[string][]Region{"N2": {{Repository: "owner/repo", Path: "*", Kind: "tree"}}})
	if len(findings) != 1 || findings[0].Place.Kind != placeTree || findings[0].Place.key() == both.key() {
		t.Fatalf("the directory named *: %+v", findings)
	}
}

// P3 (CRW-1003): the slot is reserved from the FROZEN release manifest and not from the live node: the node the slot is for is the one the stored manifest was built for. A plan revision
// that retires the node after the manifest was built does not move the reservation, and a manifest that is not stored, or is of another node, reserves nothing (fail closed).
func TestAgentReservationUsesFrozenReleaseManifest(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	k.putPlan("fz", 0, "fz-r1", addRelNode("A", dag.NodeNonPR), addRelNode("Z", dag.NodeNonPR))
	ctx := context.Background()
	digest := k.putManifest(k.snapshot("fz"), "A", []any{})
	// the plan moves on: node A is no longer live
	k.putPlan("fz", int(k.snapshot("fz").Revision), "fz-retire-a", doc{"op": dag.OpRetireNode, "node_id": "A"})
	if _, ok := nodeOf(k.snapshot("fz"), "A"); ok {
		t.Fatal("node A is still live")
	}
	reserve := func(node, manifest string) error {
		var out error
		if err := k.s.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
			out = k.sched.reserveFrozen(txCtx, k.s.Q(txCtx), "P-TEST", "fz", node, manifest, "parent", nil)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// not stored, or of another node: nothing is reserved
	if err := reserve("A", dig("never stored")); refusalReason(err) != "revision_mismatch" {
		t.Errorf("an unstored manifest: %v", err)
	}
	if err := reserve("Z", digest); refusalReason(err) != "revision_mismatch" {
		t.Errorf("a manifest of another node: %v", err)
	}
	if n := k.count("SELECT COUNT(*) FROM execution_slots"); n != 0 {
		t.Fatalf("%d slots after refused reservations", n)
	}
	// the stored manifest says which node the slot is for, although the live plan no longer has it
	if err := reserve("A", digest); err != nil {
		t.Fatalf("the reservation from the frozen manifest: %v", err)
	}
	if k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = 'fz/A' AND state = 'held'") != 1 {
		t.Fatal("the slot is not held under the node of the frozen manifest")
	}
	var detail sql.NullString
	if err := k.s.DB.QueryRow("SELECT detail FROM execution_slots WHERE subject_key = 'fz/A'").Scan(&detail); err != nil || detail.String != digest {
		t.Fatalf("the slot records the manifest %q (%v), want %s", detail.String, err, digest)
	}
	// a manifest that does not read back as its digest is not a frozen record either
	k.exec("UPDATE dag_input_manifests SET body_json = replace(body_json, '\"A\"', '\"Z\"') WHERE manifest_digest = ?", digest)
	if err := reserve("Z", digest); refusalReason(err) != "revision_mismatch" {
		t.Errorf("a tampered manifest: %v", err)
	}
}

// Both releases of a plan share one judgement, and a decode of the agent surface's request refuses the endpoints of a managed start.
func TestAgentReleaseRequestDecoding(t *testing.T) {
	t.Parallel()
	good := `{"schema":"dag-release-request/1","criteria":[{"id":"c1","title":"t","required":true}],"rule_version":{"skills_digest":"` + dig("s") + `","model":"m","effort":"e","prompt_template":"p","relay_build":"b"}}`
	if _, err := DecodeAgentReleaseRequest([]byte(good)); err != nil {
		t.Fatalf("a request with no endpoints: %v", err)
	}
	for name, document := range map[string]string{
		"a child":             strings.Replace(good, `"criteria"`, `"child":{"host_id":"h","title":"c","settings":{}},"criteria"`, 1),
		"a parent":            strings.Replace(good, `"criteria"`, `"parent":{"host_id":"h","settings":{}},"criteria"`, 1),
		"allowed recipients":  strings.Replace(good, `"criteria"`, `"allowed_recipients":["x"],"criteria"`, 1),
		"no criteria":         strings.Replace(good, `"criteria":[{"id":"c1","title":"t","required":true}]`, `"criteria":[]`, 1),
		"another schema":      strings.Replace(good, `dag-release-request/1`, `dag-release-request/2`, 1),
		"an unknown field":    strings.Replace(good, `"criteria"`, `"surprise":1,"criteria"`, 1),
		"a trailing document": good + good,
	} {
		if _, err := DecodeAgentReleaseRequest([]byte(document)); refusalReason(err) != "malformed_receipt" {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := DecodeReleaseRequest([]byte(good)); refusalReason(err) != "malformed_receipt" {
		t.Errorf("the managed release still wants its endpoints: %v", err)
	}
}

// dag-release --surface agent through the built binary: an agent execution is released with the explicit --state and --socket every dag-release has, with no marker root; the answer is the
// dag-release/1 document of the agent surface, and the relationship surface keeps asking for its marker root.
func TestCLIAgentReleaseSurface(t *testing.T) {
	t.Parallel()
	state := filepath.Join(t.TempDir(), "state")
	socket := filepath.Join(t.TempDir(), "app.sock")
	f := newFixtureOn(t, filepath.Join(state, "relay.sqlite3"), socket)
	releasePlan(f, "rp")
	f.projectParent()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	request := `{"schema":"dag-release-request/1","criteria":[{"id":"c1","title":"preserve the contract","required":true}],"rule_version":{"skills_digest":"` + dig("skills") +
		`","model":"gpt-5","effort":"medium","prompt_template":"template-1","relay_build":"test-build"}}`
	agent := func(extra ...string) []string {
		return append([]string{"--socket", socket, "dag-release", "--surface", "agent", "--plan", "rp", "--node", "A", "--actor", "parent", "--request", request}, extra...)
	}
	out, code := crw(t, state, agent("--run-id", "run-9", "--worktree", "/wt/a", "--tier", "executor-t1")...)
	answer := parseOut(t, out)
	if code != 0 || answer["ok"] != true || answer["surface"] != "agent" || answer["schema"] != "dag-release/1" || answer["kind"] != "initial" || answer["tier"] != "executor-t1" || answer["role"] != "executor" ||
		answer["replayed"] != false || answer["execution_id"] == "" || answer["slot_id"] == "" || answer["actor"] != "parent" {
		t.Fatalf("exit %d\n%s", code, out)
	}
	again, code := crw(t, state, agent()...)
	if m := parseOut(t, again); code != 0 || m["replayed"] != true || m["execution_id"] != answer["execution_id"] {
		t.Fatalf("a repeat: exit %d\n%s", code, again)
	}
	// a managed child on the same node is refused, with the closed reason in the detail
	markerRoot := t.TempDir()
	out, code = crw(t, state, "--socket", socket, "dag-release", "--plan", "rp", "--node", "A", "--actor", "parent", "--request", request, "--marker-root", markerRoot)
	if m := parseOut(t, out); code != 2 || m["reason"] != "malformed_receipt" {
		t.Fatalf("the relationship surface reads the managed request: exit %d\n%s", code, out)
	}
	// the relationship surface needs its marker root, and the agent-only options belong to the agent surface
	if out, code := crw(t, state, "--socket", socket, "dag-release", "--plan", "rp", "--node", "I", "--actor", "parent", "--request", request); code != 4 {
		t.Fatalf("a managed release without a marker root: exit %d\n%s", code, out)
	}
	if out, code := crw(t, state, "--socket", socket, "dag-release", "--plan", "rp", "--node", "I", "--actor", "parent", "--request", request, "--marker-root", markerRoot, "--run-id", "r"); code != 4 {
		t.Fatalf("an agent option on the relationship surface: exit %d\n%s", code, out)
	}
	// the managed endpoints do not belong to the agent surface
	withChild := strings.Replace(request, `"criteria"`, `"child":{"host_id":"h","title":"c","settings":{}},"criteria"`, 1)
	out, code = crw(t, state, "--socket", socket, "dag-release", "--surface", "agent", "--plan", "rp", "--node", "I", "--actor", "parent", "--request", withChild)
	if m := parseOut(t, out); code != 2 || m["reason"] != "malformed_receipt" {
		t.Fatalf("an agent request with a child: exit %d\n%s", code, out)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(state, "relay.sqlite3")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for table, want := range map[string]int{"dag_agent_executions": 1, "dag_releases": 0, "dag_release_requests": 0, "managed_start_requests": 0, "dag_node_executions": 0} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s holds %d rows (%v), want %d", table, n, err, want)
		}
	}
}
