package store

import (
	"context"
	"database/sql"
)

// Managed start and declaration tables: managed_start_requests, reporting_sessions,
// turn_declarations.

// ReserveManagedStart is registry.py:1126: a request starts reserved at revision 0.
func (s *Store) ReserveManagedStart(ctx context.Context, m ManagedStartRequestsRow) error {
	_, err := s.exec(ctx, "INSERT INTO managed_start_requests (request_id, issue_key,"+
		" request_fingerprint, fingerprint_version, workspace, marker_root,"+
		" socket_identity, create_request_id, dispatch_request_id, state, revision,"+
		" child_task_id, standby_turn_id, relationship_id, execution_generation,"+
		" receipt_status, release_reason, created_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,'reserved',0,NULL,NULL,NULL,NULL,NULL,NULL,?,?)",
		m.RequestID, m.IssueKey, m.RequestFingerprint, m.FingerprintVersion, m.Workspace, m.MarkerRoot,
		m.SocketIdentity, m.CreateRequestID, m.DispatchRequestID, m.CreatedAt, m.UpdatedAt)
	return err
}

// ManagedStartRequest is registry.py:1292 Registry.start_request.
func (s *Store) ManagedStartRequest(ctx context.Context, requestID string) (ManagedStartRequestsRow, error) {
	return queryRow(ctx, s, scanManagedStartRequests, "SELECT "+managedStartRequestsColumns+" FROM managed_start_requests WHERE request_id = ?", requestID)
}

// PendingManagedStart is registry.py:1356: the issue's reserved or armed request, if any.
func (s *Store) PendingManagedStart(ctx context.Context, issueKey string) (ManagedStartRequestsRow, error) {
	return queryRow(ctx, s, scanManagedStartRequests, "SELECT "+managedStartRequestsColumns+" FROM managed_start_requests WHERE issue_key = ?"+
		" AND state IN ('reserved','create_armed')", issueKey)
}

// ArmManagedStart is registry.py:1169; it reports whether the expected revision was current.
func (s *Store) ArmManagedStart(ctx context.Context, requestID string, expectedRevision int64, at string) (bool, error) {
	return s.changedOne(ctx, "UPDATE managed_start_requests SET state = 'create_armed', revision = revision + 1,"+
		" updated_at = ? WHERE request_id = ? AND state = 'reserved' AND revision = ?", at, requestID, expectedRevision)
}

// RecordManagedStartReceipt is registry.py:1276, on an armed request only.
func (s *Store) RecordManagedStartReceipt(ctx context.Context, requestID, status string, childTaskID, standbyTurnID sql.NullString, at string) error {
	_, err := s.exec(ctx, "UPDATE managed_start_requests SET receipt_status = ?, child_task_id = ?,"+
		" standby_turn_id = ?, updated_at = ? WHERE request_id = ? AND state = 'create_armed'",
		status, childTaskID, standbyTurnID, at, requestID)
	return err
}

// ReleaseManagedStart is registry.py:1318; only a reserved row at the expected revision moves.
func (s *Store) ReleaseManagedStart(ctx context.Context, requestID string, expectedRevision int64, reason, at string) (bool, error) {
	return s.changedOne(ctx, "UPDATE managed_start_requests SET state = 'released', revision = revision + 1,"+
		" release_reason = ?, updated_at = ?"+
		" WHERE request_id = ? AND state = 'reserved' AND revision = ?", reason, at, requestID, expectedRevision)
}

// AttachManagedStart is registry.py:1421; only an accepted, unattached armed request moves.
func (s *Store) AttachManagedStart(ctx context.Context, requestID, relationshipID string, generation int64, at string) (bool, error) {
	return s.changedOne(ctx, "UPDATE managed_start_requests SET state = 'attached', revision = revision + 1,"+
		" relationship_id = ?, execution_generation = ?, updated_at = ?"+
		" WHERE request_id = ? AND state = 'create_armed'"+
		" AND receipt_status = 'accepted' AND relationship_id IS NULL", relationshipID, generation, at, requestID)
}

func (s *Store) changedOne(ctx context.Context, query string, args ...any) (bool, error) {
	result, err := s.exec(ctx, query, args...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// RecordReportingSession is declarations.py:199 record_claim.
func (s *Store) RecordReportingSession(ctx context.Context, r ReportingSessionsRow) error {
	_, err := s.exec(ctx, "INSERT INTO reporting_sessions (assignment_id, session_id, dispatch_request_id,"+
		" marker_root, workspace, issue_key, capability, recorded_at)"+
		" VALUES (?,?,?,?,?,?,?,?)",
		r.AssignmentID, r.SessionID, r.DispatchRequestID, r.MarkerRoot, r.Workspace, r.IssueKey, r.Capability, r.RecordedAt)
	return err
}

// ReportingSession is omitted.py:570.
func (s *Store) ReportingSession(ctx context.Context, assignmentID, sessionID string) (ReportingSessionsRow, error) {
	return queryRow(ctx, s, scanReportingSessions, "SELECT "+reportingSessionsColumns+" FROM reporting_sessions WHERE assignment_id=? AND session_id=?", assignmentID, sessionID)
}

// RecordTurnDeclaration is declarations.py:175.
func (s *Store) RecordTurnDeclaration(ctx context.Context, d TurnDeclarationsRow) error {
	_, err := s.exec(ctx, "INSERT INTO turn_declarations (assignment_id, session_id, turn_id, outcome,"+
		" declared_at, recorded_at) VALUES (?,?,?,?,?,?)",
		d.AssignmentID, d.SessionID, d.TurnID, d.Outcome, d.DeclaredAt, d.RecordedAt)
	return err
}

// TurnDeclaration is omitted.py:599 (with the key columns).
func (s *Store) TurnDeclaration(ctx context.Context, assignmentID, sessionID, turnID string) (TurnDeclarationsRow, error) {
	return queryRow(ctx, s, scanTurnDeclarations, "SELECT "+turnDeclarationsColumns+" FROM turn_declarations WHERE assignment_id=? AND session_id=? AND turn_id=?", assignmentID, sessionID, turnID)
}
