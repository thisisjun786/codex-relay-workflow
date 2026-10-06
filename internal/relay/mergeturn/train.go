package mergeturn

// The merge train (CRW-768, the decision in docs/port/decisions.md section 79 and its correction):
// one bundle pull request whose single tree gets one full CI run and lands as one merge commit, so k
// members cost one run instead of k. The five commands live here, beside merge-turn-request, and are
// wired in commands.go and internal/relay/argparse/specs.json. The relay reads the pull request, the
// run, the jobs, the commits and the ancestry from the forge itself (and the chain from the given
// checkout) and uses the caller's values only to compare, so a stated value never decides a proof.
//
// No new refusal reason is introduced (D-02): a bundle that disagrees or is out of order is
// disposition_conflict, and a forge or git that cannot answer is merge_target_unreadable. A refusal
// writes no event.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// TrainExpectedJobs is the set of ci.yml job names a bundle's run must hold, each a success, before
// merge-train-verify accepts it. A test pins this list to .github/workflows/ci.yml's jobs and its
// go-product matrix, so a change to the workflow turns that test red (CRW-768 c1, c2). The list is
// the runtime commit's workflow: a bundle whose members change ci.yml's job set is refused fail-closed.
var TrainExpectedJobs = []string{
	"validate",
	"secrets",
	"skill-scripts-node",
	"go-product (lint)",
	"go-product (test-1)",
	"go-product (test-2)",
	"go-product (test-3)",
	"go-product (test-4)",
	"go-product (test-rest)",
	"go-product (dist)",
	"dev-gate",
}

// TrainLaneLabel is the label the merge lane puts on a bundle pull request; CRW-790's light mode runs
// the go-product test legs in full only on a pull request that carries it.
const TrainLaneLabel = "crw-lane"

// TrainBundleWorkflow is the workflow path a bundle's run must be, this repository's ci.yml.
const TrainBundleWorkflow = ".github/workflows/ci.yml"

// TrainMemberExpectation is one member as the relay read it when the train opened: the turn it holds,
// the relationship that turn belongs to, the pull request and the head its pull request showed.
type TrainMemberExpectation struct {
	TurnID         string
	PRNumber       int64
	RelationshipID string
	AcceptedHead   string
}

// TrainPullRequest is what the forge says about one pull request, the fields verify and land compare.
type TrainPullRequest struct {
	Number  int64
	State   string
	BaseRef string
	HeadSHA string
	Labels  []string
}

// TrainStep is one step of a job, as the forge reports it.
type TrainStep struct {
	Name       string
	Conclusion string
}

// TrainJob is one job of a workflow run. StepsReadable is false when the forge's answer carried no
// step list at all (the run's answer is malformed), which is merge_target_unreadable and not a leg
// that skipped its tests.
type TrainJob struct {
	Name          string
	Conclusion    string
	Steps         []TrainStep
	StepsReadable bool
}

// TrainRun is the run merge-train-verify proves against: the workflow it is, the commit it ran on,
// the repository the head came from, and the newest attempt's jobs.
type TrainRun struct {
	ID             string
	Path           string
	HeadSHA        string
	HeadRepository string
	Status         string
	Conclusion     string
	Attempt        int64
	Jobs           []TrainJob
}

// TrainCommit is one commit as the forge reads it: its parents and its tree.
type TrainCommit struct {
	SHA     string
	Parents []string
	Tree    string
}

// TrainForge is every forge read the train commands make. It is taken as a method argument, never
// stored on Service, so a test substitutes a stand-in and a nil reader fails closed (CRW-608).
type TrainForge interface {
	PullRequest(ctx context.Context, repository string, number int64) (TrainPullRequest, error)
	Run(ctx context.Context, repository, runID string) (TrainRun, error)
	Commit(ctx context.Context, repository, sha string) (TrainCommit, error)
	// Compare answers "identical", "ahead", "behind" or "diverged" for base..head, the forge's
	// compare API, which is the same statement as git merge-base --is-ancestor for the "ahead" case.
	Compare(ctx context.Context, repository, base, head string) (string, error)
}

// TrainCheckout proves, from git objects alone, that the head's first-parent chain down to the base
// is one two-parent merge per member in the train's order, each merge's second parent that member's
// accepted head and each merge's tree what git merges from its parents. It is a dedicated prover
// rather than a call into internal/skill: that package imports internal/relay/dagsched and dagsched
// imports this one, so importing it here would close a cycle. The rule is the same one the
// crw skill base-refresh check applies.
type TrainCheckout interface {
	Chain(ctx context.Context, checkout, head, base string, members []TrainMemberExpectation) (TrainChain, error)
}

// TrainChain is a passed chain proof: the tree of the head and the merge commits it walked.
type TrainChain struct {
	Tree    string
	Commits []string
}

// trainConflict is a bundle that disagrees or is out of order: disposition_conflict, and no event is
// written. It is a plain refusal, not a target contest, because a train has no incumbent to record.
func trainConflict(format string, args ...any) error {
	return &store.RefusedError{Reason: string(contract.RefusalDispositionConflict), Detail: fmt.Sprintf(format, args...)}
}

// trainUnreadable is a forge or git that could not answer: merge_target_unreadable.
func trainUnreadable(format string, args ...any) error {
	return &store.RefusedError{Reason: string(contract.RefusalMergeTargetUnreadable), Detail: fmt.Sprintf(format, args...)}
}

// trainID is a train's id, derived so the same target, leader, turn and head name one train.
func trainID(target, turn, head string) string {
	return key("trn", target, turn, head)
}

// Open is merge-train-open: the leader's holding turn opens a train over the members in the order
// given. One member is today's lane and makes no train.
func (s *Service) Open(ctx context.Context, turn, actor, base string, members []int64, reader Reader, forge TrainForge) (map[string]any, error) {
	if forge == nil {
		return nil, trainUnreadable("this relay has no forge reader configured, so a bundle's pull requests and base cannot be read")
	}
	if len(members) == 0 {
		return nil, trainConflict("a bundle names at least one member pull request")
	}
	early, err := s.Store.MergeTurn(ctx, turn)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err != nil {
		return nil, &store.RefusedError{Reason: string(contract.RefusalUnregisteredScope), Detail: "no merge turn " + strconv.Quote(turn)}
	}
	if early.HolderTaskID != actor {
		return nil, trainConflict("task %s does not hold turn %s, which belongs to %s, so it cannot open a bundle on it", pyvalue.StrRepr(actor), pyvalue.StrRepr(turn), pyvalue.StrRepr(early.HolderTaskID))
	}
	if early.State != Holding {
		return nil, trainConflict("turn %s is %s, and only a holding turn opens a bundle", pyvalue.StrRepr(turn), early.State)
	}
	if !early.PRNumber.Valid {
		return nil, trainConflict("turn %s records no pull request, so the leader's own pull request cannot be the bundle's first member", pyvalue.StrRepr(turn))
	}
	// D: the base the bundle is built on, read from the forge and compared with the caller's value.
	tip, why := readTarget(ctx, reader, early.Repository, early.BaseRef)
	if tip.SHA == "" {
		return nil, trainUnreadable("the base branch %s of %s was not read, so the bundle's base cannot be compared: %s", pyvalue.StrRepr(early.BaseRef), pyvalue.StrRepr(early.Repository), why)
	}
	if !SameCommit(base, tip.SHA) {
		return nil, trainConflict("the base branch %s reads %s and --base-sha states %s; the bundle is built on the base the branch points at now", pyvalue.StrRepr(early.BaseRef), pyvalue.StrRepr(tip.SHA), pyvalue.StrRepr(base))
	}
	leaderPR := early.PRNumber.Int64
	seen := map[int64]bool{}
	expectations := make([]TrainMemberExpectation, 0, len(members))
	order := make([]int64, 0, len(members))
	for _, pr := range members {
		if pr < 1 {
			return nil, trainConflict("pull request %d is not a positive whole number", pr)
		}
		if seen[pr] {
			return nil, trainConflict("pull request %d appears twice in the bundle", pr)
		}
		seen[pr] = true
		order = append(order, pr)
	}
	waiting, err := s.Store.MergeTurnsForTarget(ctx, early.TargetKey)
	if err != nil {
		return nil, err
	}
	for _, pr := range members {
		pull, err := forge.PullRequest(ctx, early.Repository, pr)
		if err != nil {
			return nil, trainUnreadable("pull request %d of %s was not read: %v", pr, pyvalue.StrRepr(early.Repository), err)
		}
		if pull.Number != pr {
			return nil, trainUnreadable("the forge's answer for pull request %d names %d", pr, pull.Number)
		}
		if pr == leaderPR {
			if !SameCommit(pull.HeadSHA, early.CandidateHead) {
				return nil, trainConflict("the leader's pull request %d reads head %s and its turn holds %s; a member's head is not refreshed by the bundle", pr, pyvalue.StrRepr(pull.HeadSHA), pyvalue.StrRepr(early.CandidateHead))
			}
			expectations = append(expectations, TrainMemberExpectation{TurnID: turn, PRNumber: pr, RelationshipID: early.RelationshipID.String, AcceptedHead: early.CandidateHead})
			continue
		}
		member, found := waitingTurnFor(waiting, pr)
		if !found {
			return nil, trainConflict("pull request %d has no waiting turn on %s, so it is not a member this bundle may carry", pr, pyvalue.StrRepr(early.TargetKey))
		}
		if !SameCommit(pull.HeadSHA, member.CandidateHead) {
			return nil, trainConflict("pull request %d reads head %s and its waiting turn holds %s; the member's head moved", pr, pyvalue.StrRepr(pull.HeadSHA), pyvalue.StrRepr(member.CandidateHead))
		}
		expectations = append(expectations, TrainMemberExpectation{TurnID: member.TurnID, PRNumber: pr, RelationshipID: member.RelationshipID.String, AcceptedHead: member.CandidateHead})
	}
	if len(members) == 1 {
		return map[string]any{"train": nil, "lane": "single", "turnId": turn, "pullRequest": leaderPR, "baseSha": tip.SHA}, nil
	}
	if err := s.trainOrderRefusal(ctx, expectations, order); err != nil {
		return nil, err
	}
	at := s.now()
	id := trainID(early.TargetKey, turn, early.CandidateHead)
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		row, e := s.row(tx, turn)
		if e != nil {
			return e
		}
		if row.HolderTaskID != actor || row.State != Holding {
			return trainConflict("turn %s changed while the bundle was being read, so it is not the holding turn this open began on; call again", pyvalue.StrRepr(turn))
		}
		if _, found, e := store.MergeTrain(tx, s.Store, id); e != nil {
			return e
		} else if found {
			return trainConflict("train %s already exists for turn %s at head %s", id, pyvalue.StrRepr(turn), pyvalue.StrRepr(row.CandidateHead))
		}
		if e := store.RecordMergeTrain(tx, s.Store, store.MergeTrainRow{TrainID: id, TargetKey: row.TargetKey, Repository: row.Repository, BaseRef: row.BaseRef, BaseSHA: tip.SHA, LeaderTaskID: actor, CreatedAt: at}); e != nil {
			return e
		}
		for i, m := range expectations {
			if e := store.RecordMergeTrainMember(tx, s.Store, store.MergeTrainMemberRow{TrainID: id, Seq: int64(i + 1), TurnID: m.TurnID, PRNumber: m.PRNumber, RelationshipID: m.RelationshipID, MemberHead: m.AcceptedHead}); e != nil {
				return e
			}
		}
		return store.RecordMergeTrainEvent(tx, s.Store, store.MergeTrainEventRow{TrainID: id, Seq: 1, Kind: store.MergeTrainOpened, Actor: actor, DetailJSON: pythonJSON(trainOpenedDetail(expectations, tip.SHA)), RecordedAt: at})
	})
	if err != nil {
		return nil, err
	}
	return s.trainAnswer(ctx, id)
}

// waitingTurnFor is the waiting turn of one pull request on a target, if any.
func waitingTurnFor(turns []store.MergeTurnsRow, pr int64) (store.MergeTurnsRow, bool) {
	for _, t := range turns {
		if t.State == Waiting && t.PRNumber.Valid && t.PRNumber.Int64 == pr {
			return t, true
		}
	}
	return store.MergeTurnsRow{}, false
}

// trainOpenedDetail is the opened event's body: the members in order and the base D.
func trainOpenedDetail(members []TrainMemberExpectation, base string) map[string]any {
	list := make([]any, 0, len(members))
	for i, m := range members {
		list = append(list, map[string]any{"seq": i + 1, "turnId": m.TurnID, "prNumber": m.PRNumber, "relationshipId": m.RelationshipID, "memberHead": m.AcceptedHead})
	}
	return map[string]any{"members": list, "baseSha": base}
}

// trainOrderRefusal refuses a bundle whose member order runs against a plan edge: when a member is an
// accepted node of some plan and a live edge makes another member its predecessor, that predecessor
// must stand earlier in the bundle.
func (s *Service) trainOrderRefusal(ctx context.Context, members []TrainMemberExpectation, order []int64) error {
	place := map[int64]int{}
	for i, pr := range order {
		place[pr] = i
	}
	for i, m := range members {
		nodes, err := s.acceptedNodesForHead(ctx, m.AcceptedHead)
		if err != nil {
			return err
		}
		for _, node := range nodes {
			preds, err := s.liveEdgePredecessors(ctx, node.planID, node.nodeID)
			if err != nil {
				return err
			}
			for _, pred := range preds {
				pr, found, err := s.acceptedNodePullRequest(ctx, node.planID, pred)
				if err != nil {
					return err
				}
				if !found {
					continue
				}
				j, isMember := place[pr]
				if !isMember {
					continue
				}
				if j > i {
					return trainConflict("pull request %d is a predecessor of pull request %d on a plan edge, so it must stand earlier in the bundle than it does", pr, m.PRNumber)
				}
			}
		}
	}
	return nil
}

type trainNode struct{ planID, nodeID string }

// acceptedNodesForHead is every active acceptance whose accepted head is this head.
func (s *Service) acceptedNodesForHead(ctx context.Context, head string) ([]trainNode, error) {
	if head == "" {
		return nil, nil
	}
	rows, err := s.Store.All(ctx, "SELECT plan_id, node_id FROM dag_acceptances WHERE head_sha = ? AND state = 'active'", head)
	if err != nil {
		return nil, err
	}
	out := make([]trainNode, 0, len(rows))
	for _, row := range rows {
		out = append(out, trainNode{planID: fmt.Sprint(row.Get("plan_id")), nodeID: fmt.Sprint(row.Get("node_id"))})
	}
	return out, nil
}

// liveEdgePredecessors is the from_node_id of every live edge whose target is this node.
func (s *Service) liveEdgePredecessors(ctx context.Context, plan, node string) ([]string, error) {
	rows, err := s.Store.All(ctx, "SELECT from_node_id FROM dag_edges WHERE plan_id = ? AND to_node_id = ? AND retired_rev IS NULL", plan, node)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, fmt.Sprint(row.Get("from_node_id")))
	}
	return out, nil
}

// acceptedNodePullRequest is the forge pull request of a node's active acceptance, if it has one.
func (s *Service) acceptedNodePullRequest(ctx context.Context, plan, node string) (int64, bool, error) {
	row, err := s.Store.One(ctx, "SELECT f.pr_number FROM dag_acceptances a"+
		" JOIN dag_acceptance_forge f ON f.acceptance_id = a.acceptance_id"+
		" WHERE a.plan_id = ? AND a.node_id = ? AND a.state = 'active'", plan, node)
	if err != nil {
		return 0, false, err
	}
	if row == nil {
		return 0, false, nil
	}
	switch v := row.Get("pr_number").(type) {
	case int64:
		return v, true, nil
	case int:
		return int64(v), true, nil
	}
	n, err := strconv.ParseInt(fmt.Sprint(row.Get("pr_number")), 10, 64)
	if err != nil {
		return 0, false, nil
	}
	return n, true, nil
}

// Verify is merge-train-verify: the leader alone, reading the bundle pull request, the run and the
// chain, and appending a verified event. Every check is a read of the forge or of git; the caller's
// values are compared, never trusted.
func (s *Service) Verify(ctx context.Context, train, actor, bundlePR, head, run, checkout string, forge TrainForge, proof TrainCheckout) (map[string]any, error) {
	if forge == nil {
		return nil, trainUnreadable("this relay has no forge reader configured, so the bundle pull request and its run cannot be read")
	}
	row, found, err := store.MergeTrain(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, trainConflict("no train %s", pyvalue.StrRepr(train))
	}
	if row.LeaderTaskID != actor {
		return nil, trainConflict("task %s is not the leader of train %s, which is %s, so it cannot verify the bundle", pyvalue.StrRepr(actor), pyvalue.StrRepr(train), pyvalue.StrRepr(row.LeaderTaskID))
	}
	state, found, err := store.MergeTrainState(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found || (state != store.MergeTrainOpened && state != store.MergeTrainVerified) {
		return nil, trainConflict("train %s is %s and a bundle is verified while it is opened or already verified", pyvalue.StrRepr(train), orNoneState(found, state))
	}
	members, err := store.MergeTrainMembers(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if len(members) < 2 {
		return nil, trainConflict("train %s holds %d member(s); a bundle carries at least two", pyvalue.StrRepr(train), len(members))
	}
	pr, err := strconv.ParseInt(bundlePR, 10, 64)
	if err != nil || pr < 1 {
		return nil, trainConflict("the bundle pull request is a positive whole number, not %s", pyvalue.StrRepr(bundlePR))
	}
	pull, err := forge.PullRequest(ctx, row.Repository, pr)
	if err != nil {
		return nil, trainUnreadable("bundle pull request %d of %s was not read: %v", pr, pyvalue.StrRepr(row.Repository), err)
	}
	if pull.Number != pr {
		return nil, trainUnreadable("the forge's answer for bundle pull request %d names %d", pr, pull.Number)
	}
	if pull.State != "open" {
		return nil, trainConflict("bundle pull request %d is %s; verify reads an open pull request", pr, pull.State)
	}
	if pull.BaseRef != row.BaseRef {
		return nil, trainConflict("bundle pull request %d targets %s and the train's base is %s", pr, pyvalue.StrRepr(pull.BaseRef), pyvalue.StrRepr(row.BaseRef))
	}
	if !SameCommit(pull.HeadSHA, head) {
		return nil, trainConflict("bundle pull request %d reads head %s and --head states %s", pr, pyvalue.StrRepr(pull.HeadSHA), pyvalue.StrRepr(head))
	}
	if !hasLabel(pull.Labels, TrainLaneLabel) {
		return nil, trainConflict("bundle pull request %d does not carry the %s label, so its run may be a light one", pr, TrainLaneLabel)
	}
	reading, err := forge.Run(ctx, row.Repository, run)
	if err != nil {
		return nil, trainUnreadable("workflow run %s of %s was not read: %v", run, pyvalue.StrRepr(row.Repository), err)
	}
	if err := trainRunRefusal(row.Repository, head, reading); err != nil {
		return nil, err
	}
	expected := make([]TrainMemberExpectation, 0, len(members))
	for _, m := range members {
		expected = append(expected, TrainMemberExpectation{TurnID: m.TurnID, PRNumber: m.PRNumber, RelationshipID: m.RelationshipID, AcceptedHead: m.MemberHead})
	}
	if proof == nil {
		return nil, trainUnreadable("this relay has no checkout prover configured, so the bundle's first-parent chain cannot be proved")
	}
	chain, err := proof.Chain(ctx, checkout, head, row.BaseSHA, expected)
	if err != nil {
		return nil, err
	}
	at := s.now()
	detail := trainVerifiedDetail(pr, head, chain.Tree, run, expected)
	seq, err := s.nextTrainEventSeq(ctx, train)
	if err != nil {
		return nil, err
	}
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		current, found, e := store.MergeTrainState(tx, s.Store, train)
		if e != nil {
			return e
		}
		if !found || (current != store.MergeTrainOpened && current != store.MergeTrainVerified) {
			return trainConflict("train %s changed while the bundle was being read, so it is no longer a train a verify may advance; call again", pyvalue.StrRepr(train))
		}
		return store.RecordMergeTrainEvent(tx, s.Store, store.MergeTrainEventRow{TrainID: train, Seq: seq, Kind: store.MergeTrainVerified, Actor: actor, DetailJSON: pythonJSON(detail), RecordedAt: at})
	})
	if err != nil {
		return nil, err
	}
	return s.trainAnswer(ctx, train)
}

// trainRunRefusal refuses a run that is not this repository's ci.yml run for this head, or whose
// newest attempt is not a success with every expected job. It is disposition_conflict for a run that
// is the wrong one or is not green, and merge_target_unreadable for a job whose step list the forge
// did not answer (CRW-768 c1, c2).
func trainRunRefusal(repository, head string, run TrainRun) error {
	if !strings.HasSuffix(run.Path, TrainBundleWorkflow) {
		return trainConflict("workflow run %s ran %s, and a bundle is verified against this repository's %s", run.ID, pyvalue.StrRepr(run.Path), TrainBundleWorkflow)
	}
	if run.HeadRepository != repository {
		return trainConflict("workflow run %s came from %s, a fork, and a bundle is verified against a run of %s itself", run.ID, pyvalue.StrRepr(run.HeadRepository), pyvalue.StrRepr(repository))
	}
	if !SameCommit(run.HeadSHA, head) {
		return trainConflict("workflow run %s ran on %s and the bundle's head is %s", run.ID, pyvalue.StrRepr(run.HeadSHA), pyvalue.StrRepr(head))
	}
	if run.Status != "completed" || run.Conclusion != "success" {
		return trainConflict("workflow run %s is %s/%s; a bundle is verified against a completed successful run", run.ID, run.Status, run.Conclusion)
	}
	present := map[string]TrainJob{}
	for _, job := range run.Jobs {
		present[job.Name] = job
	}
	for _, name := range TrainExpectedJobs {
		job, ok := present[name]
		if !ok {
			return trainConflict("workflow run %s holds no job named %s; the run must hold every job this repository's ci.yml runs", run.ID, pyvalue.StrRepr(name))
		}
		if job.Conclusion != "success" {
			return trainConflict("job %s of workflow run %s concluded %s, and every expected job must be a success", pyvalue.StrRepr(name), run.ID, job.Conclusion)
		}
		if !strings.HasPrefix(name, "go-product (test-") {
			continue
		}
		if !job.StepsReadable {
			return trainUnreadable("job %s of workflow run %s came back without a readable step list, so whether it ran its tests cannot be told", pyvalue.StrRepr(name), run.ID)
		}
		if err := trainTestStepRefusal(name, run.ID, job); err != nil {
			return err
		}
	}
	return nil
}

// trainTestStepRefusal is the go-product test leg's test step: it must be in the list and conclude
// success. A leg whose step is absent from a readable list is one whose tests did not run, which is
// disposition_conflict; the unreadable list is merge_target_unreadable and is caught above. This is
// the strict reading of CRW-768 c1: the CRW-824 mirror carve-out is not taken, so a bundle whose body
// was edited after its tests ran is refused rather than accepted on the mirror's word.
func trainTestStepRefusal(name, runID string, job TrainJob) error {
	for _, step := range job.Steps {
		if !strings.HasPrefix(step.Name, evidence.LightTestStepPrefix) {
			continue
		}
		if step.Conclusion != "success" {
			return trainConflict("the test step of job %s of workflow run %s concluded %s, so the leg did not run its tests", pyvalue.StrRepr(name), runID, step.Conclusion)
		}
		return nil
	}
	return trainConflict("job %s of workflow run %s carries no test step, so the leg did not run its tests", pyvalue.StrRepr(name), runID)
}

// trainVerifiedDetail is the verified event's body: the bundle pull request, the head, the tree, the
// run, and the per-member mapping (CRW-768 c2, decision 10) that verify and land both record.
func trainVerifiedDetail(pr int64, head, tree, run string, members []TrainMemberExpectation) map[string]any {
	list := make([]any, 0, len(members))
	for i, m := range members {
		list = append(list, map[string]any{"seq": i + 1, "turnId": m.TurnID, "prNumber": m.PRNumber, "relationshipId": m.RelationshipID, "acceptedHead": m.AcceptedHead, "memberHead": m.AcceptedHead})
	}
	return map[string]any{"bundlePr": pr, "head": head, "tree": tree, "run": run, "members": list}
}

// TrainLand is merge-train-land: the bundle merge commit M is on dev, its parents are the bundle's base D
// and the verified head H, and every member's accepted head is an ancestor of M. Then every member
// turn (the leader's included) is recorded landed with M.
func (s *Service) TrainLand(ctx context.Context, train, actor, landed, observed string, reader Reader, forge TrainForge) (map[string]any, error) {
	if forge == nil {
		return nil, trainUnreadable("this relay has no forge reader configured, so the bundle's landing cannot be read")
	}
	if strings.TrimSpace(landed) == "" {
		return nil, trainConflict("a landing names the merge commit M")
	}
	row, found, err := store.MergeTrain(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, trainConflict("no train %s", pyvalue.StrRepr(train))
	}
	if row.LeaderTaskID != actor {
		return nil, trainConflict("task %s is not the leader of train %s, which is %s", pyvalue.StrRepr(actor), pyvalue.StrRepr(train), pyvalue.StrRepr(row.LeaderTaskID))
	}
	state, found, err := store.MergeTrainState(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found || state != store.MergeTrainVerified {
		return nil, trainConflict("train %s is %s and only a verified bundle lands", pyvalue.StrRepr(train), orNoneState(found, state))
	}
	verified, found, err := s.newestVerified(ctx, train)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, trainConflict("train %s holds no verified event", pyvalue.StrRepr(train))
	}
	head, _ := verified["head"].(string)
	tip, why := readTarget(ctx, reader, row.Repository, row.BaseRef)
	if tip.SHA == "" {
		return nil, trainUnreadable("the base branch %s of %s was not read, so the landing cannot be compared: %s", pyvalue.StrRepr(row.BaseRef), pyvalue.StrRepr(row.Repository), why)
	}
	if !SameCommit(landed, tip.SHA) {
		return nil, trainConflict("the base branch %s reads %s and --landed-sha states %s", pyvalue.StrRepr(row.BaseRef), pyvalue.StrRepr(tip.SHA), pyvalue.StrRepr(landed))
	}
	if observed != "" && !SameCommit(observed, tip.SHA) {
		return nil, trainConflict("--observed-base-sha states %s and the base branch reads %s", pyvalue.StrRepr(observed), pyvalue.StrRepr(tip.SHA))
	}
	commit, err := forge.Commit(ctx, row.Repository, landed)
	if err != nil {
		return nil, trainUnreadable("merge commit %s of %s was not read: %v", landed, pyvalue.StrRepr(row.Repository), err)
	}
	if len(commit.Parents) != 2 {
		return nil, trainConflict("merge commit %s has %d parent(s), and a bundle lands as one merge commit with two", landed, len(commit.Parents))
	}
	if !SameCommit(commit.Parents[0], row.BaseSHA) || !SameCommit(commit.Parents[1], head) {
		return nil, trainConflict("merge commit %s has parents %s and %s, and a bundle's are the base %s and the verified head %s in that order", landed, pyvalue.StrRepr(commit.Parents[0]), pyvalue.StrRepr(commit.Parents[1]), pyvalue.StrRepr(row.BaseSHA), pyvalue.StrRepr(head))
	}
	members, err := store.MergeTrainMembers(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		status, err := forge.Compare(ctx, row.Repository, m.MemberHead, landed)
		if err != nil {
			return nil, trainUnreadable("the ancestry of %s in %s was not read: %v", pyvalue.StrRepr(m.MemberHead), landed, err)
		}
		if status != "identical" && status != "ahead" {
			return nil, trainConflict("member head %s of pull request %d is %s of %s, and every member's accepted head must be an ancestor of the merge commit", pyvalue.StrRepr(m.MemberHead), m.PRNumber, status, landed)
		}
	}
	at := s.now()
	reason := "landed via bundle " + train + " " + landed
	seq, err := s.nextTrainEventSeq(ctx, train)
	if err != nil {
		return nil, err
	}
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		current, found, e := store.MergeTrainState(tx, s.Store, train)
		if e != nil {
			return e
		}
		if !found || current != store.MergeTrainVerified {
			return trainConflict("train %s changed while the landing was being read, so nothing is recorded; call again", pyvalue.StrRepr(train))
		}
		// every member turn lands, the leader's included, in the same transaction as the event, so a
		// failure leaves neither the rows nor the event (the relay's one-transaction rule)
		for _, m := range members {
			turn, e := s.Store.MergeTurn(tx, m.TurnID)
			if e != nil {
				return e
			}
			if e := s.closeWith(tx, turn, "landed", reason, actor, at, nullable(landed), nullable(tip.SHA)); e != nil {
				return e
			}
		}
		detail := trainLandedDetail(landed, head, reason, members)
		return store.RecordMergeTrainEvent(tx, s.Store, store.MergeTrainEventRow{TrainID: train, Seq: seq, Kind: store.MergeTrainLanded, Actor: actor, DetailJSON: pythonJSON(detail), RecordedAt: at})
	})
	if err != nil {
		return nil, err
	}
	return s.trainAnswer(ctx, train)
}

// trainLandedDetail is the landed event's body: the merge commit M, the verified head H, the close
// reason and the per-member mapping.
func trainLandedDetail(landed, head, reason string, members []store.MergeTrainMemberRow) map[string]any {
	list := make([]any, 0, len(members))
	for _, m := range members {
		list = append(list, map[string]any{"seq": m.Seq, "turnId": m.TurnID, "prNumber": m.PRNumber, "relationshipId": m.RelationshipID, "acceptedHead": m.MemberHead, "memberHead": m.MemberHead})
	}
	return map[string]any{"landedSha": landed, "head": head, "reason": reason, "members": list}
}

// Close is merge-train-close: it appends the terminal event. abandoned returns every member turn to
// waiting so another bundle may carry it.
func (s *Service) Close(ctx context.Context, train, actor, state, reason string) (map[string]any, error) {
	if state != "done" && state != "abandoned" {
		return nil, trainConflict("a train closes done or abandoned, not %s", pyvalue.StrRepr(state))
	}
	if strings.TrimSpace(reason) == "" {
		return nil, trainConflict("closing a train states why")
	}
	row, found, err := store.MergeTrain(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, trainConflict("no train %s", pyvalue.StrRepr(train))
	}
	if row.LeaderTaskID != actor {
		return nil, trainConflict("task %s is not the leader of train %s, which is %s", pyvalue.StrRepr(actor), pyvalue.StrRepr(train), pyvalue.StrRepr(row.LeaderTaskID))
	}
	kind := store.MergeTrainDone
	if state == "abandoned" {
		kind = store.MergeTrainAbandoned
	}
	current, found, err := store.MergeTrainState(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, trainConflict("train %s holds no event to close", pyvalue.StrRepr(train))
	}
	if current == store.MergeTrainDone || current == store.MergeTrainAbandoned {
		return nil, trainConflict("train %s is already %s", pyvalue.StrRepr(train), current)
	}
	at := s.now()
	seq, err := s.nextTrainEventSeq(ctx, train)
	if err != nil {
		return nil, err
	}
	err = s.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		latest, found, e := store.MergeTrainState(tx, s.Store, train)
		if e != nil {
			return e
		}
		if !found || latest == store.MergeTrainDone || latest == store.MergeTrainAbandoned {
			return trainConflict("train %s changed while it was being closed, so nothing is recorded; call again", pyvalue.StrRepr(train))
		}
		if kind == store.MergeTrainAbandoned {
			members, e := store.MergeTrainMembers(tx, s.Store, train)
			if e != nil {
				return e
			}
			for _, m := range members {
				turn, e := s.Store.MergeTurn(tx, m.TurnID)
				if e != nil {
					return e
				}
				// a member still holding or merging returns to waiting; one already landed is left
				if turn.State == Holding || turn.State == Merging || turn.State == Unknown {
					if e := s.closeWith(tx, turn, "returned", "returned when train "+train+" was abandoned: "+reason, actor, at, sql.NullString{}, sql.NullString{}); e != nil {
						return e
					}
				}
			}
		}
		return store.RecordMergeTrainEvent(tx, s.Store, store.MergeTrainEventRow{TrainID: train, Seq: seq, Kind: kind, Actor: actor, DetailJSON: pythonJSON(map[string]any{"reason": reason}), RecordedAt: at})
	})
	if err != nil {
		return nil, err
	}
	return s.trainAnswer(ctx, train)
}

// Show is merge-train-show: the state is derived from the newest event, and when a forge reader is
// given and the train is verified, it also reconciles a landing whose answer was lost by reading the
// dev tip and each member head's ancestry from the forge (the way merge-turn-resolve reads the
// branch). It records nothing: a reading it cannot make is answered as unreadable, never written.
func (s *Service) Show(ctx context.Context, train string, reader Reader, forge TrainForge) (map[string]any, error) {
	row, found, err := store.MergeTrain(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, trainConflict("no train %s", pyvalue.StrRepr(train))
	}
	answer, err := s.trainAnswer(ctx, train)
	if err != nil {
		return nil, err
	}
	answer["trainId"] = row.TrainID
	if answer["state"] == store.MergeTrainVerified && forge != nil {
		answer["reconcile"] = s.reconcileLanding(ctx, row, reader, forge)
	}
	return answer, nil
}

// reconcileLanding reads the dev tip and each member head's ancestry for a verified train whose land
// answer was lost. It is a reading, not a record: the relay writes nothing here, and a forge it cannot
// read is reported as unreadable.
func (s *Service) reconcileLanding(ctx context.Context, row store.MergeTrainRow, reader Reader, forge TrainForge) map[string]any {
	tip, why := readTarget(ctx, reader, row.Repository, row.BaseRef)
	if tip.SHA == "" {
		return map[string]any{"unreadable": "the base branch " + row.BaseRef + " of " + row.Repository + " was not read: " + why}
	}
	members, err := store.MergeTrainMembers(ctx, s.Store, row.TrainID)
	if err != nil {
		return map[string]any{"unreadable": err.Error()}
	}
	list := make([]any, 0, len(members))
	landed := true
	for _, m := range members {
		status, err := forge.Compare(ctx, row.Repository, m.MemberHead, tip.SHA)
		if err != nil {
			return map[string]any{"unreadable": "the ancestry of " + m.MemberHead + " in " + tip.SHA + " was not read: " + err.Error()}
		}
		if status != "identical" && status != "ahead" {
			landed = false
		}
		list = append(list, map[string]any{"seq": m.Seq, "prNumber": m.PRNumber, "memberHead": m.MemberHead, "status": status})
	}
	return map[string]any{"devTip": tip.SHA, "landed": landed, "members": list}
}

// TrainPlanEdge is one live plan edge between two accepted nodes, the shape decision 10's failure rule
// reads: from_node_id is a predecessor of to_node_id.
type TrainPlanEdge struct {
	FromNodeID string
	ToNodeID   string
}

// TrainDependencyClosure is decision 10's failure rule made checkable: the members that must be
// removed with a failing one, its successors on the plan edges taken transitively. The failing pull
// request is included. It is a pure function the leader applies by hand (the lane script is out of
// scope here); it adds no command and no refusal reason.
func TrainDependencyClosure(members []TrainMemberExpectation, edges []TrainPlanEdge, failingPR int64) []int64 {
	// each member's accepted head identifies its node, and a node's successors are the to_node_id of
	// the live edges whose from_node_id is it
	nodeOf := map[string]int64{}
	for _, m := range members {
		nodeOf[m.AcceptedHead] = m.PRNumber
	}
	successors := map[string][]string{}
	for _, e := range edges {
		successors[e.FromNodeID] = append(successors[e.FromNodeID], e.ToNodeID)
	}
	removed := map[int64]bool{failingPR: true}
	queue := []string{}
	for head, pr := range nodeOf {
		if pr == failingPR {
			queue = append(queue, head)
		}
	}
	for len(queue) > 0 {
		head := queue[0]
		queue = queue[1:]
		for _, next := range successors[head] {
			if pr, ok := nodeOf[next]; ok && !removed[pr] {
				removed[pr] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]int64, 0, len(removed))
	for _, m := range members {
		if removed[m.PRNumber] {
			out = append(out, m.PRNumber)
		}
	}
	return out
}

// TrainHalve is decision 10's halving rule made checkable: when a failure's cause cannot be named,
// the bundle is split in two with every predecessor and its successors kept on the same side, so no
// half carries a member whose dependency is in the other. The split is by the members' order, then
// repaired by moving a member whose predecessor is in the other half to the predecessor's side.
func TrainHalve(members []TrainMemberExpectation, edges []TrainPlanEdge) (first, second []int64) {
	if len(members) < 2 {
		for _, m := range members {
			first = append(first, m.PRNumber)
		}
		return first, nil
	}
	half := (len(members) + 1) / 2
	side := map[int64]int{}
	for i, m := range members {
		if i < half {
			side[m.PRNumber] = 0
		} else {
			side[m.PRNumber] = 1
		}
	}
	nodeOf := map[string]int64{}
	for _, m := range members {
		nodeOf[m.AcceptedHead] = m.PRNumber
	}
	// a member whose predecessor stands on the other side moves to the predecessor's side
	for changed := true; changed; {
		changed = false
		for _, e := range edges {
			from, okFrom := nodeOf[e.FromNodeID]
			to, okTo := nodeOf[e.ToNodeID]
			if !okFrom || !okTo || side[from] == side[to] {
				continue
			}
			side[to] = side[from]
			changed = true
		}
	}
	for _, m := range members {
		if side[m.PRNumber] == 0 {
			first = append(first, m.PRNumber)
		} else {
			second = append(second, m.PRNumber)
		}
	}
	return first, second
}

// trainAnswer is one train's whole reading: the row, the members in order, the event log, and the
// state derived from the newest event.
func (s *Service) trainAnswer(ctx context.Context, train string) (map[string]any, error) {
	row, found, err := store.MergeTrain(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, trainConflict("no train %s", pyvalue.StrRepr(train))
	}
	members, err := store.MergeTrainMembers(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	events, err := store.MergeTrainEvents(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	memberList := make([]any, 0, len(members))
	for _, m := range members {
		memberList = append(memberList, map[string]any{"seq": m.Seq, "turnId": m.TurnID, "prNumber": m.PRNumber, "relationshipId": m.RelationshipID, "memberHead": m.MemberHead})
	}
	eventList := make([]any, 0, len(events))
	for _, e := range events {
		var detail any
		if json.Unmarshal([]byte(e.DetailJSON), &detail) != nil {
			detail = e.DetailJSON
		}
		eventList = append(eventList, map[string]any{"seq": e.Seq, "kind": e.Kind, "actor": e.Actor, "detail": detail, "recordedAt": e.RecordedAt})
	}
	state, _, err := store.MergeTrainState(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	return map[string]any{"train": map[string]any{
		"trainId":      row.TrainID,
		"targetKey":    row.TargetKey,
		"repository":   row.Repository,
		"baseRef":      row.BaseRef,
		"baseSha":      row.BaseSHA,
		"leaderTaskId": row.LeaderTaskID,
		"createdAt":    row.CreatedAt,
		"state":        state,
		"members":      memberList,
		"events":       eventList,
	}, "state": state}, nil
}

func orNoneState(found bool, state string) string {
	if !found {
		return "None"
	}
	return state
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// nextTrainEventSeq is one past the newest event's sequence number.
func (s *Service) nextTrainEventSeq(ctx context.Context, train string) (int64, error) {
	events, err := store.MergeTrainEvents(ctx, s.Store, train)
	if err != nil {
		return 0, err
	}
	var highest int64
	for _, e := range events {
		if e.Seq > highest {
			highest = e.Seq
		}
	}
	return highest + 1, nil
}

// newestVerified is the newest verified event's detail, the head and members land proves against.
func (s *Service) newestVerified(ctx context.Context, train string) (map[string]any, bool, error) {
	events, err := store.MergeTrainEvents(ctx, s.Store, train)
	if err != nil {
		return nil, false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != store.MergeTrainVerified {
			continue
		}
		var detail map[string]any
		if json.Unmarshal([]byte(events[i].DetailJSON), &detail) != nil {
			return nil, false, nil
		}
		return detail, true, nil
	}
	return nil, false, nil
}

// TrainForgeReader is the production TrainForge: one gh api GET per read, no shell, each bounded by
// ForgeCallTimeout like every other forge read in this package.
type TrainForgeReader struct{ GH string }

func (r TrainForgeReader) gh(ctx context.Context, argv ...string) (any, error) {
	gh := r.GH
	if gh == "" {
		gh = "gh"
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, ForgeCallTimeout)
	defer cancel()
	cmd := exec.CommandContext(timeoutCtx, gh, append([]string{"api", "--method", "GET", "-H", "Accept: application/vnd.github+json"}, argv...)...)
	output, err := cmd.Output()
	if err != nil {
		if timeoutCtx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("the forge read exceeded the timeout of %d seconds, so no answer was observed", int(ForgeCallTimeout/time.Second))
		}
		if e, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("the forge read failed: %s", excerpt(strings.TrimSpace(string(e.Stderr))))
		}
		return nil, fmt.Errorf("the forge could not be read: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	var payload any
	if decoder.Decode(&payload) != nil {
		return nil, fmt.Errorf("the forge read returned something that is not JSON")
	}
	return payload, nil
}

// PullRequest reads one pull request's state, base, head and labels.
func (r TrainForgeReader) PullRequest(ctx context.Context, repository string, number int64) (TrainPullRequest, error) {
	if !forgeRepository(repository) {
		return TrainPullRequest{}, fmt.Errorf("repository %s is not an owner/name forge repository", pyvalue.StrRepr(repository))
	}
	payload, err := r.gh(ctx, fmt.Sprintf("repos/%s/pulls/%d", repository, number))
	if err != nil {
		return TrainPullRequest{}, err
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return TrainPullRequest{}, fmt.Errorf("the forge answered with something that is not one pull request")
	}
	if named, _ := data["number"].(json.Number); named.String() != strconv.FormatInt(number, 10) {
		return TrainPullRequest{}, fmt.Errorf("the forge's answer does not name pull request %d", number)
	}
	out := TrainPullRequest{Number: number}
	out.State, _ = data["state"].(string)
	if base, ok := data["base"].(map[string]any); ok {
		out.BaseRef, _ = base["ref"].(string)
	}
	if head, ok := data["head"].(map[string]any); ok {
		out.HeadSHA, _ = head["sha"].(string)
	}
	if labels, ok := data["labels"].([]any); ok {
		for _, raw := range labels {
			if l, ok := raw.(map[string]any); ok {
				if name, ok := l["name"].(string); ok {
					out.Labels = append(out.Labels, name)
				}
			}
		}
	}
	if out.HeadSHA == "" || !githubSHA.MatchString(out.HeadSHA) {
		return TrainPullRequest{}, fmt.Errorf("the forge's answer for pull request %d does not name a full commit as its head", number)
	}
	return out, nil
}

// Run reads one workflow run and its jobs.
func (r TrainForgeReader) Run(ctx context.Context, repository, runID string) (TrainRun, error) {
	if !forgeRepository(repository) {
		return TrainRun{}, fmt.Errorf("repository %s is not an owner/name forge repository", pyvalue.StrRepr(repository))
	}
	payload, err := r.gh(ctx, fmt.Sprintf("repos/%s/actions/runs/%s", repository, runID))
	if err != nil {
		return TrainRun{}, err
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return TrainRun{}, fmt.Errorf("the forge answered with something that is not one workflow run")
	}
	if named, _ := data["id"].(json.Number); named.String() != runID {
		return TrainRun{}, fmt.Errorf("the forge's answer does not name workflow run %s", runID)
	}
	out := TrainRun{ID: runID}
	out.Path, _ = data["path"].(string)
	out.HeadSHA, _ = data["head_sha"].(string)
	out.Status, _ = data["status"].(string)
	out.Conclusion, _ = data["conclusion"].(string)
	if headRepo, ok := data["head_repository"].(map[string]any); ok {
		out.HeadRepository, _ = headRepo["full_name"].(string)
	}
	if attempt, ok := data["run_attempt"].(json.Number); ok {
		out.Attempt, _ = attempt.Int64()
	}
	jobs, err := r.gh(ctx, fmt.Sprintf("repos/%s/actions/runs/%s/jobs?per_page=100", repository, runID))
	if err != nil {
		return TrainRun{}, err
	}
	if list, ok := jobs.(map[string]any); ok {
		if items, ok := list["jobs"].([]any); ok {
			for _, raw := range items {
				job, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				out.Jobs = append(out.Jobs, trainJob(job))
			}
		}
	}
	return out, nil
}

// trainJob reads one job: its name, conclusion and steps. A job whose steps key is not a list at all
// is left with StepsReadable false, so the caller answers merge_target_unreadable rather than
// deciding the leg skipped its tests.
func trainJob(job map[string]any) TrainJob {
	out := TrainJob{}
	out.Name, _ = job["name"].(string)
	out.Conclusion, _ = job["conclusion"].(string)
	raw, present := job["steps"]
	if !present {
		return out
	}
	items, ok := raw.([]any)
	if !ok {
		return out
	}
	out.StepsReadable = true
	for _, entry := range items {
		step, ok := entry.(map[string]any)
		if !ok {
			// a malformed entry makes the whole list unreadable
			out.StepsReadable = false
			out.Steps = nil
			return out
		}
		name, _ := step["name"].(string)
		conclusion, _ := step["conclusion"].(string)
		out.Steps = append(out.Steps, TrainStep{Name: name, Conclusion: conclusion})
	}
	return out
}

// Commit reads one commit's parents and tree.
func (r TrainForgeReader) Commit(ctx context.Context, repository, sha string) (TrainCommit, error) {
	if !forgeRepository(repository) {
		return TrainCommit{}, fmt.Errorf("repository %s is not an owner/name forge repository", pyvalue.StrRepr(repository))
	}
	payload, err := r.gh(ctx, fmt.Sprintf("repos/%s/commits/%s", repository, sha))
	if err != nil {
		return TrainCommit{}, err
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return TrainCommit{}, fmt.Errorf("the forge answered with something that is not one commit")
	}
	out := TrainCommit{SHA: sha}
	if named, _ := data["sha"].(string); !SameCommit(named, sha) {
		return TrainCommit{}, fmt.Errorf("the forge's answer does not name commit %s", sha)
	}
	if parents, ok := data["parents"].([]any); ok {
		for _, raw := range parents {
			if p, ok := raw.(map[string]any); ok {
				if psha, ok := p["sha"].(string); ok {
					out.Parents = append(out.Parents, psha)
				}
			}
		}
	}
	if commit, ok := data["commit"].(map[string]any); ok {
		if tree, ok := commit["tree"].(map[string]any); ok {
			out.Tree, _ = tree["sha"].(string)
		}
	}
	return out, nil
}

// Compare answers the forge's compare API status for base..head, which is identical/ahead/behind/
// diverged: "ahead" is the same statement as git merge-base --is-ancestor base head.
func (r TrainForgeReader) Compare(ctx context.Context, repository, base, head string) (string, error) {
	if !forgeRepository(repository) {
		return "", fmt.Errorf("repository %s is not an owner/name forge repository", pyvalue.StrRepr(repository))
	}
	payload, err := r.gh(ctx, fmt.Sprintf("repos/%s/compare/%s...%s", repository, base, head))
	if err != nil {
		return "", err
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return "", fmt.Errorf("the forge answered with something that is not one comparison")
	}
	status, _ := data["status"].(string)
	if status == "" {
		return "", fmt.Errorf("the forge's comparison names no status")
	}
	return status, nil
}

// TrainCheckoutProver is the production TrainCheckout: it proves the chain from git objects alone, in
// a throwaway bare repository that borrows the checkout's objects as alternates, so replace refs,
// attributes, hooks and merge drivers of the checkout cannot change the answer. This is the same
// isolation the crw skill base-refresh check uses, kept here because importing internal/skill from
// this package would close an import cycle.
type TrainCheckoutProver struct{ Git string }

// Chain proves the bundle's first-parent chain: from the head down to the base, one two-parent merge
// per member in the train's order, each merge's second parent that member's accepted head and each
// merge's tree what git merges from its parents. A hand-resolved merge, a non-member commit or a
// different order is disposition_conflict; a git that cannot answer is merge_target_unreadable.
func (r TrainCheckoutProver) Chain(ctx context.Context, checkout, head, base string, members []TrainMemberExpectation) (TrainChain, error) {
	if strings.TrimSpace(checkout) == "" {
		return TrainChain{}, trainUnreadable("a bundle's chain is proved in a checkout, and none was named")
	}
	info, err := os.Stat(checkout)
	if err != nil || !info.IsDir() {
		return TrainChain{}, trainUnreadable("checkout %s is not a directory here", pyvalue.StrRepr(checkout))
	}
	g, err := openTrainGit(ctx, checkout, r.Git)
	if err != nil {
		return TrainChain{}, trainUnreadable("the checkout %s could not be read: %v", pyvalue.StrRepr(checkout), err)
	}
	defer g.close()
	want := make([]string, len(members))
	for i, m := range members {
		want[i] = m.AcceptedHead
	}
	return g.chain(ctx, head, base, want)
}

// trainGit is the throwaway repository the chain proof runs in.
type trainGit struct {
	gitdir string
	gitBin string
	isoEnv []string
}

func trainEnv() []string {
	env := make([]string, 0, 8)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			env = append(env, v)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_NO_REPLACE_OBJECTS=1")
}

func openTrainGit(ctx context.Context, checkout, gitBin string) (*trainGit, error) {
	if gitBin == "" {
		gitBin = "git"
	}
	if _, err := exec.LookPath(gitBin); err != nil {
		return nil, err
	}
	env := trainEnv()
	objects, err := gitOut(ctx, env, gitBin, "-C", checkout, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, err
	}
	format, err := gitOut(ctx, env, gitBin, "-C", checkout, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	format = strings.TrimSpace(format)
	if format != "sha1" && format != "sha256" {
		return nil, fmt.Errorf("git answered %q to --show-object-format", format)
	}
	dir, err := os.MkdirTemp("", "crw-train-")
	if err != nil {
		return nil, err
	}
	g := &trainGit{gitdir: filepath.Join(dir, "g.git"), gitBin: gitBin}
	g.isoEnv = append(trainEnv(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_TEMPLATE_DIR=", "HOME="+dir, "XDG_CONFIG_HOME="+dir)
	if _, err := gitOut(ctx, g.isoEnv, gitBin, "init", "--bare", "-q", "--object-format="+format, g.gitdir); err != nil {
		g.close()
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(g.gitdir, "objects", "info"), 0o700); err != nil {
		g.close()
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(g.gitdir, "objects", "info", "alternates"), []byte(strings.TrimSpace(objects)+"\n"), 0o600); err != nil {
		g.close()
		return nil, err
	}
	return g, nil
}

func (g *trainGit) close() { _ = os.RemoveAll(filepath.Dir(g.gitdir)) }

// iso runs one git command inside the throwaway repository.
func (g *trainGit) iso(ctx context.Context, args ...string) (int, string, error) {
	cmd := exec.CommandContext(ctx, g.gitBin, append([]string{"--git-dir=" + g.gitdir}, args...)...)
	cmd.Env = g.isoEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.String(), fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), exit.ExitCode(), strings.TrimSpace(stderr.String()))
	}
	return -1, "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

func trainFirstLine(s string) string {
	if i := strings.IndexAny(s, "\n\x00"); i >= 0 {
		return s[:i]
	}
	return s
}

func gitOut(ctx context.Context, env []string, gitBin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, gitBin, args...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// parents is a commit's parents.
func (g *trainGit) parents(ctx context.Context, commit string) ([]string, error) {
	code, out, err := g.iso(ctx, "rev-list", "--parents", "-n", "1", commit)
	if code != 0 {
		return nil, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 || fields[0] != commit {
		return nil, fmt.Errorf("git rev-list answered %q for %s", trainFirstLine(out), commit)
	}
	return fields[1:], nil
}

// tree is a commit's tree.
func (g *trainGit) tree(ctx context.Context, commit string) (string, error) {
	code, out, err := g.iso(ctx, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
	if code != 0 {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// mergeTree merges two commits in memory and returns the resulting tree, or an error when git cannot
// merge them without a resolution.
func (g *trainGit) mergeTree(ctx context.Context, first, second string) (string, error) {
	code, out, err := g.iso(ctx, "--attr-source="+first, "merge-tree", "-z", "--write-tree", "--name-only", "--no-messages", first, second)
	if code != 0 {
		return "", fmt.Errorf("git merge-tree could not merge %s and %s without a resolution: %w", first, second, err)
	}
	records := strings.Split(out, "\x00")
	if len(records) == 0 || !hexID(records[0]) {
		return "", fmt.Errorf("git merge-tree answered %q for %s and %s, which is not a tree", trainFirstLine(out), first, second)
	}
	return strings.TrimSpace(records[0]), nil
}

// hexID is whether a string is a full SHA-1 or SHA-256 object name.
func hexID(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// chain walks the head's first-parent line down to the base and checks every step. want holds the
// members' accepted heads in the train's order, oldest first.
func (g *trainGit) chain(ctx context.Context, head, base string, want []string) (TrainChain, error) {
	tree, err := g.tree(ctx, head)
	if err != nil {
		return TrainChain{}, trainUnreadable("the tree of %s was not read: %v", pyvalue.StrRepr(head), err)
	}
	current := head
	commits := make([]string, 0, len(want))
	for i := len(want) - 1; i >= 0; i-- {
		parents, err := g.parents(ctx, current)
		if err != nil {
			return TrainChain{}, trainUnreadable("the parents of %s were not read: %v", pyvalue.StrRepr(current), err)
		}
		if len(parents) != 2 {
			return TrainChain{}, trainConflict("commit %s has %d parent(s), and a bundle's chain is one two-parent merge per member", current, len(parents))
		}
		first, second := parents[0], parents[1]
		if !SameCommit(second, want[i]) {
			return TrainChain{}, trainConflict("the merge commit %s has %s as its second parent, and the member at that place is %s; the bundle's order or a member's head changed", current, pyvalue.StrRepr(second), pyvalue.StrRepr(want[i]))
		}
		merged, err := g.mergeTree(ctx, first, second)
		if err != nil {
			return TrainChain{}, trainConflict("the merge commit %s is not what git merges from its parents: %v", current, err)
		}
		actual, err := g.tree(ctx, current)
		if err != nil {
			return TrainChain{}, trainUnreadable("the tree of %s was not read: %v", pyvalue.StrRepr(current), err)
		}
		if actual != merged {
			return TrainChain{}, trainConflict("the tree of merge commit %s is %s and git merges %s from its parents, so the merge holds a hand resolution", current, actual, merged)
		}
		commits = append(commits, current)
		current = first
	}
	if !SameCommit(current, base) {
		return TrainChain{}, trainConflict("the chain's oldest merge stands on %s and the bundle's base is %s", pyvalue.StrRepr(current), pyvalue.StrRepr(base))
	}
	return TrainChain{Tree: tree, Commits: commits}, nil
}
