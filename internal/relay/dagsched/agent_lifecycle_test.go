package dagsched

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1215 (R07): the life of an agent node. A parent that replaces the plan's parent adopts the node with a parent_handover execution row, a stale accepted result is reworked as a correction attempt of the
// same node (and a criteria-only change is judged again on the same head), dag-restart reports the agent nodes, and an execution that is given up (cancel, escalation) returns its slot in the transaction that
// closes it while a recovery hold keeps it. Everything is written the way the run writes it: the flow journal rows of the calls, the execution and acceptance rows through the library functions, the commits in git.

// orchestrator is a replacement parent of the kit's project that holds a coordinator epoch: the former parent is archived under it, and the scheduler it returns reads the same store.
func (k *agentKit) orchestrator(actor, nonce string) *Scheduler {
	k.t.Helper()
	k.replaceParent(actor)
	s := k.session("g", actor, nonce)
	s.Tips, s.Ancestry = newLegacyLocalTipReader(k.t), GitAncestry{}.Ancestry
	return s
}

// reviseNode revises the plan with a changed implementation node.
func (k *agentKit) reviseNode(node string, edit func(n doc)) {
	k.reviseNodeKind(node, dag.NodeImplementation, edit)
}

func (k *agentKit) reviseNodeKind(node, kind string, edit func(n doc)) {
	k.t.Helper()
	n := relNode(node, kind)
	edit(n)
	rev := int(k.snapshot("g").Revision)
	k.putPlan("g", rev, fmt.Sprintf("g-r%d", rev+1), doc{"op": dag.OpUpdateNode, "node": n})
}

// routeOf is the stale reading of a node with its route.
func (k *agentKit) routeOf(node string) *Stale {
	k.t.Helper()
	ctx := context.Background()
	snap := k.snapshot("g")
	n, ok := nodeOf(snap, node)
	if !ok {
		k.t.Fatalf("no node %s", node)
	}
	q := k.s.Q(ctx)
	st, err := k.sched.staleOf(ctx, q, "g", snap, n)
	if err != nil {
		k.t.Fatal(err)
	}
	if st == nil {
		return nil
	}
	routed, err := k.sched.withRoute(ctx, q, "g", snap, n, st)
	if err != nil {
		k.t.Fatal(err)
	}
	return routed
}

// reworked is the new output of a node that is corrected: a branch from the base with another commit, checked out in a worktree of its own. It returns the worktree and the commit.
func (k *agentKit) reworked(node string) (worktree, head string) {
	k.t.Helper()
	branch := "feature-" + node + "-2"
	k.repo.git("checkout", "-q", "-B", branch, k.base)
	k.repo.write("node-"+node+".txt", node+" reworked\n")
	k.repo.git("add", "node-"+node+".txt")
	k.repo.git("commit", "-q", "-m", "node "+node+" reworked")
	head = k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	worktree = filepath.Join(k.t.TempDir(), "wt-"+node+"-2")
	k.repo.git("worktree", "add", "-q", worktree, branch)
	return worktree, head
}

// closures are the closure rows of the store as subject kind, state, slot_released and reason, in the order they were written.
func (k *agentKit) closures() []string {
	k.t.Helper()
	rows, err := k.s.DB.Query("SELECT subject_kind || ':' || state || ':' || slot_released || ':' || reason FROM dag_agent_closures ORDER BY rowid")
	if err != nil {
		k.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			k.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// slotReleasedBy is who the newest tenure of the node's slot was returned as, and why.
func (k *agentKit) slotReleasedBy(node string) (by, reason string) {
	k.t.Helper()
	if err := k.s.DB.QueryRow("SELECT COALESCE(released_by, ''), COALESCE(release_reason, '') FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ? ORDER BY tenure DESC LIMIT 1", "g/"+node).Scan(&by, &reason); err != nil {
		k.t.Fatalf("slot of %s: %v", node, err)
	}
	return by, reason
}

// callIn records a call of the node's current execution that stops at an event: a terminal one ("outcome_unknown", "held", "completed") or, for "", the dispatch intent of a turn that never got an answer.
func (k *agentKit) callIn(node, terminal string) {
	k.t.Helper()
	k.calls++
	id := fmt.Sprintf("call-%d", k.calls)
	if k.count("SELECT COUNT(*) FROM flow_runs WHERE run_id = 'run-1'") == 0 {
		k.exec("INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			"run-1", "project-lane", dig("script"), "{}", dig("args"), "g", 1, "parent", "nonce", "", k.clock())
	}
	execution := k.execution(node)
	row := func(seq int, event string) {
		k.exec("INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, plan_id, node_id, execution_id, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			id, seq, "run-1", "executor:"+node, 0, "agent", "executor", event, "g", node, execution, k.clock())
	}
	row(1, "prepared")
	row(2, "turn_requested")
	if terminal != "" {
		row(3, terminal)
	}
}

func (k *agentKit) rowsOfKind(node, kind string) int {
	return k.count("SELECT COUNT(*) FROM dag_agent_executions WHERE plan_id = 'g' AND node_id = ? AND kind = ?", node, kind)
}

// When the plan's parent changes, Adopt appends a parent_handover execution row for an agent node: the same manifest and worktree, the thread of the work, a successor of the current row. The acceptance
// made before it stands, the new orchestrator integrates the node (the previous one may not), and nothing is made again: no acceptance, no new release, no operator.
func TestAgentNodeHandoverIntegrates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	first := k.releases["a"]
	accepted := k.currentAcceptance("a")

	s2 := k.orchestrator("parent-2", "session-2")
	// the adopting task must be the registered parent of the project, and the node's executions are not copied for a task that is not
	if _, err := s2.Adopt(ctx, "g", "a", "stranger"); err == nil {
		t.Fatal("a stranger adopted the node")
	}
	if n := k.rowsOfKind("a", AgentParentHandoff); n != 0 {
		t.Fatalf("%d handover rows after a refused adoption", n)
	}
	res, err := s2.Adopt(ctx, "g", "a", "parent-2")
	if err != nil || res.Replayed || res.ExecutionKind != ExecutionAgent || res.ExecutionID == "" || res.ExecutionID == first.ExecutionID {
		t.Fatalf("adopt = %+v, %v", res, err)
	}
	var row AgentExecution
	row, live, err := currentAgentExecution(ctx, k.s.Q(ctx), "g", "a")
	if err != nil || !live || row.ExecutionID != res.ExecutionID {
		t.Fatalf("current execution = %+v %v %v, want the handover row %s", row, live, err, res.ExecutionID)
	}
	if row.Kind != AgentParentHandoff || row.Attempt != 2 || row.Supersedes != first.ExecutionID || row.Actor != "parent-2" || row.ManifestDigest != first.ManifestDigest ||
		row.Worktree != k.worktrees["a"] || row.RunID != "run-1" || row.Epoch != s2.ExpectedEpoch {
		t.Fatalf("handover row = %+v (released %+v)", row, first)
	}
	if res2, err := s2.Adopt(ctx, "g", "a", "parent-2"); err != nil || !res2.Replayed || res2.ExecutionID != res.ExecutionID || k.rowsOfKind("a", AgentParentHandoff) != 1 {
		t.Fatalf("adopt again = %+v, %v", res2, err)
	}
	// the acceptance made before the handover stands and nobody accepted anything again
	if now := k.currentAcceptance("a"); now.AcceptanceID != accepted.AcceptanceID || k.count("SELECT COUNT(*) FROM dag_agent_acceptances") != 1 {
		t.Fatalf("acceptance after the handover = %s, want %s unchanged", now.AcceptanceID, accepted.AcceptanceID)
	}
	if a := progressNode(t, k.readProgress(), "a"); a.State != StateAccepted {
		t.Fatalf("a after the handover reads %s, want accepted", a.State)
	}
	// the executor of the integration is the one executionOf names
	in := k.batchIn()
	deps := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}
	in.Actor = "parent"
	if _, err := k.sched.IntegrateBatch(ctx, in, deps); err == nil {
		t.Fatal("the previous orchestrator integrated the node")
	}
	in.Actor = "parent-2"
	batch, err := s2.IntegrateBatch(ctx, in, deps)
	if err != nil || len(batch.Merged) != 1 || batch.Merged[0].NodeID != "a" || batch.Merged[0].ExecutionKind != ExecutionAgent {
		t.Fatalf("batch by the new orchestrator: %+v, %v", batch, err)
	}
	obs, err := s2.ObserveIntegration(ctx, "g", "a", "parent-2", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
	if err != nil || !obs.Integrated || !obs.SlotReleased {
		t.Fatalf("observe by the new orchestrator: %+v, %v", obs, err)
	}
	if a := progressNode(t, k.readProgress(), "a"); a.State != StateIntegrated {
		t.Fatalf("a after the landing reads %s, want integrated", a.State)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_node_executions"); n != 0 {
		t.Fatalf("%d rows in the frozen execution table", n)
	}
}

// A slot the previous parent reserved is returned, by the new orchestrator's integration, as the task that holds it: the handover row is what puts that task in the node's agent chain, and no ceiling or
// measurement refuses the return. A holder that is not in the chain stays refused.
func TestHandoverAgentSlotReturnedAsHolder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	if res, err := k.integrate(t); err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	if state, _ := k.slotOf("a"); state != "held" {
		t.Fatalf("slot = %s, want held by the previous parent", state)
	}
	s2 := k.orchestrator("parent-2", "session-2")
	// a ceiling below the use: a reservation would be refused, a return is not one
	k.holdSlots(2)
	k.declareLimit("project", "P-TEST", "runs", 1)
	if _, err := s2.Adopt(ctx, "g", "a", "parent-2"); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var tenure int64
	if err := k.s.DB.QueryRow("SELECT tenure FROM execution_slots WHERE subject_key = 'g/a'").Scan(&tenure); err != nil {
		t.Fatal(err)
	}
	obs, err := s2.ObserveIntegration(ctx, "g", "a", "parent-2", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
	if err != nil || !obs.Integrated || !obs.SlotReleased {
		t.Fatalf("observe by the new orchestrator: %+v, %v", obs, err)
	}
	if by, reason := k.slotReleasedBy("a"); by != "parent" || reason != "dag_integrated" {
		t.Fatalf("slot returned as %q for %q, want the recorded holder parent for dag_integrated", by, reason)
	}
	var tenureAfter, slots int64
	if err := k.s.DB.QueryRow("SELECT tenure FROM execution_slots WHERE subject_key = 'g/a' AND state = 'released'").Scan(&tenureAfter); err != nil || tenureAfter != tenure {
		t.Fatalf("the original tenure %d was not the one returned (%d, %v)", tenure, tenureAfter, err)
	}
	if err := k.s.DB.QueryRow("SELECT COUNT(*) FROM execution_slots WHERE subject_key = 'g/a'").Scan(&slots); err != nil || slots != 1 {
		t.Fatalf("%d slot rows for the node (%v), want the one reserved at the release", slots, err)
	}

	// a holder that no row of the node's agent chain names is not returned as itself
	k2 := newAgentKit(t, spec)
	k2.acceptAll(spec)
	if _, err := k2.integrate(t); err != nil {
		t.Fatal(err)
	}
	k2.exec("UPDATE execution_slots SET parent_task_id = 'stranger' WHERE subject_key = 'g/a'")
	k2.sched.Tips, k2.sched.Ancestry = newLegacyLocalTipReader(t), GitAncestry{}.Ancestry
	if _, err := k2.sched.ObserveIntegration(ctx, "g", "a", "parent", []Target{{Repository: k2.repo.path, BaseRef: "dev-int"}}); refusalReason(err) != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("observe with a slot of a stranger = %v, want scope_role_mismatch", err)
	}
	if state, _ := k2.slotOf("a"); state != "held" {
		t.Fatalf("the stranger's slot is %s, want held", state)
	}
}

// An accepted agent result that the plan made stale is not corrected by dag-correct: its route is agent_correction. AgentCorrect closes the acceptance (superseded), appends a correction execution row
// for the same node (a worktree of its own, a successor of the current row, a manifest rebuilt as the plan holds the node), keeps the slot, and the node's lane runs again: the reworked output is judged and
// accepted as any output is.
func TestStaleAgentNodeReleasesCorrectionAttempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	first := k.releases["a"]
	oldAcceptance := k.currentAcceptance("a").AcceptanceID
	slot := first.SlotID

	// a revision of the node's own slice (not its criteria alone)
	k.reviseNode("a", func(n doc) { n["title"] = "a, redefined" })
	st := k.routeOf("a")
	if st == nil || st.Action != ActionAgentCorrection || st.Cause != CauseSliceChanged {
		t.Fatalf("stale reading = %+v, want slice_changed routed to %s", st, ActionAgentCorrection)
	}
	// the corrections of a relationship do not apply: dag-correct has no child to send it to
	if _, err := k.sched.PrepareCorrection(ctx, "g", "a", "parent", ManifestInput{CreatedByTaskID: "parent"}, VerifyOptions{}); err == nil {
		t.Fatal("dag-correct prepared a correction for an agent node")
	}
	// only the orchestrator that holds the execution corrects it
	if _, err := k.sched.AgentCorrect(ctx, "g", "a", "stranger", k.agentRequest(true), AgentReleaseOptions{}); err == nil {
		t.Fatal("a stranger corrected the node")
	}
	wt, head := k.reworked("a")
	res, err := k.sched.AgentCorrect(ctx, "g", "a", "parent", k.agentRequest(true), AgentReleaseOptions{RunID: "run-2", Worktree: wt})
	if err != nil || res.Replayed || res.Kind != AgentCorrection || res.Attempt != 2 || res.SupersededAcceptance != oldAcceptance || res.ExecutionID == first.ExecutionID || res.ManifestDigest == first.ManifestDigest {
		t.Fatalf("correct = %+v, %v", res, err)
	}
	cur, live, err := currentAgentExecution(ctx, k.s.Q(ctx), "g", "a")
	if err != nil || !live || cur.ExecutionID != res.ExecutionID || cur.Supersedes != first.ExecutionID || cur.Worktree != wt || cur.RunID != "run-2" || cur.Actor != "parent" || cur.Kind != AgentCorrection {
		t.Fatalf("current execution = %+v %v %v", cur, live, err)
	}
	if got := k.closures(); len(got) != 1 || got[0] != "acceptance:superseded:0:"+st.Reason() {
		t.Fatalf("closures = %v, want the acceptance superseded for %s and no closure of the execution", got, st.Reason())
	}
	if _, found, err := currentAgentAcceptance(ctx, k.s.Q(ctx), "g", "a"); err != nil || found {
		t.Fatalf("the superseded acceptance is still current (%v)", err)
	}
	if state, _ := k.slotOf("a"); state != "held" || res.SlotID != slot {
		t.Fatalf("slot = %s (%s), want the slot %s still held", state, res.SlotID, slot)
	}
	if again, err := k.sched.AgentCorrect(ctx, "g", "a", "parent", k.agentRequest(true), AgentReleaseOptions{RunID: "run-2", Worktree: wt}); err != nil || !again.Replayed || again.ExecutionID != res.ExecutionID || k.rowsOfKind("a", AgentCorrection) != 1 {
		t.Fatalf("correct again = %+v, %v", again, err)
	}
	if a := progressNode(t, k.readProgress(), "a"); a.State != StateRunning {
		t.Fatalf("a after the correction reads %s, want running (new work nobody accepted)", a.State)
	}
	if k.count("SELECT COUNT(*) FROM dag_node_executions") != 0 || k.count("SELECT COUNT(*) FROM relationships") != 0 {
		t.Fatal("the correction wrote a row of the relationship side")
	}

	// the node's lane runs again on the new execution: the reworked head is judged and accepted, following the closed acceptance
	k.heads["a"] = head
	k.worktrees["a"] = wt
	k.record(journalCall{role: "executor", node: "a", model: authorModel, head: head, run: "run-2"})
	k.record(journalCall{role: "reviewer", node: "a", model: reviewerModel, head: head, verdict: "verified", criteria: releaseCriteriaDigest(), run: "run-2"})
	acc := k.mustAccept("a")
	if acc.ExecutionID != res.ExecutionID || acc.HeadSHA != head || acc.AcceptanceID == oldAcceptance {
		t.Fatalf("accept after the correction = %+v", acc)
	}
	var supersedes string
	if err := k.s.DB.QueryRow("SELECT supersedes_acceptance_id FROM dag_agent_acceptances WHERE acceptance_id = ?", acc.AcceptanceID).Scan(&supersedes); err != nil || supersedes != oldAcceptance {
		t.Fatalf("the new acceptance supersedes %q (%v), want the closed one %s", supersedes, err, oldAcceptance)
	}
	if k.routeOf("a") != nil {
		t.Fatalf("the reworked node is still stale: %+v", k.routeOf("a"))
	}
	if a := progressNode(t, k.readProgress(), "a"); a.State != StateAccepted {
		t.Fatalf("a after the second acceptance reads %s, want accepted", a.State)
	}
}

// A correction is owed only to a stale accepted result, and a result that landed is never run again.
func TestAgentCorrectRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	wt, _ := k.reworked("a")
	opt := AgentReleaseOptions{RunID: "run-2", Worktree: wt}
	before := allRows(t, k.s.DB)
	// nothing is stale: the plan did not move
	_, err := k.sched.AgentCorrect(ctx, "g", "a", "parent", k.agentRequest(true), opt)
	wantAgentRefusal(t, "a node that is not stale", err, contract.RefusalDispositionConflict, "not stale")
	if !equalRows(before, allRows(t, k.s.DB)) {
		t.Fatal("a refused correction changed the store")
	}
	// a criteria-only change is judged again on the same head: it is no correction
	k.reviseNode("a", func(n doc) { n["criteria_set_digest"] = dig("another criterion") })
	_, err = k.sched.AgentCorrect(ctx, "g", "a", "parent", k.agentRequest(true), opt)
	wantAgentRefusal(t, "a change of the criteria alone", err, contract.RefusalDispositionConflict, ActionRevalidate)
	// a node whose head landed is never run again, whatever changed above it
	in := k.batchIn()
	in.Nodes = []string{"b"}
	if res, err := k.sched.IntegrateBatch(ctx, in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	if _, err := k.observe("b"); err != nil {
		t.Fatal(err)
	}
	k.reviseNode("b", func(n doc) { n["title"] = "b, redefined" })
	_, err = k.sched.AgentCorrect(ctx, "g", "b", "parent", k.agentRequest(true), opt)
	if refusalReason(err) != string(contract.RefusalDispositionConflict) {
		t.Fatalf("correcting a node that landed = %v, want disposition_conflict", err)
	}
	if k.rowsOfKind("b", AgentCorrection)+k.rowsOfKind("a", AgentCorrection) != 0 {
		t.Fatal("a refused correction wrote an execution row")
	}
}

// A change of the node's criteria alone leaves the output as it was: the same head is judged again by a new reviewer call under the plan's criteria and accepted again. The new acceptance row has the
// same execution id and head, the new criteria digest, and follows the old row; no execution row is written; the node is current again.
func TestCriteriaOnlyChangeRereviewsSameHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	old := k.currentAcceptance("a").Agent
	executions := k.count("SELECT COUNT(*) FROM dag_agent_executions")

	changed := dig("a new criterion of the plan")
	k.reviseNode("a", func(n doc) { n["criteria_set_digest"] = changed })
	st := k.routeOf("a")
	if st == nil || st.Cause != CauseCriteriaChanged || st.Action != ActionRevalidate {
		t.Fatalf("stale reading = %+v, want criteria_changed routed to %s", st, ActionRevalidate)
	}
	// the old verdict was given under the old criteria: the same head accepted again needs a review under the new ones
	in := k.input("a")
	in.Supersedes = old.AcceptanceID
	_, err := k.sched.Accept(ctx, "g", "a", "parent", in)
	if refusalReason(err) != string(contract.RefusalCriteriaSetChanged) {
		t.Fatalf("accepting again on the old review = %v, want criteria_set_changed", err)
	}
	k.review("a", reviewerModel, "verified", changed)
	res, err := k.sched.Accept(ctx, "g", "a", "parent", in)
	if err != nil || res.Replayed || res.SupersededID != old.AcceptanceID || res.ExecutionID != old.ExecutionID || res.HeadSHA != old.Head {
		t.Fatalf("accept again = %+v, %v", res, err)
	}
	now := k.currentAcceptance("a").Agent
	if now.AcceptanceID == old.AcceptanceID || now.ExecutionID != old.ExecutionID || now.Head != old.Head || now.CriteriaSetDigest != changed || now.Supersedes != old.AcceptanceID {
		t.Fatalf("new acceptance = %+v, old = %+v", now, old)
	}
	if now.ReviewerCallID == old.ReviewerCallID {
		t.Fatalf("the new acceptance rests on the old reviewer call %s", now.ReviewerCallID)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_agent_executions"); got != executions {
		t.Fatalf("%d execution rows, want the %d there were: no new execution", got, executions)
	}
	if k.count("SELECT COUNT(*) FROM dag_agent_acceptances") != 2 {
		t.Fatal("the old acceptance row is not kept")
	}
	if k.routeOf("a") != nil {
		t.Fatalf("the node is still stale after it was judged again: %+v", k.routeOf("a"))
	}
	// the same key again is the same acceptance
	if again, err := k.sched.Accept(ctx, "g", "a", "parent", in); err != nil || !again.Replayed || again.AcceptanceID != now.AcceptanceID {
		t.Fatalf("accept once more = %+v, %v", again, err)
	}
}

// dag-restart reports every agent node with its state (released, accepted, integrated, held), the actor and run that hold it and the action it needs: the run resumes a released node, `crw flow recover`
// settles a node whose call ended with an unknown outcome, the accepted head is integrated, and a node held by another orchestrator is adopted first.
func TestRestartReportsAgentNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}, {name: "c", files: fileOf("c")}, {name: "d", files: fileOf("d")}, {name: "e", nonPR: true}}
	k := newAgentKit(t, specs...)
	k.release("e")
	k.author("a", authorModel) // a: a call completed, the run carries on
	for _, n := range []string{"b", "c"} {
		k.judged(n)
		k.mustAccept(n)
	}
	k.callIn("d", "outcome_unknown") // d: a call whose outcome is not known
	in := k.batchIn()
	in.Nodes = []string{"c"}
	if res, err := k.sched.IntegrateBatch(ctx, in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	if obs, err := k.observe("c"); err != nil || !obs.Integrated {
		t.Fatalf("observe: %+v, %v", obs, err)
	}

	type want struct{ state, action, resume, holder string }
	check := func(s *Scheduler, actor string, wants map[string]want) {
		t.Helper()
		report, err := s.Restart(ctx, "g", actor)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]RestartNode{}
		for _, n := range report.Nodes {
			seen[n.NodeID] = n
		}
		for node, w := range wants {
			n, ok := seen[node]
			if !ok || n.Agent == nil {
				t.Fatalf("restart for %s does not report node %s as an agent node: %+v", actor, node, n)
			}
			a := n.Agent
			if a.State != w.state || a.Action != w.action || n.Resume != w.resume || a.Actor != w.holder || a.RunID != "run-1" || a.ExecutionID != k.execution(node) || a.ExecutionKind == "" {
				t.Fatalf("restart for %s, node %s = %+v (resume %s), want state %s action %q resume %s held by %s", actor, node, a, n.Resume, w.state, w.action, w.resume, w.holder)
			}
			if n.RelationshipID != "" || n.ParentTaskID != "" {
				t.Fatalf("node %s carries a relationship: %+v", node, n)
			}
		}
		if len(wants) != 0 && len(seen) != len(wants) {
			t.Fatalf("restart reports %d nodes, want %d: %+v", len(seen), len(wants), seen)
		}
	}
	check(k.sched, "parent", map[string]want{
		"a": {AgentStateReleased, AgentActionRunResume, ResumeAdopt, "parent"},
		"b": {AgentStateAccepted, AgentActionIntegrate, ResumeAdopt, "parent"},
		"c": {AgentStateIntegrated, "", ResumeNone, "parent"},
		"d": {AgentStateHeld, AgentActionFlowRecover, ResumeReconcile, "parent"},
		"e": {AgentStateReleased, AgentActionRunResume, ResumeAdopt, "parent"},
	})
	// the object the relay prints carries the agent facts of an agent node, and only of an agent node
	report, err := k.sched.Restart(ctx, "g", "parent")
	if err != nil {
		t.Fatal(err)
	}
	printed := fmt.Sprintf("%v", report.Object())
	for _, w := range []string{"crw flow recover", "run resume", "run-1", "held", "integrated"} {
		if !strings.Contains(printed, w) {
			t.Fatalf("the printed report lacks %q: %s", w, printed)
		}
	}
	if report.Counts[ResumeReconcile] != 1 || report.Counts[ResumeAdopt] != 3 {
		t.Fatalf("counts = %v", report.Counts)
	}

	// another orchestrator takes the plan over: it is told to adopt what the former one held, and what landed needs nothing
	s2 := k.orchestrator("parent-2", "session-2")
	check(s2, "parent-2", map[string]want{
		"a": {AgentStateReleased, AgentActionAdopt, ResumeAdoptNeeded, "parent"},
		"b": {AgentStateAccepted, AgentActionAdopt, ResumeAdoptNeeded, "parent"},
		"c": {AgentStateIntegrated, "", ResumeNone, "parent"},
		"d": {AgentStateHeld, AgentActionAdopt, ResumeAdoptNeeded, "parent"},
		"e": {AgentStateReleased, AgentActionAdopt, ResumeAdoptNeeded, "parent"},
	})
	for _, node := range []string{"a", "b", "d", "e"} {
		if _, err := s2.Adopt(ctx, "g", node, "parent-2"); err != nil {
			t.Fatalf("adopt %s: %v", node, err)
		}
	}
	check(s2, "parent-2", map[string]want{
		"a": {AgentStateReleased, AgentActionRunResume, ResumeAdopt, "parent-2"},
		"b": {AgentStateAccepted, AgentActionIntegrate, ResumeAdopt, "parent-2"},
		"c": {AgentStateIntegrated, "", ResumeNone, "parent"},
		"d": {AgentStateHeld, AgentActionFlowRecover, ResumeReconcile, "parent-2"},
		"e": {AgentStateReleased, AgentActionRunResume, ResumeAdopt, "parent-2"},
	})
}

// closeKit is a kit with one running agent node and one accepted (not landed) agent node.
func closeKit(t *testing.T) *agentKit {
	t.Helper()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	k.judged("b")
	k.mustAccept("b")
	return k
}

// crw flow cancel gives up the node: the execution gets a closure (cancelled) and the slot goes back in the same transaction. A result that was accepted and has not landed is revoked with it, so the
// node does not keep satisfying the edges built on it.
func TestCancelReleasesAgentSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := closeKit(t)
	first := k.releases["a"]
	if _, err := k.sched.AgentClose(ctx, "g", "a", "stranger", AgentCloseInput{Reason: AgentReasonFlowCancel}); err == nil {
		t.Fatal("a stranger cancelled the node")
	}
	if _, err := k.sched.AgentClose(ctx, "g", "a", "parent", AgentCloseInput{Reason: "because"}); refusalReason(err) != string(contract.RefusalMalformedReceipt) {
		t.Fatalf("an unknown reason = %v, want malformed_receipt", err)
	}
	if state, _ := k.slotOf("a"); state != "held" || len(k.closures()) != 0 {
		t.Fatalf("refused closes changed the slot (%s) or wrote closures %v", state, k.closures())
	}
	res, err := k.sched.AgentClose(ctx, "g", "a", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel})
	if err != nil || !res.Closed || res.Held || !res.SlotReleased || res.State != AgentClosureCancelled || res.ExecutionID != first.ExecutionID || res.RevokedAcceptance != "" {
		t.Fatalf("cancel = %+v, %v", res, err)
	}
	if got := k.closures(); len(got) != 1 || got[0] != "execution:cancelled:1:"+AgentReasonFlowCancel {
		t.Fatalf("closures = %v", got)
	}
	if state, reason := k.slotOf("a"); state != "released" || reason != "dag_agent_cancelled" {
		t.Fatalf("slot = %s for %s, want released for dag_agent_cancelled", state, reason)
	}
	if _, live, _ := currentAgentExecution(ctx, k.s.Q(ctx), "g", "a"); live {
		t.Fatal("the cancelled execution is still current")
	}
	if again, err := k.sched.AgentClose(ctx, "g", "a", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel}); err != nil || !again.Replayed || !again.Closed || len(k.closures()) != 1 {
		t.Fatalf("cancel again = %+v, %v", again, err)
	}
	if _, err := k.sched.AgentClose(ctx, "g", "a", "parent", AgentCloseInput{Reason: AgentEscalationBlockedScope}); refusalReason(err) != string(contract.RefusalDispositionConflict) {
		t.Fatalf("closing a cancelled execution another way = %v, want disposition_conflict", err)
	}
	// the node is given to an agent again like any node whose execution a closure ended: a new tenure of the slot, the next attempt
	if again := k.release("a"); again.Attempt != 2 || again.SlotID == first.SlotID {
		t.Fatalf("release after the cancel = %+v", again)
	}

	// an accepted result that has not landed: the acceptance is revoked in the same transaction
	acceptance := k.currentAcceptance("b").AcceptanceID
	res, err = k.sched.AgentClose(ctx, "g", "b", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel})
	if err != nil || !res.Closed || !res.SlotReleased || res.RevokedAcceptance != acceptance {
		t.Fatalf("cancel of an accepted node = %+v, %v", res, err)
	}
	if got := k.closures(); len(got) != 3 || got[1] != "acceptance:revoked:0:"+AgentReasonFlowCancel || got[2] != "execution:cancelled:1:"+AgentReasonFlowCancel {
		t.Fatalf("closures = %v", got)
	}
	if _, found, err := acceptanceOf(ctx, k.s.Q(ctx), "g", "b"); err != nil || found {
		t.Fatalf("the cancelled node still has a current acceptance (%v)", err)
	}
	if state, reason := k.slotOf("b"); state != "released" || reason != "dag_agent_cancelled" {
		t.Fatalf("slot of b = %s for %s", state, reason)
	}
}

// A node whose head landed is not given up: its value is in the target.
func TestAgentCloseRefusesALandedNode(t *testing.T) {
	t.Parallel()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	if _, err := k.integrate(t); err != nil {
		t.Fatal(err)
	}
	if obs, err := k.observe("a"); err != nil || !obs.Integrated {
		t.Fatalf("observe: %+v, %v", obs, err)
	}
	before := allRows(t, k.s.DB)
	_, err := k.sched.AgentClose(context.Background(), "g", "a", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel})
	wantAgentRefusal(t, "closing a node that landed", err, contract.RefusalDispositionConflict, "landed")
	if !equalRows(before, allRows(t, k.s.DB)) {
		t.Fatal("a refused close changed the store")
	}
}

// A node that is given up by an escalation (tiers_exhausted, blocked_scope, size_split) gets a closure (escalated) with the reason and its slot goes back in the same transaction.
func TestEscalationReleasesAgentSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reasons := []string{AgentEscalationTiersExhausted, AgentEscalationBlockedScope, AgentEscalationSizeSplit}
	specs := make([]agentSpec, len(reasons))
	for i := range reasons {
		specs[i] = agentSpec{name: string(rune('a' + i)), files: fileOf(string(rune('a' + i)))}
	}
	k := newAgentKit(t, specs...)
	for i, reason := range reasons {
		node := specs[i].name
		res, err := k.sched.AgentClose(ctx, "g", node, "parent", AgentCloseInput{Reason: reason})
		if err != nil || !res.Closed || res.Held || !res.SlotReleased || res.State != AgentClosureEscalated || res.Reason != reason {
			t.Fatalf("escalation %s = %+v, %v", reason, res, err)
		}
		if state, why := k.slotOf(node); state != "released" || why != "dag_agent_escalated" {
			t.Fatalf("slot of %s = %s for %s, want released for dag_agent_escalated", node, state, why)
		}
		if got := k.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND subject_id = ? AND state = 'escalated' AND slot_released = 1 AND reason = ?", k.releases[node].ExecutionID, reason); got != 1 {
			t.Fatalf("closure of %s for %s: %d rows", node, reason, got)
		}
	}
	// the escalated node is not accepted afterwards: it has no current execution
	if _, err := k.accept("a"); err == nil || k.agentAcceptanceRows() != 0 {
		t.Fatalf("an escalated node was accepted (%v)", err)
	}
}

// A recovery hold (dispatch_unknown, a host failure) closes nothing: the execution stays current and the node keeps its slot.
func TestHoldKeepsAgentSlot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := newAgentKit(t, agentSpec{name: "a", files: fileOf("a")}, agentSpec{name: "b", files: fileOf("b")})
	before := allRows(t, k.s.DB)
	for node, reason := range map[string]string{"a": AgentHoldDispatchUnknown, "b": AgentHoldHostFailure} {
		res, err := k.sched.AgentClose(ctx, "g", node, "parent", AgentCloseInput{Reason: reason})
		if err != nil || res.Closed || !res.Held || res.SlotReleased || res.Reason != reason || res.ExecutionID != k.releases[node].ExecutionID {
			t.Fatalf("hold %s = %+v, %v", reason, res, err)
		}
		if state, _ := k.slotOf(node); state != "held" {
			t.Fatalf("slot of %s = %s after a %s hold, want held", node, state, reason)
		}
		if k.execution(node) != k.releases[node].ExecutionID {
			t.Fatalf("the execution of %s is not current after a hold", node)
		}
	}
	if !equalRows(before, allRows(t, k.s.DB)) {
		t.Fatal("a hold wrote a row")
	}
	// the hold is owed to the orchestrator that holds the node
	if _, err := k.sched.AgentClose(ctx, "g", "a", "stranger", AgentCloseInput{Reason: AgentHoldDispatchUnknown}); err == nil {
		t.Fatal("a stranger held the node")
	}
	if len(k.closures()) != 0 {
		t.Fatalf("closures = %v after holds", k.closures())
	}
}

// A node that went back with its acceptance (it has no head to land) reserves a slot again for its correction attempt, as a release does, and a reservation the capacity refuses writes nothing: the acceptance
// is not closed.
func TestStaleNonImplementationAgentNodeReservesItsSlotAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	build := func() *agentKit {
		k := newAgentKit(t, agentSpec{name: "doc", nonPR: true, unreleased: true})
		k.release("doc")
		k.record(journalCall{role: "architect", node: "doc", model: authorModel})
		k.record(journalCall{role: "reviewer", node: "doc", model: reviewerModel, verdict: "verified", criteria: releaseCriteriaDigest()})
		if res, err := k.sched.Accept(ctx, "g", "doc", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"}}); err != nil || !res.SlotReleased {
			t.Fatalf("accept: %+v, %v", res, err)
		}
		k.reviseNodeKind("doc", dag.NodeNonPR, func(n doc) { n["title"] = "doc, redefined" })
		return k
	}

	// the capacity has no free slot: the correction is refused whole
	full := build()
	full.holdSlots(2)
	full.declareLimit("project", "P-TEST", "runs", 1)
	if _, err := full.sched.AgentCorrect(ctx, "g", "doc", "parent", full.agentRequest(false), AgentReleaseOptions{}); refusalReason(err) != string(contract.RefusalCapacityExhausted) {
		t.Fatalf("correction without a free slot = %v, want capacity_exhausted", err)
	}
	if len(full.closures()) != 0 || full.rowsOfKind("doc", AgentCorrection) != 0 {
		t.Fatalf("a refused correction wrote closures %v or a row", full.closures())
	}

	k := build()
	res, err := k.sched.AgentCorrect(ctx, "g", "doc", "parent", k.agentRequest(false), AgentReleaseOptions{})
	if err != nil || res.Kind != AgentCorrection || res.SlotID == "" || res.SlotID == k.releases["doc"].SlotID {
		t.Fatalf("correct = %+v, %v", res, err)
	}
	if state, _ := k.slotOf("doc"); state != "held" || k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = 'g/doc'") != 2 {
		t.Fatalf("slot = %s, want a second tenure held", state)
	}
	if got := k.closures(); len(got) != 1 || !strings.HasPrefix(got[0], "acceptance:superseded:0:stale:") {
		t.Fatalf("closures = %v", got)
	}
}

// After a handover the new orchestrator gives a node up, and the slot goes back as the task that reserved it: the closure is the return, with no reservation a ceiling could refuse.
func TestHandoverAgentSlotReturnedAsHolderByAClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := newAgentKit(t, agentSpec{name: "a", files: fileOf("a")})
	s2 := k.orchestrator("parent-2", "session-2")
	k.holdSlots(2)
	k.declareLimit("project", "P-TEST", "runs", 1)
	if _, err := s2.AgentClose(ctx, "g", "a", "parent-2", AgentCloseInput{Reason: AgentEscalationSizeSplit}); refusalReason(err) != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("closing a node the orchestrator has not adopted = %v, want scope_role_mismatch (the execution is held by the previous parent)", err)
	}
	if _, err := s2.Adopt(ctx, "g", "a", "parent-2"); err != nil {
		t.Fatal(err)
	}
	res, err := s2.AgentClose(ctx, "g", "a", "parent-2", AgentCloseInput{Reason: AgentEscalationSizeSplit})
	if err != nil || !res.Closed || !res.SlotReleased {
		t.Fatalf("close by the new orchestrator = %+v, %v", res, err)
	}
	if by, reason := k.slotReleasedBy("a"); by != "parent" || reason != "dag_agent_escalated" {
		t.Fatalf("slot returned as %q for %q, want the holder parent for dag_agent_escalated", by, reason)
	}
}

// dag-restart names what a stale accepted agent node is owed: a correction attempt for a result that must be reworked, a new review for a change of the criteria alone.
func TestRestartReportsStaleAgentNodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	specs := []agentSpec{{name: "a", files: fileOf("a")}, {name: "b", files: fileOf("b")}}
	k := newAgentKit(t, specs...)
	k.acceptAll(specs...)
	k.reviseNode("a", func(n doc) { n["title"] = "a, redefined" })
	k.reviseNode("b", func(n doc) { n["criteria_set_digest"] = dig("another criterion") })
	report, err := k.sched.Restart(ctx, "g", "parent")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": AgentActionCorrect, "b": AgentActionRereview}
	for _, n := range report.Nodes {
		if n.Agent == nil || n.Agent.State != AgentStateAccepted || n.Agent.Action != want[n.NodeID] || n.Resume != ResumeAdopt || n.State != StateStale {
			t.Fatalf("restart for node %s = %+v (resume %s, state %s), want accepted and %q", n.NodeID, n.Agent, n.Resume, n.State, want[n.NodeID])
		}
		delete(want, n.NodeID)
	}
	if len(want) != 0 {
		t.Fatalf("restart does not report %v", want)
	}
}

// ---- the correction round of the pre-merge evaluation (08a9d2b1): d1, d2, d3 ----

// IntegrateBatch moves the target and records the move before ObserveIntegration reads it. A close or a correction in that interval would revoke or supersede an acceptance whose head is already in the
// target (and ObserveIntegration needs the acceptance to stay current to finish the landing): both are refused until an observation settles the landing.
func TestAgentCloseAndCorrectKeepAMovedUnobservedLanding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	if res, err := k.integrate(t); err != nil || len(res.Merged) != 1 {
		t.Fatalf("batch: %+v, %v", res, err)
	}
	before := allRows(t, k.s.DB)
	_, err := k.sched.AgentClose(ctx, "g", "a", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel})
	wantAgentRefusal(t, "closing a node whose head moved into the target and was not observed", err, contract.RefusalDispositionConflict, "observe")
	if !equalRows(before, allRows(t, k.s.DB)) {
		t.Fatal("a refused close changed the store")
	}
	k.reviseNode("a", func(n doc) { n["title"] = "a, redefined" })
	wt, _ := k.reworked("a")
	_, err = k.sched.AgentCorrect(ctx, "g", "a", "parent", k.agentRequest(true), AgentReleaseOptions{RunID: "run-2", Worktree: wt})
	wantAgentRefusal(t, "correcting a node whose head moved into the target and was not observed", err, contract.RefusalDispositionConflict, "observe")
	if k.count("SELECT COUNT(*) FROM dag_agent_closures") != 0 {
		t.Fatal("a refused close or correction wrote a closure")
	}
	if k.rowsOfKind("a", AgentCorrection) != 0 {
		t.Fatal("a refused correction wrote an execution")
	}
	// the observation settles it: the landing finishes on the acceptance that was kept
	if obs, err := k.observe("a"); err != nil || !obs.Integrated {
		t.Fatalf("observe after the refused close: %+v, %v", obs, err)
	}
}

// A landing that was lost (the target no longer holds the head and an observation made after the batch says so) is no landing: the node is given up as before.
func TestAgentCloseAfterALostLandingIsAllowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	k.acceptAll(spec)
	if _, err := k.integrate(t); err != nil {
		t.Fatal(err)
	}
	k.repo.git("branch", "-f", "dev-int", k.base)
	if obs, err := k.observe("a"); err != nil || obs.Integrated {
		t.Fatalf("observe after the target lost the head: %+v, %v", obs, err)
	}
	res, err := k.sched.AgentClose(ctx, "g", "a", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel})
	if err != nil || !res.Closed || res.RevokedAcceptance == "" {
		t.Fatalf("close after a lost landing = %+v, %v", res, err)
	}
}

// An agent correction is admitted as a release is: a host short of memory holds it, and so does a merge turn that moves a branch the node builds on.
func TestAgentCorrectKeepsTheReleaseAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := newAgentKit(t, agentSpec{name: "doc", nonPR: true, unreleased: true})
	k.release("doc")
	k.record(journalCall{role: "architect", node: "doc", model: authorModel})
	k.record(journalCall{role: "reviewer", node: "doc", model: reviewerModel, verdict: "verified", criteria: releaseCriteriaDigest()})
	if _, err := k.sched.Accept(ctx, "g", "doc", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"}}); err != nil {
		t.Fatalf("accept: %v", err)
	}
	k.reviseNodeKind("doc", dag.NodeNonPR, func(n doc) { n["title"] = "doc, redefined" })

	k.sched.Host = hostBound(shortHosts[0].sample)
	if _, err := k.sched.AgentCorrect(ctx, "g", "doc", "parent", k.agentRequest(false), AgentReleaseOptions{}); refusalReason(err) != string(contract.RefusalCapacityExhausted) {
		t.Fatalf("correction on a host short of memory = %v, want capacity_exhausted", err)
	}
	if len(k.closures()) != 0 || k.rowsOfKind("doc", AgentCorrection) != 0 {
		t.Fatalf("a refused correction wrote closures %v or a row", k.closures())
	}

	// a merge turn that moves the branch an incoming edge of the node targets
	k.sched.Host = nil
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r-edge", addRelNode("up", dag.NodeImplementation), addEdge("e-up", "up", "doc", dag.EdgeIntegrated, doc{"target_repository": k.repo.path, "target_base_ref": "dev"}))
	k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, state, tenure, requested_at, updated_at)"+
		" VALUES ('mtn-moving', 'tgt', ?, 'dev', 'P-TEST', 'parent', 'host', NULL, NULL, 'x', 'merging', 1, 't', 't')", k.repo.path)
	snap := k.snapshot("g")
	n, _ := nodeOf(snap, "doc")
	err := k.sched.correctionAdmission(ctx, k.s.Q(ctx), "g", snap, n, false)
	if err == nil || !strings.Contains(err.Error(), DeferMergeWindow) {
		t.Fatalf("admission over a moving merge turn = %v, want %s", err, DeferMergeWindow)
	}
}

// dag-restart settles a held call per call: a call that ended with an unknown outcome stays unresolved when another call of the same execution, or of the execution a handover continues, completes.
func TestRestartKeepsAnUnresolvedCallWhenAnotherCallCompletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := newAgentKit(t, agentSpec{name: "a", files: fileOf("a")})
	stateOf := func(s *Scheduler, actor string) (string, string) {
		t.Helper()
		report, err := s.Restart(ctx, "g", actor)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range report.Nodes {
			if n.NodeID == "a" && n.Agent != nil {
				return n.Agent.State, n.Agent.Action
			}
		}
		t.Fatalf("restart does not report a: %+v", report.Nodes)
		return "", ""
	}
	k.callIn("a", "outcome_unknown")
	unresolved := fmt.Sprintf("call-%d", k.calls)
	k.author("a", authorModel) // an independent call of the same execution completes after it
	if state, action := stateOf(k.sched, "parent"); state != AgentStateHeld || action != AgentActionFlowRecover {
		t.Fatalf("restart with an unresolved call and a later completed call = %s/%s, want held/%s", state, action, AgentActionFlowRecover)
	}
	// a handover continues the work; a call of the new execution completing does not settle the old one
	s2 := k.orchestrator("parent-2", "session-2")
	if _, err := s2.Adopt(ctx, "g", "a", "parent-2"); err != nil {
		t.Fatal(err)
	}
	k.review("a", reviewerModel, "verified", releaseCriteriaDigest())
	if state, action := stateOf(s2, "parent-2"); state != AgentStateHeld || action != AgentActionFlowRecover {
		t.Fatalf("restart after a handover = %s/%s, want held/%s", state, action, AgentActionFlowRecover)
	}
	// a later event of the SAME call settles it
	k.exec("INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, plan_id, node_id, execution_id, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		unresolved, 4, "run-1", "executor:a", 0, "agent", "executor", "completed", "g", "a", k.execution("a"), k.clock())
	if state, action := stateOf(s2, "parent-2"); state != AgentStateReleased || action != AgentActionRunResume {
		t.Fatalf("restart after the call was settled = %s/%s, want released/%s", state, action, AgentActionRunResume)
	}
}
