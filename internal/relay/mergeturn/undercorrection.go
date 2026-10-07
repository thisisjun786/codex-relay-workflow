package mergeturn

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"

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
	held := strings.TrimSpace(r.CandidateHead)
	candidates := []string{}
	if r.RelationshipID.Valid && r.RelationshipID.String != "" {
		candidates = append(candidates, r.RelationshipID.String)
	}
	// The turn's own selectors are not the only identities that matter. Acceptance uniqueness is per
	// node and output, so two accepted nodes can name the same commit or the same pull request: a turn
	// that names one relationship can still be carrying a head another relationship is repairing. The
	// identities a claim may carry are resolved here whether or not the turn named a relationship, so a
	// named relationship adds to the set rather than replacing it.
	//
	// The DAG zone arrives with the first write open (D-01), and the lane serves projects that have
	// none: a store that predates the table has no accepted result for this turn, which is absence and
	// not an error.
	present, err := ucZoneTable(ctx, q, "dag_acceptances")
	if err != nil {
		return nil, err
	}
	if present {
		queries := []struct {
			query string
			args  []any
		}{}
		if r.PRNumber.Valid {
			// the acceptance's recorded forge identity maps the pull request back to its relationship
			queries = append(queries, struct {
				query string
				args  []any
			}{"SELECT DISTINCT a.relationship_id FROM dag_acceptance_forge f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id" +
				" WHERE lower(f.forge_repository) = lower(?) AND f.pr_number = ? AND a.state = 'active'",
				[]any{r.Repository, r.PRNumber.Int64}})
		}
		if held != "" {
			// The head the turn holds belongs to the acceptance that recorded it. A recorded base refresh
			// moves the head the acceptance stands on without changing the accepted head
			// (dag_base_refreshes carries the refreshed head), so both are read.
			//
			// The repository an acceptance is matched by is its FORGE identity where one was recorded
			// (dag_acceptance_forge), because an acceptance written before the forge rule keeps whatever
			// target it was accepted against — a local checkout included — while only the forge row names
			// the owner/name a merge turn is requested against. An acceptance with no forge row is
			// matched by its own repository, so a store that predates the table is unchanged.
			identity := "lower(a.repository)"
			if hasForge, err := ucZoneTable(ctx, q, "dag_acceptance_forge"); err != nil {
				return nil, err
			} else if hasForge {
				identity = "lower(COALESCE((SELECT g.forge_repository FROM dag_acceptance_forge g WHERE g.acceptance_id = a.acceptance_id), a.repository))"
			}
			query := "SELECT DISTINCT a.relationship_id FROM dag_acceptances a" +
				" WHERE a.state = 'active' AND " + identity + " = lower(?) AND lower(trim(a.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13)))"
			args := []any{r.Repository, held}
			if refreshed, err := ucZoneTable(ctx, q, "dag_base_refreshes"); err != nil {
				return nil, err
			} else if refreshed {
				query = "SELECT DISTINCT a.relationship_id FROM dag_acceptances a" +
					" WHERE a.state = 'active' AND " + identity + " = lower(?) AND lower(trim(a.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13)))" +
					" UNION ALL SELECT a.relationship_id FROM dag_base_refreshes f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id" +
					" WHERE a.state = 'active' AND " + identity + " = lower(?) AND lower(trim(f.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13)))"
				args = []any{r.Repository, held, r.Repository, held}
			}
			queries = append(queries, struct {
				query string
				args  []any
			}{query, args})
		}
		for _, one := range queries {
			found, err := ucRelationships(ctx, q, one.query, one.args...)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, found...)
		}
	}
	seen := map[string]bool{}
	unique := candidates[:0]
	for _, relationship := range candidates {
		if relationship == "" || seen[relationship] {
			continue
		}
		seen[relationship] = true
		unique = append(unique, relationship)
	}
	candidates = unique
	// A head that some relationship accepted and then moved off is not the head the plan accepts for it
	// any more. dag-accept --supersedes replaces the acceptance, so the old head no longer belongs to any
	// ACTIVE acceptance and the active-only lookups above cannot see it: a turn that was held for the old
	// head would otherwise proceed to merge exactly the result the correction replaced. The acceptance
	// that recorded the head is read whatever its state, and a turn holding it is refused.
	if held != "" {
		if replaced, err := ucReplacedHead(ctx, q, r.Repository, held); err != nil {
			return nil, err
		} else if replaced != "" {
			return coordination(r, contract.RefusalDispositionConflict,
				"the head this turn holds is no longer the accepted result of its relationship "+pyvalue.StrRepr(replaced)+
					": an acceptance of it was replaced by a later one (dag-accept --supersedes), so the result the plan accepts has moved and this turn would merge the result that was replaced. Request the turn for the head the acceptance stands on now",
				held, r.HolderTaskID), nil
		}
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
				"Accept the corrected result with dag-accept --supersedes, or withdraw the generation if it was never bound or sent, and request the turn again",
			held, r.HolderTaskID), nil
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

// ucReplacedHead names a relationship whose acceptance recorded this head and was then replaced, so the
// head is no longer the result the plan accepts for it. "" means no acceptance ever recorded the head.
// It reads the acceptance whatever its state: a superseded or revoked one is exactly the case, because
// the active-only lookups above cannot see it. A store that predates the forge table falls back to the
// acceptance's own repository.
func ucReplacedHead(ctx context.Context, q store.Querier, repository, head string) (string, error) {
	if head == "" {
		return "", nil
	}
	identity := "lower(a.repository)"
	if hasForge, err := ucZoneTable(ctx, q, "dag_acceptance_forge"); err != nil {
		return "", err
	} else if hasForge {
		identity = "lower(COALESCE((SELECT g.forge_repository FROM dag_acceptance_forge g WHERE g.acceptance_id = a.acceptance_id), a.repository))"
	}
	// A head the acceptance once stood on through a recorded base refresh is covered too: the refresh
	// rows outlive the acceptance they were recorded for, and a turn held for a refreshed head would
	// otherwise resolve to nothing once dag-accept --supersedes replaced the acceptance.
	query := "SELECT a.relationship_id FROM dag_acceptances a" +
		" WHERE a.state = 'superseded' AND " + identity + " = lower(?) AND lower(trim(a.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13)))"
	args := []any{repository, head}
	if refreshed, err := ucZoneTable(ctx, q, "dag_base_refreshes"); err != nil {
		return "", err
	} else if refreshed {
		query = "SELECT a.relationship_id FROM dag_acceptances a" +
			" WHERE a.state = 'superseded' AND " + identity + " = lower(?) AND lower(trim(a.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13)))" +
			" UNION ALL SELECT f.relationship_id FROM dag_base_refreshes f JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id" +
			" WHERE a.state = 'superseded' AND " + identity + " = lower(?) AND lower(trim(f.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13)))"
		args = []any{repository, head, repository, head}
	}
	// A head is replaced only when a correction was accepted over it (superseded): a revoked acceptance is a parent's withdrawal, not a replacement. A relationship that still holds the head actively, as its acceptance's head or through a base refresh of
	// it, is not replaced by it: the head is that relationship's own current result.
	held := "lower(trim(a2.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13)))"
	args = append(args, head)
	if refreshedHeads, err := ucZoneTable(ctx, q, "dag_base_refreshes"); err != nil {
		return "", err
	} else if refreshedHeads {
		held += " OR a2.acceptance_id IN (SELECT f2.acceptance_id FROM dag_base_refreshes f2 WHERE lower(trim(f2.head_sha, ' ' || char(9,10,11,12,13))) = lower(trim(?, ' ' || char(9,10,11,12,13))))"
		args = append(args, head)
	}
	query = "SELECT relationship_id FROM (" + query + ") WHERE relationship_id NOT IN (SELECT a2.relationship_id FROM dag_acceptances a2" +
		" WHERE a2.state = 'active' AND (" + held + ")) ORDER BY relationship_id LIMIT 1"
	var relationship string
	if err := q.QueryRowContext(ctx, query, args...).Scan(&relationship); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return relationship, nil
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
	return trainConflict("pull request %d's relationship %s is under correction: generation %d is open over the accepted result, which stands on generation %d, so the member is not a candidate a bundle may carry or land; accept the corrected result with dag-accept --supersedes, or withdraw the generation if it was never bound or sent", pr, pyvalue.StrRepr(relationship), liveGeneration, standGeneration)
}

// trainMemberCorrectionRefusal re-reads one member's correction state on the caller's querier and refuses
// the member when the head the bundle carries is not a head the plan still accepts for it (CRW-906). Open
// and verify read each member's acceptance before the transaction that writes, and a generation opened in
// that gap leaves the earlier reading stale; this is the same question asked again inside the write.
//
// The question is asked through the lane's own gate, with the member's own identities, because a member is
// not identified by its relationship alone: acceptance uniqueness is per node and output, so another
// accepted node can name the same pull request or the same commit, and a bundle member whose relationship
// is live can still carry a head another relationship is repairing. The gate resolves every matching
// acceptance and asks each one, which is the same rule the single lane applies.
func trainMemberCorrectionRefusal(ctx context.Context, q store.Querier, repository string, pr int64, relationship, memberHead string) error {
	// A member whose own relationship still holds an active acceptance is also checked against the head
	// the plan accepts for it: a correction that was opened AND accepted over inside the gap moves the
	// acceptance to the corrected head, which makes the live and stand generations equal again, so the
	// generation comparison alone would let the obsolete bundle through.
	if active, found, err := acceptance.ActiveForRelationship(ctx, q, relationship); err != nil {
		return trainUnreadable("the acceptance of relationship %s was not read: %v", pyvalue.StrRepr(relationship), err)
	} else if found {
		stand, err := acceptance.StandOf(ctx, q, active.AcceptanceID, relationship, active.Generation, active.EventID, active.RevisionHash, active.HeadSHA)
		if err != nil {
			return trainUnreadable("what acceptance %s stands on was not read: %v", pyvalue.StrRepr(active.AcceptanceID), err)
		}
		if stand.Head != "" && !SameCommit(stand.Head, memberHead) {
			return trainConflict("pull request %d's relationship %s stands on %s and the bundle carries %s for it, so the member's accepted result moved while the bundle was being read: rebuild and verify the bundle on the current result",
				pr, pyvalue.StrRepr(relationship), pyvalue.StrRepr(stand.Head), pyvalue.StrRepr(memberHead))
		}
	}
	refusal, err := underCorrectionRefusal(ctx, q, store.MergeTurnsRow{
		Repository:     repository,
		RelationshipID: sql.NullString{String: relationship, Valid: relationship != ""},
		PRNumber:       sql.NullInt64{Int64: pr, Valid: pr > 0},
		CandidateHead:  memberHead,
	})
	if err != nil {
		return trainUnreadable("the correction state of pull request %d was not read: %v", pr, err)
	}
	if refusal == nil {
		return nil
	}
	return trainConflict("%s", refusal.Detail)
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
func trainExcludedMemberRefusal(ctx context.Context, q store.Querier, repository string, pr int64, relationship, memberHead string) error {
	active, found, err := acceptance.ActiveForRelationship(ctx, q, relationship)
	if err != nil {
		return trainUnreadable("the acceptance of relationship %s was not read: %v", pyvalue.StrRepr(relationship), err)
	}
	if !found {
		// the carve-out: a revoked acceptance does not refuse the rest of the bundle. Another node's correction over
		// the head the bundle still carries for this member is a different question, and it refuses the landing.
		return trainMemberCorrectionRefusal(ctx, q, repository, pr, relationship, memberHead)
	}
	stand, err := acceptance.StandOf(ctx, q, active.AcceptanceID, relationship, active.Generation, active.EventID, active.RevisionHash, active.HeadSHA)
	if err != nil {
		return trainUnreadable("what acceptance %s stands on was not read: %v", pyvalue.StrRepr(active.AcceptanceID), err)
	}
	if stand.Head != "" && !SameCommit(stand.Head, memberHead) {
		return trainConflict("pull request %d's relationship %s now stands on %s and the bundle carries %s for it, so this bundle no longer holds the result the plan accepts for that member: rebuild and verify the bundle on the current result before landing it",
			pr, pyvalue.StrRepr(relationship), pyvalue.StrRepr(stand.Head), pyvalue.StrRepr(memberHead))
	}
	// the same question the surviving members get: the excluded member's code is still in the merge
	// commit, so a correction open over the head the bundle carries for it refuses the landing, whoever
	// opened it. The carve-out above still lets a revoked acceptance or a moved pull request leave the
	// rest of the bundle alone.
	return trainMemberCorrectionRefusal(ctx, q, repository, pr, relationship, memberHead)
}
