package mergeturn

import (
	"context"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// underCorrectionRefusal is CRW-906's gate on the single lane's own operations. A defect found in an
// accepted result before it lands is corrected in a later generation of the same relationship, and
// until that generation's result is accepted over (dag-accept --supersedes) or the generation is
// withdrawn, the accepted head is the result being repaired: it must not be merged.
//
// The DAG judgement and the bundle gate refuse it already, but a merge turn can have been granted
// before the correction was opened, and the lane's own check and land are what actually carry the
// head to the base. They read the relationship's live execution generation against the generation its
// active acceptance stands on here, inside the transaction that writes, so a generation opened while
// the check was reading the forge is seen and nothing is written for it.
//
// The refusal is the existing disposition_conflict and it names the open generation: no new refusal
// name, no column, no schema change. nil means the turn is not under correction (or names no
// relationship, which the lane's own rules already cover).
func underCorrectionRefusal(ctx context.Context, q store.Querier, r store.MergeTurnsRow) (*registry.CoordinationRefusal, error) {
	if !r.RelationshipID.Valid || r.RelationshipID.String == "" {
		return nil, nil
	}
	under, live, stand, err := acceptance.UnderCorrection(ctx, q, r.RelationshipID.String)
	if err != nil {
		return nil, err
	}
	if !under {
		return nil, nil
	}
	return coordination(r, contract.RefusalDispositionConflict,
		"the accepted result of this turn's relationship "+pyvalue.StrRepr(r.RelationshipID.String)+
			" is under correction: generation "+strconv.FormatInt(live, 10)+" is open over the acceptance, which stands on generation "+
			strconv.FormatInt(stand, 10)+", so the head this turn holds is the result being repaired and is not merged. "+
			"Accept the corrected result with dag-accept --supersedes, or withdraw the generation, and request the turn again",
		r.CandidateHead, r.HolderTaskID), nil
}

// trainUnderCorrectionRefusal is the bundle member's refusal: the member's relationship has a correction
// generation open over its accepted result, so the head the bundle would carry is the result being
// repaired. It is the existing disposition_conflict and it names the open generation.
func trainUnderCorrectionRefusal(pr int64, relationship string, liveGeneration, standGeneration int64) error {
	return trainConflict("pull request %d's relationship %s is under correction: generation %d is open over the accepted result, which stands on generation %d, so the member is not a candidate a bundle may carry or land; accept the corrected result with dag-accept --supersedes, or withdraw the generation", pr, pyvalue.StrRepr(relationship), liveGeneration, standGeneration)
}

// trainMemberCorrectionRefusal re-reads one member's correction state on the caller's querier and refuses
// the member when a correction generation is open over its accepted result (CRW-906). Open and verify
// read each member's acceptance before the transaction that writes, and a generation opened in that gap
// leaves the earlier reading stale; this is the same question asked again inside the write.
func trainMemberCorrectionRefusal(ctx context.Context, q store.Querier, pr int64, relationship, memberHead string) error {
	under, live, stand, err := acceptance.UnderCorrection(ctx, q, relationship)
	if err != nil {
		return trainUnreadable("the acceptance of relationship %s was not read: %v", pyvalue.StrRepr(relationship), err)
	}
	if !under {
		return nil
	}
	return trainUnderCorrectionRefusal(pr, relationship, live, stand)
}
