package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1214 (R06): the readers that answer a question about a NODE read both sides of it. An agent node's release, executions and acceptance are rows of dag_agent_executions, dag_agent_acceptances and
// dag_agent_integration_stages (R04, R05); nothing of it is in a frozen table, so a reader of the frozen tables saw an agent node as one nobody had touched. Every fact here is made by the production
// path of the agent: ReleaseAgent, Accept (the agent path), IntegrateBatch and ObserveIntegration over a real repository; nothing is seeded into a zone table.

// progressNode is one node of the plan's dag-progress document.
func progressNode(t *testing.T, p Progress, node string) NodeProgress {
	t.Helper()
	for _, n := range p.Nodes {
		if n.NodeID == node {
			return n
		}
	}
	t.Fatalf("progress holds no node %s", node)
	return NodeProgress{}
}

// readProgress is the dag-progress projection of plan "g".
func (k *agentKit) readProgress() Progress {
	k.t.Helper()
	p, err := k.sched.ReadProgress(context.Background(), "g")
	if err != nil {
		k.t.Fatalf("progress: %v", err)
	}
	return p
}

// pinnedEdge adds an artifact edge that pins the accepted code head, from one node to another, on the repository of the kit.
func (k *agentKit) pinnedEdge(id, from, to string) {
	k.t.Helper()
	k.putPlan("g", int(k.snapshot("g").Revision), "g-"+id, addEdge(id, from, to, dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": k.repo.path, "target_base_ref": "dev"}))
}

// edgeStatusOf reads one edge of plan "g" the way the scheduler's readers do.
func (k *agentKit) edgeStatusOf(edgeID string) EdgeStatus {
	k.t.Helper()
	ctx := context.Background()
	q := k.sched.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, "g", 0)
	if err != nil {
		k.t.Fatalf("snapshot: %v", err)
	}
	for _, e := range snap.Edges {
		if e.EdgeID == edgeID {
			st, err := k.sched.edgeStatus(ctx, q, "g", snap, e)
			if err != nil {
				k.t.Fatalf("edge %s: %v", edgeID, err)
			}
			return st
		}
	}
	k.t.Fatalf("no edge %s", edgeID)
	return EdgeStatus{}
}

// An agent release is the node's release in the reading dag-progress prints: the node is running and holds a slot, and its execution is among the node's executions, with the execution row it is.
func TestAgentReleaseVisibleToProgress(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "r", relationship: true, files: fileOf("r")}}
	k := newAgentKit(t, specs...)
	released := k.releases["a"]
	p := k.readProgress()
	a := progressNode(t, p, "a")
	if a.Stage != StageRunning || a.Reason != SkipAlreadyOwned || !a.HoldsSlot {
		t.Fatalf("the agent node reads stage %s reason %s holds_slot %v, want running, %s, a held slot", a.Stage, a.Reason, a.HoldsSlot, SkipAlreadyOwned)
	}
	if len(a.Links.Executions) != 1 || a.Links.Executions[0].ExecutionID != released.ExecutionID || a.Links.Executions[0].Kind != AgentInitial || a.Links.Executions[0].Generation != 1 {
		t.Fatalf("the agent node's executions are %+v, want its execution %s (attempt 1, initial)", a.Links.Executions, released.ExecutionID)
	}
	// a correction of the agent chain is another execution of the node
	k.handover("a", "parent")
	again := progressNode(t, k.readProgress(), "a")
	if len(again.Links.Executions) != 2 || again.Links.Executions[1].Kind != AgentParentHandoff {
		t.Fatalf("after a handover the executions are %+v, want two", again.Links.Executions)
	}
	// the printed document names the execution of an agent node and keeps the link of a relationship node as it was
	printed, err := pyjson.Encode(k.readProgress().body(), pyjson.Options{})
	if err != nil || !strings.Contains(string(printed), released.ExecutionID) {
		t.Fatalf("the printed document does not name execution %s (%v)", released.ExecutionID, err)
	}
	rel := progressNode(t, k.readProgress(), "r")
	for _, e := range rel.Links.Executions {
		if e.ExecutionID != "" {
			t.Fatalf("a relationship node's link names an agent execution: %+v", e)
		}
	}
	// the replay view carries the same node and facts, and the activity document the same stage
	snapshot, viewed, err := k.sched.ProgressSnapshotOf(context.Background(), k.sched.Store.Q(context.Background()), "g")
	if err != nil {
		t.Fatalf("progress snapshot: %v", err)
	}
	if rec, ok := snapshot.Records["a"]; !ok || len(rec.Facts.Links.Executions) != 2 || viewed.Digest == "" {
		t.Fatalf("the replay record of the agent node = %+v (%v)", snapshot.Records["a"], ok)
	}
	act, err := k.sched.ReadActivity(context.Background(), "g")
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	for _, n := range act.Nodes {
		if n.NodeID == "a" && n.Stage != StageRunning {
			t.Fatalf("the activity document reads the agent node in stage %s", n.Stage)
		}
	}
}

// An agent execution holds the node in the reading Ready makes and the slot it reserved counts against the capacity; the bundle candidates leave a released agent node out as they leave out a node a managed
// start owns, and a conflict sweep measures the head the agent works at.
func TestAgentReleaseVisibleToReadyAndCapacity(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b"), unreleased: true}, {name: "c", files: fileOf("c"), unreleased: true}}
	k := newAgentKit(t, specs...)
	ctx := context.Background()
	q := k.s.Q(ctx)
	reading := k.read("g")
	if a := reading.node("a"); a.State != StateRunning || a.Reason != SkipAlreadyOwned {
		t.Fatalf("a reads %+v", a)
	}
	if reading.Pass.Held < 1 {
		t.Fatalf("the slot the agent release reserved is not counted: held %d of %d", reading.Pass.Held, reading.Pass.Ceiling)
	}
	// capacity: the slots a node holds are the agent's too
	if slot, err := k.sched.slotOf(ctx, q, "g", "a"); err != nil || slot == "" {
		t.Fatalf("slotOf a = %q, %v", slot, err)
	}
	bundles, err := k.sched.BundleCandidates(ctx, q, "g", nil)
	if err != nil {
		t.Fatalf("bundle candidates: %v", err)
	}
	excluded := map[string]string{}
	for _, e := range bundles.Excluded {
		excluded[e.Node] = e.Reason
	}
	if excluded["a"] != BundleExcludedReleased {
		t.Fatalf("a, released to an agent, is %q in the bundle candidates, want %q (excluded: %v)", excluded["a"], BundleExcludedReleased, excluded)
	}
	if reason, ok := excluded["b"]; ok && reason == BundleExcludedReleased {
		t.Fatalf("b, which nobody released, is excluded as released")
	}
	// the node's open release and executions are the agent's: the managed recovery list does not name it
	open, err := OpenReleaseNodes(ctx, q, "g")
	if err != nil || open["a"] {
		t.Fatalf("OpenReleaseNodes = %v, %v: an agent execution is no managed recovery target", open, err)
	}
	executed, err := ExecutedNodes(ctx, q, "g")
	if err != nil || !executed["a"] || executed["b"] {
		t.Fatalf("ExecutedNodes = %v, %v: only the agent node a has executed", executed, err)
	}
	// the conflict sweep measures the head the agent works at: the commit of its worktree
	snap, _, err := dag.SnapshotAt(ctx, q, "g", 0)
	if err != nil {
		t.Fatal(err)
	}
	heads, err := k.sched.sweepHeads(ctx, q, "g", snap, k.repo.path, nil)
	if err != nil {
		t.Fatalf("sweep heads: %v", err)
	}
	var found bool
	for _, h := range heads {
		if h.Node == "a" {
			found = true
			if h.Head != k.heads["a"] || h.Source != HeadChildCheckout {
				t.Fatalf("the sweep measures a at %+v, want its worktree head %s", h, k.heads["a"])
			}
		}
	}
	if !found {
		t.Fatalf("the sweep names no head for the running agent node: %+v", heads)
	}
	// the base the sweep measures against is the one frozen in the manifest the agent was released under
	if _, _, found, err := k.sched.manifestBase(ctx, q, "g", "a"); err != nil || !found {
		t.Fatalf("manifestBase of the agent node: found %v, %v", found, err)
	}
}

// dag-release-close ends the managed start of a release. An agent release has none, so the recovery reader skips an agent row explicitly: the node is not listed, the close is refused with nothing written and
// the slot untouched, and the manifest an agent execution was released under is relied on, so a frozen copy of it is not taken away.
func TestReleaseRecoverySkipsAgentRows(t *testing.T) {
	t.Parallel()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	ctx := context.Background()
	q := k.s.Q(ctx)
	released := k.releases["a"]
	open, err := OpenReleaseNodes(ctx, q, "g")
	if err != nil || len(open) != 0 {
		t.Fatalf("OpenReleaseNodes = %v, %v, want none", open, err)
	}
	before := allRows(t, k.s.DB)
	res, err := k.sched.CloseRelease(ctx, "g", "a", "parent", released.ManifestDigest, "abandoned", ReleaseRequestID("g", "a", released.ManifestDigest))
	if err == nil || !strings.Contains(err.Error(), LiveAgentExecution) {
		t.Fatalf("CloseRelease of an agent node = %+v, %v; want a refusal naming %s", res, err, LiveAgentExecution)
	}
	if after := allRows(t, k.s.DB); len(after) != len(before) {
		t.Fatalf("the refused close wrote rows: %d before, %d after", len(before), len(after))
	}
	if state, _ := k.slotOf("a"); state != "held" {
		t.Fatalf("the slot of the agent node is %s after the refused close, want held", state)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_release_recoveries WHERE plan_id = 'g'"); got != 0 {
		t.Fatalf("%d recovery rows were written for an agent node", got)
	}
	// the manifest of an agent execution is bound: a frozen copy of it is relied on
	bound, err := agentManifestBound(ctx, q, released.ManifestDigest)
	if err != nil || !bound {
		t.Fatalf("agentManifestBound(%s) = %v, %v", released.ManifestDigest, bound, err)
	}
	if other, err := agentManifestBound(ctx, q, dig("no such manifest")); err != nil || other {
		t.Fatalf("agentManifestBound of an unknown manifest = %v, %v", other, err)
	}
}

// An agent acceptance is the node's acceptance in dag-progress: the node is accepted, the document names the acceptance, the cumulative count includes it, and the replay view carries it.
func TestAgentAcceptanceVisibleToProgress(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "n", nonPR: true}}
	k := newAgentKit(t, specs...)
	k.judged("a")
	k.mustAccept("a")
	acceptance := k.currentAcceptance("a").AcceptanceID
	p := k.readProgress()
	a := progressNode(t, p, "a")
	if a.Stage != StageAccepted || a.State != StateAccepted || a.Reason != DoneAccepted || a.AcceptanceID != acceptance {
		t.Fatalf("the accepted agent node reads stage %s state %s reason %s acceptance %q, want accepted/%s/%s", a.Stage, a.State, a.Reason, a.AcceptanceID, DoneAccepted, acceptance)
	}
	if p.Cumulative.Accepted.Nodes != 1 {
		t.Fatalf("the accepted measure is %+v, want the agent acceptance counted", p.Cumulative.Accepted)
	}
	// a node without a head (a non_pr node) is accepted as well, and its pull request link is none
	k.record(journalCall{role: "architect", node: "n", model: authorModel})
	k.record(journalCall{role: "reviewer", node: "n", model: reviewerModel, verdict: "verified", criteria: releaseCriteriaDigest()})
	if _, err := k.sched.Accept(context.Background(), "g", "n", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"}}); err != nil {
		t.Fatalf("accept n: %v", err)
	}
	if n := progressNode(t, k.readProgress(), "n"); n.State != StateAccepted || n.AcceptanceID == "" {
		t.Fatalf("the accepted non_pr agent node reads %+v", n)
	}
	if p := k.readProgress(); p.Cumulative.Accepted.Nodes != 2 {
		t.Fatalf("the accepted measure is %+v, want both agent acceptances", p.Cumulative.Accepted)
	}
	if a.Links.PullRequest != nil {
		t.Fatalf("an agent node has no pull request: %+v", a.Links.PullRequest)
	}
	snapshot, _, err := k.sched.ProgressSnapshotOf(context.Background(), k.sched.Store.Q(context.Background()), "g")
	if err != nil {
		t.Fatal(err)
	}
	if rec := snapshot.Records["a"]; rec.Facts.AcceptanceID != acceptance {
		t.Fatalf("the replay record carries acceptance %q, want %s", rec.Facts.AcceptanceID, acceptance)
	}
	// the acceptance is the same row through the union and through the reader that carries it as an Acceptance
	loaded, err := loadAcceptanceByID(context.Background(), k.s.Q(context.Background()), acceptance)
	if err != nil || loaded.AcceptanceID != acceptance || loaded.HeadSHA != k.heads["a"] {
		t.Fatalf("loadAcceptanceByID = %+v, %v", loaded, err)
	}
	// an execution of new work after the acceptance (a handover continues it) does not take the acceptance away; a correction does
	k.handover("a", "parent2")
	if a := progressNode(t, k.readProgress(), "a"); a.State != StateAccepted {
		t.Fatalf("a handover of the accepted node reads %s, want it still accepted (the new orchestrator continues the work)", a.State)
	}
}

// The successor of an agent node over an artifact edge that pins the accepted head becomes ready when the agent acceptance stands, and waits before it. An artifact edge out of an agent node that is not a code
// pin has no list of artifacts to hand over (an agent acceptance names a commit): the successor reads it as an input that is missing.
func TestAgentAcceptanceVisibleToReady(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b"), unreleased: true}, {name: "n", nonPR: true}}
	k := newAgentKit(t, specs...)
	k.pinnedEdge("ab", "a", "b")
	if st := k.edgeStatusOf("ab"); st.Satisfied {
		t.Fatalf("the edge is satisfied before a is accepted: %+v", st)
	}
	if b := k.read("g").node("b"); b.Disposition != DispWait {
		t.Fatalf("b reads %+v before its predecessor is accepted, want it waiting", b)
	}
	k.judged("a")
	res := k.mustAccept("a")
	st := k.edgeStatusOf("ab")
	if !st.Satisfied || st.AcceptanceID != res.AcceptanceID || st.Since == "" {
		t.Fatalf("the edge after the acceptance = %+v, want it satisfied by acceptance %s", st, res.AcceptanceID)
	}
	b := k.read("g").node("b")
	if b.Disposition != DispReady || b.State != StateReady {
		t.Fatalf("b reads %+v after its predecessor's agent acceptance, want it ready", b)
	}
	// an artifact edge out of a node that has no head is not a code pin: the accepted agent node hands over no list of artifacts
	k.putPlan("g", int(k.snapshot("g").Revision), "g-nb", addEdge("nb", "n", "b", dag.EdgeArtifactVerified, nil))
	k.record(journalCall{role: "architect", node: "n", model: authorModel})
	k.record(journalCall{role: "reviewer", node: "n", model: reviewerModel, verdict: "verified", criteria: releaseCriteriaDigest()})
	if _, err := k.sched.Accept(context.Background(), "g", "n", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"}}); err != nil {
		t.Fatalf("accept n: %v", err)
	}
	if st := k.edgeStatusOf("nb"); !st.Satisfied {
		t.Fatalf("the unpinned edge out of the accepted node n = %+v, want it satisfied", st)
	}
	if b := k.read("g").node("b"); b.Disposition != DispBlocked || b.Reason != BlockedInputMissing {
		t.Fatalf("b reads %+v over an artifact edge out of an agent node, want it blocked as %s", b, BlockedInputMissing)
	}
}

// An integrated edge out of an agent node is satisfied by the three facts of its landing, each written by the agent's own path: the batch moved the integration ref and marked the acceptance (the agent marked
// row), and the relay observed the head contained in the branch the batch moved. It waits before them.
func TestAgentIntegratedEdgeSatisfied(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b"), unreleased: true}}
	k := newAgentKit(t, specs...)
	k.putPlan("g", int(k.snapshot("g").Revision), "g-ab", addEdge("ab", "a", "b", dag.EdgeIntegrated, doc{"target_repository": k.repo.path, "target_base_ref": "dev"}))
	k.judged("a")
	k.mustAccept("a")
	if st := k.edgeStatusOf("ab"); st.Satisfied || st.Reason != WaitEdge("ab") {
		t.Fatalf("the integrated edge after the acceptance and before the integration = %+v, want it waiting", st)
	}
	res, err := k.integrate(t)
	if err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	if countAgentStage(k.agentStages(), "marked", "a") != 1 {
		t.Fatalf("the batch left no agent marked row: %+v", k.agentStages())
	}
	if st := k.edgeStatusOf("ab"); st.Satisfied {
		t.Fatalf("the integrated edge is satisfied by the ref move and the mark alone: %+v", st)
	}
	obs, err := k.observe("a")
	if err != nil || !obs.Integrated {
		t.Fatalf("observe: %+v, %v", obs, err)
	}
	st := k.edgeStatusOf("ab")
	if !st.Satisfied || st.ObservationID == "" || st.AcceptanceID != k.currentAcceptance("a").AcceptanceID {
		t.Fatalf("the integrated edge after the observation = %+v, want it satisfied by the agent acceptance and the observation", st)
	}
	if b := k.read("g").node("b"); b.Disposition != DispReady {
		t.Fatalf("b reads %+v after a landed, want it ready", b)
	}
	// what the successor would consume is the merge commit the batch recorded, not the tip the observation read
	landed, err := landedOf(context.Background(), k.s.Q(context.Background()), st.ObservationID)
	if err != nil || landed != res.Merged[0].MergeCommit {
		t.Fatalf("landedOf = %q, %v; want the batch's merge commit %s", landed, err, res.Merged[0].MergeCommit)
	}
}

// The integration state of an agent node is dag-progress's: the node is integrated once its head is in every target it has to land in with the agent mark, and not before, and the stage, the count and the
// reason say so. Each issue's integration is visible to the plan's progress (hierarchy.perIssueVisible).
func TestAgentNodeIntegratedVisibleToProgress(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	res, err := k.integrate(t)
	if err != nil || len(res.Merged) != 2 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	if a := progressNode(t, k.readProgress(), "a"); a.Stage != StageAccepted {
		t.Fatalf("a after the batch and before the observation reads stage %s, want accepted", a.Stage)
	}
	if _, err := k.observe("a"); err != nil {
		t.Fatalf("observe a: %v", err)
	}
	p := k.readProgress()
	a, b := progressNode(t, p, "a"), progressNode(t, p, "b")
	if a.Stage != StageIntegrated || a.Reason != DoneIntegrated || a.Disposition != DispDone {
		t.Fatalf("a after its observation reads stage %s reason %s, want integrated/%s", a.Stage, a.Reason, DoneIntegrated)
	}
	if b.Stage != StageAccepted {
		t.Fatalf("b, observed by nobody, reads stage %s, want accepted", b.Stage)
	}
	if p.Cumulative.Integrated.Nodes != 1 || p.Cumulative.Accepted.Nodes != 2 {
		t.Fatalf("cumulative = %+v, want one integrated of two accepted", p.Cumulative)
	}
	// the integration refs of the acceptance are the agent's (integrationRefsOf joins the agent stage rows to the batch)
	refs, err := integrationRefsOf(context.Background(), k.s.Q(context.Background()), k.currentAcceptance("a").AcceptanceID)
	if err != nil || len(refs) != 1 || refs[0] != "dev-int" {
		t.Fatalf("integrationRefsOf = %v, %v, want [dev-int]", refs, err)
	}
	if a.Links.Executions[0].ExecutionID == "" || a.AcceptanceID == "" {
		t.Fatalf("the integrated node's links are %+v, acceptance %q", a.Links, a.AcceptanceID)
	}
	// an integrated node's slot is back, and it holds nothing
	if a.HoldsSlot {
		t.Fatalf("the integrated node still holds its slot: %+v", a)
	}
}

// An acceptance of an execution does not stand on new work: a correction after the acceptance is an execution nobody accepted, so the node reads running again and an edge out of it is not satisfied by the
// acceptance of the execution before (blocked:stale_head), while a handover, which continues the same work, leaves both as they were.
func TestAgentAcceptanceDoesNotStandOnNewWork(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b"), unreleased: true}}
	k := newAgentKit(t, specs...)
	k.pinnedEdge("ab", "a", "b")
	k.judged("a")
	k.mustAccept("a")
	k.handover("a", "parent2")
	if st := k.edgeStatusOf("ab"); !st.Satisfied {
		t.Fatalf("the edge after a handover = %+v, want it satisfied by the acceptance of the work the new orchestrator continues", st)
	}
	var cur AgentExecution
	cur, live, err := currentAgentExecution(context.Background(), k.s.Q(context.Background()), "g", "a")
	if err != nil || !live {
		t.Fatalf("no current execution: %v", err)
	}
	k.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at, supersedes_execution_id) VALUES ('correction-1','g','a',?,?,?,?,?,?,?,?,?)",
		cur.Attempt+1, AgentCorrection, cur.ManifestDigest, cur.Role, cur.Tier, "parent2", 0, k.clock(), cur.ExecutionID)
	if st := k.edgeStatusOf("ab"); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge after a correction = %+v, want blocked:stale_head", st)
	}
	if a := progressNode(t, k.readProgress(), "a"); a.State != StateRunning {
		t.Fatalf("a after a correction reads %s, want running (new work nobody accepted)", a.State)
	}
}

// A node that holds a current execution or acceptance on both sides is read as neither: it is ambiguous, the reading of the rest of the plan goes on, and an edge out of it is blocked, not an error.
func TestAgentCrossKindNodeIsAmbiguousAndTheRestOfThePlanIsRead(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "r", files: fileOf("r"), relationship: true}, {name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b"), unreleased: true}}
	k := newAgentKit(t, specs...)
	k.pinnedEdge("rb", "r", "b")
	k.acceptAll(specs[0], specs[1])
	var manifest string
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions WHERE plan_id = 'g' AND node_id = 'r' LIMIT 1").Scan(&manifest); err != nil {
		t.Fatal(err)
	}
	k.exec("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		"e-forced", "g", "r", 1, AgentInitial, manifest, "executor", "executor", "parent", 0, k.clock())
	k.exec("INSERT INTO dag_agent_acceptances (acceptance_id, plan_id, node_id, manifest_digest, execution_id, criteria_set_digest, reviewer_call_id, reviewer_model, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		"acc-forced", "g", "r", manifest, "e-forced", releaseCriteriaDigest(), "call", reviewerModel, "authors", "parent", 0, k.clock())
	p := k.readProgress()
	if r := progressNode(t, p, "r"); r.Stage != StageAmbiguous || r.AcceptanceID != "" {
		t.Fatalf("the node that holds both kinds reads stage %s acceptance %q, want ambiguous with none", r.Stage, r.AcceptanceID)
	}
	if a := progressNode(t, p, "a"); a.Stage != StageAccepted {
		t.Fatalf("a, which holds one kind, reads %s: the rest of the plan is read", a.Stage)
	}
	if st := k.edgeStatusOf("rb"); st.Satisfied || st.Reason != BlockedAmbiguousHead {
		t.Fatalf("the edge out of the node that holds both kinds = %+v, want blocked:ambiguous_head", st)
	}
}

// The evidence the activity document keeps of an agent node is its acceptance, its verification record and its pre-merge record, and its commit link names the commit proof the row holds and the integration
// ref its batch moved.
func TestAgentEvidenceVisibleToActivity(t *testing.T) {
	t.Parallel()
	specs := []agentSpec{{name: "a", files: fileOf("a")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	if res, err := k.integrate(t); err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	row := k.currentAcceptance("a").Agent
	act, err := k.sched.ReadActivity(context.Background(), "g")
	if err != nil {
		t.Fatalf("activity: %v", err)
	}
	for _, n := range act.Nodes {
		if n.NodeID != "a" {
			continue
		}
		if n.Evidence == nil || n.Evidence.RecordedAt != row.AcceptedAt {
			t.Fatalf("the evidence of the agent node = %+v, want an entry recorded at the acceptance (%s)", n.Evidence, row.AcceptedAt)
		}
		if n.Commit == nil || n.Commit.HeadCommit != row.Head || n.Commit.TreeSHA != row.TreeSHA || n.Commit.RecordDigest != row.VerificationDigest || len(n.Commit.IntegrationRefs) != 1 || n.Commit.IntegrationRefs[0] != "dev-int" {
			t.Fatalf("the commit link of the agent node = %+v, want the row's commit proof and the ref dev-int", n.Commit)
		}
		if n.Artifact != nil || n.PullRequest != nil {
			t.Fatalf("an agent node has no artifact link and no pull request: %+v %+v", n.Artifact, n.PullRequest)
		}
		return
	}
	t.Fatal("the activity document holds no node a")
}
