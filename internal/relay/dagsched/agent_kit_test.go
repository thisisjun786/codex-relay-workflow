package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The kit of the agent acceptance and landing tests (R05, CRW-1213): one plan "g" of implementation nodes over one real repository whose dev branch is the base. A node is either released to an agent
// (its work is a commit on a feature branch, checked out in a linked worktree of the repository that the execution row records) or accepted the relationship way (the fixture of the integration batch tests).
// Everything the agent path reads is written the way the run writes it: the flow journal rows of the executor and reviewer calls, the execution row of the release, the commits in git. Nothing is seeded into an
// acceptance or stage table.

const (
	authorModel   = "claude-fake-haiku"
	reviewerModel = "gpt-fake-sol"
)

// agentSpec is one node of the kit.
type agentSpec struct {
	name  string
	files map[string]string
	// relationship accepts the node the relationship way (a managed child's report and a commit acceptance); the default is an agent node released with its worktree.
	relationship bool
	// unreleased leaves an agent node unreleased.
	unreleased bool
	// nonPR makes the node a non_pr node (no files, no head).
	nonPR bool
	// headOf makes the node's head the head of another node of the kit (the work is the same commit).
	headOf string
}

type agentKit struct {
	*releaseKit
	repo      *gitRepo
	base      string
	heads     map[string]string
	worktrees map[string]string
	releases  map[string]AgentReleaseResult
	calls     int
}

func newAgentKit(t *testing.T, specs ...agentSpec) *agentKit { return newAgentKitOn(t, nil, specs...) }

// newAgentKitOn is a kit over a repository that another kit shares (a second project integrating into the same target); nil makes a repository of its own.
func newAgentKitOn(t *testing.T, repo *gitRepo, specs ...agentSpec) *agentKit {
	t.Helper()
	if repo == nil {
		repo = newGitRepo(t)
	}
	k := &agentKit{releaseKit: newReleaseKit(t), repo: repo, heads: map[string]string{}, worktrees: map[string]string{}, releases: map[string]AgentReleaseResult{}}
	k.base = k.repo.git("rev-parse", "dev")
	var changes []doc
	for _, n := range specs {
		if n.nonPR {
			changes = append(changes, addRelNode(n.name, dag.NodeNonPR))
			continue
		}
		if n.headOf != "" {
			k.repo.git("branch", "-f", "feature-"+n.name, k.heads[n.headOf])
			k.heads[n.name] = k.heads[n.headOf]
		} else {
			k.repo.git("checkout", "-q", "-B", "feature-"+n.name, k.base)
			for path, body := range n.files {
				k.repo.write(path, body)
				k.repo.git("add", path)
			}
			k.repo.git("commit", "-q", "-m", "node "+n.name)
			k.heads[n.name] = k.repo.git("rev-parse", "HEAD")
			k.repo.git("checkout", "-q", "dev")
		}
		if !n.relationship {
			wt := filepath.Join(t.TempDir(), "wt-"+n.name)
			k.repo.git("worktree", "add", "-q", wt, "feature-"+n.name)
			k.worktrees[n.name] = wt
		}
		changes = append(changes, addRelNode(n.name, dag.NodeImplementation))
	}
	k.putPlan("g", 0, "g-r1", changes...)
	// the nodes of one issue edit their own places: an undeclared region overlaps every other node that is running or accepted
	for _, n := range specs {
		if !n.nonPR {
			k.declare("g", n.name, "node-"+n.name+".txt")
		}
	}
	for _, n := range specs {
		switch {
		case n.relationship:
			r := k.reportNode("g", n.name, acceptOpts{})
			k.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
				r.Event, r.Acceptance.RelationshipID, 1, "verdict-turn", k.heads[n.name], "parent", k.clock())
		case !n.unreleased:
			k.release(n.name)
		}
	}
	return k
}

// release releases an agent node to an agent with its worktree.
func (k *agentKit) release(node string) AgentReleaseResult {
	k.t.Helper()
	n, _ := nodeOf(k.snapshot("g"), node)
	res, err := k.sched.ReleaseAgent(context.Background(), "g", node, "parent", k.agentRequest(n.Kind == dag.NodeImplementation), AgentReleaseOptions{RunID: "run-1", Worktree: k.worktrees[node]})
	if err != nil {
		k.t.Fatalf("release agent %s: %v", node, err)
	}
	k.releases[node] = res
	return res
}

// execution is the id of the agent execution that is current for a node.
func (k *agentKit) execution(node string) string {
	k.t.Helper()
	cur, live, err := currentAgentExecution(context.Background(), k.s.Q(context.Background()), "g", node)
	if err != nil || !live {
		k.t.Fatalf("node %s has no current agent execution: %v", node, err)
	}
	return cur.ExecutionID
}

// journalCall is one call the run records: the rows of its events in flow_calls.
type journalCall struct {
	role, node, model, effort string
	// head is the commit under review (a reviewer) or produced (an executor); verdict and criteria are the reviewer's verdict and the criteria digest its input carried.
	head, verdict, criteria string
	// execution is the execution the call carries; the node's current one when empty.
	execution string
	// run is the run that recorded the call; run-1 when empty.
	run string
	// noPair leaves out the thread_started row, so the pair the host read back is unknown.
	noPair bool
	// resumeOf makes the call a continuation of that earlier call: the engine records its prepared row with resume_of and the pair the thread was read back with (configured_model, configured_effort) and
	// emits no thread_started row of its own.
	resumeOf string
	// selfReported leaves the criteria digest out of detail_json (the input the call was given) and names it only in the verdict document the reviewer answered.
	selfReported bool
}

// record writes the journal rows of a call: prepared, thread_started (the configured pair), and the terminal completed row. It returns the call id.
func (k *agentKit) record(c journalCall) string {
	k.t.Helper()
	k.calls++
	id := fmt.Sprintf("call-%d", k.calls)
	if c.run == "" {
		c.run = "run-1"
	}
	if k.count("SELECT COUNT(*) FROM flow_runs WHERE run_id = ?", c.run) == 0 {
		k.exec("INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			c.run, "project-lane", dig("script"), "{}", dig("args"), "g", 1, "parent", "nonce", "", k.clock())
	}
	if c.execution == "" && k.count("SELECT COUNT(*) FROM dag_agent_executions WHERE plan_id = 'g' AND node_id = ?", c.node) > 0 {
		c.execution = k.execution(c.node)
	}
	if c.effort == "" {
		c.effort = "high"
	}
	row := func(seq int, event, thread, turn string, result any, detail string) {
		k.exec("INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, commit_sha, thread_id, turn_id, plan_id, node_id, execution_id, result_bytes, detail_json, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			id, seq, c.run, c.role+":"+c.node, 0, "agent", c.role, event, c.head, thread, turn, "g", c.node, c.execution, result, detail, k.clock())
	}
	prepared := ""
	if c.resumeOf != "" {
		d, _ := json.Marshal(map[string]string{"resume_of": c.resumeOf, "thread_id": "thread-" + c.resumeOf, "configured_model": c.model, "configured_effort": c.effort})
		prepared = string(d)
	}
	row(1, "prepared", "", "", nil, prepared)
	thread, turn := "thread-"+id, "turn-"+id
	if c.resumeOf != "" {
		thread = "thread-" + c.resumeOf
	} else if !c.noPair {
		detail, _ := json.Marshal(map[string]string{"model": c.model, "effort": c.effort})
		row(2, "thread_started", thread, "", nil, string(detail))
	}
	var result any
	detail := ""
	if c.verdict != "" {
		doc := map[string]any{"verdict": c.verdict, "summary": "recorded by the test"}
		if c.selfReported {
			doc["criteria_digest"] = c.criteria
		} else {
			d, _ := json.Marshal(map[string]string{"criteria_digest": c.criteria})
			detail = string(d)
		}
		raw, _ := json.Marshal(doc)
		result = string(raw)
	}
	row(3, "completed", thread, turn, result, detail)
	return id
}

// unitCheckCall is one unit check the run records (CRW-1456): the completed row of the op crw.unitCheck, written the way the journal maps the op's Data (role, node, execution_id, commit, tree and
// criteria_digest) and its result (the reviewadapter.Result document).
type unitCheckCall struct {
	node string
	res  reviewadapter.Result
	// commit is the head the check ran on; empty for a check run before the commit (its row then names only the tree it saw).
	commit, tree string
	// criteria is the criteria digest the check was given (detail_json).
	criteria string
	// execution is the execution the row carries; the node's current one when empty.
	execution string
	// run is the run that recorded the row; run-1 when empty.
	run string
	// sub is the sub-task of a split node the check judged alone (CRW-1459); empty for a check of the node.
	sub string
}

// unitCheck writes the single completed flow_calls row of a crw.unitCheck op: an op row has no prepared or thread_started row, so its pair is the model and effort of its result. It returns the call id.
func (k *agentKit) unitCheck(c unitCheckCall) string {
	k.t.Helper()
	k.calls++
	id := fmt.Sprintf("call-%d", k.calls)
	if c.run == "" {
		c.run = "run-1"
	}
	if k.count("SELECT COUNT(*) FROM flow_runs WHERE run_id = ?", c.run) == 0 {
		k.exec("INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
			c.run, "project-lane", dig("script"), "{}", dig("args"), "g", 1, "parent", "nonce", "", k.clock())
	}
	if c.execution == "" {
		c.execution = k.execution(c.node)
	}
	data := map[string]string{"role": execution.AgentUnitCheck, "node": c.node, "execution_id": c.execution, "tree": c.tree, "criteria_digest": c.criteria}
	if c.commit != "" {
		data["commit"] = c.commit
	}
	if c.sub != "" {
		data["sub"] = c.sub
	}
	detail, _ := json.Marshal(data)
	result, _ := json.Marshal(c.res)
	k.exec("INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, commit_sha, tree_digest, thread_id, turn_id, plan_id, node_id, execution_id, result_bytes, detail_json, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, 1, c.run, "unitCheck:"+c.node, 0, "crw.unitCheck", execution.AgentUnitCheck, "completed", c.commit, c.tree, "", "", "g", c.node, c.execution, string(result), string(detail), k.clock())
	return id
}

// author records the executor call that wrote a node's commit.
func (k *agentKit) author(node, model string) string {
	k.t.Helper()
	return k.record(journalCall{role: "executor", node: node, model: model, head: k.heads[node]})
}

// review records a reviewer call on the node's head.
func (k *agentKit) review(node, model, verdict, criteria string) string {
	k.t.Helper()
	return k.record(journalCall{role: "reviewer", node: node, model: model, head: k.heads[node], verdict: verdict, criteria: criteria})
}

// judged records what a passing review leaves: the executor call of the author family and a verified reviewer call of another family under the criteria the plan holds.
func (k *agentKit) judged(node string) {
	k.t.Helper()
	k.author(node, authorModel)
	k.review(node, reviewerModel, "verified", releaseCriteriaDigest())
}

// treeOf is the tree of a commit.
func (k *agentKit) treeOf(commit string) string {
	k.t.Helper()
	return k.repo.git("rev-parse", commit+"^{tree}")
}

// input is the acceptance input of an agent node: the commit it names, the base, the checkout, a verification record sealed for its tree and a passing pre-merge record.
func (k *agentKit) input(node string) AcceptInput {
	k.t.Helper()
	head := k.heads[node]
	record := writeSealedRecord(k.t, k.repo.path, head, k.base, k.treeOf(head), "pass")
	return AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"},
		Commit:   &CommitRef{Head: head, Base: k.base, Checkout: k.repo.path, Record: record},
		Premerge: premergeAt(k.sched, context.Background(), "g", node, head)}
}

func (k *agentKit) accept(node string) (AcceptResult, error) {
	k.t.Helper()
	return k.sched.Accept(context.Background(), "g", node, "parent", k.input(node))
}

func (k *agentKit) mustAccept(node string) AcceptResult {
	k.t.Helper()
	res, err := k.accept(node)
	if err != nil {
		k.t.Fatalf("accept %s: %v", node, err)
	}
	return res
}

// acceptRelationship accepts a relationship node through the commit path.
func (k *agentKit) acceptRelationship(node string) {
	k.t.Helper()
	head := k.heads[node]
	record := writeSealedRecord(k.t, k.repo.path, head, k.base, k.treeOf(head), "pass")
	in := AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"}, Commit: &CommitRef{Head: head, Base: k.base, Checkout: k.repo.path, Record: record},
		Premerge: premergeAt(k.sched, context.Background(), "g", node, head)}
	if _, err := k.sched.Accept(context.Background(), "g", node, "parent", in); err != nil {
		k.t.Fatalf("accept %s: %v", node, err)
	}
}

// acceptAll judges and accepts every agent node of the kit that has a head, and accepts the relationship ones.
func (k *agentKit) acceptAll(specs ...agentSpec) {
	k.t.Helper()
	for _, n := range specs {
		if n.relationship {
			k.acceptRelationship(n.name)
			continue
		}
		k.judged(n.name)
		k.mustAccept(n.name)
	}
}

func (k *agentKit) batchIn() IntegrationBatchInput {
	return IntegrationBatchInput{Plan: "g", Actor: "parent", Checkout: k.repo.path, IntegrationRef: "dev-int", BaseRef: "dev"}
}

func (k *agentKit) integrate(t *testing.T) (IntegrationBatchResult, error) {
	t.Helper()
	return k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
}

// currentAcceptance is the node's current acceptance through the union reader.
func (k *agentKit) currentAcceptance(node string) NodeAcceptance {
	k.t.Helper()
	na, found, err := acceptanceOf(context.Background(), k.s.Q(context.Background()), "g", node)
	if err != nil || !found {
		k.t.Fatalf("node %s has no current acceptance: %v", node, err)
	}
	return na
}

// observe reads where the node's head landed, on the integration branch the batch moved.
func (k *agentKit) observe(node string) (IntegrationResult, error) {
	k.t.Helper()
	k.sched.Tips = newLegacyLocalTipReader(k.t)
	k.sched.Ancestry = GitAncestry{}.Ancestry
	return k.sched.ObserveIntegration(context.Background(), "g", node, "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
}

// agentStages are the node-level stage rows of agent acceptances.
func (k *agentKit) agentStages() []store.AgentIntegrationStageRow {
	k.t.Helper()
	rows, err := store.AgentIntegrationStagesOfPlan(context.Background(), k.sched.Store, "g")
	if err != nil {
		k.t.Fatal(err)
	}
	return rows
}

func countAgentStage(rows []store.AgentIntegrationStageRow, stage, node string) int {
	n := 0
	for _, r := range rows {
		if r.Stage == stage && (node == "" || r.NodeID == node) {
			n++
		}
	}
	return n
}

// slotState is the state and the reason of the node's slot.
func (k *agentKit) slotOf(node string) (state, reason string) {
	k.t.Helper()
	row := k.s.DB.QueryRow("SELECT state, COALESCE(release_reason, '') FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ? ORDER BY tenure DESC LIMIT 1", "g/"+node)
	if err := row.Scan(&state, &reason); err != nil {
		k.t.Fatalf("slot of %s: %v", node, err)
	}
	return state, reason
}
