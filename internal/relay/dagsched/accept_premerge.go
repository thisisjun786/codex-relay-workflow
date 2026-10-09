package dagsched

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The pre-merge gate inside the relay (CRW-952). An implementation node's acceptance judges the pre-merge record the
// parent supplies, with the judgment internal/relay/acceptance/premerge owns, and stores the record with the
// acceptance. Integration judges the stored text again. Two heads may differ from the evaluated one: a chain of up to
// four merges of dev-only commits keeps the evaluation, and a later plain commit needs the parent's afterEvaluation
// statement (the judgment reads it). A hand-resolved merge, or a merge of a commit outside dev, does not.

// maxPremergeHeadWalk bounds the first-parent walk from a head back to the evaluated head.
const maxPremergeHeadWalk = 200

// maxPremergeDevMerges is how many dev-only merges a chain from the evaluated head may hold.
const maxPremergeDevMerges = 4

// premergeJudgment is a pre-merge record judged for one acceptance: the record bytes as read, their digest and the
// head the evaluation names.
type premergeJudgment struct {
	raw           []byte
	digest        string
	evaluatedHead string
	acceptedHead  string
}

// premergeRefusal carries a judgment's refusal into the relay's own refusal names.
func premergeRefusal(err error) error {
	var refusal *premerge.Refusal
	if errors.As(err, &refusal) {
		return refuse(refusal.Reason, "%s", refusal.Detail)
	}
	return err
}

// judgePremerge decides the record for an acceptance of node n (live in the current plan) on acceptedHead. commit is
// the pull-request-less path's checkout and base, and nil on the pull-request path, where the record head must be the
// pull request head exactly.
func judgePremerge(ctx context.Context, raw []byte, node string, n dag.SnapNode, acceptedHead string, commit *premergeCheckout) (premergeJudgment, error) {
	if len(raw) == 0 {
		return premergeJudgment{}, refuse(contract.RefusalPremergeMissing, "node %s is an implementation node: its acceptance needs the pre-merge record (--premerge)", node)
	}
	rec, err := premerge.Decode(raw)
	if err != nil {
		return premergeJudgment{}, premergeRefusal(err)
	}
	digest, err := premerge.Digest(raw)
	if err != nil {
		return premergeJudgment{}, refuse(contract.RefusalPremergeMissing, "the pre-merge record cannot be canonicalised: %v", err)
	}
	if rec.Node != node || rec.Issue != n.IssueKey {
		return premergeJudgment{}, refuse(contract.RefusalPremergeSubjectMismatch, "the record is for issue %s node %s, and the acceptance is for issue %s node %s", rec.Issue, rec.Node, n.IssueKey, node)
	}
	afterEvaluation := false
	if commit == nil {
		if rec.Head != acceptedHead {
			return premergeJudgment{}, refuse(contract.RefusalPremergeHeadMismatch, "the record evaluated head %s and the pull request head is %s", rec.Head, acceptedHead)
		}
	} else if afterEvaluation, err = premergeHeadRelation(ctx, commit.checkout, commit.base, rec.Head, acceptedHead); err != nil {
		return premergeJudgment{}, err
	}
	if rec.CriteriaDigest != n.CriteriaSetDigest {
		return premergeJudgment{}, refuse(contract.RefusalPremergeCriteriaStale, "the record was judged against criteria %s and node %s is registered with %s", rec.CriteriaDigest, node, n.CriteriaSetDigest)
	}
	if err := premerge.Judge(rec, premerge.Options{AfterEvaluation: afterEvaluation}); err != nil {
		return premergeJudgment{}, premergeRefusal(err)
	}
	return premergeJudgment{raw: raw, digest: digest, evaluatedHead: rec.Head, acceptedHead: acceptedHead}, nil
}

// premergeCheckout is the pull-request-less path's local checkout and the base the head must descend from.
// readPremergeRecord reads --premerge: the record inline, or @file.
func readPremergeRecord(input string) ([]byte, error) {
	if !strings.HasPrefix(input, "@") {
		if err := store.EncodeUTF8(input); err != nil {
			return nil, usage("--premerge is not valid UTF-8: " + err.Error())
		}
		return []byte(input), nil
	}
	raw, err := os.ReadFile(input[1:])
	if err != nil {
		return nil, usage("--premerge names a file that cannot be read: " + err.Error())
	}
	return raw, nil
}

type premergeCheckout struct{ checkout, base string }

// premergeHeadRelation reports whether accepted is the evaluated head, or a head that is later than it by a plain commit
// (true: the parent's afterEvaluation statement applies) or by a chain of dev-only merges (false: the evaluation keeps).
// Every other shape is a head mismatch, including a merge the git automatic merge does not produce, a merge with a second
// parent outside the base, and a chain longer than maxPremergeDevMerges.
func premergeHeadRelation(ctx context.Context, checkout, base, evaluated, accepted string) (bool, error) {
	mismatch := func(format string, args ...any) error {
		return refuse(contract.RefusalPremergeHeadMismatch, format, args...)
	}
	if strings.EqualFold(evaluated, accepted) {
		return false, nil
	}
	cur, plain, merges := accepted, false, 0
	for step := 0; step < maxPremergeHeadWalk; step++ {
		if cur == evaluated {
			return plain, nil
		}
		line, err := runGit(ctx, checkout, nil, "rev-list", "--parents", "-n", "1", cur)
		if err != nil {
			return false, mismatch("git could not read the parents of %s in %s: %v", cur, checkout, err)
		}
		fields := strings.Fields(line)
		switch len(fields) - 1 {
		case 1:
			plain, cur = true, fields[1]
		case 2:
			first, second := fields[1], fields[2]
			if merges >= maxPremergeDevMerges {
				return false, mismatch("the record head %s is more than %d merges behind %s", evaluated, maxPremergeDevMerges, accepted)
			}
			inBase, err := commitIsAncestor(ctx, checkout, second, base)
			if err != nil || !inBase {
				return false, mismatch("the second parent %s of %s is not in the base %s", second, cur, base)
			}
			tree, err := runGit(ctx, checkout, nil, "rev-parse", cur+"^{tree}")
			if err != nil {
				return false, mismatch("git could not read the tree of %s: %v", cur, err)
			}
			code, merged, err := runGitExit(ctx, checkout, nil, "merge-tree", "--write-tree", first, second)
			if err != nil || code != 0 || len(strings.Fields(merged)) == 0 || strings.Fields(merged)[0] != strings.TrimSpace(tree) {
				return false, mismatch("the merge %s is not the automatic merge of its parents (a hand-resolved merge or a merge outside the base)", cur)
			}
			merges++
			cur = first
		default:
			return false, mismatch("%s has %d parents; only a merge of two commits can keep the evaluation", cur, len(fields)-1)
		}
	}
	return false, mismatch("the record head %s is not an ancestor of %s within %d commits", evaluated, accepted, maxPremergeHeadWalk)
}

// premergeAttachPath is the checkout a judgment reads for an acceptance: the commit path names one, the pull request
// path names none.
func premergeAttachPath(in AcceptInput) *premergeCheckout {
	if in.Commit == nil {
		return nil
	}
	return &premergeCheckout{checkout: in.Commit.Checkout, base: in.Commit.Base}
}

// premergeHeld reports the pre-merge refusal a candidate is held by, when err is one.
func premergeHeld(err error) (string, bool) {
	var refused *store.RefusedError
	if errors.As(err, &refused) && strings.HasPrefix(refused.Reason, "premerge_") {
		return refused.Reason, true
	}
	return "", false
}

// premergeOfAcceptance judges the record stored with an active acceptance of node n. Integration reads the stored text,
// never a file: the stored digest must match the text, the stored head must be the acceptance's head, and the same
// judgment as acceptance applies. A pull-request-less acceptance is judged against its own checkout and base.
func (s *Scheduler) premergeOfAcceptance(ctx context.Context, q store.Querier, acc Acceptance, n dag.SnapNode, head string) (premergeJudgment, error) {
	var raw, digest, evaluated, accepted string
	// the latest re-validation's record is the one that stands once the criteria were re-registered (CRW-952 answer 4); otherwise the acceptance's own
	has, err := queryOne(ctx, q, "SELECT p.record_json, p.record_digest, p.evaluated_head, p.accepted_head FROM dag_acceptance_revalidations r JOIN dag_revalidation_premerge p ON p.revalidation_id = r.revalidation_id WHERE r.acceptance_id = ? ORDER BY r.reval_seq DESC LIMIT 1", []any{acc.AcceptanceID}, &raw, &digest, &evaluated, &accepted)
	if err != nil {
		return premergeJudgment{}, err
	}
	if !has {
		has, err = queryOne(ctx, q, "SELECT record_json, record_digest, evaluated_head, accepted_head FROM dag_acceptance_premerge WHERE acceptance_id = ?", []any{acc.AcceptanceID}, &raw, &digest, &evaluated, &accepted)
		if err != nil {
			return premergeJudgment{}, err
		}
	}
	if !has {
		return premergeJudgment{}, refuse(contract.RefusalPremergeMissing, "acceptance %s has no pre-merge record: it was accepted before the gate or without --premerge", acc.AcceptanceID)
	}
	if accepted != head {
		return premergeJudgment{}, refuse(contract.RefusalPremergeHeadMismatch, "the record was stored for head %s and acceptance %s stands on %s", accepted, acc.AcceptanceID, head)
	}
	var commit *premergeCheckout
	var base string
	if isCommit, err := queryOne(ctx, q, "SELECT base_commit FROM dag_acceptance_verifications WHERE acceptance_id = ?", []any{acc.AcceptanceID}, &base); err != nil {
		return premergeJudgment{}, err
	} else if isCommit {
		commit = &premergeCheckout{checkout: acc.Repository, base: base}
	}
	j, err := judgePremerge(ctx, []byte(raw), acc.NodeID, n, head, commit)
	if err != nil {
		return premergeJudgment{}, err
	}
	if j.digest != digest {
		return premergeJudgment{}, refuse(contract.RefusalPremergeMissing, "the stored pre-merge record of acceptance %s does not match its digest", acc.AcceptanceID)
	}
	return j, nil
}

// premergeLeft is a plan node the pre-merge gate holds back from integration: its reason and the acceptance it holds.
type premergeLeft struct {
	NodeID, Reason, AcceptanceID, HeadSHA string
}

// premergeLeftOut lists, in node order, the otherwise-current candidates of a plan that their stored pre-merge record holds
// back. Integration reports them with their reason and refuses a --node that names one (CRW-952 answer 3).
func (s *Scheduler) premergeLeftOut(ctx context.Context, plan string) ([]premergeLeft, error) {
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return nil, err
	}
	nodes := append([]dag.SnapNode(nil), snap.Nodes...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	var out []premergeLeft
	for _, n := range nodes {
		if n.Kind != dag.NodeImplementation {
			continue
		}
		acc, found, err := loadActiveAcceptance(ctx, q, plan, n.NodeID)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		_, _, err = s.currentCandidate(ctx, q, plan, snap, n)
		if reason, held := premergeHeld(err); held {
			out = append(out, premergeLeft{NodeID: n.NodeID, Reason: reason, AcceptanceID: acc.AcceptanceID, HeadSHA: acc.HeadSHA})
		}
	}
	return out, nil
}

// attachPremerge stores the pre-merge record of an acceptance that has none (the attach path, CRW-952 answer 5). The
// record is judged by the same rules and stored under the same acceptance id; a second attach is refused.
func (s *Scheduler) attachPremerge(ctx context.Context, q store.Querier, in AcceptInput, acceptanceID, node string, n dag.SnapNode, acceptedHead, actor string) error {
	var stored string
	has, err := queryOne(ctx, q, "SELECT acceptance_id FROM dag_acceptance_premerge WHERE acceptance_id = ?", []any{acceptanceID}, &stored)
	if err != nil {
		return err
	}
	if has {
		return refuse(contract.RefusalDispositionConflict, "acceptance %s already holds its pre-merge record; a second attach is refused", acceptanceID)
	}
	j, err := judgePremerge(ctx, in.Premerge, node, n, acceptedHead, premergeAttachPath(in))
	if err != nil {
		return err
	}
	return s.storePremerge(ctx, acceptanceID, acceptedHead, actor, j, s.ExpectedEpoch)
}

// storePremerge appends the judged pre-merge record with its acceptance.
func (s *Scheduler) storePremerge(ctx context.Context, acceptanceID, acceptedHead, actor string, j premergeJudgment, epoch int64) error {
	return store.RecordAcceptancePremerge(ctx, s.Store, store.AcceptancePremergeRow{
		AcceptanceID: acceptanceID, RecordDigest: j.digest, RecordJSON: string(j.raw), EvaluatedHead: j.evaluatedHead,
		AcceptedHead: acceptedHead, RecordedBy: actor, CoordinatorEpoch: epoch, RecordedAt: s.now(),
	})
}

// standingPremergeHead is the head the pre-merge record an acceptance stands on was stored for: the latest re-validation's record, else the acceptance's own
// (premergeOfAcceptance reads them in that order). false when the acceptance holds neither.
func standingPremergeHead(ctx context.Context, q store.Querier, acceptanceID string) (string, bool, error) {
	var head string
	has, err := queryOne(ctx, q, "SELECT p.accepted_head FROM dag_acceptance_revalidations r JOIN dag_revalidation_premerge p ON p.revalidation_id = r.revalidation_id WHERE r.acceptance_id = ? ORDER BY r.reval_seq DESC LIMIT 1", []any{acceptanceID}, &head)
	if err != nil || has {
		return head, has, err
	}
	has, err = queryOne(ctx, q, "SELECT accepted_head FROM dag_acceptance_premerge WHERE acceptance_id = ?", []any{acceptanceID}, &head)
	return head, has, err
}

// appendRevalidation appends the next re-validation of an acceptance, ruled by head's verdict under head's criteria, and the pre-merge record it was judged on when
// judged is not nil (an implementation node).
func (s *Scheduler) appendRevalidation(ctx context.Context, q store.Querier, acceptanceID string, head verifiedHead, actor string, judged *premergeJudgment) error {
	var last sql.NullInt64
	if err := q.QueryRowContext(ctx, "SELECT MAX(reval_seq) FROM dag_acceptance_revalidations WHERE acceptance_id = ?", acceptanceID).Scan(&last); err != nil {
		return err
	}
	seq := last.Int64 + 1
	id := revalidationID(acceptanceID, seq)
	if _, err := q.ExecContext(ctx, "INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES (?,?,?,?,?,?,?,?)",
		id, acceptanceID, head.SetDigest, head.EventID, head.VerdictTurn, seq, actor, s.now()); err != nil {
		return err
	}
	if judged == nil {
		return nil
	}
	return store.RecordRevalidationPremerge(ctx, s.Store, id, store.AcceptancePremergeRow{AcceptanceID: acceptanceID, RecordDigest: judged.digest, RecordJSON: string(judged.raw), EvaluatedHead: judged.evaluatedHead,
		AcceptedHead: judged.acceptedHead, RecordedBy: actor, CoordinatorEpoch: s.ExpectedEpoch, RecordedAt: s.now()})
}
