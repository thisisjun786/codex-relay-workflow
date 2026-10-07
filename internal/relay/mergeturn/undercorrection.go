package mergeturn

import (
	"context"
	"database/sql"
	"errors"
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
// A turn that names no relationship is not exempt: the CLI lets a claim carry a forge pull request
// instead, and a node under correction would otherwise be carried on the head the correction is
// repairing by the very route that omits the relationship. The acceptance's forge identity is the
// mapping from that pull request back to the relationship (dag_acceptance_forge, written with the
// acceptance), and it is read here so both routes meet the same gate. The forge slug is compared
// case-insensitively, as every other repository identity comparison in the relay is: GitHub slugs are
// case-insensitive, so a turn that spells the repository differently names the same pull request.
//
// A turn that names neither is not exempt either: --relationship and --pr are both optional on
// merge-turn-request, so the head alone is left, and a turn holding the accepted head of a node under
// correction would slip through the very route that omits both selectors. The active acceptance
// records its own repository and head (dag_acceptances.repository, head_sha), so a turn whose head is
// an accepted head of its repository resolves to that acceptance's relationship. A turn whose head no
// active acceptance of that repository stands on is left to the lane's own rules.
//
// The refusal is the existing disposition_conflict and it names the open generation: no new refusal
// name, no column, no schema change.
func underCorrectionRefusal(ctx context.Context, q store.Querier, r store.MergeTurnsRow) (*registry.CoordinationRefusal, error) {
	relationship := r.RelationshipID.String
	if !r.RelationshipID.Valid || relationship == "" {
		// The DAG zone arrives with the first write open (D-01), and the lane serves projects that
		// have none: a store that predates the table has no accepted result for this turn, which is
		// absence and not an error.
		present, err := ucZoneTable(ctx, q, "dag_acceptances")
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, nil
		}
		var found string
		switch {
		case r.PRNumber.Valid:
			// the turn names the pull request: the acceptance's recorded forge identity maps it back
			if err := q.QueryRowContext(ctx, "SELECT a.relationship_id FROM dag_acceptance_forge f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id"+
				" WHERE lower(f.forge_repository) = lower(?) AND f.pr_number = ? AND a.state = 'active' ORDER BY a.accepted_at DESC, a.acceptance_id DESC LIMIT 1",
				r.Repository, r.PRNumber.Int64).Scan(&found); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, nil
				}
				return nil, err
			}
		case r.CandidateHead != "":
			// the turn names neither selector: the head it holds is the only identity left, and the
			// acceptance that recorded it is what the head belongs to. A recorded base refresh moves
			// the head the acceptance stands on without changing the accepted head (dag_base_refreshes
			// carries the refreshed head), so both are read: the head the turn holds is the accepted
			// head or the head of a refresh of it, and either belongs to the same acceptance.
			query := "SELECT a.relationship_id FROM dag_acceptances a" +
				" WHERE a.state = 'active' AND lower(a.repository) = lower(?) AND a.head_sha = ?"
			args := []any{r.Repository, r.CandidateHead}
			if present, err := ucZoneTable(ctx, q, "dag_base_refreshes"); err != nil {
				return nil, err
			} else if present {
				query = "SELECT a.relationship_id FROM dag_acceptances a" +
					" WHERE a.state = 'active' AND lower(a.repository) = lower(?) AND a.head_sha = ?" +
					" UNION ALL SELECT a.relationship_id FROM dag_base_refreshes f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id" +
					" WHERE a.state = 'active' AND lower(a.repository) = lower(?) AND f.head_sha = ?"
				args = []any{r.Repository, r.CandidateHead, r.Repository, r.CandidateHead}
			}
			if err := q.QueryRowContext(ctx, query+" ORDER BY 1 LIMIT 1", args...).Scan(&found); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return nil, nil
				}
				return nil, err
			}
		default:
			return nil, nil
		}
		relationship = found
	}
	under, live, stand, err := acceptance.UnderCorrection(ctx, q, relationship)
	if err != nil {
		return nil, err
	}
	if !under {
		return nil, nil
	}
	return coordination(r, contract.RefusalDispositionConflict,
		"the accepted result of this turn's relationship "+pyvalue.StrRepr(relationship)+
			" is under correction: generation "+strconv.FormatInt(live, 10)+" is open over the acceptance, which stands on generation "+
			strconv.FormatInt(stand, 10)+", so the head this turn holds is the result being repaired and is not merged. "+
			"Accept the corrected result with dag-accept --supersedes, or withdraw the generation, and request the turn again",
		r.CandidateHead, r.HolderTaskID), nil
}

// trainUnderCorrectionRefusal is the bundle member's refusal: the member's relationship has a correction
// ucZoneTable reports whether the store holds the named DAG zone table. The zone arrives with the first
// write open (D-01), so a store that predates it holds no acceptance and the lane's turn is left to the
// lane's own rules; that is absence and not an error, the way acceptance.Stands reads dag_base_refreshes.
func ucZoneTable(ctx context.Context, q store.Querier, table string) (bool, error) {
	var name string
	err := q.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return name == table, nil
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
