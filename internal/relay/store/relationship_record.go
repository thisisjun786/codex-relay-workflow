package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

const reasonAnchorAlreadyBound = "anchor_already_bound"

// BindAnchor binds a pending generation to the exact turn a dispatch receipt reported
// (registry.bind_anchor). An anchor is never inferred from whichever turn appears next.
func (s *Store) BindAnchor(ctx context.Context, id string, number int64, turn, source, at string) error {
	if source != "dispatch_receipt" {
		return refuse(ReasonUnboundGeneration, "an anchor binds only from a dispatch receipt, not from %q", source)
	}
	if err := validatedTurnID(sql.NullString{String: turn, Valid: true}); err != nil {
		return err
	}
	current, err := s.RegistryGeneration(ctx, id, number)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse(ReasonUnknownGeneration, "%q has no generation %d", id, number)
	}
	if err != nil {
		return err
	}
	if current.AnchorState == AnchorBound {
		if current.DispatchTurnID.String == turn {
			return nil
		}
		return refuse(reasonAnchorAlreadyBound, "generation %d is already bound to %q", number, current.DispatchTurnID.String)
	}
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `UPDATE generations SET anchor_state=?,dispatch_turn_id=?,bound_at=? WHERE relationship_id=? AND execution_generation=?`, AnchorBound, turn, at, id, number); err != nil {
			return fmt.Errorf("bind anchor: %w", err)
		}
		return journal(ctx, conn, "anchor_bound", id, fmt.Sprintf(`{"generation": %d}`, number), at)
	})
}

// RelationshipRecord is the relationship as the frozen schema defines it
// (registry.contract_record), in Python's field order, with the generation invariant enforced.
func (s *Store) RelationshipRecord(ctx context.Context, id string) (string, error) {
	if _, err := s.CurrentRelationship(ctx, id); err != nil {
		return "", err
	}
	var r struct {
		issue, status, parent, parentHost, child, childHost, created, roots, recipients string
		parentCwd, childCwd, scopeRef, supersedes                                       sql.NullString
		generation                                                                      int64
	}
	err := s.q(ctx).QueryRowContext(ctx, `SELECT issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,scope_ref,supersedes,created_at FROM relationships WHERE relationship_id=?`, id).
		Scan(&r.issue, &r.status, &r.parent, &r.parentHost, &r.parentCwd, &r.child, &r.childHost, &r.childCwd, &r.generation, &r.roots, &r.recipients, &r.scopeRef, &r.supersedes, &r.created)
	if err != nil {
		return "", fmt.Errorf("relationship record %q: %w", id, err)
	}
	generations, err := s.generationRecords(ctx, id)
	if err != nil {
		return "", err
	}
	roots, err := decodeOrdered([]byte(r.roots))
	if err != nil {
		return "", fmt.Errorf("artifact roots: %w", err)
	}
	recipients, err := decodeOrdered([]byte(r.recipients))
	if err != nil {
		return "", fmt.Errorf("allowed recipients: %w", err)
	}
	scope := []jsonField{{"artifactRoots", roots}, {"allowedRecipients", recipients}}
	if r.scopeRef.Valid {
		scope = append(scope, jsonField{"scopeRef", jText(r.scopeRef.String)})
	}
	record := []jsonField{
		{"relationshipId", jText(id)},
		{"parent", endpoint(r.parent, r.parentHost, r.parentCwd)},
		{"child", endpoint(r.child, r.childHost, r.childCwd)},
		{"issueKey", jText(r.issue)},
		{"status", jText(r.status)},
		{"createdAt", jText(r.created)},
		{"executionGeneration", jInt(r.generation)},
		{"generations", jsonValue{kind: jsonArray, array: generations}},
		{"authorizedScope", jsonValue{kind: jsonObject, object: scope}},
	}
	if r.supersedes.Valid {
		record = append(record, jsonField{"supersedes", jText(r.supersedes.String)})
	}
	return pythonDumps(jsonValue{kind: jsonObject, object: record})
}

func (s *Store) generationRecords(ctx context.Context, id string) (_ []jsonValue, err error) {
	rows, err := s.q(ctx).QueryContext(ctx, `SELECT execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at,bound_at,reason FROM generations WHERE relationship_id=? ORDER BY execution_generation`, id)
	if err != nil {
		return nil, fmt.Errorf("generations of %q: %w", id, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var generations []jsonValue
	for rows.Next() {
		var number int64
		var request, anchor, opened string
		var turn, bound, reason sql.NullString
		if err := rows.Scan(&number, &request, &anchor, &turn, &opened, &bound, &reason); err != nil {
			return nil, fmt.Errorf("generation row: %w", err)
		}
		generations = append(generations, jsonValue{kind: jsonObject, object: []jsonField{
			{"executionGeneration", jInt(number)},
			{"dispatchRequestId", jText(request)},
			{"anchorState", jText(anchor)},
			{"dispatchTurnId", jNullable(turn)},
			{"openedAt", jText(opened)},
			{"boundAt", jNullable(bound)},
			{"reason", jNullable(reason)},
		}})
	}
	return generations, rows.Err()
}

func endpoint(task, host string, cwd sql.NullString) jsonValue {
	return jsonValue{kind: jsonObject, object: []jsonField{{"taskId", jText(task)}, {"hostId", jText(host)}, {"cwd", jNullable(cwd)}}}
}

func jText(text string) jsonValue { return jsonValue{kind: jsonScalar, scalar: text} }

func jInt(number int64) jsonValue {
	return jsonValue{kind: jsonScalar, scalar: json.Number(strconv.FormatInt(number, 10))}
}

func jNullable(value sql.NullString) jsonValue {
	if !value.Valid {
		return jsonValue{kind: jsonScalar}
	}
	return jText(value.String)
}
