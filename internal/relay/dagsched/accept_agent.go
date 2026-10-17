package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The agent path of dag-accept (R05, CRW-1213). A node whose current execution is an agent execution (dag_agent_executions) is accepted by the orchestrator that holds the plan, and the acceptance is a
// row of dag_agent_acceptances, never a row of a frozen table. The path requires five things together, and each refusal is its own reason:
//
//  1. the current agent execution of the node (a node with a closed or superseded execution has none);
//  2. a verified verdict the run recorded for this execution (flow_calls), of a reviewer call or of a unit check, under the criteria set the plan fixed and on the head under review;
//  3. the pre-merge record, judged by judgePremerge, the function the relationship path uses;
//  4. the verification record and the commit proof: the head is read by the relay from git, in the execution's worktree, and is never taken from the call (P-AV-2); it descends from the base and the
//     record is judged against its tree (JudgeVerificationRecord);
//  5. the plan's actor and epoch: the fence, the project's registered parent, and the orchestrator that holds the execution.
//
// The pre-merge and verification bodies are stored in the acceptance row, because dag_acceptance_premerge and dag_acceptance_verifications reference dag_acceptances. A node without a head (a
// non-implementation node) takes the reviewer, the execution and the actor, and returns its slot in the same transaction (dag_accepted), as the relationship path does. There is no pull request route and no
// 'local CI only' route: an agent node lands on the plan's local target branch, and this path refuses a pull request for an agent node. A pull request carries agent nodes only as a
// milestone's (PR-unit landing, CRW-1355): the nodes land on the milestone integration branch, whose one pull request the dev queue (internal/integrator) merges onto dev.
//
// What the run records, and what this path reads of it (the journal is written by the flow engine into flow_calls; these are the columns and keys the acceptance depends on):
//
//   - a reviewer call is the terminal row (event completed, kind agent, role reviewer or reviewer-final) of a call that carries the plan, the node and the execution; commit_sha is the head the review was
//     made on (empty for a node without a head); result_bytes is the canonical verdict document, whose member verdict is verified or needs_changes; detail_json names the criteria_digest the reviewer's input
//     carried (a digest the verdict document names itself is not read: it is the reviewer's claim, not a record of its input).
//   - a unit check is the completed row of the op crw.unitCheck (kind crw.unitCheck, role unit-check) that carries the plan, the node and the execution; result_bytes is the reviewadapter.Result the check
//     left (state, verdict, model, effort, engine_family); detail_json names the criteria_digest the check was given, read as a reviewer's is. It is on the head when commit_sha is the head, or, for a
//     check run before the commit, when commit_sha is empty and tree_digest is the tree of the head (a node without a head: commit_sha empty). Its model and effort are retained as provenance when
//     present; engine_family remains a readable historical field but does not gate acceptance. A check whose state is not ok gave no judgment: it is no basis and withdraws nothing.
//   - the latest reviewer call or unit check of the execution on the head decides, so a later needs_changes of either withdraws an earlier verified.
//   - the pair a call ran on is the configured pair the host read back for the call's thread: the thread_started row of the call, whose detail_json names model and effort. A continuation (resume) has no thread_started row: its prepared row names resume_of and the pair the thread was read back with (configured_model, configured_effort), and that is its pair.
//   - an author pair is the pair of a completed call of the node whose role is executor, executor-t1 or architect, whatever execution of the node it ran in, and the pair of the execution rows the node has.

// The closed reasons of the agent path. D-02: this build registers no new refusal reason, so each travels in the detail of a disposition_conflict.
const (
	ReviewerMissing     = "reviewer_missing"
	ReviewerNotVerified = "reviewer_not_verified"
	// These refusal labels remain exported for legacy readers of stored dispositions; acceptance no longer emits them.
	IndependenceRefused     = "independence_refused"
	IndependenceUnproven    = "independence_unproven"
	AgentWorktreeUnrecorded = "agent_worktree_unrecorded"
	AgentNotOrchestrator    = "agent_not_orchestrator"
)

// reviewerRoles and authorRoles are the roles of the flow engine whose calls are read as a review and as an authorship.
var (
	reviewerRoles = []string{execution.AgentReviewer, execution.AgentReviewerFinal}
	authorRoles   = []string{execution.AgentExecutor, execution.AgentExecutorT1, execution.AgentArchitect}
)

// agentPair is the pair a call ran on.
type agentPair struct{ Model, Effort string }

// reviewerCall is the verdict of the reviewer call that decides an acceptance, with the facts the acceptance row keeps of it.
type reviewerCall struct {
	CallID, ThreadID, TurnID string
	RunID                    string
	Pair                     agentPair
	Verdict, CriteriaDigest  string
	SameThreadAuthorIDs      []string
}

// agentProof is the commit proof of an implementation node, read from git before the acceptance transaction: the head, the base, the checkout both were proved in, the tree and the verification record.
type agentProof struct {
	head, base, checkout, tree string
	recordRaw                  []byte
}

// notOrchestrator is the refusal of a task that does not hold the agent execution of a node.
func notOrchestrator(actor string, e AgentExecution) error {
	return refuse(contract.RefusalScopeRoleMismatch, "%s: task %s is not the orchestrator of execution %s of node %s, which is held by %s", AgentNotOrchestrator, actor, e.ExecutionID, e.NodeID, e.Actor)
}

// acceptAgent is dag-accept for a node whose current execution is the agent execution e.
func (s *Scheduler) acceptAgent(ctx context.Context, snap dag.Snapshot, n dag.SnapNode, plan, node, actor string, e AgentExecution, in AcceptInput) (AcceptResult, error) {
	out := AcceptResult{PlanID: plan, NodeID: node, ExecutionID: e.ExecutionID, ExecutionKind: ExecutionAgent}
	q := s.Store.Q(ctx)
	// a node with a current execution on both sides is read as neither
	if _, _, err := executionOf(ctx, q, plan, node); err != nil {
		return out, err
	}
	implementation := n.Kind == dag.NodeImplementation
	switch {
	case in.PullRequest != nil:
		return out, refuse(contract.RefusalMalformedReceipt, "an agent node is accepted on its commit and never on a pull request: it lands on the plan's target branch, and a milestone's pull request goes to dev through the dev queue (crw manage integrate), so drop --repository and --pull-request")
	case implementation && in.Commit == nil:
		return out, refuse(contract.RefusalMalformedReceipt, "node %s is an agent implementation node: it is accepted with --commit, --base, --verification and --checkout, and the relay reads its head from git", node)
	case !implementation && in.Commit != nil:
		return out, refuse(contract.RefusalMalformedReceipt, "node %s is a %s node: it has no commit to accept", node, n.Kind)
	case !implementation && len(in.Premerge) > 0:
		return out, refuse(contract.RefusalMalformedReceipt, "node %s is a %s node: it takes no pre-merge record", node, n.Kind)
	}
	// the plan's actor: the registered parent of the project and the orchestrator that holds the execution (the epoch is fenced in the transaction below)
	var project string
	if has, err := queryOne(ctx, q, "SELECT project_key FROM dag_plans WHERE plan_id = ?", []any{plan}, &project); err != nil {
		return out, err
	} else if !has {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no revision", plan)
	}
	if err := requireProjectParent(ctx, q, project, actor); err != nil {
		return out, err
	}
	if e.Actor != actor {
		return out, notOrchestrator(actor, e)
	}
	var proof agentProof
	if implementation {
		var err error
		if proof, err = s.proveAgentCommit(ctx, e, *in.Commit); err != nil {
			return out, err
		}
	}
	if s.testBeforeAcceptTx != nil {
		s.testBeforeAcceptTx()
	}
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		if err := s.fence(txCtx, tx, plan, actor); err != nil {
			return err
		}
		current, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		cn, ok := nodeOf(current, node)
		if !ok || cn.SliceDigest != n.SliceDigest || cn.CriteriaSetDigest != n.CriteriaSetDigest {
			return refuse(contract.RefusalDispositionConflict, "the plan changed while the result of %s was being verified", node)
		}
		if err := lifecycleRefusal(current, cn, "accepting its result", false); err != nil {
			return err
		}
		// the execution read before the transaction is still the node's current one, and still held by the actor
		now, live, err := currentAgentExecution(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !live || now.ExecutionID != e.ExecutionID {
			return refuse(contract.RefusalStaleGeneration, "execution %s of %s is no longer the node's current execution: read the node again", e.ExecutionID, node)
		}
		if now.Actor != actor {
			return notOrchestrator(actor, now)
		}
		if implementation {
			if _, err := s.externalCommitProof(txCtx, now, *in.Commit); err != nil {
				return err
			}
		}
		if _, _, err := executionOf(txCtx, tx, plan, node); err != nil {
			return err
		}
		head := proof.head
		criteria := cn.CriteriaSetDigest
		id := AgentAcceptanceID(plan, node, e.ExecutionID, head, criteria)
		// the same output accepted before: a replay answers the stored row, whatever the reviewer has recorded since
		prior, replay, err := agentAcceptanceOfKey(txCtx, tx, e.ExecutionID, head, criteria)
		if err != nil {
			return err
		}
		end, hasEnd, err := agentAcceptanceChainEnd(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		endClosed := false
		if hasEnd {
			if _, endClosed, err = agentClosureOf(txCtx, tx, "acceptance", end.AcceptanceID); err != nil {
				return err
			}
		}
		if replay {
			if !hasEnd || end.AcceptanceID != prior.AcceptanceID || endClosed {
				return refuse(contract.RefusalDispositionConflict, "this output was accepted as %s and that acceptance is no longer the node's current one; accept a new output instead", prior.AcceptanceID)
			}
			out.AcceptanceID, out.HeadSHA, out.Replayed = prior.AcceptanceID, prior.Head, true
			if prior.Head != "" {
				out.EvidenceDigest = shaOf([]byte(prior.VerificationJSON))
			}
			return nil
		}
		// a new output follows the node's current acceptance only explicitly; after a closure it follows the closed one
		switch {
		case hasEnd && !endClosed:
			if in.Supersedes != end.AcceptanceID {
				return refuse(contract.RefusalDispositionConflict, "node %s already has the current acceptance %s of another output; supersede it explicitly", node, end.AcceptanceID)
			}
			out.SupersededID = end.AcceptanceID
		case hasEnd:
			if in.Supersedes != "" && in.Supersedes != end.AcceptanceID {
				return refuse(contract.RefusalDispositionConflict, "node %s has no current acceptance to supersede but %s", node, end.AcceptanceID)
			}
		case in.Supersedes != "":
			return refuse(contract.RefusalDispositionConflict, "node %s has no current acceptance to supersede", node)
		}
		review, authors, err := s.agentReview(txCtx, tx, plan, node, now, head, proof.tree)
		if err != nil {
			return err
		}
		if review.CriteriaDigest != criteria {
			return refuse(contract.RefusalCriteriaSetChanged, "the review %s saw criteria %s and the plan fixed %s for %s", review.CallID, orDash(review.CriteriaDigest), criteria, node)
		}
		var judged premergeJudgment
		repository := ""
		if implementation {
			// the target the head is judged against is the checkout the commit was proved in, and the node's outgoing integrated and code-pinned edges have to name the same one (acceptCommitTarget, as the commit path of a relationship)
			if repository, err = s.acceptCommitTarget(txCtx, tx, current, node, proof.checkout); err != nil {
				return err
			}
			if judged, err = judgePremerge(txCtx, in.Premerge, node, cn, head, &premergeCheckout{checkout: proof.checkout, base: proof.base}); err != nil {
				return err
			}
		}
		a := AgentAcceptance{AcceptanceID: id, PlanID: plan, NodeID: node, ManifestDigest: now.ManifestDigest, ExecutionID: e.ExecutionID, CriteriaSetDigest: criteria,
			ReviewerCallID: review.CallID, ReviewerThreadID: review.ThreadID, ReviewerTurnID: review.TurnID, ReviewerModel: review.Pair.Model, ReviewerEffort: review.Pair.Effort,
			AuthorPairsDigest: authorPairsDigest(authors), AcceptedBy: actor, Epoch: s.ExpectedEpoch, AcceptedAt: s.now(), Supersedes: end.AcceptanceID}
		if !hasEnd {
			a.Supersedes = ""
		}
		if implementation {
			a.Head, a.Base, a.Repository, a.TreeSHA = head, proof.base, repository, proof.tree
			a.PremergeDigest, a.PremergeJSON, a.PremergeEvaluatedHead = judged.digest, string(judged.raw), judged.evaluatedHead
			a.VerificationDigest, a.VerificationJSON = shaOf(proof.recordRaw), string(proof.recordRaw)
		}
		if err := recordSameThreadWarning(txCtx, tx, review, node, a.AcceptedAt); err != nil {
			// The warning is advisory: keep acceptance fail-open, but surface a failed journal write to the process log.
			log.Printf("dagsched: failed to persist review_same_thread warning for plan=%q node=%q reviewer_call=%q: %v", plan, node, review.CallID, err)
		}
		var supersedes any
		if a.Supersedes != "" {
			supersedes = a.Supersedes
		}
		if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_agent_acceptances ("+agentAcceptanceInsertColumns+") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			a.AcceptanceID, a.PlanID, a.NodeID, a.ManifestDigest, a.ExecutionID, a.Head, a.Base, a.Repository, a.PremergeDigest, a.PremergeJSON, a.PremergeEvaluatedHead,
			a.VerificationDigest, a.VerificationJSON, a.TreeSHA, a.CriteriaSetDigest, a.ReviewerCallID, a.ReviewerThreadID, a.ReviewerTurnID, a.ReviewerModel, a.ReviewerEffort,
			a.AuthorPairsDigest, a.AcceptedBy, a.Epoch, a.AcceptedAt, supersedes); err != nil {
			return err
		}
		if !implementation {
			// a node without a head has nothing to integrate: its slot goes back with its acceptance, as the relationship path returns it
			if out.SlotReleased, err = s.releaseSlot(txCtx, tx, plan, node, actor, "dag_accepted"); err != nil {
				return err
			}
		}
		out.AcceptanceID, out.HeadSHA = a.AcceptanceID, a.Head
		if implementation {
			out.EvidenceDigest = a.VerificationDigest
		}
		return nil
	})
	return out, err
}

// agentAcceptanceOfKey is the acceptance row of an execution's head under a criteria set, current or not.
func agentAcceptanceOfKey(ctx context.Context, q store.Querier, executionID, head, criteria string) (AgentAcceptance, bool, error) {
	has, err := tableExists(ctx, q, "dag_agent_acceptances")
	if err != nil || !has {
		return AgentAcceptance{}, false, err
	}
	a, err := scanAgentAcceptance(q.QueryRowContext(ctx, "SELECT "+agentAcceptanceColumns+" FROM dag_agent_acceptances WHERE execution_id = ? AND head = ? AND criteria_set_digest = ?", executionID, head, criteria))
	if err == sql.ErrNoRows {
		return AgentAcceptance{}, false, nil
	}
	return a, err == nil, err
}

// proveAgentCommit reads the head of an agent implementation node from git and proves it, before the acceptance transaction: the head is what the execution's worktree is at (never what the call says: a
// head the call names that git does not read is refused), it descends from the base in the checkout, and the verification record is judged against its tree, base, ci.yml, dependencies and host.
func (s *Scheduler) proveAgentCommit(ctx context.Context, e AgentExecution, in CommitRef) (agentProof, error) {
	var out agentProof
	var externalErr error
	e, externalErr = s.externalCommitProof(ctx, e, in)
	if externalErr != nil {
		return out, externalErr
	}
	if in.Base == "" || in.Checkout == "" || in.Record == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "an agent acceptance names --base, --verification and --checkout; the relay reads the head from git")
	}
	if !refreshCommitPattern.MatchString(in.Base) || (in.Head != "" && !refreshCommitPattern.MatchString(in.Head)) {
		return out, refuse(contract.RefusalMalformedReceipt, "--commit and --base are full commit ids, not %q and %q", in.Head, in.Base)
	}
	if !filepath.IsAbs(in.Checkout) {
		return out, refuse(contract.RefusalMalformedReceipt, "--checkout is an absolute path to a local checkout, not %q", in.Checkout)
	}
	if e.Worktree == "" {
		return out, refuse(contract.RefusalDispositionConflict, "%s: execution %s records no worktree, so the relay cannot read the head of %s from git", AgentWorktreeUnrecorded, e.ExecutionID, e.NodeID)
	}
	raw, err := readVerificationRecord(in.Record)
	if err != nil {
		return out, err
	}
	head, err := runGit(ctx, e.Worktree, nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read the head of the worktree %s of execution %s: %v", e.Worktree, e.ExecutionID, err)
	}
	head = strings.TrimSpace(head)
	if in.Head != "" && !strings.EqualFold(in.Head, head) {
		return out, refuse(contract.RefusalHeadNotReceiptHead, "the call names head %s and git reads %s in the worktree %s of execution %s: the relay accepts the head it reads", in.Head, head, e.Worktree, e.ExecutionID)
	}
	if ancestor, err := commitIsAncestor(ctx, in.Checkout, in.Base, head); err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s in %s: %v", head, in.Checkout, err)
	} else if !ancestor {
		return out, refuse(contract.RefusalMergeBaseMismatch, "%s is not an ancestor of %s in %s: the commit does not descend from the base", in.Base, head, in.Checkout)
	}
	base, err := runGit(ctx, in.Checkout, nil, "rev-parse", "--verify", in.Base+"^{commit}")
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not resolve the base %s in %s: %v", in.Base, in.Checkout, err)
	}
	keys, err := CommitVerificationKeys(ctx, in.Checkout, head)
	if err != nil {
		return out, err
	}
	keys.Base = strings.TrimSpace(base)
	keys.OS, keys.Arch = runtime.GOOS, runtime.GOARCH
	if _, err := JudgeVerificationRecord(raw, keys); err != nil {
		return out, err
	}
	return agentProof{head: head, base: in.Base, checkout: in.Checkout, tree: keys.Tree, recordRaw: raw}, nil
}

// unitCheckKind is the kind of the flow_calls row of a unit check: the label of the op crw.unitCheck (flow/ops OpUnitCheck), whose row names the role execution.AgentUnitCheck.
const unitCheckKind = "crw.unitCheck"

// unitCheckResult is what the relay reads of the reviewadapter.Result a unit check leaves in result_bytes.
type unitCheckResult struct {
	State        string `json:"state"`
	Verdict      string `json:"verdict"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	EngineFamily string `json:"engine_family"`
}

// unitCheckOnHead tells whether a unit check row is on the head under acceptance: it names the head as its commit, or it names no commit (a check run before the commit) and saw the tree of the head.
// A node without a head (head and tree empty) takes a check that names no commit.
func unitCheckOnHead(commit, treeDigest, head, tree string) bool {
	if commit != "" || head == "" {
		return strings.EqualFold(commit, head)
	}
	return tree != "" && strings.EqualFold(treeDigest, tree)
}

// agentReview reads, in the caller's transaction, the verdict that decides an acceptance and the author pairs it is judged against. The latest completed reviewer call or unit check of the execution on
// the head decides (tree is the tree of the head, which a check run before the commit is matched on; a unit check that gave no judgment is skipped): it has to be verified, to have seen the criteria set
// in force (compared by the caller). Model and effort observations are retained when available; model family does not gate acceptance. It answers the author pairs observed, sorted, for the digest the
// acceptance keeps.
func (s *Scheduler) agentReview(ctx context.Context, q store.Querier, plan, node string, e AgentExecution, head, tree string) (reviewerCall, []agentPair, error) {
	var review reviewerCall
	has, err := tableExists(ctx, q, "flow_calls")
	if err != nil {
		return review, nil, err
	}
	if !has {
		return review, nil, refuse(contract.RefusalDispositionConflict, "%s: this store records no flow journal, so no reviewer call of execution %s can be read", ReviewerMissing, e.ExecutionID)
	}
	rows, err := q.QueryContext(ctx, "SELECT call_id, run_id, kind, thread_id, turn_id, commit_sha, tree_digest, COALESCE(CAST(result_bytes AS TEXT), ''), detail_json FROM flow_calls"+
		" WHERE event = 'completed' AND plan_id = ? AND node_id = ? AND execution_id = ?"+
		" AND ((kind = 'agent' AND role IN ('"+strings.Join(reviewerRoles, "','")+"')) OR (kind = ? AND role = ?)) ORDER BY rowid DESC", plan, node, e.ExecutionID, unitCheckKind, execution.AgentUnitCheck)
	if err != nil {
		return review, nil, err
	}
	var decided, unit bool
	var run, result, detail string
	var check unitCheckResult
	for rows.Next() {
		var call, runID, kind, thread, turn, commit, treeDigest, resultText, detailText string
		if err := rows.Scan(&call, &runID, &kind, &thread, &turn, &commit, &treeDigest, &resultText, &detailText); err != nil {
			_ = rows.Close()
			return review, nil, err
		}
		// the run that holds the execution recorded the call
		if e.RunID != "" && runID != e.RunID {
			continue
		}
		if kind == unitCheckKind {
			// a unit check on the head that gave a judgment; one that gave none (no answer, quota, a failed tool, a changed worktree, an invalid answer) is no basis and withdraws nothing
			check = unitCheckResult{}
			if json.Unmarshal([]byte(resultText), &check) != nil || check.State != "ok" || !unitCheckOnHead(commit, treeDigest, head, tree) {
				continue
			}
			review.CallID, run, detail, unit, decided = call, runID, detailText, true, true
			break
		}
		// a head under review is the head the acceptance stands on
		if !strings.EqualFold(commit, head) {
			continue
		}
		review.CallID, review.ThreadID, review.TurnID, run, result, detail, decided = call, thread, turn, runID, resultText, detailText, true
		break
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return review, nil, err
	}
	if err := rows.Close(); err != nil {
		return review, nil, err
	}
	if !decided {
		return review, nil, refuse(contract.RefusalDispositionConflict, "%s: the run recorded no completed reviewer call or unit check of execution %s on head %s, so the result has no verdict", ReviewerMissing, e.ExecutionID, orDash(head))
	}
	review.RunID = run
	// the criteria the reviewer saw are the digest its input recorded (detail_json), never a digest the reviewer wrote into its own verdict: that would be a claim about the input, not a record of it
	var seen struct {
		CriteriaDigest string `json:"criteria_digest"`
	}
	_ = json.Unmarshal([]byte(detail), &seen)
	review.CriteriaDigest = seen.CriteriaDigest
	label := "reviewer call"
	if unit {
		label = "unit check"
		review.Verdict = check.Verdict
	} else {
		var verdict struct {
			Verdict string `json:"verdict"`
		}
		_ = json.Unmarshal([]byte(result), &verdict)
		review.Verdict = verdict.Verdict
	}
	if review.Verdict != "verified" {
		return review, nil, refuse(contract.RefusalDispositionConflict, "%s: the latest %s %s of execution %s ruled %q, not verified", ReviewerNotVerified, label, review.CallID, e.ExecutionID, review.Verdict)
	}
	if unit {
		// an op row has no thread: the pair is the one the check's result names
		review.Pair = agentPair{Model: check.Model, Effort: check.Effort}
		if review.Pair.Model == "" {
			return review, nil, refuse(contract.RefusalDispositionConflict, "%s: unit check %s records no model for the acceptance provenance", ReviewerMissing, review.CallID)
		}
	} else {
		if review.Pair, err = callPair(ctx, q, review.CallID, run); err != nil {
			return review, nil, err
		}
		if review.Pair.Model == "" {
			return review, nil, refuse(contract.RefusalDispositionConflict, "%s: reviewer call %s has no host-read model for the acceptance provenance", ReviewerMissing, review.CallID)
		}
	}
	authors, err := authorPairs(ctx, q, plan, node, e)
	if err != nil {
		return review, nil, err
	}
	if !unit && review.ThreadID != "" {
		review.SameThreadAuthorIDs, err = authorCallsOnThread(ctx, q, plan, node, review.ThreadID)
		if err != nil {
			return review, nil, err
		}
	}
	return review, authors, nil
}

// authorCallsOnThread finds author call IDs with an observed attempt on the reviewer's thread. thread_started rows retain prior attempts
// for session calls, so this covers earlier attempts when the journal recorded them. Tool reviews have no thread and never reach here.
func authorCallsOnThread(ctx context.Context, q store.Querier, plan, node, thread string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT call_id FROM flow_calls WHERE kind = 'agent' AND role IN ('"+strings.Join(authorRoles, "','")+"') AND plan_id = ? AND node_id = ? AND thread_id = ? AND event IN ('thread_started', 'session_attempt_lost', 'completed') ORDER BY call_id", plan, node, thread)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []string
	for rows.Next() {
		var call string
		if err := rows.Scan(&call); err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

// recordSameThreadWarning appends an advisory event to the existing flow journal. It records the observed collision and never changes
// the acceptance verdict. The foreign sequence range keeps this write separate from the run's own append-only journal sequence.
func recordSameThreadWarning(ctx context.Context, q store.Querier, review reviewerCall, node, recordedAt string) error {
	if len(review.SameThreadAuthorIDs) == 0 {
		return nil
	}
	// Keep aligned with flow/engine.ForeignSeqBase: command-written flow events use a disjoint sequence range.
	const foreignSeqBase int64 = 1 << 40
	var top sql.NullInt64
	if err := q.QueryRowContext(ctx, "SELECT MAX(seq) FROM flow_events WHERE run_id = ? AND seq >= ?", review.RunID, foreignSeqBase).Scan(&top); err != nil {
		return err
	}
	seq := foreignSeqBase
	if top.Valid {
		seq = top.Int64 + 1
	}
	payload, err := json.Marshal(map[string]any{
		"_kind":                    "flow",
		"warning":                  "review_same_thread",
		"reviewer_call_id":         review.CallID,
		"reviewer_thread_id":       review.ThreadID,
		"same_thread_author_calls": review.SameThreadAuthorIDs,
	})
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, "INSERT INTO flow_events (run_id, seq, kind, call_id, node_id, subject, payload_json, recorded_at) VALUES (?,?,?,?,?,?,?,?)",
		review.RunID, seq, "review_same_thread", review.CallID, node, "review_same_thread", string(payload), recordedAt)
	return err
}

// callPair is the pair a call's thread was configured with, as the host read it back. A call that opened a thread has the thread_started row. A continuation (resume) opens none: the engine records, in its
// prepared row, resume_of and the pair the continued thread was read back with (configured_model, configured_effort), and that is its pair. Empty when the call has neither.
func callPair(ctx context.Context, q store.Querier, call, run string) (agentPair, error) {
	var detail string
	found, err := queryOne(ctx, q, "SELECT detail_json FROM flow_calls WHERE call_id = ? AND run_id = ? AND event = 'thread_started' ORDER BY event_seq DESC LIMIT 1", []any{call, run}, &detail)
	if err != nil {
		return agentPair{}, err
	}
	if found {
		var pair struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		}
		_ = json.Unmarshal([]byte(detail), &pair)
		return agentPair{Model: pair.Model, Effort: pair.Effort}, nil
	}
	if found, err = queryOne(ctx, q, "SELECT detail_json FROM flow_calls WHERE call_id = ? AND run_id = ? AND event = 'prepared' ORDER BY event_seq DESC LIMIT 1", []any{call, run}, &detail); err != nil || !found {
		return agentPair{}, err
	}
	var prepared struct {
		ResumeOf string `json:"resume_of"`
		Model    string `json:"configured_model"`
		Effort   string `json:"configured_effort"`
	}
	_ = json.Unmarshal([]byte(detail), &prepared)
	if prepared.ResumeOf == "" {
		return agentPair{}, nil
	}
	return agentPair{Model: prepared.Model, Effort: prepared.Effort}, nil
}

// priorPairs are the pairs the earlier attempts of a session call were bound with: the prior_pairs of its newest thread_started row.
// A call of the App Server path names none.
func priorPairs(ctx context.Context, q store.Querier, call, run string) ([]agentPair, error) {
	var detail string
	found, err := queryOne(ctx, q, "SELECT detail_json FROM flow_calls WHERE call_id = ? AND run_id = ? AND event = 'thread_started' ORDER BY event_seq DESC LIMIT 1", []any{call, run}, &detail)
	if err != nil || !found {
		return nil, err
	}
	var row struct {
		Prior []struct {
			Model  string `json:"model"`
			Effort string `json:"effort"`
		} `json:"prior_pairs"`
	}
	_ = json.Unmarshal([]byte(detail), &row)
	out := make([]agentPair, 0, len(row.Prior))
	for _, p := range row.Prior {
		out = append(out, agentPair{Model: p.Model, Effort: p.Effort})
	}
	return out, nil
}

// authorPairs are the distinct pairs the node's author calls ran on, sorted: every completed executor, executor-t1 or architect call of the node (whatever execution of its chain it ran in: the commits of
// an earlier attempt are in the head too), and the pair the execution rows of the node record. A pair the host never read back is not an observed one.
func authorPairs(ctx context.Context, q store.Querier, plan, node string, e AgentExecution) ([]agentPair, error) {
	seen := map[agentPair]bool{}
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT call_id, run_id FROM flow_calls WHERE kind = 'agent' AND event = 'completed' AND role IN ('"+strings.Join(authorRoles, "','")+"') AND plan_id = ? AND node_id = ?", plan, node)
	if err != nil {
		return nil, err
	}
	type callRun struct{ call, run string }
	var calls []callRun
	for rows.Next() {
		var c callRun
		if err := rows.Scan(&c.call, &c.run); err != nil {
			_ = rows.Close()
			return nil, err
		}
		calls = append(calls, c)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, c := range calls {
		pair, err := callPair(ctx, q, c.call, c.run)
		if err != nil {
			return nil, err
		}
		if pair.Model != "" {
			seen[pair] = true
		}
		// a session call's earlier attempts, whose edits stay in the worktree, ran on the pairs its newest thread_started row
		// names (CRW-1316 §4)
		prior, err := priorPairs(ctx, q, c.call, c.run)
		if err != nil {
			return nil, err
		}
		for _, p := range prior {
			if p.Model != "" {
				seen[p] = true
			}
		}
	}
	chain, err := q.QueryContext(ctx, "SELECT DISTINCT author_model, author_effort FROM dag_agent_executions WHERE plan_id = ? AND node_id = ? AND author_model <> ''", plan, node)
	if err != nil {
		return nil, err
	}
	for chain.Next() {
		var p agentPair
		if err := chain.Scan(&p.Model, &p.Effort); err != nil {
			_ = chain.Close()
			return nil, err
		}
		seen[p] = true
	}
	if err := chain.Err(); err != nil {
		_ = chain.Close()
		return nil, err
	}
	if err := chain.Close(); err != nil {
		return nil, err
	}
	out := make([]agentPair, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].Effort < out[j].Effort
	})
	return out, nil
}

// authorPairsDigest is the digest of the observed author pairs an acceptance was judged against.
func authorPairsDigest(pairs []agentPair) string {
	list := make([]string, 0, len(pairs))
	for _, p := range pairs {
		list = append(list, p.Model+"@"+p.Effort)
	}
	return dag.Digest(list)
}

// premergeOfAgentAcceptance judges the pre-merge record stored in an agent acceptance for node n, as premergeOfAcceptance judges the record stored with a relationship's: integration reads the stored text,
// never a file, the stored digest must match the text, and the same judgement as acceptance applies, against the checkout and base the acceptance proved the head in.
func (s *Scheduler) premergeOfAgentAcceptance(ctx context.Context, q store.Querier, a AgentAcceptance, n dag.SnapNode) (premergeJudgment, error) {
	if a.PremergeJSON == "" {
		return premergeJudgment{}, refuse(contract.RefusalPremergeMissing, "acceptance %s has no pre-merge record", a.AcceptanceID)
	}
	j, err := judgePremerge(ctx, []byte(a.PremergeJSON), a.NodeID, n, a.Head, &premergeCheckout{checkout: a.Repository, base: a.Base})
	if err != nil {
		return premergeJudgment{}, err
	}
	if j.digest != a.PremergeDigest {
		return premergeJudgment{}, refuse(contract.RefusalPremergeMissing, "the stored pre-merge record of acceptance %s does not match its digest", a.AcceptanceID)
	}
	return j, nil
}
