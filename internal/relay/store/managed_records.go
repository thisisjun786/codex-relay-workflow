package store

import (
	"context"
	"database/sql"
	"errors"
)

// Managed start, criteria, settings, delivery sidecars and assignment tables:
// managed_start_requests, reporting_sessions, turn_declarations, authorized_settings,
// attempt_settings_violations, canonical_criteria, verification_mode, claim_context,
// verdict_context, assignment_marks, delivery_intent, poll_observations, recipient_rate,
// recipient_lifecycle, fault_overtaken_deliveries.

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

// RecordAuthorizedSettings is registry.py:1827: the latest settings of a task replace the prior.
func (s *Store) RecordAuthorizedSettings(ctx context.Context, a AuthorizedSettingsRow) error {
	_, err := s.exec(ctx, "INSERT INTO authorized_settings (task_id, settings, source, recorded_at)"+
		" VALUES (?,?,?,?)"+
		" ON CONFLICT(task_id) DO UPDATE SET settings = excluded.settings,"+
		"   source = excluded.source, recorded_at = excluded.recorded_at",
		a.TaskID, a.Settings, a.Source, a.RecordedAt)
	return err
}

// AuthorizedSettings reads one task's row (linkage.py:441 reads settings, registry.py:1818 source).
func (s *Store) AuthorizedSettings(ctx context.Context, taskID string) (AuthorizedSettingsRow, error) {
	return queryRow(ctx, s, scanAuthorizedSettings, "SELECT "+authorizedSettingsColumns+" FROM authorized_settings WHERE task_id = ?", taskID)
}

// RecordSettingsViolation is delivery.py:1281: a later observation replaces the findings.
func (s *Store) RecordSettingsViolation(ctx context.Context, v AttemptSettingsViolationsRow) error {
	_, err := s.exec(ctx, "INSERT INTO attempt_settings_violations (request_id, event_id, findings,"+
		" observed_at) VALUES (?,?,?,?)"+
		" ON CONFLICT(request_id) DO UPDATE SET findings = excluded.findings,"+
		"   observed_at = excluded.observed_at",
		v.RequestID, v.EventID, v.Findings, v.ObservedAt)
	return err
}

// SettingsViolation is delivery.py:1295 (with the key columns).
func (s *Store) SettingsViolation(ctx context.Context, requestID string) (AttemptSettingsViolationsRow, error) {
	return queryRow(ctx, s, scanAttemptSettingsViolations, "SELECT "+attemptSettingsViolationsColumns+" FROM attempt_settings_violations WHERE request_id = ?", requestID)
}

// ReplaceCanonicalCriteria is criteria.py:337/341 _replace_criteria: one complete set per
// relationship, written inside the caller's transaction.
func (s *Store) ReplaceCanonicalCriteria(ctx context.Context, relationshipID string, criteria []CanonicalCriteriaRow) error {
	if _, err := s.exec(ctx, "DELETE FROM canonical_criteria WHERE relationship_id = ?", relationshipID); err != nil {
		return err
	}
	for _, c := range criteria {
		if _, err := s.exec(ctx, "INSERT INTO canonical_criteria (relationship_id, criterion_id, title,"+
			" required, source_ref, set_digest, recorded_at) VALUES (?,?,?,?,?,?,?)",
			relationshipID, c.CriterionID, c.Title, c.Required, c.SourceRef, c.SetDigest, c.RecordedAt); err != nil {
			return err
		}
	}
	return nil
}

// CanonicalCriteria is criteria.py:228 CriteriaService.get.
func (s *Store) CanonicalCriteria(ctx context.Context, relationshipID string) ([]CanonicalCriteriaRow, error) {
	return queryRows(ctx, s, scanCanonicalCriteria, "SELECT "+canonicalCriteriaColumns+" FROM canonical_criteria WHERE relationship_id = ? ORDER BY criterion_id", relationshipID)
}

// WriteVerificationMode is criteria.py:356 _write_mode.
func (s *Store) WriteVerificationMode(ctx context.Context, relationshipID, mode, at string) error {
	_, err := s.exec(ctx, "INSERT INTO verification_mode (relationship_id, mode, recorded_at) VALUES (?,?,?)"+
		" ON CONFLICT(relationship_id) DO UPDATE SET mode = excluded.mode,"+
		" recorded_at = excluded.recorded_at", relationshipID, mode, at)
	return err
}

// VerificationMode is criteria.py:245 (with the key columns).
func (s *Store) VerificationMode(ctx context.Context, relationshipID string) (VerificationModeRow, error) {
	return queryRow(ctx, s, scanVerificationMode, "SELECT "+verificationModeColumns+" FROM verification_mode WHERE relationship_id = ?", relationshipID)
}

// BindClaimContext is criteria.py:374 bind_review: the first binding of a claim stays.
func (s *Store) BindClaimContext(ctx context.Context, eventID string, setDigest sql.NullString, at string) error {
	_, err := s.exec(ctx, "INSERT OR IGNORE INTO claim_context (event_id, set_digest, bound_at) VALUES (?,?,?)", eventID, setDigest, at)
	return err
}

// ClearClaimContext is ack.py:152, run before a reclaim rebinds.
func (s *Store) ClearClaimContext(ctx context.Context, eventID string) error {
	_, err := s.exec(ctx, "DELETE FROM claim_context WHERE event_id = ?", eventID)
	return err
}

// ClaimContext is criteria.py:381 bound_digest (with the key columns).
func (s *Store) ClaimContext(ctx context.Context, eventID string) (ClaimContextRow, error) {
	return queryRow(ctx, s, scanClaimContext, "SELECT "+claimContextColumns+" FROM claim_context WHERE event_id = ?", eventID)
}

// RecordVerdictContext is ack.py:717: a re-review replaces every context column.
func (s *Store) RecordVerdictContext(ctx context.Context, v VerdictContextRow) error {
	_, err := s.exec(ctx, "INSERT INTO verdict_context (event_id, set_digest, coverage, findings, reason,"+
		" currency, head_event_id, head_revision, ack_evidence, recorded_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT(event_id) DO UPDATE SET set_digest = excluded.set_digest,"+
		" coverage = excluded.coverage, findings = excluded.findings,"+
		" reason = excluded.reason, currency = excluded.currency,"+
		" head_event_id = excluded.head_event_id,"+
		" head_revision = excluded.head_revision,"+
		" ack_evidence = excluded.ack_evidence, recorded_at = excluded.recorded_at",
		v.EventID, v.SetDigest, v.Coverage, v.Findings, v.Reason, v.Currency, v.HeadEventID,
		v.HeadRevision, v.AckEvidence, v.RecordedAt)
	return err
}

// VerdictContext is ack.py:185 / assignment.py:888 (every column).
func (s *Store) VerdictContext(ctx context.Context, eventID string) (VerdictContextRow, error) {
	return queryRow(ctx, s, scanVerdictContext, "SELECT "+verdictContextColumns+" FROM verdict_context WHERE event_id = ?", eventID)
}

// RecordAssignmentMark is assignment.py:816: a restated mark updates its evidence.
func (s *Store) RecordAssignmentMark(ctx context.Context, m AssignmentMarksRow) error {
	_, err := s.exec(ctx, "INSERT INTO assignment_marks (relationship_id, mark, event_id,"+
		" execution_generation, revision_hash, evidence, actor, marked_at)"+
		" VALUES (?,?,?,?,?,?,?,?)"+
		" ON CONFLICT(relationship_id, mark, event_id) DO UPDATE SET"+
		"   evidence = excluded.evidence, actor = excluded.actor,"+
		"   marked_at = excluded.marked_at",
		m.RelationshipID, m.Mark, m.EventID, m.ExecutionGeneration, m.RevisionHash, m.Evidence, m.Actor, m.MarkedAt)
	return err
}

// AssignmentMarks is assignment.py:864.
func (s *Store) AssignmentMarks(ctx context.Context, relationshipID string) ([]AssignmentMarksRow, error) {
	return queryRows(ctx, s, scanAssignmentMarks, "SELECT "+assignmentMarksColumns+" FROM assignment_marks WHERE relationship_id = ? ORDER BY marked_at", relationshipID)
}

// NoteDeliveryIntent is delivery.py:442: a refused delivery keeps its first noted_at.
func (s *Store) NoteDeliveryIntent(ctx context.Context, i DeliveryIntentRow) error {
	_, err := s.exec(ctx, "INSERT INTO delivery_intent (event_id, relationship_id, kind, recipient_task_id,"+
		" attempts, next_retry_at, last_error, noted_at) VALUES (?,?,?,?,?,?,?,?)"+
		" ON CONFLICT(event_id) DO UPDATE SET attempts = excluded.attempts,"+
		" next_retry_at = excluded.next_retry_at, last_error = excluded.last_error",
		i.EventID, i.RelationshipID, i.Kind, i.RecipientTaskID, i.Attempts, i.NextRetryAt, i.LastError, i.NotedAt)
	return err
}

// DeliveryIntent reads one intent (delivery.py:437 reads its attempts).
func (s *Store) DeliveryIntent(ctx context.Context, eventID string) (DeliveryIntentRow, error) {
	return queryRow(ctx, s, scanDeliveryIntent, "SELECT "+deliveryIntentColumns+" FROM delivery_intent WHERE event_id = ?", eventID)
}

// ClearDeliveryIntent is delivery.py:304, once the delivery exists.
func (s *Store) ClearDeliveryIntent(ctx context.Context, eventID string) error {
	_, err := s.exec(ctx, "DELETE FROM delivery_intent WHERE event_id = ?", eventID)
	return err
}

// PendingIntents is delivery.py:476: undelivered intents due by now, soonest first.
func (s *Store) PendingIntents(ctx context.Context, now float64, limit int) ([]DeliveryIntentRow, error) {
	return queryRows(ctx, s, scanDeliveryIntent, "SELECT "+prefixed("i.", deliveryIntentColumns)+" FROM delivery_intent i"+
		"  LEFT JOIN deliveries d ON d.event_id = i.event_id"+
		" WHERE d.event_id IS NULL"+
		"   AND (i.next_retry_at IS NULL OR i.next_retry_at <= ?)"+
		" ORDER BY i.next_retry_at LIMIT ?", now, limit)
}

// RecordPoll is daemon.py:666: a failed read keeps the last successful poll time.
func (s *Store) RecordPoll(ctx context.Context, p PollObservationsRow) error {
	_, err := s.exec(ctx, "INSERT INTO poll_observations (relationship_id, execution_generation,"+
		" turn_id, last_status, last_polled_at, last_attempt_at, last_error)"+
		" VALUES (?,?,?,?,?,?,?)"+
		" ON CONFLICT(relationship_id, execution_generation, turn_id) DO UPDATE"+
		"   SET last_status = excluded.last_status,"+
		"       last_polled_at = COALESCE(excluded.last_polled_at,"+
		"                                 poll_observations.last_polled_at),"+
		"       last_attempt_at = excluded.last_attempt_at,"+
		"       last_error = excluded.last_error",
		p.RelationshipID, p.ExecutionGeneration, p.TurnID, p.LastStatus, p.LastPolledAt, p.LastAttemptAt, p.LastError)
	return err
}

// PollObservation reads one anchor's poll row (delivery.py:1695 joins it).
func (s *Store) PollObservation(ctx context.Context, relationshipID string, generation int64, turnID string) (PollObservationsRow, error) {
	return queryRow(ctx, s, scanPollObservations, "SELECT "+pollObservationsColumns+" FROM poll_observations"+
		" WHERE relationship_id = ? AND execution_generation = ? AND turn_id = ?", relationshipID, generation, turnID)
}

// CountSend is delivery.py:2329: one more send in the recipient's hourly window.
func (s *Store) CountSend(ctx context.Context, recipient string, window, now float64) error {
	_, err := s.exec(ctx, "INSERT INTO recipient_rate (recipient_task_id, window_start, sends, last_send_at)"+
		" VALUES (?,?,1,?)"+
		" ON CONFLICT(recipient_task_id, window_start) DO UPDATE SET"+
		" sends = sends + 1, last_send_at = excluded.last_send_at", recipient, window, now)
	return err
}

// RecipientRate reads one recipient window (delivery.py:2309 reads its sends).
func (s *Store) RecipientRate(ctx context.Context, recipient string, window float64) (RecipientRateRow, error) {
	return queryRow(ctx, s, scanRecipientRate, "SELECT "+recipientRateColumns+" FROM recipient_rate WHERE recipient_task_id = ? AND window_start = ?", recipient, window)
}

// WindowSends is delivery.py:2309; 0 for a window with no send.
func (s *Store) WindowSends(ctx context.Context, recipient string, window float64) (int64, error) {
	row, err := s.RecipientRate(ctx, recipient, window)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return row.Sends, err
}

// LastSend is delivery.py:2304: the latest send in [earliest, window].
func (s *Store) LastSend(ctx context.Context, recipient string, earliest, window float64) (sql.NullFloat64, error) {
	var last sql.NullFloat64
	err := s.q(ctx).QueryRowContext(ctx, "SELECT MAX(last_send_at) AS last FROM recipient_rate WHERE recipient_task_id = ?"+
		"   AND window_start BETWEEN ? AND ?", recipient, earliest, window).Scan(&last)
	return last, err
}

// RecordRecipientLifecycle is lifecycle.py:108: the latest observation of a task wins.
func (s *Store) RecordRecipientLifecycle(ctx context.Context, l RecipientLifecycleRow) error {
	_, err := s.exec(ctx, "INSERT INTO recipient_lifecycle (task_id, runtime_status, archived, goal_status,"+
		" can_accept_input, deliverable, withhold_reason, detail, observed_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT(task_id) DO UPDATE SET runtime_status=excluded.runtime_status,"+
		" archived=excluded.archived, goal_status=excluded.goal_status,"+
		" can_accept_input=excluded.can_accept_input, deliverable=excluded.deliverable,"+
		" withhold_reason=excluded.withhold_reason, detail=excluded.detail,"+
		" observed_at=excluded.observed_at",
		l.TaskID, l.RuntimeStatus, l.Archived, l.GoalStatus, l.CanAcceptInput, l.Deliverable,
		l.WithholdReason, l.Detail, l.ObservedAt)
	return err
}

// RecipientLifecycle is supervision.py:493 (every column).
func (s *Store) RecipientLifecycle(ctx context.Context, taskID string) (RecipientLifecycleRow, error) {
	return queryRow(ctx, s, scanRecipientLifecycle, "SELECT "+recipientLifecycleColumns+" FROM recipient_lifecycle WHERE task_id = ?", taskID)
}

// NoteOvertakenDelivery is faultsweep.py:387: a delivery judged overtaken is judged once.
func (s *Store) NoteOvertakenDelivery(ctx context.Context, eventID, reason, at string) error {
	_, err := s.exec(ctx, "INSERT OR IGNORE INTO fault_overtaken_deliveries (event_id, reason, noted_at) VALUES (?,?,?)", eventID, reason, at)
	return err
}

// OvertakenDelivery reads the judgement faultsweep.py:367 excludes on.
func (s *Store) OvertakenDelivery(ctx context.Context, eventID string) (FaultOvertakenDeliveriesRow, error) {
	return queryRow(ctx, s, scanFaultOvertakenDeliveries, "SELECT "+faultOvertakenDeliveriesColumns+" FROM fault_overtaken_deliveries WHERE event_id = ?", eventID)
}
