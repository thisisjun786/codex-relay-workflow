package dagsched

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The integration batch (CRW-965). One batch takes the plan's ready accepted candidates, merges them in node-id order
// with merge commits in a temporary detached worktree, verifies the merged tree (the verify command runs in that
// worktree with a fresh record path, and the record is judged against that exact tree), and moves the local
// integration branch to the verified commit by a compare-and-swap on the head it read. When the merged tree fails, the
// failing candidate is found by bisecting the ordered prefixes; it and every candidate that depends on it through the
// plan's edges are left out with a reason. A candidate that conflicts is deferred and, once the surviving set is
// verified, retried once in order on top of it.
//
// Each candidate is frozen before git runs (an intent row), so the merged marks name the acceptance and event the merge
// stood on. A batch whose marks did not all land records mark_pending, and the next run completes them from the frozen
// rows, so a partial integration is always visible and never repeated.

// zeroObjectID is git's create-only expected value for update-ref.
const zeroObjectID = "0000000000000000000000000000000000000000"

// IntegrationVerifierHostError is a verify command that could not be started at all (a missing executable, a bad
// directory): not a failed verification, so the batch stops instead of splitting every candidate out.
type IntegrationVerifierHostError struct{ Detail string }

func (e *IntegrationVerifierHostError) Error() string { return e.Detail }

// IntegrationBatchVerifier runs the one verification command in dir with the extra environment. A non-nil error is a
// failing run (a non-zero exit); the command decides the result only together with the record it writes.
type IntegrationBatchVerifier func(ctx context.Context, dir string, env []string) error

// IntegrationBatchDeps are the seams the batch's verification and ref update go through, so a test can interleave a ref
// move between the read and the update.
type IntegrationBatchDeps struct {
	Verify IntegrationBatchVerifier
	Update func(ctx context.Context, checkout, ref, newCommit, oldCommit string) error
	// AfterMove runs once the branch has moved and the moved record is committed, before the marks are written. It is a test seam
	// for a change that lands between the move and the marks; it runs outside the store transaction (CRW-965, D6).
	AfterMove func(ctx context.Context) error
}

// IntegrationBatchInput is what one batch is asked to do.
type IntegrationBatchInput struct {
	Plan, Actor, Checkout, IntegrationRef, BaseRef string
	Nodes                                          []string
}

// IntegrationBatchMerged is one candidate the batch merged onto the branch.
type IntegrationBatchMerged struct {
	NodeID, AcceptanceID, HeadSHA, MergeCommit string
}

// IntegrationBatchSplit is one candidate the batch left out, with the reason it is returned to its parent.
type IntegrationBatchSplit struct {
	NodeID, AcceptanceID, HeadSHA, Reason string
}

// IntegrationBatchResult is the answer of one batch.
type IntegrationBatchResult struct {
	BatchID, Plan, Checkout, Ref, BaseRef string
	OldHead, NewHead                      string
	Merged                                []IntegrationBatchMerged
	Split                                 []IntegrationBatchSplit
	Verification                          VerificationRecord
	VerificationDigest                    string
	MarkedEvents                          []string
	Pending                               []string
	Targets                               []string
	AlreadyContained                      []IntegrationBatchContained
	ContainedUnverified                   []IntegrationBatchContained
	Reconciled                            []string
	Abandoned                             []string
}

// IntegrateBatch runs one batch: it completes the marks earlier batches left pending when their commit is on the
// branch, then merges, verifies and moves the branch for the plan's ready candidates.
func (s *Scheduler) IntegrateBatch(ctx context.Context, in IntegrationBatchInput, deps IntegrationBatchDeps) (IntegrationBatchResult, error) {
	out := IntegrationBatchResult{Plan: in.Plan, Checkout: in.Checkout, Ref: in.IntegrationRef, BaseRef: in.BaseRef}
	if in.Plan == "" || in.Actor == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "an integration batch names --plan and --actor")
	}
	if in.Checkout == "" || in.IntegrationRef == "" || in.BaseRef == "" {
		return out, refuse(contract.RefusalMalformedReceipt, "an integration batch names --checkout, --integration-ref and --base")
	}
	if deps.Verify == nil || deps.Update == nil {
		return out, errors.New("an integration batch needs a verifier and a ref updater")
	}
	if _, err := runGit(ctx, in.Checkout, nil, "rev-parse", "--git-dir"); err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s is not a repository git can read", in.Checkout)
	}
	// the coordinator-epoch fence is checked before anything is read or written
	if err := s.IntegrationWrite(ctx, in.Plan, in.Actor, func(context.Context) error { return nil }); err != nil {
		return out, err
	}
	baseTip, err := resolveIntegrationCommit(ctx, in.Checkout, in.BaseRef)
	if err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "%s names no commit in %s: %v", in.BaseRef, in.Checkout, err)
	}
	start, old := baseTip, zeroObjectID
	if head, found, err := integrationBranchTip(ctx, in.Checkout, in.IntegrationRef); err != nil {
		return out, refuse(contract.RefusalMergeTargetUnreadable, "git could not read %s in %s: %v", in.IntegrationRef, in.Checkout, err)
	} else if found {
		start, old = head, head
	}
	out.OldHead = old
	if err := s.completePendingMarks(ctx, in); err != nil {
		return out, err
	}
	if old != zeroObjectID {
		if err := s.reconcilePlannedMoves(ctx, in, old, &out); err != nil {
			return out, err
		}
	}
	candidates, err := s.readyIntegrationCandidates(ctx, in)
	if err != nil {
		return out, err
	}
	if len(candidates) == 0 {
		return out, refuse(contract.RefusalDispositionConflict, "plan %s has no ready accepted candidate to integrate", in.Plan)
	}
	batch := integrationBatchID(in, old, candidates)
	out.BatchID = batch
	if err := s.recordIntent(ctx, in, batch, old, baseTip, candidates); err != nil {
		return out, err
	}
	// A candidate whose head the branch already contains is never merged again (CRW-965). When the branch head is a head
	// this relay verified, the candidate is marked from its frozen row; otherwise it is reported and stays ready.
	var fresh, contained []Candidate
	for _, c := range candidates {
		// containment is judged against the branch head the batch read once (start), the same head the coverage check reads (CRW-965 review)
		inside := false
		if old != zeroObjectID {
			var err error
			if inside, err = commitIsAncestor(ctx, in.Checkout, c.HeadSHA, start); err != nil {
				return out, err
			}
		}
		if inside {
			contained = append(contained, c)
		} else {
			fresh = append(fresh, c)
		}
	}
	var covered []Candidate
	if len(contained) > 0 {
		rowFound, coveredNodes, err := s.verifiedHeadState(ctx, in, start, contained)
		if err != nil {
			return out, err
		}
		// a candidate that no stored verification covers (its keys changed, or its criteria set is not the one verified) makes the
		// head verified again, with no merge; only that PASS covers the candidates (CRW-965, parent decisions D2 and D1)
		if rowFound && !allNodesCovered(contained, coveredNodes) {
			_, dig, pass, err := s.verifyHead(ctx, in, deps, start, baseTip)
			if err != nil {
				return out, err
			}
			if pass {
				if err := s.recordVerifiedHead(ctx, in, batch, start, dig, contained); err != nil {
					return out, err
				}
				coveredNodes = nodesOf(contained)
			}
		}
		for _, c := range contained {
			if coveredNodes[c.NodeID] {
				covered = append(covered, c)
				continue
			}
			out.ContainedUnverified = append(out.ContainedUnverified, IntegrationBatchContained{NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, HeadSHA: c.HeadSHA, ContainedIn: start})
		}
	}
	var settled settledIntegration
	if len(fresh) > 0 {
		if settled, err = s.settleCandidates(ctx, in, deps, start, baseTip, fresh); err != nil {
			return out, err
		}
	}
	out.Split = settled.split
	for _, m := range settled.merged {
		out.Merged = append(out.Merged, IntegrationBatchMerged{NodeID: m.NodeID, AcceptanceID: m.AcceptanceID, HeadSHA: m.HeadSHA, MergeCommit: m.MergeCommit})
	}
	if len(settled.merged) == 0 {
		out.NewHead = old
	} else {
		out.NewHead, out.Verification, out.VerificationDigest = settled.head, settled.record, settled.digest
		// the coordinator-epoch fence is checked again right before the branch moves: verification can take long, and a
		// session that lost its epoch in the meantime must not move the branch (finding d3)
		if err := s.IntegrationWrite(ctx, in.Plan, in.Actor, func(context.Context) error { return nil }); err != nil {
			return out, err
		}
		// the verified head is recorded before the branch moves, so a batch that dies after the move leaves its head known
		if err := s.recordVerifiedHead(ctx, in, batch, settled.head, settled.digest, mergedCandidates(settled.merged)); err != nil {
			return out, err
		}
		// the compare-and-swap and the moved record share one fenced transaction: the epoch is checked inside it (CRW-965, D6)
		if err := s.IntegrationWrite(ctx, in.Plan, in.Actor, func(txCtx context.Context) error {
			// every frozen candidate is proved current again inside the fenced transaction, before the branch moves: a candidate
			// that changed after the verification is never published on the old verification (CRW-965, D3)
			for _, m := range settled.merged {
				current, err := s.frozenCandidateStillCurrent(txCtx, m.Candidate)
				if err != nil {
					return err
				}
				if !current {
					return refuse(contract.RefusalMergeCandidateMoved, "candidate %s (acceptance %s) is no longer current after the verification of %s: the branch did not move, and the next batch freezes again without it", m.NodeID, m.AcceptanceID, in.IntegrationRef)
				}
			}
			if err := deps.Update(txCtx, in.Checkout, in.IntegrationRef, settled.head, old); err != nil {
				return refuse(contract.RefusalStaleMarkContext, "%s moved while the merged tree was being verified (the batch read %s): read it again and run the batch again", in.IntegrationRef, old)
			}
			return s.recordMovedIn(txCtx, in, batch, out)
		}); err != nil {
			return out, err
		}
		if deps.AfterMove != nil {
			if err := deps.AfterMove(ctx); err != nil {
				return out, err
			}
		}
		for _, m := range settled.merged {
			event, err := s.MarkFrozen(ctx, m.Candidate, in.Actor, settled.digest)
			if err != nil {
				out.Pending = append(out.Pending, m.NodeID)
				if rerr := s.recordMark(ctx, in, batch, m.Candidate, "mark_pending", err.Error()); rerr != nil {
					return out, rerr
				}
				continue
			}
			out.MarkedEvents = append(out.MarkedEvents, event)
			if rerr := s.recordMark(ctx, in, batch, m.Candidate, "marked", event); rerr != nil {
				return out, rerr
			}
		}
	}
	if err := s.markContained(ctx, in, batch, covered, start, &out); err != nil {
		return out, err
	}
	if len(settled.merged) > 0 {
		out.Targets = []string{in.Checkout + "@" + in.IntegrationRef}
	}
	return out, err
}

// integrationMerge is one candidate the verified set holds, with the merge commit it produced.
type integrationMerge struct {
	Candidate
	MergeCommit string
}

// settledIntegration is the verified outcome: what merged onto which head, what was left out, and the record of that
// exact merged tree.
type settledIntegration struct {
	merged []integrationMerge
	split  []IntegrationBatchSplit
	head   string
	record VerificationRecord
	digest string
}

// readyIntegrationCandidates is the candidates a batch considers: the named subset or every ready accepted candidate.
// Every candidate's relationship must be the batch's actor's, so a parent integrates only its own nodes.
func (s *Scheduler) readyIntegrationCandidates(ctx context.Context, in IntegrationBatchInput) ([]Candidate, error) {
	all, err := s.AcceptedCandidates(ctx, in.Plan)
	if err != nil {
		return nil, err
	}
	all = candidatesInCheckout(all, in.Checkout)
	pick := all
	if len(in.Nodes) > 0 {
		byNode := map[string]Candidate{}
		for _, c := range all {
			byNode[c.NodeID] = c
		}
		pick = nil
		for _, id := range in.Nodes {
			c, ok := byNode[id]
			if !ok {
				landed, err := s.alreadyIntegratedNode(ctx, in.Plan, id)
				if err != nil {
					return nil, err
				}
				if landed {
					return nil, refuse(contract.RefusalDispositionConflict, "node %s is already integrated into an integration target, so it has no candidate to merge again", id)
				}
				return nil, refuse(contract.RefusalDispositionConflict, "node %s is not a ready accepted candidate of plan %s in this checkout (a candidate accepted for another repository is judged there)", id, in.Plan)
			}
			pick = append(pick, c)
		}
	}
	for _, c := range pick {
		rel, found, err := currentRelationshipOf(ctx, s.Store.Q(ctx), in.Plan, c.NodeID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, refuse(contract.RefusalUnregisteredRelationship, "node %s has no execution to integrate", c.NodeID)
		}
		if rel.ParentTaskID != in.Actor {
			return nil, notParentHeldBy(in.Actor, rel)
		}
	}
	sort.Slice(pick, func(i, j int) bool { return pick[i].NodeID < pick[j].NodeID })
	return pick, nil
}

// integrationBatchID is the identity of one batch: the plan, the checkout, the branch, its old head and the frozen
// candidates, so the same batch recorded twice is the same identity.
func integrationBatchID(in IntegrationBatchInput, old string, candidates []Candidate) string {
	var parts []string
	for _, c := range candidates {
		parts = append(parts, c.NodeID+"="+c.AcceptanceID+"@"+c.EventID+"#"+c.HeadSHA)
	}
	return registry.CoordinationID("dib", in.Plan, in.Checkout, in.IntegrationRef, old, in.BaseRef, strings.Join(parts, ","))
}

// recordIntent writes the intent before any git call: one batch-level row and one row per frozen candidate.
func (s *Scheduler) recordIntent(ctx context.Context, in IntegrationBatchInput, batch, old, baseTip string, candidates []Candidate) error {
	rows, err := store.IntegrationStagesOfPlan(ctx, s.Store, in.Plan)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if r.BatchID == batch && r.Stage == "intent" {
			// the same batch recorded its intent before: the identity is the same, so the rows stand
			return nil
		}
	}
	detail, err := json.Marshal(map[string]string{"integration_ref": in.IntegrationRef, "base": baseTip, "old_head": old})
	if err != nil {
		return err
	}
	return s.IntegrationWrite(ctx, in.Plan, in.Actor, func(txCtx context.Context) error {
		if err := s.stageRow(txCtx, in, batch, "intent", Candidate{}, string(detail)); err != nil {
			return err
		}
		for _, c := range candidates {
			// each frozen row names the criteria set the batch verifies the candidate under (CRW-965, parent decision d2)
			if err := s.stageRow(txCtx, in, batch, "intent", c, c.CriteriaSetDigest); err != nil {
				return err
			}
		}
		return nil
	})
}

// recordMoved writes the batch row once the branch moved: the merged and split lists, the verified record's digest and the

// recordMark writes one node's mark outcome: marked with the event it named, or mark_pending with the refusal.
func (s *Scheduler) recordMark(ctx context.Context, in IntegrationBatchInput, batch string, c Candidate, stage, detail string) error {
	return s.IntegrationWrite(ctx, in.Plan, in.Actor, func(txCtx context.Context) error {
		return s.stageRow(txCtx, in, batch, stage, c, detail)
	})
}

// stageRow appends one stage row. A batch-level row carries no node; a node row carries the frozen identity.
func (s *Scheduler) stageRow(ctx context.Context, in IntegrationBatchInput, batch, stage string, c Candidate, detail string) error {
	stageID := registry.CoordinationID("dis", batch, stage, c.NodeID, detail)
	return store.RecordIntegrationStage(ctx, s.Store, store.IntegrationStageRow{StageID: stageID, BatchID: batch, PlanID: in.Plan, Stage: stage,
		NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, EventID: c.EventID, RevisionHash: c.RevisionHash, Generation: c.Generation,
		HeadSHA: c.HeadSHA, Detail: detail, RecordedBy: in.Actor, RecordedAt: registry.SystemISO()})
}

// completePendingMarks completes the marks earlier batches left pending. A pending mark is completed only when its
// batch's merge commit is on the integration branch now, from the frozen candidate; a mark already present counts as done.
func (s *Scheduler) completePendingMarks(ctx context.Context, in IntegrationBatchInput) error {
	rows, err := store.IntegrationStagesOfPlan(ctx, s.Store, in.Plan)
	if err != nil {
		return err
	}
	done := map[string]bool{}
	for _, r := range rows {
		if r.Stage == "marked" {
			done[r.BatchID+"/"+r.NodeID] = true
		}
	}
	for _, r := range rows {
		if r.Stage != "mark_pending" || done[r.BatchID+"/"+r.NodeID] {
			continue
		}
		moved := false
		for _, x := range rows {
			if x.BatchID == r.BatchID && x.Stage == "ref_moved" {
				ok, err := isAncestorOf(ctx, in.Checkout, x.Detail, in.IntegrationRef)
				if err != nil {
					return err
				}
				moved = ok
			}
		}
		if !moved {
			continue
		}
		c := Candidate{PlanID: in.Plan, NodeID: r.NodeID, AcceptanceID: r.AcceptanceID, EventID: r.EventID, RevisionHash: r.RevisionHash,
			Generation: r.Generation, HeadSHA: r.HeadSHA}
		event, err := s.MarkFrozen(ctx, c, in.Actor, r.BatchID)
		if err != nil {
			continue
		}
		if err := s.recordMark(ctx, in, r.BatchID, c, "marked", event); err != nil {
			return err
		}
	}
	return nil
}

// settleCandidates merges, verifies and settles the candidates in a temporary worktree, removed on every path.
func (s *Scheduler) settleCandidates(ctx context.Context, in IntegrationBatchInput, deps IntegrationBatchDeps, start, baseTip string, candidates []Candidate) (settledIntegration, error) {
	tmp, err := os.MkdirTemp("", "crw-965-integrate-")
	if err != nil {
		return settledIntegration{}, err
	}
	wt := filepath.Join(tmp, "worktree")
	defer func() {
		_, _ = runGit(context.WithoutCancel(ctx), in.Checkout, nil, "worktree", "remove", "--force", wt)
		_, _ = runGit(context.WithoutCancel(ctx), in.Checkout, nil, "worktree", "prune")
		_ = os.RemoveAll(tmp)
	}()
	if _, err := runGit(ctx, in.Checkout, nil, "worktree", "add", "--detach", wt, start); err != nil {
		return settledIntegration{}, refuse(contract.RefusalMergeTargetUnreadable, "git could not create a worktree at %s: %v", start, err)
	}
	w := &integrationWorktree{s: s, in: in, deps: deps, dir: wt, tmp: tmp, start: start, baseTip: baseTip}
	return w.settle(ctx, candidates)
}

// integrationWorktree is the temporary worktree a batch merges into and verifies in.
type integrationWorktree struct {
	s       *Scheduler
	in      IntegrationBatchInput
	deps    IntegrationBatchDeps
	dir     string
	tmp     string
	start   string
	baseTip string
	records int
}

// integrationIdentity is the author and committer of the merge commits a batch makes.
var integrationIdentity = []string{"GIT_AUTHOR_NAME=crw-relay", "GIT_AUTHOR_EMAIL=crw-relay@invalid", "GIT_COMMITTER_NAME=crw-relay", "GIT_COMMITTER_EMAIL=crw-relay@invalid"}

// reset moves the worktree to a commit.
func (w *integrationWorktree) reset(ctx context.Context, commit string) error {
	_, err := runGit(ctx, w.dir, nil, "reset", "--hard", "-q", commit)
	return err
}

// head is the worktree's commit.
func (w *integrationWorktree) head(ctx context.Context) (string, error) {
	out, err := runGit(ctx, w.dir, nil, "rev-parse", "HEAD")
	return strings.TrimSpace(out), err
}

// merge merges one candidate into the worktree with a merge commit. A conflict is an answer (false) and leaves the
// worktree as it was; a host failure is an error.
func (w *integrationWorktree) merge(ctx context.Context, c Candidate) (bool, string, error) {
	code, _, err := runGitExit(ctx, w.dir, integrationIdentity, "merge", "--no-ff", "--no-edit", "-q", "-m", "CRW-965: integrate "+c.NodeID, c.HeadSHA)
	if code < 0 {
		return false, "", fmt.Errorf("git merge of %s: %v", c.HeadSHA, err)
	}
	if code != 0 {
		if code != 1 {
			// git answers 1 for a conflict; anything else (for example 128, a head this checkout does not hold) is a host failure, not a conflict
			return false, "", fmt.Errorf("git merge of %s exited %d: %v", c.HeadSHA, code, err)
		}
		_, _, _ = runGitExit(ctx, w.dir, integrationIdentity, "merge", "--abort")
		return false, "", nil
	}
	head, err := w.head(ctx)
	return true, head, err
}

// build resets the worktree to the start and merges the given candidates in order; a prefix that merged cleanly once
// merges cleanly again. It answers the merge commits.
func (w *integrationWorktree) build(ctx context.Context, set []Candidate) ([]string, *Candidate, error) {
	if err := w.reset(ctx, w.start); err != nil {
		return nil, nil, err
	}
	var commits []string
	for i := range set {
		c := set[i]
		ok, commit, err := w.merge(ctx, c)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			// a survivor that no longer merges cleanly is reported, not an error: the settle loop defers it (CRW-965, decision D-A)
			return nil, &c, nil
		}
		commits = append(commits, commit)
	}
	return commits, nil, nil
}

// verify runs the verification in the worktree at its current head, with a fresh record path, and judges the record
// against that merged tree. A non-zero exit is a failure whatever the record says; a record that does not pass is a
// failure with the judge's reason. Only a host failure is an error.
func (w *integrationWorktree) verify(ctx context.Context) (VerificationRecord, string, bool, error) {
	commit, err := w.head(ctx)
	if err != nil {
		return VerificationRecord{}, "", false, err
	}
	w.records++
	dir := filepath.Join(w.tmp, fmt.Sprintf("record-%d", w.records))
	if err := os.Mkdir(dir, 0o700); err != nil {
		return VerificationRecord{}, "", false, err
	}
	recordPath := filepath.Join(dir, "verification-record.json")
	env := []string{"CRW_VERIFY_RECORD=" + recordPath, "CRW_VERIFY_BASE=" + w.baseTip, "CRW_VERIFY_HEAD=" + commit}
	if err := w.deps.Verify(ctx, w.dir, env); err != nil {
		var host *IntegrationVerifierHostError
		if errors.As(err, &host) {
			return VerificationRecord{}, "", false, err
		}
		return VerificationRecord{}, "", false, nil
	}
	// the verify command may move HEAD; what it verified must still be the commit that would land (CRW-965 review)
	if moved, err := w.head(ctx); err != nil {
		return VerificationRecord{}, "", false, err
	} else if moved != commit {
		return VerificationRecord{}, "", false, nil
	}
	raw, err := os.ReadFile(recordPath)
	if err != nil {
		return VerificationRecord{}, "", false, nil
	}
	keys, err := CommitVerificationKeys(ctx, w.in.Checkout, commit)
	if err != nil {
		return VerificationRecord{}, "", false, err
	}
	base, err := resolveIntegrationCommit(ctx, w.in.Checkout, w.baseTip)
	if err != nil {
		return VerificationRecord{}, "", false, err
	}
	keys.Base = base
	keys.OS, keys.Arch = runtime.GOOS, runtime.GOARCH
	record, err := JudgeVerificationRecord(raw, keys)
	if err != nil {
		var refused *store.RefusedError
		if errors.As(err, &refused) {
			return record, "", false, nil
		}
		return record, "", false, err
	}
	return record, sha256Digest(raw), true, nil
}

// settle is the merge-verify-split loop (see settleCandidates). The worktree is at the verified head when it returns.
func (w *integrationWorktree) settle(ctx context.Context, candidates []Candidate) (settledIntegration, error) {
	var out settledIntegration
	if err := w.reset(ctx, w.start); err != nil {
		return out, err
	}
	var kept, deferred []Candidate
	for _, c := range candidates {
		ok, _, err := w.merge(ctx, c)
		if err != nil {
			return out, err
		}
		if ok {
			kept = append(kept, c)
		} else {
			deferred = append(deferred, c)
		}
	}
	if err := w.reset(ctx, w.start); err != nil {
		return out, err
	}
	// the merged set is verified once; when it fails, the first failing prefix is found by bisection and its candidate
	// and every dependant of it are left out, until the rest verifies
	var commits []string
	var record VerificationRecord
	var digest string
	excluded := map[string]string{}
	for len(kept) > 0 {
		var err error
		var conflicted *Candidate
		if commits, conflicted, err = w.build(ctx, kept); err != nil {
			return out, err
		}
		if conflicted != nil {
			// it no longer merges with the set left after a removal: it is deferred and retried on the verified set
			deferred = append(deferred, *conflicted)
			kept = withoutCandidateNode(kept, conflicted.NodeID)
			continue
		}
		rec, dig, pass, err := w.verify(ctx)
		if err != nil {
			return out, err
		}
		if pass {
			record, digest = rec, dig
			break
		}
		k, err := w.firstFailingPrefix(ctx, kept)
		if err != nil {
			return out, err
		}
		failing := kept[k-1]
		// the bisection shows only that this candidate breaks the prefix before it; it is reported as failing only when it fails
		// verified alone, and otherwise it is left for the next batch (CRW-965, parent decision D5)
		reason := "verification_failed"
		alone, err := w.failsAlone(ctx, failing)
		if err != nil {
			return out, err
		}
		if !alone {
			reason = "not_proven_failing"
		}
		left := map[string]string{failing.NodeID: reason}
		edges, err := w.s.Successors(ctx, w.in.Plan)
		if err != nil {
			return out, err
		}
		for _, d := range transitiveSuccessors(edges, failing.NodeID) {
			left[d] = "depends_on_" + failing.NodeID
		}
		for node, reason := range left {
			excluded[node] = reason
		}
		var next []Candidate
		for _, c := range kept {
			if reason, gone := left[c.NodeID]; gone {
				out.split = append(out.split, IntegrationBatchSplit{NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, HeadSHA: c.HeadSHA, Reason: reason})
				continue
			}
			next = append(next, c)
		}
		kept = next
	}
	for i, c := range kept {
		out.merged = append(out.merged, integrationMerge{Candidate: c, MergeCommit: commits[i]})
	}
	if len(kept) == 0 {
		// nothing verified: the deferred candidates retry on top of the start, not on a failing merge
		if err := w.reset(ctx, w.start); err != nil {
			return out, err
		}
	}
	// the deferred candidates: each is retried once, in order, on top of the verified set
	for _, d := range deferred {
		if reason, gone := excluded[d.NodeID]; gone {
			out.split = append(out.split, IntegrationBatchSplit{NodeID: d.NodeID, AcceptanceID: d.AcceptanceID, HeadSHA: d.HeadSHA, Reason: reason})
			continue
		}
		before, err := w.head(ctx)
		if err != nil {
			return out, err
		}
		ok, commit, err := w.merge(ctx, d)
		if err != nil {
			return out, err
		}
		if !ok {
			reason := "conflict"
			if len(kept) > 0 {
				reason = "conflict_with_" + w.conflictNode(ctx, d, kept)
			}
			out.split = append(out.split, IntegrationBatchSplit{NodeID: d.NodeID, AcceptanceID: d.AcceptanceID, HeadSHA: d.HeadSHA, Reason: reason})
			continue
		}
		rec, dig, pass, err := w.verify(ctx)
		if err != nil {
			return out, err
		}
		if !pass {
			if err := w.reset(ctx, before); err != nil {
				return out, err
			}
			out.split = append(out.split, IntegrationBatchSplit{NodeID: d.NodeID, AcceptanceID: d.AcceptanceID, HeadSHA: d.HeadSHA, Reason: "verification_failed"})
			continue
		}
		kept = append(kept, d)
		out.merged = append(out.merged, integrationMerge{Candidate: d, MergeCommit: commit})
		record, digest = rec, dig
	}
	head, err := w.head(ctx)
	if err != nil {
		return out, err
	}
	out.head, out.record, out.digest = head, record, digest
	return out, nil
}

// firstFailingPrefix is the smallest k such that the first k candidates fail verification, found by bisection over the
// prefixes. The full set is known to fail when this is called.
func (w *integrationWorktree) firstFailingPrefix(ctx context.Context, kept []Candidate) (int, error) {
	lo, hi := 1, len(kept)
	for lo < hi {
		mid := (lo + hi) / 2
		_, conflicted, err := w.build(ctx, kept[:mid])
		if err != nil {
			return 0, err
		}
		if conflicted != nil {
			return 0, fmt.Errorf("candidate %s conflicted while a prefix of a clean set was rebuilt", conflicted.NodeID)
		}
		_, _, pass, err := w.verify(ctx)
		if err != nil {
			return 0, err
		}
		if pass {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, nil
}

// failsAlone is whether a candidate fails verification when it is merged alone onto the start. A candidate that does not
// merge alone, or that verifies, does not fail alone (CRW-965, parent decision D5).
func (w *integrationWorktree) failsAlone(ctx context.Context, c Candidate) (bool, error) {
	if err := w.reset(ctx, w.start); err != nil {
		return false, err
	}
	ok, _, err := w.merge(ctx, c)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	_, _, pass, err := w.verify(ctx)
	if err != nil {
		return false, err
	}
	return !pass, nil
}

// conflictNode names the first kept candidate a deferred candidate conflicts with when merged on its own, by git's own
// merge-tree answer (exit 1 is a conflict). The first kept candidate is named when none is found.
func (w *integrationWorktree) conflictNode(ctx context.Context, d Candidate, kept []Candidate) string {
	for _, k := range kept {
		if code, _, _ := runGitExit(ctx, w.in.Checkout, nil, "merge-tree", "--write-tree", "--name-only", k.HeadSHA, d.HeadSHA); code == 1 {
			return k.NodeID
		}
	}
	return ""
}

// transitiveSuccessors is every node that depends on a node through the plan's edges, sorted.
func transitiveSuccessors(edges map[string][]string, node string) []string {
	seen := map[string]bool{}
	queue := []string{node}
	var out []string
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, to := range edges[next] {
			if !seen[to] {
				seen[to] = true
				out = append(out, to)
				queue = append(queue, to)
			}
		}
	}
	sort.Strings(out)
	return out
}

// integrationBranchTip is the commit a local branch names, and whether the branch exists.
func integrationBranchTip(ctx context.Context, checkout, branch string) (string, bool, error) {
	code, out, err := runGitExit(ctx, checkout, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if code == 1 {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

// resolveIntegrationCommit is the full commit id a revision names in a checkout.
func resolveIntegrationCommit(ctx context.Context, checkout, rev string) (string, error) {
	out, err := runGit(ctx, checkout, nil, "rev-parse", "--verify", rev+"^{commit}")
	return strings.TrimSpace(out), err
}

// isAncestorOf reports whether a commit is an ancestor of (or equal to) a branch's commit in a checkout.
func isAncestorOf(ctx context.Context, checkout, commit, branch string) (bool, error) {
	tip, found, err := integrationBranchTip(ctx, checkout, branch)
	if err != nil || !found {
		return false, err
	}
	code, _, err := runGitExit(ctx, checkout, nil, "merge-base", "--is-ancestor", commit, tip)
	if code == 0 {
		return true, nil
	}
	if code == 1 {
		return false, nil
	}
	return false, err
}

// allNodesCovered is whether every candidate of the set is among the covered nodes.
func allNodesCovered(set []Candidate, covered map[string]bool) bool {
	for _, c := range set {
		if !covered[c.NodeID] {
			return false
		}
	}
	return true
}

// nodesOf is the set of the node ids of the candidates.
func nodesOf(set []Candidate) map[string]bool {
	out := map[string]bool{}
	for _, c := range set {
		out[c.NodeID] = true
	}
	return out
}

// mergedCandidates are the candidates a settled merge holds, in merge order.
func mergedCandidates(merged []integrationMerge) []Candidate {
	out := make([]Candidate, 0, len(merged))
	for _, m := range merged {
		out = append(out, m.Candidate)
	}
	return out
}

// sha256Digest is sha256:<hex> of bytes.
func sha256Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// IntegrationBatchContained is one candidate the branch already contained when the batch ran. A covered one is marked
// (MarkedEvent) or refused (Reason); an uncovered one is only reported and stays ready (ContainedUnverified).
type IntegrationBatchContained struct {
	NodeID, AcceptanceID, HeadSHA, ContainedIn string
	MarkedEvent, Reason                        string
}

// markContained writes the merged marks of the candidates the branch already contained, from their frozen rows (CRW-965).
// A mark this batch already recorded (a retried run) is reported from its stage row and is not inserted again.
func (s *Scheduler) markContained(ctx context.Context, in IntegrationBatchInput, batch string, candidates []Candidate, tip string, out *IntegrationBatchResult) error {
	rows, err := store.IntegrationStagesOfPlan(ctx, s.Store, in.Plan)
	if err != nil {
		return err
	}
	prior := map[string]string{}
	for _, r := range rows {
		if r.BatchID == batch && r.Stage == "marked" {
			prior[r.NodeID] = r.Detail
		}
	}
	for _, c := range candidates {
		row := IntegrationBatchContained{NodeID: c.NodeID, AcceptanceID: c.AcceptanceID, HeadSHA: c.HeadSHA, ContainedIn: tip}
		if event, done := prior[c.NodeID]; done {
			row.MarkedEvent = event
			out.AlreadyContained = append(out.AlreadyContained, row)
			continue
		}
		event, err := s.MarkFrozen(ctx, c, in.Actor, tip)
		if err != nil {
			row.Reason = err.Error()
			out.AlreadyContained = append(out.AlreadyContained, row)
			continue
		}
		row.MarkedEvent = event
		out.AlreadyContained = append(out.AlreadyContained, row)
		out.MarkedEvents = append(out.MarkedEvents, event)
		if rerr := s.recordMark(ctx, in, batch, c, "marked", event); rerr != nil {
			return rerr
		}
	}
	return nil
}

// withoutCandidateNode is the candidates with one node removed, in their order.
func withoutCandidateNode(set []Candidate, node string) []Candidate {
	var out []Candidate
	for _, c := range set {
		if c.NodeID != node {
			out = append(out, c)
		}
	}
	return out
}

// candidatesInCheckout is the candidates accepted for the checkout a batch integrates into: a candidate accepted in
// another repository is judged there, never merged here (CRW-965 review).
func candidatesInCheckout(set []Candidate, checkout string) []Candidate {
	var out []Candidate
	for _, c := range set {
		if c.Repository == checkout {
			out = append(out, c)
		}
	}
	return out
}

// verifiedHeadState reads the verified-head rows this relay wrote for a head on the integration ref. rowFound is whether any
// such row exists. covered names the candidates a stored verification stands for: a row covers a candidate when the keys it
// was judged under still hold for the head's tree, ci.yml, dependencies and platform, and the row names the candidate's
// criteria set as the one it verified (CRW-965, parent decisions D2 and D1). A commit's content does not change, so the keys
// differ only when the verification no longer stands for the tree.
func (s *Scheduler) verifiedHeadState(ctx context.Context, in IntegrationBatchInput, head string, candidates []Candidate) (bool, map[string]bool, error) {
	rows, err := store.IntegrationStagesOfPlan(ctx, s.Store, in.Plan)
	if err != nil {
		return false, nil, err
	}
	var stored []map[string]string
	for _, r := range rows {
		if r.Stage != "intent" || r.NodeID != "" {
			continue
		}
		var d map[string]string
		if json.Unmarshal([]byte(r.Detail), &d) == nil && d["verified_head"] == head && d["integration_ref"] == in.IntegrationRef {
			stored = append(stored, d)
		}
	}
	if len(stored) == 0 {
		return false, map[string]bool{}, nil
	}
	now, err := verifiedKeysOf(ctx, in.Checkout, head)
	if err != nil {
		return true, map[string]bool{}, err
	}
	covered := map[string]bool{}
	for _, d := range stored {
		if !verifiedKeysHold(d, now) {
			continue
		}
		criteria := map[string]string{}
		if json.Unmarshal([]byte(d["criteria"]), &criteria) != nil {
			continue
		}
		for _, c := range candidates {
			if c.CriteriaSetDigest == "" {
				continue
			}
			// a node the row names is covered under the criteria set it names; a node the row does not name is covered only
			// while its criteria set is still the accepted one (a revalidated set was never verified on this head)
			named, ok := criteria[c.NodeID]
			if (ok && named == c.CriteriaSetDigest) || (!ok && c.CriteriaSetDigest == c.AcceptedCriteriaDigest) {
				covered[c.NodeID] = true
			}
		}
	}
	return true, covered, nil
}

// verifiedKeysHold is whether a stored verification stands for the keys the verifier would judge now: the same tree, ci.yml
// digest, dependency digests and platform (CRW-965, parent decisions D2 and D2 remainder). A row with a key missing is not
// reused, so an older row is verified again.
func verifiedKeysHold(stored, now map[string]string) bool {
	for _, key := range verifiedKeyNames {
		value, ok := stored[key]
		if !ok || value != now[key] {
			return false
		}
	}
	return true
}

// verifiedKeyNames are the keys a verified head's row carries, in the order they are judged.
var verifiedKeyNames = []string{"tree", "ci_digest", "dependency_go_sum", "dependency_web_lock", "os", "arch"}

// verifiedKeysOf is what a verified head's record is judged under: its tree, its ci.yml digest and its dependency digests.
func verifiedKeysOf(ctx context.Context, checkout, head string) (map[string]string, error) {
	tree, err := runGit(ctx, checkout, nil, "rev-parse", head+"^{tree}")
	if err != nil {
		return nil, err
	}
	keys, err := CommitVerificationKeys(ctx, checkout, head)
	if err != nil {
		return nil, err
	}
	return map[string]string{"tree": strings.TrimSpace(tree), "ci_digest": keys.CiDigest,
		"dependency_go_sum": keys.Dependencies["go.sum"], "dependency_web_lock": keys.Dependencies["web/package-lock.json"],
		"os": runtime.GOOS, "arch": runtime.GOARCH}, nil
}

// recordVerifiedHead writes the batch's intent to move the branch to a verified head, before the branch moves. The row
// names the tree and the keys the verification stands on, and the criteria set of each candidate it covers (CRW-965,
// parent decisions D2 and D1), so a later batch compares them without reading old records.
func (s *Scheduler) recordVerifiedHead(ctx context.Context, in IntegrationBatchInput, batch, head, digest string, covered []Candidate) error {
	keys, err := verifiedKeysOf(ctx, in.Checkout, head)
	if err != nil {
		return err
	}
	criteria := map[string]string{}
	for _, c := range covered {
		criteria[c.NodeID] = c.CriteriaSetDigest
	}
	criteriaJSON, err := json.Marshal(criteria)
	if err != nil {
		return err
	}
	keys["criteria"] = string(criteriaJSON)
	keys["verified_head"], keys["verification_digest"], keys["integration_ref"] = head, digest, in.IntegrationRef
	detail, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	return s.IntegrationWrite(ctx, in.Plan, in.Actor, func(txCtx context.Context) error {
		return s.stageRow(txCtx, in, batch, "intent", Candidate{}, string(detail))
	})
}

// verifyHead verifies the tree of a branch head in a temporary worktree, with no merge: the record is judged by the same
// judge a merged tree is (CRW-965, parent decision D2). The worktree is removed on every path.
func (s *Scheduler) verifyHead(ctx context.Context, in IntegrationBatchInput, deps IntegrationBatchDeps, head, baseTip string) (VerificationRecord, string, bool, error) {
	tmp, err := os.MkdirTemp("", "crw-965-verify-")
	if err != nil {
		return VerificationRecord{}, "", false, err
	}
	wt := filepath.Join(tmp, "worktree")
	defer func() {
		_, _ = runGit(context.WithoutCancel(ctx), in.Checkout, nil, "worktree", "remove", "--force", wt)
		_, _ = runGit(context.WithoutCancel(ctx), in.Checkout, nil, "worktree", "prune")
		_ = os.RemoveAll(tmp)
	}()
	if _, err := runGit(ctx, in.Checkout, nil, "worktree", "add", "--detach", wt, head); err != nil {
		return VerificationRecord{}, "", false, refuse(contract.RefusalMergeTargetUnreadable, "git could not create a worktree at %s: %v", head, err)
	}
	w := &integrationWorktree{s: s, in: in, deps: deps, dir: wt, tmp: tmp, start: head, baseTip: baseTip}
	return w.verify(ctx)
}

// recordMovedIn writes the batch row once the branch moved and the ref_moved stage, inside the transaction that moved
// the branch (CRW-965, parent decision D6).
func (s *Scheduler) recordMovedIn(txCtx context.Context, in IntegrationBatchInput, batch string, out IntegrationBatchResult) error {
	mergedJSON, err := json.Marshal(out.Merged)
	if err != nil {
		return err
	}
	splitJSON, err := json.Marshal(out.Split)
	if err != nil {
		return err
	}
	verification, err := json.Marshal(map[string]string{"result": out.Verification.Result, "treeHash": out.Verification.TreeHash, "digest": out.VerificationDigest})
	if err != nil {
		return err
	}
	row := store.IntegrationBatchRow{BatchID: batch, PlanID: in.Plan, Repository: in.Checkout, IntegrationRef: in.IntegrationRef, BaseRef: in.BaseRef,
		OldHead: out.OldHead, NewHead: out.NewHead, MergedJSON: string(mergedJSON), SplitJSON: string(splitJSON), VerificationJSON: string(verification),
		RecordedBy: in.Actor, CoordinatorEpoch: s.ExpectedEpoch, RecordedAt: registry.SystemISO()}
	if err := store.RecordIntegrationBatch(txCtx, s.Store, row); err != nil {
		return err
	}
	return s.stageRow(txCtx, in, batch, "ref_moved", Candidate{}, out.NewHead)
}

// reconcilePlannedMoves settles the batches that planned a move (CRW-965, parent decision D6). A planned head the branch
// holds and no ref_moved row records is recorded as moved, once: the batch died after the swap. A planned head the branch
// does not hold is abandoned and reported.
func (s *Scheduler) reconcilePlannedMoves(ctx context.Context, in IntegrationBatchInput, tip string, out *IntegrationBatchResult) error {
	rows, err := store.IntegrationStagesOfPlan(ctx, s.Store, in.Plan)
	if err != nil {
		return err
	}
	moved := map[string]bool{}
	for _, r := range rows {
		if r.Stage == "ref_moved" {
			moved[r.BatchID] = true
		}
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Stage != "intent" || r.NodeID != "" || moved[r.BatchID] || seen[r.BatchID] {
			continue
		}
		var d map[string]string
		if json.Unmarshal([]byte(r.Detail), &d) != nil || d["verified_head"] == "" || d["integration_ref"] != in.IntegrationRef {
			continue
		}
		seen[r.BatchID] = true
		if d["verified_head"] != tip {
			out.Abandoned = append(out.Abandoned, r.BatchID)
			continue
		}
		batch, head := r.BatchID, tip
		if err := s.IntegrationWrite(ctx, in.Plan, in.Actor, func(txCtx context.Context) error {
			return s.stageRow(txCtx, in, batch, "ref_moved", Candidate{}, head)
		}); err != nil {
			return err
		}
		out.Reconciled = append(out.Reconciled, r.BatchID)
	}
	return nil
}
