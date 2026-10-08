package mergeturn

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ucNamed is one acceptance a turn or a bundle member is held on: its relationship, and the acceptance that
// recorded it.
type ucNamed struct {
	acceptanceID string
	relationship string
}

// ucAcceptancesNamed reads the acceptances query selects (acceptance_id, relationship_id, repository, one row
// per match) and keeps those whose repository names turnRepository under any spelling the acceptance has: its
// own repository and its forge repository. The repository is compared by SameRepository, the lane's one
// reading of a repository, so a local checkout, its .git directory, a symlink to it and the forge slug of the
// same repository all meet here. The rows are read in full before any further query runs on the same querier.
func ucAcceptancesNamed(ctx context.Context, q store.Querier, turnRepository, query string, args ...any) ([]ucNamed, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	type candidate struct{ id, relationship, repository string }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.relationship, &c.repository); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []ucNamed
	for _, c := range candidates {
		spellings, err := ucSpellings(ctx, q, c.id, c.repository)
		if err != nil {
			return nil, err
		}
		for _, s := range spellings {
			if SameRepository(turnRepository, s) {
				out = append(out, ucNamed{acceptanceID: c.id, relationship: c.relationship})
				break
			}
		}
	}
	return out, nil
}

// ucSpellings are every repository spelling an acceptance has: the repository it was accepted against and the
// forge repository recorded with it (dag_acceptance_forge), when that table exists.
func ucSpellings(ctx context.Context, q store.Querier, acceptanceID, repository string) ([]string, error) {
	out := []string{repository}
	present, err := ucZoneTable(ctx, q, "dag_acceptance_forge")
	if err != nil || !present {
		return out, err
	}
	rows, err := q.QueryContext(ctx, "SELECT forge_repository FROM dag_acceptance_forge WHERE acceptance_id = ?", acceptanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var forge string
		if err := rows.Scan(&forge); err != nil {
			return nil, err
		}
		out = append(out, forge)
	}
	return out, rows.Err()
}

// ucHeldQuery selects the acceptances of the given state that hold head: their accepted head, and each head a
// base refresh recorded for them, read as recorded. It
// selects acceptance_id, relationship_id and repository, and the head comparison is crw_same_commit.
func ucHeldQuery(ctx context.Context, q store.Querier, state, head string) (string, []any, error) {
	query := "SELECT a.acceptance_id, a.relationship_id, a.repository FROM dag_acceptances a" +
		" WHERE a.state = '" + state + "' AND crw_same_commit(a.head_sha, ?)"
	args := []any{head}
	refreshed, err := ucZoneTable(ctx, q, "dag_base_refreshes")
	if err != nil {
		return "", nil, err
	}
	if refreshed {
		query += " UNION ALL SELECT a.acceptance_id, a.relationship_id, a.repository FROM dag_base_refreshes f" +
			" JOIN dag_acceptances a ON a.acceptance_id = f.acceptance_id" +
			" WHERE a.state = '" + state + "' AND crw_same_commit(f.head_sha, ?)"
		args = append(args, head)
	}
	return query, args, nil
}
