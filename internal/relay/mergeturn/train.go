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
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
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
	"gui",
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
	// AcceptedHead is the stand head: the head the member's acceptance stands on, which the bundle
	// carries and the chain proof matches.
	AcceptedHead string
	// RuledEventID is the acceptance event the ruling recorded (CRW-742's event), and RulingHead the
	// head that ruling fixed, with RulingHeadSource naming which reading gave it.
	RuledEventID     string
	RulingHead       string
	RulingHeadSource string
}

// TrainPullRequest is what the forge says about one pull request, the fields verify and land compare.
type TrainPullRequest struct {
	Number  int64
	State   string
	BaseRef string
	HeadSHA string
	Labels  []string
	// Merged is the forge's merged fact. It is a pointer because "the forge answered no merged field"
	// and "the forge answered not merged" are different statements, and a closed answer that carries
	// no merged field must never be read as a merge (CRW-897, answer 1).
	Merged *bool
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
	// File answers the bytes a path holds at one commit in the checkout, read from git objects alone
	// (never the working tree, which may sit on another branch). It is how verify reads the head's
	// .github/workflows/ci.yml (CRW-897, answer 6).
	File(ctx context.Context, checkout, commit, path string) (string, error)
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

// trainSkipEarlyMembership is a test seam: the in-transaction membership re-check (CRW-897, answer
// 4) is what makes two concurrent opens safe, and the early check in front of it hides that. A test
// sets this to drive the transaction with the early check out of the way. It is never set outside
// tests.
var trainSkipEarlyMembership = false

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
	// the leader's pull request is the first member and its turn is today's lane; a list that omits it
	// or puts another pull request first would bypass the head-of-line candidate (finding 1)
	if members[0] != leaderPR {
		return nil, trainConflict("the leader's own pull request %d must be the bundle's first member, and the list starts with %d; a bundle does not bypass the head-of-line candidate", leaderPR, members[0])
	}
	if early.DeclaredReady != 1 {
		return nil, trainConflict("the leader's turn %s has not declared its candidate ready, so it is not a candidate the lane may carry", pyvalue.StrRepr(turn))
	}
	if !early.RelationshipID.Valid || early.RelationshipID.String == "" {
		return nil, trainConflict("the leader's turn %s records no relationship, so its landing cannot be recorded", pyvalue.StrRepr(turn))
	}
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
	// a member that already belongs to another live train is refused here rather than wasting a whole
	// CI run on a bundle whose land would refuse it (gap e)
	if !trainSkipEarlyMembership {
		for _, member := range waiting {
			if _, _, live, err := store.MergeTrainOfTurn(ctx, s.Store, member.TurnID); err != nil {
				return nil, err
			} else if live {
				return nil, trainConflict("turn %s of pull request %d already belongs to a live train, so it cannot ride another", pyvalue.StrRepr(member.TurnID), member.PRNumber.Int64)
			}
		}
	}
	// every member (the leader included) must stand on an active acceptance whose stand head is the
	// head its pull request shows and its turn holds (Blocking 1): in the single lane that tie is
	// dag-accept, which the bundle path would otherwise never pass through
	standFor := func(relationship, memberHead string) (acceptance.Active, string, string, string, error) {
		active, found, err := acceptance.ActiveForRelationship(ctx, s.Store.Querier(ctx), relationship)
		if err != nil {
			return acceptance.Active{}, "", "", "", trainUnreadable("the acceptance of relationship %s was not read: %v", pyvalue.StrRepr(relationship), err)
		}
		if !found {
			return acceptance.Active{}, "", "", "", nil
		}
		stand, err := acceptance.StandOf(ctx, s.Store.Querier(ctx), active.AcceptanceID, relationship, active.Generation, active.EventID, active.RevisionHash, active.HeadSHA)
		if err != nil {
			return acceptance.Active{}, "", "", "", trainUnreadable("what acceptance %s stands on was not read: %v", pyvalue.StrRepr(active.AcceptanceID), err)
		}
		rulingHead, rulingSource, err := acceptance.RulingHead(ctx, s.Store.Querier(ctx), active.EventID, active.HeadSHA)
		if err != nil {
			return acceptance.Active{}, "", "", "", trainUnreadable("the ruling head of event %s was not read: %v", pyvalue.StrRepr(active.EventID), err)
		}
		return active, stand.Head, rulingHead, rulingSource, nil
	}
	for _, pr := range members {
		pull, err := forge.PullRequest(ctx, early.Repository, pr)
		if err != nil {
			return nil, trainUnreadable("pull request %d of %s was not read: %v", pr, pyvalue.StrRepr(early.Repository), err)
		}
		if pull.Number != pr {
			return nil, trainUnreadable("the forge's answer for pull request %d names %d", pr, pull.Number)
		}
		if pull.State != "open" {
			return nil, trainConflict("member pull request %d is %s, and a bundle carries open pull requests", pr, pull.State)
		}
		if pull.BaseRef != early.BaseRef {
			return nil, trainConflict("member pull request %d targets %s and the bundle's base is %s", pr, pyvalue.StrRepr(pull.BaseRef), pyvalue.StrRepr(early.BaseRef))
		}
		if pr == leaderPR {
			if !SameCommit(pull.HeadSHA, early.CandidateHead) {
				return nil, trainConflict("the leader's pull request %d reads head %s and its turn holds %s; a member's head is not refreshed by the bundle", pr, pyvalue.StrRepr(pull.HeadSHA), pyvalue.StrRepr(early.CandidateHead))
			}
			active, standHead, rulingHead, rulingSource, err := standFor(early.RelationshipID.String, early.CandidateHead)
			if err != nil {
				return nil, err
			}
			if err := trainStandRefusal(pr, early.RelationshipID.String, early.CandidateHead, active, standHead); err != nil {
				return nil, err
			}
			expectations = append(expectations, TrainMemberExpectation{TurnID: turn, PRNumber: pr, RelationshipID: early.RelationshipID.String, AcceptedHead: early.CandidateHead, RuledEventID: active.EventID, RulingHead: rulingHead, RulingHeadSource: rulingSource})
			continue
		}
		member, found := waitingTurnFor(waiting, pr)
		if !found {
			return nil, trainConflict("pull request %d has no waiting turn on %s, so it is not a member this bundle may carry", pr, pyvalue.StrRepr(early.TargetKey))
		}
		if !SameCommit(pull.HeadSHA, member.CandidateHead) {
			return nil, trainConflict("pull request %d reads head %s and its waiting turn holds %s; the member's head moved", pr, pyvalue.StrRepr(pull.HeadSHA), pyvalue.StrRepr(member.CandidateHead))
		}
		if member.DeclaredReady != 1 {
			return nil, trainConflict("pull request %d's turn %s has not declared its candidate ready, so it is not a candidate the lane may carry", pr, pyvalue.StrRepr(member.TurnID))
		}
		if !member.RelationshipID.Valid || member.RelationshipID.String == "" {
			return nil, trainConflict("pull request %d's turn %s records no relationship, so its landing cannot be recorded", pr, pyvalue.StrRepr(member.TurnID))
		}
		active, standHead, rulingHead, rulingSource, err := standFor(member.RelationshipID.String, member.CandidateHead)
		if err != nil {
			return nil, err
		}
		if err := trainStandRefusal(pr, member.RelationshipID.String, member.CandidateHead, active, standHead); err != nil {
			return nil, err
		}
		expectations = append(expectations, TrainMemberExpectation{TurnID: member.TurnID, PRNumber: pr, RelationshipID: member.RelationshipID.String, AcceptedHead: member.CandidateHead, RuledEventID: active.EventID, RulingHead: rulingHead, RulingHeadSource: rulingSource})
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
		// the membership check again, inside the transaction that records the train (CRW-897, answer
		// 4): the early check above is a CI-saving pre-filter, and two opens that overlap between it
		// and here would otherwise register the same turns in two live trains. The leader's turn is
		// re-checked here too, so a leader that joined another train while this open was being read is
		// refused rather than opening a second one.
		if e := s.trainMembershipRefusal(tx, actor, expectations); e != nil {
			return e
		}
		// a train id names one opening, not the candidate: a turn that abandons a train and reopens
		// one for the same head gets a fresh id rather than colliding with the abandoned record
		for suffix := 2; ; suffix++ {
			_, found, e := store.MergeTrain(tx, s.Store, id)
			if e != nil {
				return e
			}
			if !found {
				break
			}
			id = trainID(early.TargetKey, turn, early.CandidateHead) + "-" + strconv.Itoa(suffix)
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

// trainMembershipRefusal is answer 4's in-transaction check: no member turn (the leader's included)
// belongs to a live train, and the leader task leads none. It reads through the transaction's
// connection, so the rows it sees are the ones the recording transaction will write beside. Each
// member turn is also reread here: open read the members' turns and their pull requests before this
// transaction, and a claimant can withdraw or restate one during those reads (Devin's
// check-then-act finding), so the turn that will be recorded must still be the one open chose.
func (s *Service) trainMembershipRefusal(ctx context.Context, actor string, members []TrainMemberExpectation) error {
	for _, m := range members {
		if _, _, live, err := store.MergeTrainOfTurn(ctx, s.Store, m.TurnID); err != nil {
			return err
		} else if live {
			return trainConflict("turn %s of pull request %d already belongs to a live train, so it cannot ride another", pyvalue.StrRepr(m.TurnID), m.PRNumber)
		}
		turn, err := s.Store.MergeTurn(ctx, m.TurnID)
		if err != nil {
			return err
		}
		if turn.State != Holding && turn.State != Waiting {
			return trainConflict("turn %s of pull request %d is %s, so it left the lane while the bundle was being read; call again", pyvalue.StrRepr(m.TurnID), m.PRNumber, turn.State)
		}
		if !SameCommit(turn.CandidateHead, m.AcceptedHead) {
			return trainConflict("turn %s of pull request %d now holds %s and the bundle was opened on %s, so it changed while the bundle was being read; call again", pyvalue.StrRepr(m.TurnID), m.PRNumber, pyvalue.StrRepr(turn.CandidateHead), pyvalue.StrRepr(m.AcceptedHead))
		}
	}
	if live, err := s.trainLedBy(ctx, actor); err != nil {
		return err
	} else if live != "" {
		return trainConflict("task %s already leads the live train %s, so it cannot open another until that one lands, is done or is abandoned", pyvalue.StrRepr(actor), pyvalue.StrRepr(live))
	}
	return nil
}

// trainLedBy is the live train a task leads, or "" when it leads none. A train is live while its
// newest event is opened or verified; the leader of a train is not a member of it, so the member
// table cannot answer this.
func (s *Service) trainLedBy(ctx context.Context, actor string) (string, error) {
	rows, err := s.Store.All(ctx, "SELECT train_id, leader_task_id FROM merge_trains")
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if fmt.Sprint(row.Get("leader_task_id")) != actor {
			continue
		}
		trainID := fmt.Sprint(row.Get("train_id"))
		state, found, err := store.MergeTrainState(ctx, s.Store, trainID)
		if err != nil {
			return "", err
		}
		if !found || state == store.MergeTrainOpened || state == store.MergeTrainVerified {
			return trainID, nil
		}
	}
	return "", nil
}

// membersWhoLeftTheLane is the member turns that are neither holding nor waiting. Opening a train
// admits only a holding leader and waiting members, and the member guard keeps a member of a live
// train out of check and single land, so such a turn left the lane after the train opened (a parent
// returned or withdrew it). Land excludes those from the landing and names them in the landed
// event (CRW-897, answer 1); a moved head there is not a reason to refuse the rest of the bundle.
func (s *Service) membersWhoLeftTheLane(ctx context.Context, members []store.MergeTrainMemberRow) (map[string]bool, error) {
	out := map[string]bool{}
	for _, m := range members {
		turn, err := s.Store.MergeTurn(ctx, m.TurnID)
		if err != nil {
			return nil, err
		}
		if turn.State != Holding && turn.State != Waiting {
			out[m.TurnID] = true
		}
	}
	return out, nil
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

// trainStandRefusal is Blocking 1's gate: the member must have an active acceptance whose stand head
// is the head its pull request shows and its turn holds.
func trainStandRefusal(pr int64, relationship, memberHead string, active acceptance.Active, standHead string) error {
	if active.AcceptanceID == "" {
		return trainConflict("pull request %d's relationship %s has no active acceptance, so the member is not a verified, accepted candidate this bundle may carry; run dag-accept on its head first", pr, pyvalue.StrRepr(relationship))
	}
	if !SameCommit(standHead, memberHead) {
		return trainConflict("pull request %d stands on %s and its acceptance %s stands on %s, so the member's head is not the head the ruling covers", pr, pyvalue.StrRepr(memberHead), pyvalue.StrRepr(active.AcceptanceID), pyvalue.StrRepr(standHead))
	}
	return nil
}

// trainMemberRefusal rereads every member pull request from the forge and refuses one that no
// longer proposes what the train carries (CRW-897, answer 1). The train's member rows are a
// snapshot taken at open, and both verify and land write against it: a member that moved must not
// be verified into a bundle whose CI ran on the old head, nor recorded landed while its new commits
// are left behind; and a member whose pull request was closed to cancel it or retargeted away from
// the bundle's base no longer proposes this landing at all, so it is refused too, on the same
// invariants Open applies. disposition_conflict names the pull request and what changed, and
// nothing is written; a forge that cannot answer is merge_target_unreadable.
//
// exempt names the member turns that left the lane (a parent returned or withdrew them): land
// excludes those from the landing and names them in the landed event, so their pull request is not
// read at all and neither a moved head nor an unreadable pull request there can refuse the rest of
// the bundle. A nil exempt refuses every member that disagrees, which is what verify wants.
func trainMemberRefusal(ctx context.Context, forge TrainForge, repository, baseRef string, members []store.MergeTrainMemberRow, exempt map[string]bool) error {
	for _, m := range members {
		if exempt[m.TurnID] {
			continue
		}
		pull, err := forge.PullRequest(ctx, repository, m.PRNumber)
		if err != nil {
			return trainUnreadable("member pull request %d of %s was not read: %v", m.PRNumber, pyvalue.StrRepr(repository), err)
		}
		if err := trainMemberDisposition(m.PRNumber, m.MemberHead, baseRef, pull, false); err != nil {
			return err
		}
	}
	return nil
}

// trainMemberDisposition judges one member pull request the forge just read against what the bundle
// carries. admitMerged is land's rule: a member the bundle has already merged reads closed with merged
// true, which is the very landing the bundle recorded, so land admits it; verify passes false and
// takes open pull requests only. The head and base checks are unchanged by the merged fact, so a
// member that moved or retargeted still refuses disposition_conflict, and a closed answer that carries
// no merged field is not a merge, so it refuses too (CRW-897, answer 1).
func trainMemberDisposition(prNumber int64, memberHead, baseRef string, pull TrainPullRequest, admitMerged bool) error {
	if pull.Number != prNumber {
		return trainUnreadable("the forge's answer for member pull request %d names %d", prNumber, pull.Number)
	}
	if !SameCommit(pull.HeadSHA, memberHead) {
		return trainConflict("member pull request %d reads head %s and the bundle carries %s, so the member moved after the train opened", prNumber, pyvalue.StrRepr(pull.HeadSHA), pyvalue.StrRepr(memberHead))
	}
	if pull.State != "open" && !trainMemberAdmitsMerged(pull, admitMerged) {
		return trainConflict("member pull request %d is %s and the bundle carries its head %s, so the member no longer proposes this landing", prNumber, pull.State, pyvalue.StrRepr(memberHead))
	}
	if pull.BaseRef != baseRef {
		return trainConflict("member pull request %d targets %s and the bundle's base is %s, so the member no longer proposes this landing", prNumber, pyvalue.StrRepr(pull.BaseRef), pyvalue.StrRepr(baseRef))
	}
	return nil
}

// trainMemberAdmitsMerged is the one rule that admits a member pull request that is not open: land,
// and only land, takes a member the forge read as merged (state closed with merged true), because the
// bundle merge put its head in the base. A closed answer the forge gave no merged field for is not a
// merge, so it is refused like any other closed pull request.
func trainMemberAdmitsMerged(pull TrainPullRequest, admitMerged bool) bool {
	return admitMerged && pull.State == "closed" && pull.Merged != nil && *pull.Merged
}

// trainMemberMapping is the per-member mapping decision 10 requires, built the same way at open,
// verify and land so the three event details carry the same columns: the relationship, the ruled
// event, the head fixed at the ruling, the accepted (stand) head, the member head in the train and
// the order. It is the immutable snapshot of the ruling context, because dag_acceptances is refreshable.
func trainMemberMapping(members []TrainMemberExpectation) []any {
	list := make([]any, 0, len(members))
	for i, m := range members {
		entry := map[string]any{"seq": i + 1, "turnId": m.TurnID, "prNumber": m.PRNumber, "relationshipId": m.RelationshipID,
			"ruledEventId": m.RuledEventID, "rulingHead": m.RulingHead, "acceptedHead": m.AcceptedHead, "memberHead": m.AcceptedHead}
		if m.RulingHeadSource != "" {
			entry["rulingHeadSource"] = m.RulingHeadSource
		}
		list = append(list, entry)
	}
	return list
}

// trainOpenedDetail is the opened event's body: the members in order and the base D.
func trainOpenedDetail(members []TrainMemberExpectation, base string) map[string]any {
	return map[string]any{"members": trainMemberMapping(members), "baseSha": base}
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
		nodes, err := s.acceptedNodesForRelationship(ctx, m.RelationshipID)
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

// acceptedNodesForRelationship is the plan node a member's active acceptance names: relationship ->
// acceptance -> plan/node (CRW-897, answer 2). Looking the node up by commit head misses a member
// whose acceptance was base-refreshed, because the refreshed stand head is not the row's head_sha;
// the relationship is what the train already carries. The row selection is the one
// acceptance.ActiveForRelationship makes, so the train and the stand reading name the same row.
func (s *Service) acceptedNodesForRelationship(ctx context.Context, relationship string) ([]trainNode, error) {
	if relationship == "" {
		return nil, nil
	}
	rows, err := s.Store.All(ctx, "SELECT plan_id, node_id FROM dag_acceptances WHERE relationship_id = ? AND state = 'active' ORDER BY accepted_at DESC, acceptance_id DESC LIMIT 1", relationship)
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
	// every member pull request's head is reread from the forge here (CRW-897, answer 1): the head
	// the train opened on is a snapshot, and a member that moved after it must not be verified into
	// the bundle whose CI ran on the old head. A moved member is disposition_conflict naming the
	// pull request and both heads, and nothing is written.
	if err := trainMemberRefusal(ctx, forge, row.Repository, row.BaseRef, members, nil); err != nil {
		return nil, err
	}
	if proof == nil {
		return nil, trainUnreadable("this relay has no checkout prover configured, so the bundle's first-parent chain cannot be proved")
	}
	// the head's own job set, read from the checkout at H (never its working tree, which may sit on
	// another branch), is compared with the set this runtime verifies BEFORE the run's own jobs
	// (CRW-897, answer 3). A head that renamed or added a ci.yml job must be refused naming what it
	// lost and what it added, and the run — which carries only the new name — cannot say that; the
	// run's own check would refuse first with "holds no job named X" and hide the diagnosis. The read
	// runs after --head was compared with the forge's bundle head above, so the commit named here is
	// the forge's own; a checkout that cannot resolve H is merge_target_unreadable here just as the
	// chain proof would answer. A workflow that cannot be read or parsed is merge_target_unreadable,
	// never a pass.
	if err := trainWorkflowRefusal(ctx, proof, checkout, head); err != nil {
		return nil, err
	}
	reading, err := forge.Run(ctx, row.Repository, run)
	if err != nil {
		return nil, trainUnreadable("workflow run %s of %s was not read: %v", run, pyvalue.StrRepr(row.Repository), err)
	}
	if err := trainRunRefusal(row.Repository, head, reading); err != nil {
		return nil, err
	}
	expected, err := s.trainExpectations(ctx, members)
	if err != nil {
		return nil, err
	}
	chain, err := proof.Chain(ctx, checkout, head, row.BaseSHA, expected)
	if err != nil {
		return nil, err
	}
	at := s.now()
	detail := trainVerifiedDetail(row.BaseSHA, pr, head, chain.Tree, run, expected)
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

// trainExpectations rebuilds the per-member mapping from the member rows and a fresh acceptance read:
// the rows stay as they are (decision 8, the event detail is the snapshot), and the ruling columns come
// from the acceptance as it stands now. A member whose acceptance was withdrawn or moved is refused
// disposition_conflict, naming the member, so no verified event or landing is written for it.
func (s *Service) trainExpectations(ctx context.Context, members []store.MergeTrainMemberRow) ([]TrainMemberExpectation, error) {
	out := make([]TrainMemberExpectation, 0, len(members))
	for _, m := range members {
		active, found, err := acceptance.ActiveForRelationship(ctx, s.Store.Querier(ctx), m.RelationshipID)
		if err != nil {
			return nil, trainUnreadable("the acceptance of relationship %s was not read: %v", pyvalue.StrRepr(m.RelationshipID), err)
		}
		if !found {
			return nil, trainConflict("pull request %d's relationship %s has no active acceptance, so the member is no longer a verified, accepted candidate", m.PRNumber, pyvalue.StrRepr(m.RelationshipID))
		}
		stand, err := acceptance.StandOf(ctx, s.Store.Querier(ctx), active.AcceptanceID, m.RelationshipID, active.Generation, active.EventID, active.RevisionHash, active.HeadSHA)
		if err != nil {
			return nil, trainUnreadable("what acceptance %s stands on was not read: %v", pyvalue.StrRepr(active.AcceptanceID), err)
		}
		if err := trainStandRefusal(m.PRNumber, m.RelationshipID, m.MemberHead, active, stand.Head); err != nil {
			return nil, err
		}
		rulingHead, rulingSource, err := acceptance.RulingHead(ctx, s.Store.Querier(ctx), active.EventID, active.HeadSHA)
		if err != nil {
			return nil, trainUnreadable("the ruling head of event %s was not read: %v", pyvalue.StrRepr(active.EventID), err)
		}
		out = append(out, TrainMemberExpectation{TurnID: m.TurnID, PRNumber: m.PRNumber, RelationshipID: m.RelationshipID, AcceptedHead: m.MemberHead, RuledEventID: active.EventID, RulingHead: rulingHead, RulingHeadSource: rulingSource})
	}
	return out, nil
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
func trainVerifiedDetail(base string, pr int64, head, tree, run string, members []TrainMemberExpectation) map[string]any {
	return map[string]any{"bundlePr": pr, "baseSha": base, "head": head, "tree": tree, "run": run, "members": trainMemberMapping(members)}
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
	tested, _ := verified["tree"].(string)
	if head == "" || tested == "" {
		return nil, trainUnreadable("the verified event of train %s names no %s, so the landing cannot be compared with it", pyvalue.StrRepr(train), map[bool]string{true: "head", false: "tree"}[head == ""])
	}
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
	// the landed tree must be the tree the bundle CI passed on: a commit can carry the right parents
	// and an arbitrary tree, so the verified tree is compared here rather than trusted from the shape
	// the landed tree must equal the tree the bundle CI passed on. A forge answer that carries no tree
	// is unreadable, never a skipped comparison (gap c).
	if commit.Tree == "" {
		return nil, trainUnreadable("merge commit %s was read without a tree, so whether it preserves the tested tree cannot be told", landed)
	}
	if commit.Tree != tested {
		return nil, trainConflict("merge commit %s has tree %s and the verified bundle tree is %s, so the landed tree is not the tree CI passed", landed, commit.Tree, tested)
	}
	members, err := store.MergeTrainMembers(ctx, s.Store, train)
	if err != nil {
		return nil, err
	}
	// every member pull request is reread from the forge before anything is written (CRW-897,
	// answer 1): the stored member head is the snapshot the train opened on, and a member that moved
	// while the bundle was in CI must not be recorded landed with its new commits left behind. A
	// member the bundle has already merged reads closed with merged true and is admitted here, since
	// its head is in the base through this very landing; a member closed without a merge, one whose
	// answer carries no merged field, and one that moved are refused disposition_conflict with
	// nothing written. A member whose turn left the lane is excluded from the landing and is not read
	// at all.
	left, err := s.membersWhoLeftTheLane(ctx, members)
	if err != nil {
		return nil, err
	}
	for _, m := range members {
		if left[m.TurnID] {
			continue
		}
		pull, err := forge.PullRequest(ctx, row.Repository, m.PRNumber)
		if err != nil {
			return nil, trainUnreadable("member pull request %d of %s was not read: %v", m.PRNumber, pyvalue.StrRepr(row.Repository), err)
		}
		if err := trainMemberDisposition(m.PRNumber, m.MemberHead, row.BaseRef, pull, true); err != nil {
			return nil, err
		}
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
		// each member turn is read inside this transaction (CRW-897, answer 1). Opening a train
		// admits only a holding leader and waiting members, so a member turn that is neither has
		// left the lane since the train opened: it is not recorded landed, and it is named in the
		// landed event with the reason instead. A member whose turn is still live and whose head
		// moved is still refused, as finding 2 requires.
		survivors := make([]store.MergeTrainMemberRow, 0, len(members))
		turns := make(map[string]store.MergeTurnsRow, len(members))
		excluded := make([]any, 0, len(members))
		for _, m := range members {
			turn, e := s.Store.MergeTurn(tx, m.TurnID)
			if e != nil {
				return e
			}
			if turn.State != Holding && turn.State != Waiting {
				excluded = append(excluded, map[string]any{"seq": m.Seq, "turnId": m.TurnID, "prNumber": m.PRNumber, "relationshipId": m.RelationshipID, "state": turn.State, "closeReason": value(turn.CloseReason)})
				continue
			}
			if !SameCommit(turn.CandidateHead, m.MemberHead) {
				return trainConflict("member turn %s of pull request %d now holds %s and the bundle carries %s, so the member changed after the train opened", pyvalue.StrRepr(m.TurnID), m.PRNumber, pyvalue.StrRepr(turn.CandidateHead), pyvalue.StrRepr(m.MemberHead))
			}
			turns[m.TurnID] = turn
			survivors = append(survivors, m)
		}
		// the acceptance of each surviving member is re-read inside this transaction: one withdrawn
		// or moved after open refuses the landing with nothing written (Blocking 1). A member that
		// left the lane is excluded above and is not re-read, so its parent revoking the acceptance
		// does not refuse the rest of the bundle.
		expectations, e := s.trainExpectations(tx, survivors)
		if e != nil {
			return e
		}
		// every surviving member turn lands, the leader's included, in the same transaction as the
		// event, so a failure leaves neither the rows nor the event (the relay's one-transaction
		// rule).
		for _, m := range survivors {
			if e := s.closeWith(tx, turns[m.TurnID], "landed", reason, actor, at, nullable(landed), nullable(tip.SHA)); e != nil {
				return e
			}
		}
		detail := trainLandedDetail(row.BaseSHA, landed, head, tested, reason, expectations, excluded)
		if e := store.RecordMergeTrainEvent(tx, s.Store, store.MergeTrainEventRow{TrainID: train, Seq: seq, Kind: store.MergeTrainLanded, Actor: actor, DetailJSON: pythonJSON(detail), RecordedAt: at}); e != nil {
			return e
		}
		// the landing released the target: a ready turn that joined after the train opened takes the
		// next lane turn in the same transaction (finding 5). An external turn may already hold it —
		// an excluded member's parent can return or withdraw its turn between verify and land, and a
		// return promotes the next waiter — so the occupant is read first: promoting while another
		// turn already holds the target would break the one-live-holder constraint and roll the
		// whole landing back (CRW-897, answer 1).
		occupant, occupantErr := s.Store.MergeTargetOccupant(tx, row.TargetKey)
		if occupantErr != nil && !errors.Is(occupantErr, sql.ErrNoRows) {
			return occupantErr
		}
		if occupant.TurnID == "" {
			if _, e := s.promote(tx, row.TargetKey, at); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.trainAnswer(ctx, train)
}

// trainLandedDetail is the landed event's body: the merge commit M, the verified head H, the close
// reason, the per-member mapping of the members that landed, and the members left out because their
// turn left the lane after the train opened (CRW-897, answer 1). The excluded list is always
// present, empty when every member landed, so a reader never has to tell an absent key from none.
func trainLandedDetail(base, landed, head, tree, reason string, members []TrainMemberExpectation, excluded []any) map[string]any {
	if excluded == nil {
		excluded = []any{}
	}
	return map[string]any{"landedSha": landed, "baseSha": base, "head": head, "tree": tree, "reason": reason, "members": trainMemberMapping(members), "excluded": excluded}
}

// Close is merge-train-close: it appends the terminal event. It moves no member turn: opening a
// train never moved them, so an abandoned close leaves the leader holding the lane turn (which is
// what a reopen needs) and every other member waiting (so a replacement bundle may carry it). A
// member whose turn a parent returned or withdrew in the meantime stays as that parent left it.
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
	if err := trainCloseStateRefusal(train, kind, current); err != nil {
		return nil, err
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
		if e := trainCloseStateRefusal(train, kind, latest); e != nil {
			return e
		}
		// An abandoned train leaves the member turns exactly where they are: opening a train never
		// moved them, so the leader is still holding and every other member is still waiting. That is
		// what "the member turns return to the waiting queue" means here, and it is what makes the
		// documented abandon-and-reopen flow work: merge-train-open requires a holding turn, and the
		// leader is still the one holding it. Closing the leader as returned would leave the target
		// with no holder and no way to reopen without an extra claim.
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
	verified, found, err := s.newestVerified(ctx, row.TrainID)
	if err != nil {
		return map[string]any{"unreadable": err.Error()}
	}
	if !found {
		return map[string]any{"unreadable": "the train holds no verified event to reconcile against"}
	}
	head, _ := verified["head"].(string)
	if head == "" {
		return map[string]any{"unreadable": "the verified event names no head to reconcile against"}
	}
	// the combined head H is what the bundle landed: every member head being an ancestor of the tip
	// is not enough, because separate merges could have put them there while H never landed (finding 11)
	combined, err := forge.Compare(ctx, row.Repository, head, tip.SHA)
	if err != nil {
		return map[string]any{"unreadable": "the ancestry of the verified head " + head + " in " + tip.SHA + " was not read: " + err.Error()}
	}
	list := make([]any, 0, len(members))
	landed := combined == "identical" || combined == "ahead"
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
	return map[string]any{"devTip": tip.SHA, "verifiedHead": head, "verifiedHeadStatus": combined, "landed": landed, "members": list}
}

// TrainPlanEdge is one live plan edge between two accepted nodes, the shape decision 10's failure rule
// reads: FromNodeID is a predecessor of ToNodeID. The ids are plan node ids, so the helpers below
// take an explicit node-to-member mapping rather than matching them against a member's commit head.
type TrainPlanEdge struct {
	FromNodeID string
	ToNodeID   string
}

// TrainMemberNode is one member's plan node: the pull request the member carries and the node id its
// accepted head was accepted under, which is the key the plan edges are written in.
type TrainMemberNode struct {
	PRNumber int64
	NodeID   string
}

// TrainDependencyClosure is decision 10's failure rule made checkable: the members that must be
// removed with a failing one, its successors on the plan edges taken transitively. The failing pull
// request is included. It is a pure function the leader applies by hand (the lane script is out of
// scope here); it adds no command and no refusal reason.
func TrainDependencyClosure(nodes []TrainMemberNode, edges []TrainPlanEdge, failingPR int64) []int64 {
	prOf := map[string]int64{}
	for _, n := range nodes {
		if n.NodeID != "" {
			prOf[n.NodeID] = n.PRNumber
		}
	}
	successors := map[string][]string{}
	for _, e := range edges {
		successors[e.FromNodeID] = append(successors[e.FromNodeID], e.ToNodeID)
	}
	removed := map[int64]bool{failingPR: true}
	queue := []string{}
	for _, n := range nodes {
		if n.PRNumber == failingPR && n.NodeID != "" {
			queue = append(queue, n.NodeID)
		}
	}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, next := range successors[node] {
			if pr, ok := prOf[next]; ok && !removed[pr] {
				removed[pr] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]int64, 0, len(removed))
	for _, n := range nodes {
		if removed[n.PRNumber] {
			out = append(out, n.PRNumber)
		}
	}
	return out
}

// TrainHalve is decision 10's halving rule made checkable: when a failure's cause cannot be named,
// the bundle is split in two with every predecessor and its successors kept on the same side, so no
// half carries a member whose dependency is in the other. Members joined by plan edges (in either
// direction) form groups; the groups are filled into the first half in the bundle's order, each
// whole group at a time, while the group still fits the half, and the rest go to the second. The
// order inside each half is the bundle's order. One group holding every member cannot be split, so
// it is all of the first half and the second is empty. The split is structural, so it always
// terminates: the repair loop the earlier version ran could oscillate forever on a join whose
// predecessors land in different halves (CRW-897, answer 3).
func TrainHalve(order []int64, nodes []TrainMemberNode, edges []TrainPlanEdge) (first, second []int64) {
	if len(order) < 2 {
		return append([]int64{}, order...), nil
	}
	prOf := map[string]int64{}
	for _, n := range nodes {
		if n.NodeID != "" {
			prOf[n.NodeID] = n.PRNumber
		}
	}
	// groups are the members joined by plan edges, in either direction: an edge whose endpoints are
	// both members joins them, and an edge to a member outside the bundle joins nothing.
	group := map[int64]int64{}
	for _, pr := range order {
		group[pr] = pr
	}
	find := func(pr int64) int64 {
		for group[pr] != pr {
			group[pr] = group[group[pr]]
			pr = group[pr]
		}
		return pr
	}
	for _, e := range edges {
		from, okFrom := prOf[e.FromNodeID]
		to, okTo := prOf[e.ToNodeID]
		if !okFrom || !okTo {
			continue
		}
		if _, inOrder := group[from]; !inOrder {
			continue
		}
		if _, inOrder := group[to]; !inOrder {
			continue
		}
		rootFrom, rootTo := find(from), find(to)
		if rootFrom != rootTo {
			group[rootTo] = rootFrom
		}
	}
	// the groups in the bundle's order, each by the place of its first member
	type trainGroup struct {
		root    int64
		members []int64
	}
	var groups []trainGroup
	index := map[int64]int{}
	for _, pr := range order {
		root := find(pr)
		if i, seen := index[root]; seen {
			groups[i].members = append(groups[i].members, pr)
			continue
		}
		index[root] = len(groups)
		groups = append(groups, trainGroup{root: root, members: []int64{pr}})
	}
	// one group holding every member cannot be split
	if len(groups) == 1 {
		return append([]int64{}, order...), nil
	}
	half := (len(order) + 1) / 2
	// the groups are filled into the first half in order, each whole group while it still fits;
	// the rest go to the second. Each half is then emitted in the bundle's order, so the two sides
	// stay in the order the bundle named (the halving moves members between halves, never within one).
	// the first half is a PREFIX of the ordered groups: whole groups are taken in order until the
	// next one would exceed half, and everything from there on — including a smaller group that
	// would still fit — goes to the second (CRW-897, answer 3). The first group is always taken, so
	// a bundle whose leading joined group is larger than half is still split with the first half
	// non-empty, which is the case the shipped chain test pins; taking a later small group after
	// the overflow is what would make the split a non-prefix selection.
	inFirst := map[int64]bool{}
	taken := 0
	for _, g := range groups {
		if taken+len(g.members) > half && taken > 0 {
			break
		}
		taken += len(g.members)
		for _, pr := range g.members {
			inFirst[pr] = true
		}
	}
	first = []int64{}
	second = []int64{}
	for _, pr := range order {
		if inFirst[pr] {
			first = append(first, pr)
			continue
		}
		second = append(second, pr)
	}
	return first, second
}

// trainCloseStateRefusal is finding 7: done closes only a landed train, and abandoned closes only a
// train that has not landed, so a premature done cannot remove the member guards without a landing
// and an abandonment after a landing cannot make the derived state report a failure that did not
// happen.
func trainCloseStateRefusal(train string, kind, current string) error {
	if kind == store.MergeTrainDone && current != store.MergeTrainLanded {
		return trainConflict("train %s is %s and closes done only after it landed", pyvalue.StrRepr(train), current)
	}
	if kind == store.MergeTrainAbandoned && current == store.MergeTrainLanded {
		return trainConflict("train %s landed and is closed done, not abandoned", pyvalue.StrRepr(train))
	}
	return nil
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
	if merged, ok := data["merged"].(bool); ok {
		out.Merged = &merged
	}
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
	if out.Tree == "" || !githubSHA.MatchString(out.Tree) {
		return TrainCommit{}, fmt.Errorf("the forge's answer for commit %s names no tree", sha)
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

// File answers the bytes a path holds at one commit, read from the checkout's git objects rather
// than its working tree: the checkout may sit on another branch, and verify must read what the head
// it is verifying actually declares. A checkout or commit git cannot read is merge_target_unreadable.
func (r TrainCheckoutProver) File(ctx context.Context, checkout, commit, path string) (string, error) {
	if strings.TrimSpace(checkout) == "" {
		return "", trainUnreadable("a bundle's head is read in a checkout, and none was named")
	}
	info, err := os.Stat(checkout)
	if err != nil || !info.IsDir() {
		return "", trainUnreadable("checkout %s is not a directory here", pyvalue.StrRepr(checkout))
	}
	g, err := openTrainGit(ctx, checkout, r.Git)
	if err != nil {
		return "", trainUnreadable("the checkout %s could not be read: %v", pyvalue.StrRepr(checkout), err)
	}
	defer g.close()
	return g.file(ctx, commit, path)
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

// file is one path's bytes at one commit. --end-of-options keeps a commit that looks like a flag
// from being read as one, and the commit and path are joined with the object syntax git defines.
func (g *trainGit) file(ctx context.Context, commit, path string) (string, error) {
	code, out, err := g.iso(ctx, "show", "--end-of-options", commit+":"+path)
	if code != 0 {
		return "", err
	}
	return out, nil
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
