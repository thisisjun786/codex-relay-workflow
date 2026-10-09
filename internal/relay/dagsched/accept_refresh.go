package dagsched

import (
	"context"
	"errors"
	"reflect"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The proof of a parent-made refresh at acceptance (CRW-731, CRW-666 decision section 78 piece 1). A pull request an implementation
// node is accepted on may show a head N that is not the head P the verdict fixed for the event (dag_verified_heads, CRW-742): the parent
// merged the base into the branch after the ruling. dag-accept then proves from git that N is P plus merges of the base and nothing else,
// before the acceptance is written, and stores the proof beside it (dag_acceptance_refreshes, CRW-728). P is read from the ruling's record
// and never from the call, so a head the parent pushed by hand cannot be named as the ruled one. An event with no record is accepted as it
// always was (the transition: the record becomes required once every parent's lane passes --verified-head), and P == N needs no proof.

// acceptRefreshProof is a passed proof of the head a pull request shows now, from the head the ruling fixed, with everything it rested on, so the
// transaction can read that again before the acceptance and the proof row are written together.
type acceptRefreshProof struct {
	event, revision, relationship    string
	generation                       int64
	verified, head                   string
	baseRepository, baseRef, baseTip string
	proofJSON, resolvedJSON          string
	declarations                     map[string][]Region
	contributors                     map[string][][]Region
	activeAcceptance                 string
}

// proveAcceptedRefresh makes the proof an acceptance on pr owes, or nil when it owes none: no ruling record for the event, a head equal to
// the ruled one, an output accepted before (a replay or a re-validation, which never need a checkout) or a link of the acceptance's own
// chain that the write transaction refuses by its own reason (the relationship, the verified head). It runs before the transaction, as RecordBaseRefresh does.
func (s *Scheduler) proveAcceptedRefresh(ctx context.Context, q store.Querier, snap dag.Snapshot, plan, node, actor string, in AcceptInput, pr PullRequest) (*acceptRefreshProof, error) {
	rel, found, err := currentRelationshipOf(ctx, q, plan, node)
	if err != nil || !found {
		return nil, err
	}
	head, err := s.verifiedHead(ctx, q, rel, in.Event)
	if err != nil {
		// the write transaction reads the same link and refuses with its reason
		return nil, nil
	}
	var one int
	if accepted, err := queryOne(ctx, q, "SELECT 1 FROM dag_acceptances WHERE relationship_id = ? AND execution_generation = ? AND revision_hash = ? LIMIT 1", []any{rel.ID, rel.Generation, head.RevisionHash}, &one); err != nil || accepted {
		return nil, err
	}
	active, hasActive, err := loadActiveAcceptance(ctx, q, plan, node)
	if err != nil {
		return nil, err
	}
	if hasActive {
		stand, err := s.standOf(ctx, q, active)
		if err != nil {
			return nil, err
		}
		if stand.RefreshID != "" && stand.Generation == rel.Generation && stand.EventID == head.EventID && stand.RevisionHash == head.RevisionHash {
			return nil, nil
		}
	}
	ruled, hasRuled, err := store.VerifiedHead(ctx, s.Store, head.EventID)
	if err != nil {
		return nil, err
	}
	if !hasRuled || ruled.HeadSHA == pr.HeadSHA {
		return nil, nil
	}
	repository, err := s.acceptTarget(ctx, q, snap, node, in.PullRequest.Repository)
	if err != nil {
		return nil, err
	}
	if s.Tips == nil {
		return nil, errors.New("this scheduler has no target reader, so it cannot read the base branch to prove the refresh")
	}
	tip, err := s.Tips.Tip(ctx, repository, pr.BaseRef)
	if err != nil {
		return nil, err
	}
	checkout, err := refreshCheckout(repository, in.Checkout)
	if err != nil {
		return nil, err
	}
	g, err := openRefreshRepo(ctx, checkout)
	if err != nil {
		return nil, refuse(contract.RefusalMergeTargetUnreadable, "%s is not a repository git can read: %v", checkout, err)
	}
	defer g.close()
	for _, c := range []struct{ what, sha string }{{"the head the ruling fixed", ruled.HeadSHA}, {"the head of the pull request", pr.HeadSHA}, {"the tip of " + pr.BaseRef, tip.SHA}} {
		if !refreshCommitPattern.MatchString(c.sha) || !g.hasCommit(ctx, c.sha) {
			return nil, refuse(contract.RefusalMergeTargetUnreadable, "%s (%s) is not in %s: fetch it there first (git fetch) and accept again", c.what, c.sha, checkout)
		}
	}
	proof, refusal, err := proveBaseRefresh(ctx, g, ruled.HeadSHA, pr.HeadSHA, tip.SHA)
	if err != nil {
		return nil, refuse(contract.RefusalMergeTargetUnreadable, "git could not answer for %s: %v", checkout, err)
	}
	if refusal != nil {
		return nil, refusedProof(refusal)
	}
	// authority is checked before any declared regeneration command runs; the write checks it again in its transaction
	if err := s.fence(ctx, q, plan, actor); err != nil {
		return nil, err
	}
	declarations, err := loadDeclarations(ctx, q, plan)
	if err != nil {
		return nil, err
	}
	canonical, err := canonicalRepository(repository)
	if err != nil {
		return nil, err
	}
	names := []string{repository, in.PullRequest.Repository, canonical}
	var regions []Region
	for _, r := range declarations[node] {
		if hasName(names, r.Repository) {
			r.Repository = canonical
			regions = append(regions, r)
		}
	}
	contributors, err := s.refreshContributors(ctx, q, plan, node, canonical, names, declarations)
	if err != nil {
		return nil, err
	}
	if refusal, err := proof.applyMechanical(ctx, g, checkout, regions, contributors); err != nil {
		return nil, refuse(contract.RefusalMergeTargetUnreadable, "mechanical check could not answer: %v", err)
	} else if refusal != nil {
		return nil, refusedProof(refusal)
	}
	if left := proof.resolvedPaths(); len(left) > 0 {
		return nil, refuse(contract.RefusalDispositionConflict, "not a base refresh (%s): the merges between %s and %s resolved these files by hand and no declared or built-in rule proved them: %v; the head is not accepted, and its content goes back to the child", RefreshTreeDiffers, short(ruled.HeadSHA), short(pr.HeadSHA), left)
	}
	proofJSON, resolvedJSON := refreshRecordJSON(ruled.HeadSHA, proof, nil)
	out := &acceptRefreshProof{event: head.EventID, revision: head.RevisionHash, relationship: rel.ID, generation: rel.Generation, verified: ruled.HeadSHA, head: pr.HeadSHA,
		baseRepository: repository, baseRef: pr.BaseRef, baseTip: tip.SHA, proofJSON: proofJSON, resolvedJSON: resolvedJSON, declarations: declarations, contributors: contributors}
	if hasActive {
		out.activeAcceptance = active.AcceptanceID
	}
	return out, nil
}

// settleAcceptRefresh is the transaction's reading of what the proof rests on, for the output about to be accepted at pr: the ruling record of
// the event, the declarations and contributor heads, the relationship's generation and the acceptance it replaces. It returns the proof to
// store (nil when none is owed) and the ruled head to report (empty when the event has no record). A proof that no longer holds is a
// refusal to call again, never a stored proof of something else.
func (s *Scheduler) settleAcceptRefresh(ctx context.Context, q store.Querier, plan, node string, rel relRow, head verifiedHead, pr PullRequest, proof *acceptRefreshProof) (*acceptRefreshProof, string, error) {
	ruled, has, err := store.VerifiedHead(ctx, s.Store, head.EventID)
	if err != nil {
		return nil, "", err
	}
	if !has {
		return nil, "", nil
	}
	if ruled.HeadSHA == pr.HeadSHA {
		return nil, ruled.HeadSHA, nil
	}
	moved := func(what string) error {
		return refuse(contract.RefusalDispositionConflict, "%s changed while the head of pull request %s#%d was proved from the head the ruling fixed (%s): call again", what, pr.Repository, pr.Number, short(ruled.HeadSHA))
	}
	if proof == nil {
		return nil, "", moved("the ruling record, the pull request or the output")
	}
	if proof.event != head.EventID || proof.revision != head.RevisionHash || proof.relationship != rel.ID || proof.generation != rel.Generation || proof.verified != ruled.HeadSHA || proof.head != pr.HeadSHA {
		return nil, "", moved("the head of the generation or the ruling record")
	}
	declarations, err := loadDeclarations(ctx, q, plan)
	if err != nil {
		return nil, "", err
	}
	if !reflect.DeepEqual(declarations, proof.declarations) {
		return nil, "", moved("the plan's regions")
	}
	canonical, err := canonicalRepository(proof.baseRepository)
	if err != nil {
		return nil, "", err
	}
	contributors, err := s.refreshContributors(ctx, q, plan, node, canonical, []string{proof.baseRepository, pr.Repository, canonical}, declarations)
	if err != nil {
		return nil, "", err
	}
	if !reflect.DeepEqual(contributors, proof.contributors) {
		return nil, "", moved("the contributor heads")
	}
	var activeID string
	if _, err := queryOne(ctx, q, "SELECT acceptance_id FROM dag_acceptances WHERE plan_id = ? AND node_id = ? AND state = 'active'", []any{plan, node}, &activeID); err != nil {
		return nil, "", err
	}
	if activeID != proof.activeAcceptance {
		return nil, "", moved("the acceptance this one replaces")
	}
	return proof, ruled.HeadSHA, nil
}

// recordAcceptRefresh appends the proof row of an acceptance written in the same transaction and answers its id.
func (s *Scheduler) recordAcceptRefresh(ctx context.Context, a Acceptance, proof *acceptRefreshProof, actor string) (string, error) {
	row := store.AcceptanceRefreshRow{AcceptanceID: a.AcceptanceID, RefreshSeq: 1, RelationshipID: proof.relationship, ExecutionGeneration: proof.generation, EventID: proof.event,
		RevisionHash: proof.revision, HeadSHA: proof.head, VerifiedHeadSHA: proof.verified, BaseRepository: proof.baseRepository, BaseRef: proof.baseRef, BaseTipSHA: proof.baseTip,
		ProofJSON: proof.proofJSON, ResolvedPathsJSON: proof.resolvedJSON, RecordedByTaskID: actor, CoordinatorEpoch: s.ExpectedEpoch, RecordedAt: s.now()}
	row.RefreshID = store.RefreshDigest(row)
	if err := store.RecordAcceptanceRefresh(ctx, s.Store, row); err != nil {
		return "", err
	}
	return row.RefreshID, nil
}
