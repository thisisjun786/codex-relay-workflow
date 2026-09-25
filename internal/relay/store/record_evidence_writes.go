package store

import (
	"context"
	"database/sql"
)

func (s *Store) RecordGenerationTurn(ctx context.Context, turn GenerationTurn) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO generation_turns (relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES (?,?,?,?,?,?,?)`, turn.RelationshipID, turn.Generation, turn.TurnID, turn.Evidence, turn.Actor, turn.Detail, turn.AdmittedAt)
		return err
	})
}

func (s *Store) RecordLineage(ctx context.Context, lineage RevisionLineage) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO revision_lineage (relationship_id,execution_generation,event_id,revision_hash,supersedes_hash,declared_by,recorded_at) VALUES (?,?,?,?,?,?,?)`, lineage.RelationshipID, lineage.Generation, lineage.EventID, lineage.RevisionHash, lineage.SupersedesHash, lineage.DeclaredBy, lineage.RecordedAt)
		return err
	})
}

func (s *Store) RecordClaim(ctx context.Context, claim VerificationClaim) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO verification_claims (event_id,claim_turn_id,claimed_at) VALUES (?,?,?)`, claim.EventID, claim.TurnID, claim.ClaimedAt)
		return err
	})
}

func (s *Store) RecordAckEvidence(ctx context.Context, evidence AckEvidence) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO ack_evidence (event_id,tier,detail,attempts,last_reason,fingerprint,next_check_at,observed_at) VALUES (?,?,?,?,?,?,?,?)`, evidence.EventID, evidence.Tier, evidence.Detail, evidence.Attempts, evidence.LastReason, evidence.Fingerprint, evidence.NextCheckAt, evidence.ObservedAt)
		return err
	})
}

func (s *Store) RecordRefusal(ctx context.Context, refusal Refusal) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO refusals (at,relationship_id,event_id,reason,detail,payload) VALUES (?,?,?,?,?,?)`, refusal.At, refusal.RelationshipID, refusal.EventID, refusal.Reason, refusal.Detail, refusal.Payload)
		return err
	})
}

func (s *Store) RecordWorkReport(ctx context.Context, report WorkReport) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,'','','',?,?,?)`, report.EventID, report.Submission, report.RelationshipID, report.Generation, report.RevisionHash, report.Repository, report.Summary, report.NextAction, report.RecordedAt)
		return err
	})
}

func (s *Store) RecordWorkReportHandoff(ctx context.Context, handoff WorkReportHandoff) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,required_declared,checks,review_coverage,thread_dispositions,recorded_at) VALUES (?,?,?,?,?,?,?,?)`, handoff.EventID, handoff.Submission, handoff.IsDraft, handoff.RequiredDeclared, handoff.Checks, handoff.ReviewCoverage, handoff.ThreadDispositions, handoff.RecordedAt)
		return err
	})
}

func (s *Store) RecordAttemptReportSubmission(ctx context.Context, submission AttemptReportSubmission) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO attempt_report_submissions (request_id,event_id,submission_no,frozen_at) VALUES (?,?,?,?)`, submission.RequestID, submission.EventID, submission.Submission, submission.FrozenAt)
		return err
	})
}

func (s *Store) RecordFailedOperation(ctx context.Context, operation FailedOperation) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO failed_operations (scope_key,operation,relationship_id,detail,error_code,occurred_at) VALUES (?,?,?,?,?,?) ON CONFLICT(scope_key,operation) DO UPDATE SET relationship_id=excluded.relationship_id,detail=excluded.detail,error_code=excluded.error_code,occurred_at=excluded.occurred_at`, operation.ScopeKey, operation.Operation, operation.RelationshipID, operation.Detail, operation.ErrorCode, operation.OccurredAt)
		return err
	})
}

func (s *Store) RecordReconcileGate(ctx context.Context, gate ReconcileGate) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO reconcile_gate (request_id,fingerprint,retry_required,last_error,updated_at) VALUES (?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET fingerprint=excluded.fingerprint,retry_required=excluded.retry_required,last_error=excluded.last_error,updated_at=excluded.updated_at`, gate.RequestID, gate.Fingerprint, gate.RetryRequired, gate.LastError, gate.UpdatedAt)
		return err
	})
}

func (s *Store) RecordDiscoveryCursor(ctx context.Context, cursor DiscoveryCursor) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO discovery_cursors (task_id,listing,cursor,exhausted,scanned,updated_at) VALUES (?,?,?,?,?,?) ON CONFLICT(task_id,listing) DO UPDATE SET cursor=excluded.cursor,exhausted=excluded.exhausted,scanned=excluded.scanned,updated_at=excluded.updated_at`, cursor.TaskID, cursor.Listing, cursor.Cursor, cursor.Exhausted, cursor.Scanned, cursor.UpdatedAt)
		return err
	})
}
