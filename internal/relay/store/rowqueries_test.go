package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Typed row queries the product no longer carries (it reads and writes these tables with its
// own SQL in the domain packages). The store tests keep them to seed rows and read back what a
// live writer stored; decision 51.

// EditRegion is editregion.py:536.
func (s *Store) EditRegion(ctx context.Context, regionID string) (EditRegionsRow, error) {
	return queryRow(ctx, s, scanEditRegions, "SELECT "+editRegionsColumns+" FROM edit_regions WHERE region_id = ?", regionID)
}

// EditAgreement is editregion.py:190 EditRegions.agreement.
func (s *Store) EditAgreement(ctx context.Context, agreementID string) (EditAgreementsRow, error) {
	return queryRow(ctx, s, scanEditAgreements, "SELECT "+editAgreementsColumns+" FROM edit_agreements WHERE agreement_id = ?", agreementID)
}

// LiveEditAgreement is editregion.py:569: the pair's live agreement on one region (a replay).
func (s *Store) LiveEditAgreement(ctx context.Context, regionID, left, right string) (EditAgreementsRow, error) {
	return queryRow(ctx, s, scanEditAgreements, "SELECT "+editAgreementsColumns+" FROM edit_agreements"+
		"  WHERE region_id = ? AND left_project = ? AND right_project = ?"+
		"    AND state IN ('proposed','agreed','reopened') AND superseded_by IS NULL", regionID, left, right)
}

// RegionAgreement is one row of editregion.py:208 EditRegions.show: an agreement with its place.
type RegionAgreement struct {
	EditAgreementsRow
	RegionPath     string
	RegionKind     string
	RegionKey      string
	RegionClass    string
	RegenerateFrom sql.NullString
}

// RegionAgreements is editregion.py:208, in proposal order.
func (s *Store) RegionAgreements(ctx context.Context, repository string) ([]RegionAgreement, error) {
	return queryRows(ctx, s, func(row scanner) (RegionAgreement, error) {
		var r RegionAgreement
		a := &r.EditAgreementsRow
		return r, row.Scan(&a.AgreementID, &a.RegionID, &a.Repository, &a.BaseRevision, &a.LeftProject,
			&a.RightProject, &a.PeerLinkID, &a.ProposerTaskID, &a.IssueKey, &a.ConstraintText,
			&a.LeftCondition, &a.RightCondition, &a.LeftAcceptedAt, &a.RightAcceptedAt, &a.NextOwner,
			&a.State, &a.Tenure, &a.Supersedes, &a.SupersededBy, &a.CloseReason, &a.ProposedAt,
			&a.UpdatedAt, &a.ClosedAt, &r.RegionPath, &r.RegionKind, &r.RegionKey, &r.RegionClass,
			&r.RegenerateFrom)
	}, "SELECT "+prefixed("a.", editAgreementsColumns)+", r.path AS region_path, r.region_kind AS region_kind,"+
		"       r.region_key AS region_key, r.region_class AS region_class,"+
		"       r.regenerate_from AS regenerate_from"+
		"  FROM edit_agreements a JOIN edit_regions r ON r.region_id = a.region_id"+
		" WHERE a.repository = ? ORDER BY a.proposed_at, a.agreement_id", repository)
}

// CloseEditAgreement is editregion.py:1022 (withdrawn or released).
func (s *Store) CloseEditAgreement(ctx context.Context, agreementID, state, reason, at string) error {
	_, err := s.exec(ctx, "UPDATE edit_agreements SET state = ?, close_reason = ?, closed_at = ?,"+
		" updated_at = ? WHERE agreement_id = ?", state, reason, at, at, agreementID)
	return err
}

// EditFollowup is editregion.py:1385.
func (s *Store) EditFollowup(ctx context.Context, followupID string) (EditFollowupsRow, error) {
	return queryRow(ctx, s, scanEditFollowups, "SELECT "+editFollowupsColumns+" FROM edit_followups WHERE followup_id = ?", followupID)
}

// EditFollowups is editregion.py:251.
func (s *Store) EditFollowups(ctx context.Context, agreementID string) ([]EditFollowupsRow, error) {
	return queryRows(ctx, s, scanEditFollowups, "SELECT "+editFollowupsColumns+" FROM edit_followups WHERE agreement_id = ?"+
		" ORDER BY recorded_at, followup_id", agreementID)
}

// EditRevisionMark is editregion.py:201: the mark leaving a revision, if it was superseded.
func (s *Store) EditRevisionMark(ctx context.Context, repository, fromRevision string) (EditRevisionMarksRow, error) {
	return queryRow(ctx, s, scanEditRevisionMarks, "SELECT "+editRevisionMarksColumns+" FROM edit_revision_marks"+
		"  WHERE repository = ? AND from_revision = ?", repository, fromRevision)
}

// EditReaffirmation is editregion.py:310.
func (s *Store) EditReaffirmation(ctx context.Context, agreementID string) (EditReaffirmationsRow, error) {
	return queryRow(ctx, s, scanEditReaffirmations, "SELECT "+editReaffirmationsColumns+" FROM edit_reaffirmations WHERE agreement_id = ?", agreementID)
}

// prefixed qualifies every column of a column list with a table alias.
func prefixed(alias, columns string) string {
	return alias + strings.ReplaceAll(columns, ", ", ", "+alias)
}

// InsertScopeBinding is linkage.py:565 _insert_binding; superseded_by is always NULL.
func (s *Store) InsertScopeBinding(ctx context.Context, b ScopeBindingsRow) error {
	_, err := s.exec(ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"+
		" host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"+
		" handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)",
		b.BindingID, b.Role, b.ScopeKind, b.ScopeKey, b.TaskID, b.HostID, b.CWD, b.CXCSession,
		b.Status, b.Revision, b.Supersedes, b.HandoverNote, b.CreatedAt, b.UpdatedAt)
	return err
}

// ScopeOwner is linkage.py:226 Linkage.owner: the newest live binding of a scope.
func (s *Store) ScopeOwner(ctx context.Context, scopeKind, scopeKey string) (ScopeBindingsRow, error) {
	return queryRow(ctx, s, scanScopeBindings, "SELECT "+scopeBindingsColumns+" FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL"+
		"  ORDER BY revision DESC LIMIT 1", scopeKind, scopeKey)
}

// LiveOwnerTasks is linkage.py:262 _live_owners_in: the writer's view of a scope's owners.
func (s *Store) LiveOwnerTasks(ctx context.Context, scopeKind, scopeKey, role string) ([]string, error) {
	return queryRows(ctx, s, scanString, "SELECT task_id FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY task_id", scopeKind, scopeKey, role)
}

// ArchiveScopeBinding is linkage.py:2345, the outgoing half of a handover.
func (s *Store) ArchiveScopeBinding(ctx context.Context, bindingID, archived, supersededBy, at string) error {
	_, err := s.exec(ctx, "UPDATE scope_bindings SET status = ?, superseded_by = ?, updated_at = ?"+
		"  WHERE binding_id = ?", archived, supersededBy, at, bindingID)
	return err
}

// InsertScopeLink is linkage.py:812 _insert_link; superseded_by is always NULL.
func (s *Store) InsertScopeLink(ctx context.Context, l ScopeLinksRow) error {
	_, err := s.exec(ctx, "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"+
		" upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"+
		" superseded_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
		l.LinkID, l.LinkKind, l.UpperKind, l.UpperKey, l.UpperTaskID, l.LowerKind, l.LowerKey,
		l.LowerTaskID, l.Status, l.Revision, l.CreatedAt, l.UpdatedAt)
	return err
}

// ExecutionLinksBelow is linkage.py:1992: the live execution edges out of a scope.
func (s *Store) ExecutionLinksBelow(ctx context.Context, upperKind, upperKey string) ([]ScopeLinksRow, error) {
	return queryRows(ctx, s, scanScopeLinks, "SELECT "+scopeLinksColumns+" FROM scope_links"+
		"  WHERE upper_kind = ? AND upper_key = ? AND link_kind = 'execution'"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY lower_key", upperKind, upperKey)
}

// RecordLinkageConflict is linkage.py:587 _record_conflict_in: a retried loser converges.
func (s *Store) RecordLinkageConflict(ctx context.Context, c LinkageConflictsRow) error {
	_, err := s.exec(ctx, "INSERT INTO linkage_conflicts (at, scope_kind, scope_key, reason, incumbent,"+
		" challenger, detail) VALUES (?,?,?,?,?,?,?)"+
		" ON CONFLICT(scope_kind, scope_key, reason, incumbent, challenger)"+
		"   DO UPDATE SET at = excluded.at, detail = excluded.detail",
		c.At, c.ScopeKind, c.ScopeKey, c.Reason, c.Incumbent, c.Challenger, c.Detail)
	return err
}

// RecordRelationshipScope is linkage.py:1061; the first project recorded for a relationship stays.
func (s *Store) RecordRelationshipScope(ctx context.Context, relationshipID, projectKey, at string) error {
	_, err := s.exec(ctx, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"+
		" VALUES (?,?,?) ON CONFLICT(relationship_id) DO NOTHING", relationshipID, projectKey, at)
	return err
}

// RecordCoordinationConflict is coordination.py:108 Conflicts.record_in.
func (s *Store) RecordCoordinationConflict(ctx context.Context, c CoordinationConflictsRow) error {
	_, err := s.exec(ctx, "INSERT INTO coordination_conflicts (at, domain, subject, reason, incumbent,"+
		" challenger, detail) VALUES (?,?,?,?,?,?,?)"+
		" ON CONFLICT (domain, subject, reason, incumbent, challenger) DO UPDATE SET"+
		" at = excluded.at, detail = excluded.detail",
		c.At, c.Domain, c.Subject, c.Reason, c.Incumbent, c.Challenger, c.Detail)
	return err
}

// CoordinationConflicts is coordination.py:125 Conflicts.all.
func (s *Store) CoordinationConflicts(ctx context.Context, domain, subject string) ([]CoordinationConflictsRow, error) {
	return queryRows(ctx, s, scanCoordinationConflicts, "SELECT "+coordinationConflictsColumns+" FROM coordination_conflicts"+
		"  WHERE domain = ? AND subject = ? ORDER BY id", domain, subject)
}

func (s *Store) Refusals(ctx context.Context, relationshipID string) ([]Refusal, error) {
	rows, err := s.q(ctx).QueryContext(ctx, `SELECT id,at,relationship_id,event_id,reason,detail,payload FROM refusals WHERE relationship_id=? ORDER BY id`, relationshipID)
	if err != nil {
		return nil, fmt.Errorf("refusals %q: %w", relationshipID, err)
	}
	defer rows.Close()
	var result []Refusal
	for rows.Next() {
		var r Refusal
		if err := rows.Scan(&r.ID, &r.At, &r.RelationshipID, &r.EventID, &r.Reason, &r.Detail, &r.Payload); err != nil {
			return nil, fmt.Errorf("refusal scan: %w", err)
		}
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("refusals rows: %w", err)
	}
	return result, nil
}

type Ack struct {
	EventID         string
	Record          string
	TurnID          string
	Accepted        bool
	Verified        string
	RejectionReason sql.NullString
	At              string
}

func (s *Store) Ack(ctx context.Context, eventID string) (Ack, error) {
	var row Ack
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id,record,ack_turn_id,accepted,verified,rejection_reason,ack_at FROM acks WHERE event_id=?`, eventID).Scan(&row.EventID, &row.Record, &row.TurnID, &row.Accepted, &row.Verified, &row.RejectionReason, &row.At)
	if err != nil {
		return Ack{}, fmt.Errorf("ack %q: %w", eventID, err)
	}
	return row, nil
}

type Verdict struct {
	EventID        string
	Record         string
	Decision       string
	NextGeneration sql.NullInt64
	TurnID         string
	DecidedAt      string
}

func (s *Store) Verdict(ctx context.Context, eventID string) (Verdict, error) {
	var row Verdict
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id,record,verdict,next_generation,verdict_turn_id,decided_at FROM verdicts WHERE event_id=?`, eventID).Scan(&row.EventID, &row.Record, &row.Decision, &row.NextGeneration, &row.TurnID, &row.DecidedAt)
	if err != nil {
		return Verdict{}, fmt.Errorf("verdict %q: %w", eventID, err)
	}
	return row, nil
}

// RecordRelationship creates the assignment and its first generation as one fact.
// The caller supplies Python-compatible identifiers and serialized JSON fields.
func (s *Store) RecordRelationship(ctx context.Context, relationship Relationship, generation Generation, parentHostID, childHostID string) error {
	if err := validatedTurnID(generation.DispatchTurnID); err != nil {
		return err
	}
	if generation.AnchorState == AnchorBound && !generation.DispatchTurnID.Valid {
		return refuse(ReasonUnboundGeneration, "a bound generation needs its dispatch turn id")
	}
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `INSERT INTO relationships (relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, relationship.ID, relationship.IssueKey, relationship.Status, relationship.ParentTaskID, parentHostID, relationship.ChildTaskID, childHostID, relationship.Generation, relationship.ArtifactRoots, relationship.AllowedRecipients, relationship.CreatedAt, relationship.UpdatedAt); err != nil {
			return fmt.Errorf("insert relationship: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at,bound_at) VALUES (?,?,?,?,?,?,?)`, generation.RelationshipID, generation.Number, generation.DispatchRequestID, generation.AnchorState, generation.DispatchTurnID, generation.OpenedAt, generation.BoundAt); err != nil {
			return fmt.Errorf("insert generation: %w", err)
		}
		return nil
	})
}

type Event struct {
	ID             string
	RelationshipID string
	Generation     int64
	RevisionHash   string
	Outcome        string
	Producer       string
	TurnThreadID   string
	TurnID         string
	TurnStatus     string
	Receipt        string
	PathBinding    sql.NullString
	Stage          string
}

func (s *Store) Event(ctx context.Context, id string) (Event, error) {
	var row Event
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id, relationship_id, execution_generation, revision_hash,
 outcome, producer, turn_thread_id, turn_id, turn_status, receipt, path_binding_mode, stage
 FROM events WHERE event_id=?`, id).Scan(&row.ID, &row.RelationshipID, &row.Generation,
		&row.RevisionHash, &row.Outcome, &row.Producer, &row.TurnThreadID,
		&row.TurnID, &row.TurnStatus, &row.Receipt, &row.PathBinding, &row.Stage)
	if err != nil {
		return Event{}, fmt.Errorf("event %q: %w", id, err)
	}
	return row, nil
}

func (s *Store) AppendJournal(ctx context.Context, entry JournalEntry) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO journal (at, kind, subject, detail)
   VALUES (?,?,?,?)`, entry.At, entry.Kind, entry.Subject, entry.Detail)
		return err
	})
}

const AnchorPending = "anchor_pending"

const ReasonNeedsChanges = "needs_changes_revision"

// RegistryGeneration is registry.generation: the invariant is checked before the row is read.
func (s *Store) RegistryGeneration(ctx context.Context, id string, number int64) (Generation, error) {
	if _, err := s.CurrentRelationship(ctx, id); err != nil {
		return Generation{}, err
	}
	return s.Generation(ctx, id, number)
}

// SetStatus is deactivation only (registry.set_status). Reactivation must go through resume,
// which restates the generation and scope; a dead assignment is never brought back here.
func (s *Store) SetStatus(ctx context.Context, id, status, at string) error {
	if !isDeactivation(status) {
		return refuse(ReasonRelationshipNotActive, "bad status %q", status)
	}
	if _, err := s.CurrentRelationship(ctx, id); err != nil {
		return err
	}
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var before string
		if err := conn.QueryRowContext(ctx, `SELECT status FROM relationships WHERE relationship_id=?`, id).Scan(&before); err != nil {
			return fmt.Errorf("status before write: %w", err)
		}
		if !isLive(before) && isLive(status) {
			return refuse(ReasonRelationshipNotActive, "%q is %q, so %q would bring it back to life", id, before, status)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE relationships SET status=?,updated_at=? WHERE relationship_id=?`, status, at, id); err != nil {
			return fmt.Errorf("write status: %w", err)
		}
		return journal(ctx, conn, "status_changed", id, `{"status": `+quoteJSON(status)+`}`, at)
	})
}

// OpenGeneration opens the next generation of an active relationship (registry.open_generation).
// A replay of the same dispatch request id returns its generation instead of opening another.
func (s *Store) OpenGeneration(ctx context.Context, id, dispatchRequest, reason string, dispatchTurn sql.NullString, at string) (int64, error) {
	if reason != ReasonInitial && reason != ReasonNeedsChanges {
		return 0, refuse(ReasonUnknownGeneration, "bad reason %q", reason)
	}
	if err := validatedTurnID(dispatchTurn); err != nil {
		return 0, err
	}
	var number int64
	err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		var status string
		var current int64
		var supersededBy sql.NullString
		err := conn.QueryRowContext(ctx, `SELECT execution_generation,status,superseded_by FROM relationships WHERE relationship_id=?`, id).Scan(&current, &status, &supersededBy)
		if errors.Is(err, sql.ErrNoRows) {
			return refuse(ReasonUnregisteredRelationship, "no relationship %q", id)
		}
		if err != nil {
			return fmt.Errorf("current generation: %w", err)
		}
		if status != StatusActive || supersededBy.Valid && supersededBy.String != "" {
			return refuse(ReasonRelationshipNotActive, "relationship %q is not active", id)
		}
		// Refused before the replay lookup, as Python's require_active: a replay is not a way
		// to read a generation back out of a relationship that is no longer active.
		replay := conn.QueryRowContext(ctx, `SELECT execution_generation FROM generations WHERE relationship_id=? AND dispatch_request_id=?`, id, dispatchRequest).Scan(&number)
		if replay == nil {
			return nil
		}
		if !errors.Is(replay, sql.ErrNoRows) {
			return fmt.Errorf("generation replay: %w", replay)
		}
		number = current + 1
		anchor, bound := AnchorPending, sql.NullString{}
		if dispatchTurn.Valid {
			anchor, bound = AnchorBound, sql.NullString{String: at, Valid: true}
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO generations (relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES (?,?,?,?,?,?,?,?)`, id, number, dispatchRequest, anchor, dispatchTurn, reason, at, bound); err != nil {
			return fmt.Errorf("insert generation: %w", err)
		}
		if _, err := conn.ExecContext(ctx, `UPDATE relationships SET execution_generation=?,updated_at=? WHERE relationship_id=?`, number, at, id); err != nil {
			return fmt.Errorf("advance generation: %w", err)
		}
		if err := journal(ctx, conn, "generation_opened", id, fmt.Sprintf(`{"generation": %d, "reason": %s}`, number, quoteJSON(reason)), at); err != nil {
			return err
		}
		// Deliveries left behind by the advance are annotated, never rewritten (registry.py
		// _supersede_older_deliveries_in).
		_, err = conn.ExecContext(ctx, `INSERT INTO delivery_supersession (event_id,reason,noted_at,applied) SELECT d.event_id,'stale_generation',?,0 FROM deliveries d JOIN events e ON e.event_id=d.event_id WHERE d.relationship_id=? AND e.execution_generation<? AND e.outcome NOT IN ('merge_turn_grant') AND d.state IN ('queued','sending','held_uncertain','dispatched','deferred_busy','withheld_pre_send','inbox_only') ON CONFLICT(event_id) DO NOTHING`, at, id, number)
		if err != nil {
			return fmt.Errorf("annotate superseded deliveries: %w", err)
		}
		return nil
	})
	return number, err
}

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
	scope := pyjson.Object{{Key: "artifactRoots", Value: roots}, {Key: "allowedRecipients", Value: recipients}}
	if r.scopeRef.Valid {
		scope = append(scope, pyjson.Field{Key: "scopeRef", Value: r.scopeRef.String})
	}
	record := pyjson.Object{
		{Key: "relationshipId", Value: id},
		{Key: "parent", Value: endpoint(r.parent, r.parentHost, r.parentCwd)},
		{Key: "child", Value: endpoint(r.child, r.childHost, r.childCwd)},
		{Key: "issueKey", Value: r.issue},
		{Key: "status", Value: r.status},
		{Key: "createdAt", Value: r.created},
		{Key: "executionGeneration", Value: r.generation},
		{Key: "generations", Value: generations},
		{Key: "authorizedScope", Value: scope},
	}
	if r.supersedes.Valid {
		record = append(record, pyjson.Field{Key: "supersedes", Value: r.supersedes.String})
	}
	encoded, err := pyjson.Encode(record, receiptRecord)
	return string(encoded), err
}

func (s *Store) generationRecords(ctx context.Context, id string) (_ []any, err error) {
	rows, err := s.q(ctx).QueryContext(ctx, `SELECT execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,opened_at,bound_at,reason FROM generations WHERE relationship_id=? ORDER BY execution_generation`, id)
	if err != nil {
		return nil, fmt.Errorf("generations of %q: %w", id, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	generations := []any{}
	for rows.Next() {
		var number int64
		var request, anchor, opened string
		var turn, bound, reason sql.NullString
		if err := rows.Scan(&number, &request, &anchor, &turn, &opened, &bound, &reason); err != nil {
			return nil, fmt.Errorf("generation row: %w", err)
		}
		generations = append(generations, pyjson.Object{
			{Key: "executionGeneration", Value: number},
			{Key: "dispatchRequestId", Value: request},
			{Key: "anchorState", Value: anchor},
			{Key: "dispatchTurnId", Value: jNullable(turn)},
			{Key: "openedAt", Value: opened},
			{Key: "boundAt", Value: jNullable(bound)},
			{Key: "reason", Value: jNullable(reason)},
		})
	}
	return generations, rows.Err()
}

func endpoint(task, host string, cwd sql.NullString) pyjson.Object {
	return pyjson.Object{{Key: "taskId", Value: task}, {Key: "hostId", Value: host}, {Key: "cwd", Value: jNullable(cwd)}}
}

func jNullable(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

// SupervisorMessageFor is supervisorchannel.py:1457: the message staged for one obligation.
func (s *Store) SupervisorMessageFor(ctx context.Context, obligationKind, obligationID string) (SupervisorMessagesRow, error) {
	return queryRow(ctx, s, scanSupervisorMessages, "SELECT "+supervisorMessagesColumns+" FROM supervisor_messages WHERE obligation_kind = ? AND obligation_id = ?", obligationKind, obligationID)
}

// ClaimableSupervisorMessages is supervisorchannel.py:1927: eligible rows in staging order.
func (s *Store) ClaimableSupervisorMessages(ctx context.Context, now float64, limit int) ([]SupervisorMessagesRow, error) {
	return queryRows(ctx, s, scanSupervisorMessages, "SELECT "+supervisorMessagesColumns+" FROM supervisor_messages"+
		" WHERE "+SupervisorClaimableSQL("")+
		" ORDER BY staged_at, message_id LIMIT ?", now, limit)
}

// InsertSupervisorAttempt is supervisorchannel.py:2518: retry_safe 0 and no turn until settled.
func (s *Store) InsertSupervisorAttempt(ctx context.Context, a SupervisorAttemptsRow) error {
	_, err := s.exec(ctx, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message,"+
		" state, send_attempted, retry_safe, turn_id, record, sent_at, observed_at,"+
		" delivery_token) VALUES (?,?,?,?,?,?,0,NULL,?,?,?,?)",
		a.RequestID, a.MessageID, a.AttemptNo, a.Message, a.State, a.SendAttempted, a.Record,
		a.SentAt, a.ObservedAt, a.DeliveryToken)
	return err
}

// SupervisorAttemptMessage is supervisorchannel.py:2487: which message a request id belongs to.
func (s *Store) SupervisorAttemptMessage(ctx context.Context, requestID string) (string, error) {
	return queryRow(ctx, s, scanString, "SELECT message_id FROM supervisor_attempts WHERE request_id = ?", requestID)
}

// SyncJob is sync.py:467 SyncOutbox.get.
func (s *Store) SyncJob(ctx context.Context, syncID string) (SyncOutboxRow, error) {
	return queryRow(ctx, s, scanSyncOutbox, "SELECT "+syncOutboxColumns+" FROM sync_outbox WHERE sync_id = ?", syncID)
}

// ProductRegistry is routing.py:113 ProductRouter.registry (with the key columns).
func (s *Store) ProductRegistry(ctx context.Context, productKey string) (ProductRegistryRow, error) {
	return queryRow(ctx, s, scanProductRegistry, "SELECT "+productRegistryColumns+" FROM product_registry WHERE product_key = ?", productKey)
}

// RoutingPolicy is routing.py:188 (with the key columns).
func (s *Store) RoutingPolicy(ctx context.Context, policyKey string) (RoutingPolicyRow, error) {
	return queryRow(ctx, s, scanRoutingPolicy, "SELECT "+routingPolicyColumns+" FROM routing_policy WHERE policy_key = ?", policyKey)
}

// IncidentRoute is routes.py:50 get.
func (s *Store) IncidentRoute(ctx context.Context, faultID string) (IncidentRoutesRow, error) {
	return queryRow(ctx, s, scanIncidentRoutes, "SELECT "+incidentRoutesColumns+" FROM incident_routes WHERE fault_id = ?", faultID)
}

// CoordinationConflictsRow is one coordination_conflicts row, every column in DDL order.
type CoordinationConflictsRow struct {
	ID         int64
	At         string
	Domain     string
	Subject    string
	Reason     string
	Incumbent  string
	Challenger string
	Detail     sql.NullString
}

const coordinationConflictsColumns = "id, at, domain, subject, reason, incumbent, challenger, detail"

func scanCoordinationConflicts(row scanner) (CoordinationConflictsRow, error) {
	var r CoordinationConflictsRow
	err := row.Scan(&r.ID, &r.At, &r.Domain, &r.Subject, &r.Reason, &r.Incumbent, &r.Challenger, &r.Detail)
	return r, err
}

const editAgreementsColumns = "agreement_id, region_id, repository, base_revision, left_project, right_project, peer_link_id, proposer_task_id, issue_key, constraint_text, left_condition, right_condition, left_accepted_at, right_accepted_at, next_owner, state, tenure, supersedes, superseded_by, close_reason, proposed_at, updated_at, closed_at"

func scanEditAgreements(row scanner) (EditAgreementsRow, error) {
	var r EditAgreementsRow
	err := row.Scan(&r.AgreementID, &r.RegionID, &r.Repository, &r.BaseRevision, &r.LeftProject, &r.RightProject, &r.PeerLinkID, &r.ProposerTaskID, &r.IssueKey, &r.ConstraintText, &r.LeftCondition, &r.RightCondition, &r.LeftAcceptedAt, &r.RightAcceptedAt, &r.NextOwner, &r.State, &r.Tenure, &r.Supersedes, &r.SupersededBy, &r.CloseReason, &r.ProposedAt, &r.UpdatedAt, &r.ClosedAt)
	return r, err
}

const editFollowupsColumns = "followup_id, agreement_id, trigger_text, acceptance_text, issue_ref, assignee_task_id, assignee_project, accepted_at, state, close_reason, recorded_by, recorded_at, updated_at"

func scanEditFollowups(row scanner) (EditFollowupsRow, error) {
	var r EditFollowupsRow
	err := row.Scan(&r.FollowupID, &r.AgreementID, &r.TriggerText, &r.AcceptanceText, &r.IssueRef, &r.AssigneeTaskID, &r.AssigneeProject, &r.AcceptedAt, &r.State, &r.CloseReason, &r.RecordedBy, &r.RecordedAt, &r.UpdatedAt)
	return r, err
}

const editReaffirmationsColumns = "agreement_id, predecessor_id, actor, actor_project, from_revision, to_revision, constraint_revision, left_condition_revision, right_condition_revision, recorded_at"

func scanEditReaffirmations(row scanner) (EditReaffirmationsRow, error) {
	var r EditReaffirmationsRow
	err := row.Scan(&r.AgreementID, &r.PredecessorID, &r.Actor, &r.ActorProject, &r.FromRevision, &r.ToRevision, &r.ConstraintRevision, &r.LeftConditionRevision, &r.RightConditionRevision, &r.RecordedAt)
	return r, err
}

const editRegionsColumns = "region_id, repository, base_revision, path, region_kind, region_key, region_class, regenerate_from, recorded_at"

func scanEditRegions(row scanner) (EditRegionsRow, error) {
	var r EditRegionsRow
	err := row.Scan(&r.RegionID, &r.Repository, &r.BaseRevision, &r.Path, &r.RegionKind, &r.RegionKey, &r.RegionClass, &r.RegenerateFrom, &r.RecordedAt)
	return r, err
}

const editRevisionMarksColumns = "mark_id, repository, from_revision, to_revision, actor, recorded_at"

func scanEditRevisionMarks(row scanner) (EditRevisionMarksRow, error) {
	var r EditRevisionMarksRow
	err := row.Scan(&r.MarkID, &r.Repository, &r.FromRevision, &r.ToRevision, &r.Actor, &r.RecordedAt)
	return r, err
}

const incidentRoutesColumns = "fault_id, product_key, workspace, disposition, stage, target, origin, claimed_severity, goal, classification, superseded_by, reported, detail, checked_seq, created_at, updated_at"

func scanIncidentRoutes(row scanner) (IncidentRoutesRow, error) {
	var r IncidentRoutesRow
	err := row.Scan(&r.FaultID, &r.ProductKey, &r.Workspace, &r.Disposition, &r.Stage, &r.Target, &r.Origin, &r.ClaimedSeverity, &r.Goal, &r.Classification, &r.SupersededBy, &r.Reported, &r.Detail, &r.CheckedSeq, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

// RoutingPolicyRow is one routing_policy row, every column in DDL order.
type RoutingPolicyRow struct {
	PolicyKey  string
	Record     string
	Basis      string
	RecordedAt string
}

const routingPolicyColumns = "policy_key, record, basis, recorded_at"

func scanRoutingPolicy(row scanner) (RoutingPolicyRow, error) {
	var r RoutingPolicyRow
	err := row.Scan(&r.PolicyKey, &r.Record, &r.Basis, &r.RecordedAt)
	return r, err
}

const ReasonInitial = "initial_assignment"

func isDeactivation(status string) bool {
	return status == "paused" || status == "cancelled" || status == "archived"
}

func isLive(status string) bool { return status == StatusActive || status == "paused" }

// validatedTurnID is registry.validated_turn_id: absent, or an exact non-blank turn id.
func validatedTurnID(turn sql.NullString) error {
	if turn.Valid && strings.TrimSpace(turn.String) == "" {
		return refuse(ReasonUnboundGeneration, "an anchor needs an exact dispatch turn id, not %q", turn.String)
	}
	return nil
}
