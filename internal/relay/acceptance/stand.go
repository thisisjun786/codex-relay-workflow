// Package acceptance reads what a plan acceptance stands on. It is the one place the rule lives,
// so the DAG scheduler and the merge train apply the same reading rather than two copies that can
// drift. It imports only leaves (dag, registry, store), so neither caller creates a cycle.
package acceptance

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SchemaBaseRefresh names the answer of dag-base-refresh. It is part of the refresh digest, so the
// spelling is fixed and shared with the scheduler's own constant of the same value.
const SchemaBaseRefresh = "dag-base-refresh/1"

// Stand is what an acceptance currently stands on: the relationship, generation, event, revision and
// head that integration and the merge train are judged against. It is the acceptance's own unless a
// base refresh was recorded for it, and then the newest valid refresh's.
type Stand struct {
	RelationshipID string
	Generation     int64
	EventID        string
	RevisionHash   string
	Head           string
	RefreshID      string // "" when the acceptance stands on itself
}

// Own is what an acceptance stands on before any base refresh was recorded for it.
func Own(relationshipID string, generation int64, eventID, revisionHash, head string) Stand {
	return Stand{RelationshipID: relationshipID, Generation: generation, EventID: eventID, RevisionHash: revisionHash, Head: head}
}

// RefreshDigest is the identity of a base refresh: its refresh_id is the digest of everything it
// records but the time and the author, so a row written by hand under another content is not read as
// one. The spelling matches the scheduler's, because existing stores hold ids it produced.
func RefreshDigest(acceptanceID, relationshipID string, generation int64, eventID, revisionHash, head, baseRepository, baseRef, baseTip, proofJSON, resolvedJSON string) string {
	return registry.CoordinationID("dbr", dag.Canonical(map[string]any{
		"schema": SchemaBaseRefresh, "acceptance_id": acceptanceID, "relationship_id": relationshipID, "execution_generation": generation,
		"event_id": eventID, "revision_hash": revisionHash, "head_sha": head, "base_repository": baseRepository, "base_ref": baseRef,
		"base_tip_sha": baseTip, "proof": proofJSON, "resolved_paths": resolvedJSON}))
}

// tableExists reports whether the store holds the named zone table. The zone arrives with the first
// write open, so a reader of a store that predates it finds no table; that is absence and not an error.
func tableExists(ctx context.Context, q store.Querier, name string) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// Stands are the base refreshes recorded for an acceptance that digest to their ids, newest first. A
// store that predates the table has none; a row that does not digest to its id is ignored.
func Stands(ctx context.Context, q store.Querier, acceptanceID, relationshipID string, generation int64) ([]Stand, error) {
	present, err := tableExists(ctx, q, "dag_base_refreshes")
	if err != nil || !present {
		return nil, err
	}
	rows, err := q.QueryContext(ctx, "SELECT refresh_id, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json"+
		" FROM dag_base_refreshes WHERE acceptance_id = ? ORDER BY refresh_seq DESC", acceptanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Stand
	for rows.Next() {
		var id, rid, event, revision, head, baseRepo, baseRef, baseTip, proof, resolved string
		var rowGeneration int64
		if err := rows.Scan(&id, &rid, &rowGeneration, &event, &revision, &head, &baseRepo, &baseRef, &baseTip, &proof, &resolved); err != nil {
			return nil, err
		}
		if rid == relationshipID && rowGeneration > generation && id == RefreshDigest(acceptanceID, rid, rowGeneration, event, revision, head, baseRepo, baseRef, baseTip, proof, resolved) {
			out = append(out, Stand{RelationshipID: rid, Generation: rowGeneration, EventID: event, RevisionHash: revision, Head: head, RefreshID: id})
		}
	}
	return out, rows.Err()
}

// StandOf reads what an acceptance stands on now: the newest valid base refresh recorded for it, else
// its own. The caller passes the querier it wants the read to run on, so a command reading inside its
// transaction gets the transaction's connection.
func StandOf(ctx context.Context, q store.Querier, acceptanceID, relationshipID string, generation int64, eventID, revisionHash, head string) (Stand, error) {
	stands, err := Stands(ctx, q, acceptanceID, relationshipID, generation)
	if err != nil || len(stands) == 0 {
		return Own(relationshipID, generation, eventID, revisionHash, head), err
	}
	return stands[0], nil
}

// ActiveForRelationship is the newest active acceptance of a relationship, the row the merge train
// requires before it carries a member. found is false when the relationship has none.
type Active struct {
	AcceptanceID string
	EventID      string
	HeadSHA      string
	RevisionHash string
	Generation   int64
}

// ErrNoAcceptance is returned by RequireStand when the relationship has no active acceptance.
var ErrNoAcceptance = errors.New("acceptance: the relationship has no active acceptance")

// ActiveForRelationship reads the newest active acceptance of a relationship. A store that predates
// the zone table holds none.
func ActiveForRelationship(ctx context.Context, q store.Querier, relationshipID string) (Active, bool, error) {
	present, err := tableExists(ctx, q, "dag_acceptances")
	if err != nil || !present {
		return Active{}, false, err
	}
	row := q.QueryRowContext(ctx, "SELECT acceptance_id, event_id, head_sha, revision_hash, execution_generation FROM dag_acceptances"+
		" WHERE relationship_id = ? AND state = 'active' ORDER BY accepted_at DESC, acceptance_id DESC LIMIT 1", relationshipID)
	var a Active
	var head sql.NullString
	if err := row.Scan(&a.AcceptanceID, &a.EventID, &head, &a.RevisionHash, &a.Generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Active{}, false, nil
		}
		return Active{}, false, err
	}
	a.HeadSHA = head.String
	return a, true, nil
}

// ActiveWithLiveGeneration is ActiveForRelationship with the relationship's own live execution
// generation taken in the same statement, so the acceptance and the generation are one reading. The
// generation moves when a generation is opened, withdrawn or accepted over, and the acceptance moves
// when one is accepted: a correction gate that read the two apart could see neither move, and would let
// a node under correction be judged on the head its old acceptance stands on (CRW-906).
//
// The acceptance is read exactly as ActiveForRelationship reads it, and the generation comes from the
// relationship the acceptance names, in the same statement. A store whose relationships row is absent
// (a fixture that seeded only the acceptance, as some tests do) answers the acceptance's own
// generation, which is the reading that says no correction is open. found is false when the
// relationship has no active acceptance; the live generation is then read on its own, because no
// correction gate turns on it there (the caller's own "no acceptance" refusal wins), and a store that
// predates the zone table has no acceptance at all.
func ActiveWithLiveGeneration(ctx context.Context, q store.Querier, relationshipID string) (active Active, generation int64, found bool, err error) {
	present, err := tableExists(ctx, q, "dag_acceptances")
	if err != nil || !present {
		return Active{}, 0, false, err
	}
	row := q.QueryRowContext(ctx, "SELECT a.acceptance_id, a.event_id, a.head_sha, a.revision_hash, a.execution_generation,"+
		" COALESCE(r.execution_generation, a.execution_generation)"+
		" FROM dag_acceptances a LEFT JOIN relationships r ON r.relationship_id = a.relationship_id"+
		" WHERE a.relationship_id = ? AND a.state = 'active' ORDER BY a.accepted_at DESC, a.acceptance_id DESC LIMIT 1", relationshipID)
	var head sql.NullString
	if err := row.Scan(&active.AcceptanceID, &active.EventID, &head, &active.RevisionHash, &active.Generation, &generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			live, liveErr := liveGeneration(ctx, q, relationshipID)
			return Active{}, live, false, liveErr
		}
		return Active{}, 0, false, err
	}
	active.HeadSHA = head.String
	return active, generation, true, nil
}

// liveGeneration is the generation a relationship stands on now, for the relationship that has no active
// acceptance. 0 means the store holds no such relationship.
func liveGeneration(ctx context.Context, q store.Querier, relationshipID string) (int64, error) {
	var generation int64
	if err := q.QueryRowContext(ctx, "SELECT execution_generation FROM relationships WHERE relationship_id = ?", relationshipID).Scan(&generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return generation, nil
}

// UnderCorrection is the CRW-906 gate: whether the relationship's live execution generation is later
// than the generation its active acceptance stands on. A correction generation is then open (opened by
// hand, a ruling or a decision reply) and has not been accepted over yet, so the node is under
// correction and its old head is not a merge candidate: the defect the correction answers was found in
// the accepted result, and carrying that head to the base would land exactly what is being repaired.
//
// The stand generation counts a recorded base refresh (StandOf), so a generation that only carried the
// accepted head onto a moved base is not a correction: it moves the stand generation with it and the
// member may ride again. A withdrawn correction generation moves the live generation back to the
// generation the acceptance stands on, and the old head may ride again as well.
//
// under is false when the relationship has no active acceptance: that is the caller's own refusal, not
// this one, so the two are told apart. A store that predates the acceptance zone answers false.
func UnderCorrection(ctx context.Context, q store.Querier, relationshipID string) (under bool, live, stand int64, err error) {
	active, live, found, err := ActiveWithLiveGeneration(ctx, q, relationshipID)
	if err != nil || !found {
		return false, live, 0, err
	}
	reading, err := StandOf(ctx, q, active.AcceptanceID, relationshipID, active.Generation, active.EventID, active.RevisionHash, active.HeadSHA)
	if err != nil {
		return false, live, 0, err
	}
	return live > reading.Generation, live, reading.Generation, nil
}

// RulingHead is the head a ruling fixed for an event: dag_verified_heads.head_sha for that event
// (CRW-742), or the acceptance's own head when the event has no row, with source "acceptance".
func RulingHead(ctx context.Context, q store.Querier, eventID, acceptanceHead string) (head, source string, err error) {
	present, err := tableExists(ctx, q, "dag_verified_heads")
	if err != nil || !present {
		return acceptanceHead, "acceptance", err
	}
	var sha string
	err = q.QueryRowContext(ctx, "SELECT head_sha FROM dag_verified_heads WHERE event_id = ?", eventID).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return acceptanceHead, "acceptance", nil
	}
	if err != nil {
		return "", "", err
	}
	return sha, "verified_heads", nil
}
