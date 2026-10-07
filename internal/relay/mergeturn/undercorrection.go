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
// Acceptance uniqueness is per node and output, not per repository and pull request or per repository
// and head, so more than one accepted node can name the same pull request or the same commit. Every
// matching relationship is asked, not the newest one: a turn is refused when ANY of them has a
// correction open, because the head it holds is then a result being repaired by at least one of them.
//
// The refusal is the existing disposition_conflict and it names the open generation: no new refusal
// name, no column, no schema change.
func underCorrectionRefusal(ctx context.Context, q store.Querier, r store.MergeTurnsRow) (*registry.CoordinationRefusal, error) {
	candidates := []string{}
	if r.RelationshipID.Valid && r.RelationshipID.String != "" {
		candidates = append(candidates, r.RelationshipID.String)
	} else {
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
		var query string
		var args []any
		switch {
		case r.PRNumber.Valid:
			// the turn names the pull request: the acceptance's recorded forge identity maps it back
			query = "SELECT DISTINCT a.relationship_id FROM dag_acceptance_forge f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id" +
				" WHERE lower(f.forge_repository) = lower(?) AND f.pr_number = ? AND a.state = 'active'"
			args = []any{r.Repository, r.PRNumber.Int64}
		case r.CandidateHead != "":
			// the turn names neither selector: the head it holds is the only identity left, and the
			// acceptance that recorded it is what the head belongs to. A recorded base refresh moves
			// the head the acceptance stands on without changing the accepted head (dag_base_refreshes
			// carries the refreshed head), so both are read: the head the turn holds is the accepted
			// head or the head of a refresh of it, and either belongs to the same acceptance.
			query = "SELECT DISTINCT a.relationship_id FROM dag_acceptances a" +
				" WHERE a.state = 'active' AND lower(a.repository) = lower(?) AND a.head_sha = ?"
			args = []any{r.Repository, r.CandidateHead}
			if present, err := ucZoneTable(ctx, q, "dag_base_refreshes"); err != nil {
				return nil, err
			} else if present {
				query = "SELECT DISTINCT a.relationship_id FROM dag_acceptances a" +
					" WHERE a.state = 'active' AND lower(a.repository) = lower(?) AND a.head_sha = ?" +
					" UNION ALL SELECT a.relationship_id FROM dag_base_refreshes f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id" +
					" WHERE a.state = 'active' AND lower(a.repository) = lower(?) AND f.head_sha = ?"
				args = []any{r.Repository, r.CandidateHead, r.Repository, r.CandidateHead}
			}
		default:
			return nil, nil
		}
		found, err := ucRelationships(ctx, q, query, args...)
		if err != nil {
			return nil, err
		}
		candidates = found
	}
	for _, relationship := range candidates {
		under, live, stand, err := acceptance.UnderCorrection(ctx, q, relationship)
		if err != nil {
			return nil, err
		}
		if !under {
			continue
		}
		return coordination(r, contract.RefusalDispositionConflict,
			"the accepted result of this turn's relationship "+pyvalue.StrRepr(relationship)+
				" is under correction: generation "+strconv.FormatInt(live, 10)+" is open over the acceptance, which stands on generation "+
				strconv.FormatInt(stand, 10)+", so the head this turn holds is the result being repaired and is not merged. "+
				"Accept the corrected result with dag-accept --supersedes, or withdraw the generation, and request the turn again",
			r.CandidateHead, r.HolderTaskID), nil
	}
	return nil, nil
}

// ucRelationships are every relationship a resolution query names, in the query's order. The callers
// read all of them rather than the newest: acceptance uniqueness is per node and output, so more than
// one accepted node can name the same pull request or the same head, and the turn is refused when any
// of them has a correction open.
func ucRelationships(ctx context.Context, q store.Querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var relationship string
		if err := rows.Scan(&relationship); err != nil {
			return nil, err
		}
		out = append(out, relationship)
	}
	return out, rows.Err()
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

// trainExcludedMemberRefusal is CRW-906's guard on a member whose turn left the lane, and it answers the
// question the excluded path must still ask: is the result the bundle carries for this member still the
// result the plan stands on?
//
// The CRW-897 carve-out keeps holding for what it is about: a member whose turn was returned or withdrawn
// is not recorded landed, and neither its pull request moving nor its acceptance being revoked refuses
// the rest of the bundle. This is a different question. A correction of the member's accepted result is
// accepted over by dag-accept --supersedes, which moves the acceptance to the corrected head; the old
// bundle's tree still carries the defective head it was verified on, so landing it would land exactly the
// result the correction repaired. Once the acceptance stands on a head the bundle does not carry, the
// bundle no longer describes what the plan accepts for that member and must be rebuilt and verified.
func trainExcludedMemberRefusal(ctx context.Context, q store.Querier, pr int64, relationship, memberHead string) error {
	active, found, err := acceptance.ActiveForRelationship(ctx, q, relationship)
	if err != nil {
		return trainUnreadable("the acceptance of relationship %s was not read: %v", pyvalue.StrRepr(relationship), err)
	}
	if !found {
		// the carve-out: a revoked acceptance does not refuse the rest of the bundle
		return nil
	}
	stand, err := acceptance.StandOf(ctx, q, active.AcceptanceID, relationship, active.Generation, active.EventID, active.RevisionHash, active.HeadSHA)
	if err != nil {
		return trainUnreadable("what acceptance %s stands on was not read: %v", pyvalue.StrRepr(active.AcceptanceID), err)
	}
	if stand.Head != "" && !SameCommit(stand.Head, memberHead) {
		return trainConflict("pull request %d's relationship %s now stands on %s and the bundle carries %s for it, so this bundle no longer holds the result the plan accepts for that member: rebuild and verify the bundle on the current result before landing it",
			pr, pyvalue.StrRepr(relationship), pyvalue.StrRepr(stand.Head), pyvalue.StrRepr(memberHead))
	}
	return trainMemberCorrectionRefusal(ctx, q, pr, relationship, memberHead)
}
