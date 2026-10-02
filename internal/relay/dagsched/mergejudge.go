package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// JudgeInput optionally names the pull request the parent means: it must be the one recorded with the acceptance.
type JudgeInput struct{ PullRequest *PRRef }

// JudgeResult is the answer of Judge: the outcome of this judgement of the accepted pull request, the round it belongs to (a flaky required check is retried once on the same head and the
// second failure evicts, decision D-12), the row it left (CheckSeq) and whether it only restated the latest row.
type JudgeResult struct {
	PlanID, NodeID, AcceptanceID, Outcome, Reason string
	Round                                         int
	CheckSeq                                      int64
	HeadSHA, ObservedHeadSHA, BaseTipSHA, BaseRef string
	FailedRequired                                []string
	Replayed                                      bool
}

// Eligible is whether the judgement lets the pull request go to the merge lane.
func (r JudgeResult) Eligible() bool { return r.Outcome == OutcomeEligible }

const maxRounds = 2

// completedConclusions are the answers a finished check gives; anything else (empty, pending, queued, in_progress) has not finished.
var completedConclusions = map[string]bool{"success": true, "failure": true, "neutral": true, "skipped": true, "cancelled": true, "timed_out": true, "action_required": true, "startup_failure": true, "stale": true}

// requiredChecks classifies the checks the base branch requires for the exact head of the pull request, the way merge-evidence and the merge lane's own check read them: of every run
// only its newest attempt counts, every such run of a required name (and of the provider the branch rule names for it) has to be a success, and no run ordering is guessed from the
// opaque run ids. A required name with no run on that head, or with a run that has not finished, is pending; the runs that finished and are not a success are the failures. They count only
// when every required check has finished, so one red job seen before its siblings finish never uses up the retry.
func requiredChecks(pr PullRequest) (pending, failed []string) {
	newest := map[string]Check{}
	for _, c := range pr.Checks {
		if c.HeadSHA != pr.HeadSHA {
			continue
		}
		if cur, ok := newest[c.RunID]; !ok || c.Attempt > cur.Attempt {
			newest[c.RunID] = c
		}
	}
	runs := make([]string, 0, len(newest))
	for id := range newest {
		runs = append(runs, id)
	}
	slices.Sort(runs)
	for _, name := range pr.RequiredDeclared {
		providers := pr.RequiredProviders[name]
		answered := false
		for _, id := range runs {
			c := newest[id]
			if c.Name != name || (len(providers) > 0 && !slices.Contains(providers, c.Provider)) {
				continue
			}
			answered = true
			switch {
			case !completedConclusions[c.Conclusion]:
				pending = append(pending, name+": run "+id+" has not finished")
			case c.Conclusion != "success":
				failed = append(failed, name+"|"+id+"|"+strconv.FormatInt(c.Attempt, 10))
			}
		}
		if !answered {
			pending = append(pending, name+": no run of the head")
		}
	}
	slices.Sort(pending)
	slices.Sort(failed)
	return pending, failed
}

// mergeable reads the relationship of an accepted result for a judgement: not cancelled or paused (contract 3.2), and the caller is its parent. An archived relationship is fine, its child's
// work ended and the pull request still has to land.
func mergeable(ctx context.Context, q store.Querier, acc Acceptance, actor string) error {
	rel, found, err := loadRelationship(ctx, q, acc.RelationshipID)
	if err != nil {
		return err
	}
	if !found {
		return refuse(contract.RefusalUnregisteredRelationship, "relationship %s of the accepted result is not in the store", acc.RelationshipID)
	}
	if rel.Status == "paused" || rel.Status == "cancelled" || rel.Superseded {
		return refuse(contract.RefusalRelationshipNotActive, "the relationship %s is %s: nothing is judged for merge while it is", rel.ID, map[bool]string{true: "superseded", false: rel.Status}[rel.Superseded])
	}
	if rel.ParentTaskID != actor {
		return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the parent of relationship %s, which is held by %s", actor, rel.ID, rel.ParentTaskID)
	}
	return nil
}

// Judge decides whether the accepted pull request of an implementation node may go to the merge lane (criterion c8). The relay reads the pull request and the base branch itself, outside any
// transaction, and then one transaction applies the rules in order, the first decisive one winning, and appends the judgement to the node's history of merge checks (a judgement that
// restates the latest one writes nothing):
//
//  0. an evicted head is evicted for good: nothing later can bring that head back, a new head needs a new acceptance;
//  1. stale_criteria: the plan's or the relationship's criteria are no longer the ones the acceptance stands on;
//  2. stale_head: the pull request is at another head than the accepted one (E-10);
//  3. predecessor_not_landed: an incoming integrated edge or code-pinned edge (a stacked pull request) whose predecessor has not landed;
//  4. stale_base: the base branch tip is not contained in the head, so the checks did not run on a tree that contains it. The dev ruleset is strict (D-11), so a head that contains the tip is
//     the tree the base would become; the pull request's own base field says nothing about what the checks ran against and is stored for information only;
//  5. the required checks of the exact head (requiredChecks): pending, eligible, retry_same_sha on the first failure, evicted on a second, different failure.
//
// A pull request whose evidence cannot be read completely, or whose list of required checks is unknown, is not judged and writes nothing: ignorance is not "none required".
func (s *Scheduler) Judge(ctx context.Context, plan, node, actor string, in JudgeInput) (JudgeResult, error) {
	out := JudgeResult{PlanID: plan, NodeID: node}
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return out, err
	}
	n, ok := nodeOf(snap, node)
	if !ok {
		return out, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
	}
	if n.Kind != dag.NodeImplementation {
		return out, refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it has no pull request to merge", node, n.Kind)
	}
	acc, has, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return out, err
	}
	if !has || acc.HeadSHA == "" {
		return out, refuse(contract.RefusalDispositionConflict, "node %s has no accepted head to judge", node)
	}
	out.AcceptanceID, out.HeadSHA = acc.AcceptanceID, acc.HeadSHA
	if err := mergeable(ctx, q, acc, actor); err != nil {
		return out, err
	}
	var forge string
	var number int64
	if has, err := queryOne(ctx, q, "SELECT forge_repository, pr_number FROM dag_acceptance_forge WHERE acceptance_id = ?", []any{acc.AcceptanceID}, &forge, &number); err != nil {
		return out, err
	} else if !has {
		return out, refuse(contract.RefusalDispositionConflict, "acceptance %s has no forge identity recorded, so there is no pull request to read", acc.AcceptanceID)
	}
	if in.PullRequest != nil && (in.PullRequest.Repository != forge || in.PullRequest.Number != number) {
		return out, refuse(contract.RefusalDispositionConflict, "the accepted pull request of %s is %s#%d and the call names %s#%d", node, forge, number, in.PullRequest.Repository, in.PullRequest.Number)
	}
	if s.PRs == nil || s.Tips == nil || s.Ancestry == nil {
		return out, fmt.Errorf("this scheduler has no pull request reader, target reader or ancestry check, so it cannot judge a merge")
	}
	pr, err := s.PRs(ctx, forge, number)
	if err != nil {
		return out, err
	}
	if err := ClassifyPullRequest(pr); err != nil {
		return out, err
	}
	if pr.State != "open" || pr.IsDraft {
		return out, refuse(contract.RefusalDispositionConflict, "pull request %s#%d is %s%s: only an open, non-draft pull request goes to the merge lane", forge, number, pr.State, map[bool]string{true: " and a draft", false: ""}[pr.IsDraft])
	}
	if !pr.RequiredReadable {
		return out, refuseEvidenceMalformed("the checks the base branch of %s#%d requires could not be read, so a failing required check cannot be told from an optional one", forge, number)
	}
	tip, err := s.Tips.Tip(ctx, acc.Repository, pr.BaseRef)
	if err != nil {
		return out, err
	}
	out.ObservedHeadSHA, out.BaseTipSHA, out.BaseRef = pr.HeadSHA, tip.SHA, pr.BaseRef
	// whether the tip is in the head is asked only about the accepted head; for another head the judgement is stale_head before it matters
	var tipInHead *bool
	if pr.HeadSHA == acc.HeadSHA {
		contained, _, err := s.Ancestry(ctx, acc.Repository, tip.SHA, pr.HeadSHA)
		if err != nil {
			return out, err
		}
		tipInHead = &contained
	}
	err = s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		current, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		cn, ok := nodeOf(current, node)
		if !ok {
			return refuse(contract.RefusalUnregisteredScope, "plan %s no longer has the live node %s", plan, node)
		}
		still, found, err := loadActiveAcceptance(txCtx, tx, plan, node)
		if err != nil {
			return err
		}
		if !found || still.AcceptanceID != acc.AcceptanceID {
			return refuse(contract.RefusalDispositionConflict, "the accepted result of %s changed while its pull request was being judged", node)
		}
		if err := mergeable(txCtx, tx, acc, actor); err != nil {
			return err
		}
		history, err := loadMergeHistory(txCtx, tx, plan, node, forge, number, pr.HeadSHA)
		if err != nil {
			return err
		}
		if history.evicted != nil {
			// the eviction of this head of this pull request is final, whichever acceptance recorded it: a new acceptance of the same head does not bring back the retry the head used up.
			// The acceptance in force carries the eviction too (the reading blocks it from its own history); an acceptance that already has it is only restated.
			e := history.evicted
			out.Outcome, out.Reason, out.Round, out.CheckSeq, out.FailedRequired, out.Replayed = e.outcome, e.reason, e.round, e.seq, e.failed, true
			var one int
			own, err := queryOne(txCtx, tx, "SELECT 1 FROM dag_merge_checks WHERE acceptance_id = ? AND outcome = ? AND observed_head_sha = ?", []any{acc.AcceptanceID, OutcomeEvicted, pr.HeadSHA}, &one)
			if err != nil || own {
				return err
			}
			seq, _, err := s.appendMergeCheck(txCtx, tx, mergeCheck{Acceptance: acc, Observed: pr, BaseTip: tip.SHA, ChecksBase: pr.BaseSHA, Failed: e.failed, Round: maxRounds, Outcome: OutcomeEvicted, Reason: e.reason})
			out.CheckSeq, out.Replayed = seq, false
			return err
		}
		effective, err := effectiveCriteria(txCtx, tx, acc)
		if err != nil {
			return err
		}
		var rows, distinct int
		var canonical sql.NullString
		if _, err := queryOne(txCtx, tx, "SELECT COUNT(*), COUNT(DISTINCT set_digest), MIN(set_digest) FROM canonical_criteria WHERE relationship_id = ?", []any{acc.RelationshipID}, &rows, &distinct, &canonical); err != nil {
			return err
		}
		m := mergeCheck{Acceptance: acc, Observed: pr, BaseTip: tip.SHA, ChecksBase: pr.BaseSHA, Round: min(1+history.retries, maxRounds)}
		switch {
		case cn.CriteriaSetDigest != effective || rows == 0 || distinct != 1 || canonical.String != effective:
			m.Outcome, m.Reason = OutcomeStaleCriteria, "the criteria of the plan or of the relationship are no longer the ones the acceptance stands on"
		case pr.HeadSHA != acc.HeadSHA:
			m.Outcome, m.Reason = OutcomeStaleHead, "the pull request head is "+pr.HeadSHA+" and the accepted head is "+acc.HeadSHA
		default:
			landed, why, err := s.predecessorsLanded(txCtx, tx, plan, current, node)
			if err != nil {
				return err
			}
			switch {
			case !landed:
				m.Outcome, m.Reason = OutcomePredecessor, why
			case tipInHead != nil && !*tipInHead:
				m.Outcome, m.Reason = OutcomeStaleBase, "the base branch is at "+tip.SHA+" and the head does not contain it: the checks did not run on the tree that would land"
			default:
				s.judgeChecks(&m, history)
			}
		}
		seq, appended, err := s.appendMergeCheck(txCtx, tx, m)
		if err != nil {
			return err
		}
		out.Outcome, out.Reason, out.Round, out.CheckSeq, out.FailedRequired, out.Replayed = m.Outcome, m.Reason, m.Round, seq, m.Failed, !appended
		return nil
	})
	return out, err
}

// judgeChecks applies the required-checks rule (5) to m, given the history of the same head.
func (s *Scheduler) judgeChecks(m *mergeCheck, h mergeHistory) {
	pending, failed := requiredChecks(m.Observed)
	if len(pending) == 0 {
		// failures count only once every required check has finished; a red job seen beside a running one is not recorded
		m.Failed = failed
	}
	switch {
	case len(pending) > 0:
		m.Outcome, m.Reason = OutcomeChecksPending, "required checks have not finished on this head: "+strings.Join(pending, "; ")
	case len(failed) == 0 && len(m.Observed.CheckProblems) > 0:
		m.Outcome, m.Reason = OutcomeChecksPending, "the checks of this head are not usable evidence: "+strings.Join(m.Observed.CheckProblems, "; ")
	case len(failed) == 0:
		m.Outcome, m.Reason = OutcomeEligible, "every required check passed on this head"
		if h.failures > 0 {
			m.Reason = "every required check passed on this head after a recorded failure (a flaky check)"
		}
	case h.retries == 0:
		m.Outcome, m.Round, m.Reason = OutcomeRetrySameSHA, 1, "required checks failed ("+strings.Join(failed, ", ")+"): run them again on the same head, once"
	case subset(failed, h.failedBefore):
		// the snapshot the retry round was opened on, read again: it is not a second failure
		m.Outcome, m.Round, m.Reason = OutcomeRetrySameSHA, 1, "required checks failed ("+strings.Join(failed, ", ")+") and the retry round is open: run them again on the same head, once"
	default:
		m.Outcome, m.Round, m.Reason = OutcomeEvicted, maxRounds, "a required check failed again on the same head after its retry ("+strings.Join(failed, ", ")+"): the node left the merge lane"
	}
}

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

type historicRow struct {
	seq             int64
	acceptance      string
	outcome, reason string
	round           int
	failed          []string
}

// mergeHistory is what the judgements of one head already say: how many retry rounds were opened, every failed check seen, and the eviction when there is one.
type mergeHistory struct {
	retries      int
	failures     int
	failedBefore []string
	evicted      *historicRow
}

// loadMergeHistory reads what the judgements of one head of one pull request of a node already say, across every acceptance the node had of it: a head that used up its retry or was evicted
// stays so when the node is accepted again at the same head. The rows come in the order they were written.
func loadMergeHistory(ctx context.Context, q store.Querier, plan, node, forge string, number int64, head string) (mergeHistory, error) {
	var h mergeHistory
	rows, err := q.QueryContext(ctx, "SELECT c.check_seq, c.acceptance_id, c.outcome, c.reason, c.round_no, c.failed_required_json, c.checks_digest, c.evidence_json FROM dag_merge_checks c"+
		" JOIN dag_acceptances a ON a.acceptance_id = c.acceptance_id JOIN dag_acceptance_forge f ON f.acceptance_id = a.acceptance_id"+
		" WHERE a.plan_id = ? AND a.node_id = ? AND f.forge_repository = ? AND f.pr_number = ? AND c.observed_head_sha = ? ORDER BY c.rowid", plan, node, forge, number, head)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var r historicRow
		var failed, digest, evidence string
		if err := rows.Scan(&r.seq, &r.acceptance, &r.outcome, &r.reason, &r.round, &failed, &digest, &evidence); err != nil {
			return h, err
		}
		// the history decides how many retries are left, so a row that no longer digests to what was recorded (B-13) is not history
		if recomputed, err := RecomputeEvidenceDigest(evidence); err != nil || recomputed != digest {
			return h, refuse(contract.RefusalRevisionMismatch, "merge check %d of acceptance %s no longer digests to what was recorded", r.seq, r.acceptance)
		}
		if err := json.Unmarshal([]byte(failed), &r.failed); err != nil {
			return h, fmt.Errorf("a merge check of %s holds a failed list that is not JSON: %w", r.acceptance, err)
		}
		if r.outcome == OutcomeRetrySameSHA {
			h.retries++
		}
		if len(r.failed) > 0 {
			h.failures++
			for _, f := range r.failed {
				if !slices.Contains(h.failedBefore, f) {
					h.failedBefore = append(h.failedBefore, f)
				}
			}
		}
		if r.outcome == OutcomeEvicted && h.evicted == nil {
			row := r
			h.evicted = &row
		}
	}
	return h, rows.Err()
}

// predecessorsLanded is rule 3: every live incoming edge that waits for a landing (integrated, or a code pin: a stacked pull request) has its predecessor's accepted head in the edge's target.
func (s *Scheduler) predecessorsLanded(ctx context.Context, q store.Querier, plan string, snap dag.Snapshot, node string) (bool, string, error) {
	for _, e := range snap.Edges {
		if e.ToNodeID != node || !(e.Kind == dag.EdgeIntegrated || (e.Kind == dag.EdgeArtifactVerified && e.PinsCodeHead)) {
			continue
		}
		pred, found, err := loadActiveAcceptance(ctx, q, plan, e.FromNodeID)
		if err != nil {
			return false, "", err
		}
		if !found {
			return false, "predecessor " + e.FromNodeID + " of edge " + e.EdgeID + " has no accepted result", nil
		}
		at, err := s.integratedAt(ctx, q, plan, pred, e.TargetRepository, e.TargetBaseRef)
		if err != nil {
			return false, "", err
		}
		if !at.Satisfied {
			return false, "predecessor " + e.FromNodeID + " of edge " + e.EdgeID + " has not landed in " + e.TargetRepository + " " + e.TargetBaseRef, nil
		}
	}
	return true, "", nil
}

// MergeRequestInput is what RequestMergeTurn needs besides the node: the host the holder runs on, and optionally the pull request.
type MergeRequestInput struct {
	PullRequest *PRRef
	Host        string
}

// RequestMergeTurn asks the existing merge lane for a turn for an accepted pull request, after a judgement made now (never a remembered one) says it is eligible. A pull request that is not
// eligible gets no merge turn, and the judgement stays in the node's history: its refusal is the relay's own reason for it (stale_head merge_candidate_moved, stale_base
// merge_currency_stale, stale_criteria criteria_set_changed, everything else disposition_conflict naming the outcome). The turn itself, its FIFO order (requested_at, then turn id; decision
// D-16), the check against the base tip and the landing proof are the merge lane's and are not changed here. The parent's order is accept, judge, request, merge-turn-check, merge-turn-land,
// assignment-mark merged, dag-integration-observe.
func (s *Scheduler) RequestMergeTurn(ctx context.Context, plan, node, actor string, in MergeRequestInput) (JudgeResult, map[string]any, error) {
	if in.Host == "" {
		return JudgeResult{PlanID: plan, NodeID: node}, nil, refuse(contract.RefusalMalformedReceipt, "a merge turn is requested for the host the holder runs on")
	}
	res, err := s.Judge(ctx, plan, node, actor, JudgeInput{PullRequest: in.PullRequest})
	if err != nil {
		return res, nil, err
	}
	if !res.Eligible() {
		detail := fmt.Sprintf("the pull request of %s is %s: %s", node, res.Outcome, res.Reason)
		switch res.Outcome {
		case OutcomeStaleHead:
			return res, nil, refuseCandidateMoved("%s", detail)
		case OutcomeStaleBase:
			return res, nil, refuse(contract.RefusalMergeCurrencyStale, "%s", detail)
		case OutcomeStaleCriteria:
			return res, nil, refuse(contract.RefusalCriteriaSetChanged, "%s", detail)
		}
		return res, nil, refuse(contract.RefusalDispositionConflict, "%s", detail)
	}
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return res, nil, err
	}
	acc, found, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil || !found || acc.AcceptanceID != res.AcceptanceID {
		if err == nil {
			err = refuse(contract.RefusalDispositionConflict, "the accepted result of %s changed while its merge turn was requested", node)
		}
		return res, nil, err
	}
	service := &mergeturn.Service{Store: s.Store, Registry: &registry.Registry{Store: s.Store, Now: s.now}, Now: s.now, Delivery: mergeturn.StoreDelivery{Store: s.Store}}
	turn, err := service.Request(ctx, acc.Repository, res.BaseRef, snap.ProjectKey, actor, in.Host, acc.HeadSHA, true,
		mergeturn.ClaimOptions{PR: sql.NullInt64{Int64: acc.PRNumber, Valid: true}, Relationship: sql.NullString{String: acc.RelationshipID, Valid: true}})
	if err != nil {
		return res, nil, err
	}
	// a holder has one live claim per target: asking again while the earlier turn is open answers that turn, which is not this node's
	if head, _ := turn["candidateHead"].(string); head != acc.HeadSHA || fmt.Sprint(turn["relationshipId"]) != acc.RelationshipID || fmt.Sprint(turn["prNumber"]) != strconv.FormatInt(acc.PRNumber, 10) || turn["projectKey"] != snap.ProjectKey {
		return res, nil, refuse(contract.RefusalDispositionConflict, "task %s already has the live merge turn %v on this target (head %s, relationship %v, pull request %v): land or return it before the pull request of %s asks for its own", actor, turn["turnId"], head, turn["relationshipId"], turn["prNumber"], node)
	}
	return res, turn, nil
}
