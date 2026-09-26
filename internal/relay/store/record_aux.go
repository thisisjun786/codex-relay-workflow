package store

import (
	"context"
	"database/sql"
	"fmt"
)

type VerificationClaim struct {
	EventID   string
	TurnID    sql.NullString
	ClaimedAt string
}

func (s *Store) VerificationClaim(ctx context.Context, eventID string) (VerificationClaim, error) {
	var r VerificationClaim
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id,claim_turn_id,claimed_at FROM verification_claims WHERE event_id=?`, eventID).Scan(&r.EventID, &r.TurnID, &r.ClaimedAt)
	if err != nil {
		return VerificationClaim{}, fmt.Errorf("verification claim %q: %w", eventID, err)
	}
	return r, nil
}

type Refusal struct {
	ID             int64
	At             string
	RelationshipID sql.NullString
	EventID        sql.NullString
	Reason         string
	Detail         sql.NullString
	Payload        sql.NullString
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

type WorkReport struct {
	EventID        string
	Submission     int64
	RelationshipID string
	Generation     int64
	RevisionHash   string
	Repository     string
	Summary        string
	NextAction     string
	RecordedAt     string
}

func (s *Store) WorkReport(ctx context.Context, eventID string, submission int64) (WorkReport, error) {
	var r WorkReport
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,summary,next_action,recorded_at FROM work_reports WHERE event_id=? AND submission_no=?`, eventID, submission).Scan(&r.EventID, &r.Submission, &r.RelationshipID, &r.Generation, &r.RevisionHash, &r.Repository, &r.Summary, &r.NextAction, &r.RecordedAt)
	if err != nil {
		return WorkReport{}, fmt.Errorf("work report %q/%d: %w", eventID, submission, err)
	}
	return r, nil
}

type WorkReportHandoff struct {
	EventID            string
	Submission         int64
	IsDraft            bool
	RequiredDeclared   string
	Checks             string
	ReviewCoverage     string
	ThreadDispositions string
	RecordedAt         string
}

func (s *Store) WorkReportHandoff(ctx context.Context, eventID string, submission int64) (WorkReportHandoff, error) {
	var r WorkReportHandoff
	err := s.q(ctx).QueryRowContext(ctx, `SELECT event_id,submission_no,is_draft,required_declared,checks,review_coverage,thread_dispositions,recorded_at FROM work_report_handoffs WHERE event_id=? AND submission_no=?`, eventID, submission).Scan(&r.EventID, &r.Submission, &r.IsDraft, &r.RequiredDeclared, &r.Checks, &r.ReviewCoverage, &r.ThreadDispositions, &r.RecordedAt)
	if err != nil {
		return WorkReportHandoff{}, fmt.Errorf("work report handoff %q/%d: %w", eventID, submission, err)
	}
	return r, nil
}

type AttemptReportSubmission struct {
	RequestID  string
	EventID    string
	Submission int64
	FrozenAt   string
}

func (s *Store) AttemptReportSubmission(ctx context.Context, requestID string) (AttemptReportSubmission, error) {
	var r AttemptReportSubmission
	err := s.q(ctx).QueryRowContext(ctx, `SELECT request_id,event_id,submission_no,frozen_at FROM attempt_report_submissions WHERE request_id=?`, requestID).Scan(&r.RequestID, &r.EventID, &r.Submission, &r.FrozenAt)
	if err != nil {
		return AttemptReportSubmission{}, fmt.Errorf("attempt report submission %q: %w", requestID, err)
	}
	return r, nil
}

type FailedOperation struct {
	ScopeKey       string
	Operation      string
	RelationshipID sql.NullString
	Detail         string
	ErrorCode      sql.NullString
	OccurredAt     string
}

func (s *Store) FailedOperation(ctx context.Context, scopeKey, operation string) (FailedOperation, error) {
	var r FailedOperation
	err := s.q(ctx).QueryRowContext(ctx, `SELECT scope_key,operation,relationship_id,detail,error_code,occurred_at FROM failed_operations WHERE scope_key=? AND operation=?`, scopeKey, operation).Scan(&r.ScopeKey, &r.Operation, &r.RelationshipID, &r.Detail, &r.ErrorCode, &r.OccurredAt)
	if err != nil {
		return FailedOperation{}, fmt.Errorf("failed operation %q/%q: %w", scopeKey, operation, err)
	}
	return r, nil
}

type ReconcileGate struct {
	RequestID     string
	Fingerprint   sql.NullString
	RetryRequired bool
	LastError     sql.NullString
	UpdatedAt     string
}

func (s *Store) ReconcileGate(ctx context.Context, requestID string) (ReconcileGate, error) {
	var r ReconcileGate
	err := s.q(ctx).QueryRowContext(ctx, `SELECT request_id,fingerprint,retry_required,last_error,updated_at FROM reconcile_gate WHERE request_id=?`, requestID).Scan(&r.RequestID, &r.Fingerprint, &r.RetryRequired, &r.LastError, &r.UpdatedAt)
	if err != nil {
		return ReconcileGate{}, fmt.Errorf("reconcile gate %q: %w", requestID, err)
	}
	return r, nil
}

type DiscoveryCursor struct {
	TaskID    string
	Listing   string
	Cursor    sql.NullString
	Exhausted bool
	Scanned   int64
	UpdatedAt string
}

func (s *Store) DiscoveryCursor(ctx context.Context, taskID, listing string) (DiscoveryCursor, error) {
	var r DiscoveryCursor
	err := s.q(ctx).QueryRowContext(ctx, `SELECT task_id,listing,cursor,exhausted,scanned,updated_at FROM discovery_cursors WHERE task_id=? AND listing=?`, taskID, listing).Scan(&r.TaskID, &r.Listing, &r.Cursor, &r.Exhausted, &r.Scanned, &r.UpdatedAt)
	if err != nil {
		return DiscoveryCursor{}, fmt.Errorf("discovery cursor %q/%q: %w", taskID, listing, err)
	}
	return r, nil
}
