package store

import (
	"context"
	"database/sql"
)

// Merge-turn and capacity tables: merge_turns, merge_turn_ledger, merge_turn_checks,
// execution_slots, execution_limits, execution_usage.

// InsertMergeTurn is mergeturn.py:898 MergeTurn.request.
func (s *Store) InsertMergeTurn(ctx context.Context, m MergeTurnsRow) error {
	_, err := s.exec(ctx, "INSERT INTO merge_turns (turn_id, target_key, repository, base_ref,"+
		" project_key, holder_task_id, holder_host_id, relationship_id, pr_number,"+
		" candidate_head, declared_ready, state, tenure, requested_at, held_at,"+
		" updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		m.TurnID, m.TargetKey, m.Repository, m.BaseRef, m.ProjectKey, m.HolderTaskID, m.HolderHostID,
		m.RelationshipID, m.PRNumber, m.CandidateHead, m.DeclaredReady, m.State, m.Tenure,
		m.RequestedAt, m.HeldAt, m.UpdatedAt)
	return err
}

// MergeTurn is mergeturn.py:301 MergeTurn.turn / _row_in.
func (s *Store) MergeTurn(ctx context.Context, turnID string) (MergeTurnsRow, error) {
	return queryRow(ctx, s, scanMergeTurns, "SELECT "+mergeTurnsColumns+" FROM merge_turns WHERE turn_id = ?", turnID)
}

// MergeTurnsForTarget is mergeturn.py:410 MergeTurn.target.
func (s *Store) MergeTurnsForTarget(ctx context.Context, targetKey string) ([]MergeTurnsRow, error) {
	return queryRows(ctx, s, scanMergeTurns, "SELECT "+mergeTurnsColumns+" FROM merge_turns WHERE target_key = ?"+
		"  ORDER BY requested_at, turn_id", targetKey)
}

// LiveMergeClaim is mergeturn.py:867: the holder's own live claim on a target (the replay).
func (s *Store) LiveMergeClaim(ctx context.Context, targetKey, holderTaskID string) (MergeTurnsRow, error) {
	return queryRow(ctx, s, scanMergeTurns, "SELECT "+mergeTurnsColumns+" FROM merge_turns"+
		"  WHERE target_key = ? AND holder_task_id = ?"+
		"    AND state IN ('waiting','holding','merging','unknown')", targetKey, holderTaskID)
}

// MergeTargetOccupant is mergeturn.py:1009: the turn holding a target, if any.
type MergeTargetOccupant struct{ TurnID, State string }

func (s *Store) MergeTargetOccupant(ctx context.Context, targetKey string) (MergeTargetOccupant, error) {
	return queryRow(ctx, s, func(row scanner) (MergeTargetOccupant, error) {
		var o MergeTargetOccupant
		return o, row.Scan(&o.TurnID, &o.State)
	}, "SELECT turn_id, state FROM merge_turns"+
		"  WHERE target_key = ? AND state IN ('holding','merging','unknown')", targetKey)
}

// HighestMergeTenure is mergeturn.py:885; 0 when the holder never claimed the target.
func (s *Store) HighestMergeTenure(ctx context.Context, targetKey, holderTaskID string) (int64, error) {
	var highest sql.NullInt64
	err := s.q(ctx).QueryRowContext(ctx, "SELECT MAX(tenure) AS highest FROM merge_turns"+
		"  WHERE target_key = ? AND holder_task_id = ?", targetKey, holderTaskID).Scan(&highest)
	return highest.Int64, err
}

// ReadyMergeWaiters is mergeturn.py:1302 _promote_in: the promotion order.
func (s *Store) ReadyMergeWaiters(ctx context.Context, targetKey string) ([]MergeTurnsRow, error) {
	return queryRows(ctx, s, scanMergeTurns, "SELECT "+mergeTurnsColumns+" FROM merge_turns"+
		"  WHERE target_key = ? AND state = 'waiting' AND declared_ready = 1"+
		"  ORDER BY requested_at, turn_id", targetKey)
}

// OutstandingMergeClaim is one row of mergeturn.py:341 MergeTurn.outstanding.
type OutstandingMergeClaim struct{ TurnID, TargetKey, State string }

func (s *Store) OutstandingMergeClaims(ctx context.Context, holderTaskID string) ([]OutstandingMergeClaim, error) {
	return queryRows(ctx, s, func(row scanner) (OutstandingMergeClaim, error) {
		var o OutstandingMergeClaim
		return o, row.Scan(&o.TurnID, &o.TargetKey, &o.State)
	}, "SELECT turn_id, target_key, state FROM merge_turns"+
		"  WHERE holder_task_id = ? AND state IN ('waiting','holding','merging','unknown')"+
		"  ORDER BY requested_at, turn_id", holderTaskID)
}

// PromoteMergeTurn is mergeturn.py:1319: a waiter takes the freed target.
func (s *Store) PromoteMergeTurn(ctx context.Context, turnID, holding, at string) error {
	_, err := s.exec(ctx, "UPDATE merge_turns SET state = ?, held_at = ?, updated_at = ?"+
		" WHERE turn_id = ?", holding, at, at, turnID)
	return err
}

// DeclareMergeReadiness is mergeturn.py:1034 MergeTurn.declare_ready.
func (s *Store) DeclareMergeReadiness(ctx context.Context, turnID string, ready int64, head, state string, heldAt sql.NullString, at string) error {
	_, err := s.exec(ctx, "UPDATE merge_turns SET declared_ready = ?, candidate_head = ?, state = ?,"+
		" held_at = ?, updated_at = ? WHERE turn_id = ?", ready, head, state, heldAt, at, turnID)
	return err
}

// CloseMergeTurn is mergeturn.py:1276 _close_in; landed and observed shas keep a prior value.
func (s *Store) CloseMergeTurn(ctx context.Context, turnID, state, reason, at string, landedSHA, observedBaseSHA sql.NullString) error {
	_, err := s.exec(ctx, "UPDATE merge_turns SET state = ?, close_reason = ?, closed_at = ?,"+
		" landed_sha = COALESCE(?, landed_sha),"+
		" observed_base_sha = COALESCE(?, observed_base_sha), updated_at = ?"+
		" WHERE turn_id = ?", state, reason, at, landedSHA, observedBaseSHA, at, turnID)
	return err
}

// WriteMergeLedger is mergeturn.py:716 _write_ledger: a replayed key is one fact.
func (s *Store) WriteMergeLedger(ctx context.Context, e MergeTurnLedgerRow) error {
	_, err := s.exec(ctx, "INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state,"+
		" evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT (turn_id, idempotency_key) DO NOTHING",
		e.EntryID, e.TurnID, e.Kind, e.FromState, e.ToState, e.EvidenceKind, e.ActorTaskID,
		e.Evidence, e.IdempotencyKey, e.RecordedAt)
	return err
}

// MergeLedger is mergeturn.py:395 MergeTurn.ledger.
func (s *Store) MergeLedger(ctx context.Context, turnID string) ([]MergeTurnLedgerRow, error) {
	return queryRows(ctx, s, scanMergeTurnLedger, "SELECT "+mergeTurnLedgerColumns+" FROM merge_turn_ledger WHERE turn_id = ?"+
		"  ORDER BY recorded_at, entry_id", turnID)
}

// MergeLedgerEntry is mergeturn.py:668/736: the entry one idempotency key recorded.
func (s *Store) MergeLedgerEntry(ctx context.Context, turnID, idempotencyKey string) (MergeTurnLedgerRow, error) {
	return queryRow(ctx, s, scanMergeTurnLedger, "SELECT "+mergeTurnLedgerColumns+" FROM merge_turn_ledger"+
		"  WHERE turn_id = ? AND idempotency_key = ?", turnID, idempotencyKey)
}

// RecordMergeCheck is mergeturn.py:1593: a restated check converges on its derived id.
func (s *Store) RecordMergeCheck(ctx context.Context, c MergeTurnChecksRow) error {
	_, err := s.exec(ctx, "INSERT INTO merge_turn_checks (check_id, turn_id, head_sha, base_sha,"+
		" required, checks_digest, checks, review_digest, review, result,"+
		" refusal_reason, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT (check_id) DO UPDATE SET result = excluded.result,"+
		" refusal_reason = excluded.refusal_reason, recorded_at = excluded.recorded_at",
		c.CheckID, c.TurnID, c.HeadSHA, c.BaseSHA, c.Required, c.ChecksDigest, c.Checks,
		c.ReviewDigest, c.Review, c.Result, c.RefusalReason, c.RecordedAt)
	return err
}

// MergeChecks is mergeturn.py:474 _blocked_report: newest first.
func (s *Store) MergeChecks(ctx context.Context, turnID string) ([]MergeTurnChecksRow, error) {
	return queryRows(ctx, s, scanMergeTurnChecks, "SELECT "+mergeTurnChecksColumns+" FROM merge_turn_checks WHERE turn_id = ?"+
		"  ORDER BY recorded_at DESC, check_id DESC", turnID)
}

// InsertExecutionSlot is capacity.py:375 Capacity.reserve.
func (s *Store) InsertExecutionSlot(ctx context.Context, e ExecutionSlotsRow) error {
	_, err := s.exec(ctx, "INSERT INTO execution_slots (slot_id, subject_kind, subject_key,"+
		" parent_task_id, project_key, initiative_key, tenure, state, reserved_by,"+
		" reserved_at, detail) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
		e.SlotID, e.SubjectKind, e.SubjectKey, e.ParentTaskID, e.ProjectKey, e.InitiativeKey,
		e.Tenure, e.State, e.ReservedBy, e.ReservedAt, e.Detail)
	return err
}

// ExecutionSlot is capacity.py:79 Capacity.slot: the newest tenure of a subject.
func (s *Store) ExecutionSlot(ctx context.Context, subjectKind, subjectKey string) (ExecutionSlotsRow, error) {
	return queryRow(ctx, s, scanExecutionSlots, "SELECT "+executionSlotsColumns+" FROM execution_slots"+
		"  WHERE subject_kind = ? AND subject_key = ?"+
		"  ORDER BY tenure DESC LIMIT 1", subjectKind, subjectKey)
}

// ExecutionSlotTenures is capacity.py:456 Capacity.release: every tenure, newest first.
func (s *Store) ExecutionSlotTenures(ctx context.Context, subjectKind, subjectKey string) ([]ExecutionSlotsRow, error) {
	return queryRows(ctx, s, scanExecutionSlots, "SELECT "+executionSlotsColumns+" FROM execution_slots"+
		"  WHERE subject_kind = ? AND subject_key = ?"+
		"  ORDER BY tenure DESC", subjectKind, subjectKey)
}

// HeldExecutionSlot is capacity.py:344: the subject's held slot (the replay check).
func (s *Store) HeldExecutionSlot(ctx context.Context, subjectKind, subjectKey, held string) (ExecutionSlotsRow, error) {
	return queryRow(ctx, s, scanExecutionSlots, "SELECT "+executionSlotsColumns+" FROM execution_slots"+
		"  WHERE subject_kind = ? AND subject_key = ? AND state = ?", subjectKind, subjectKey, held)
}

// ExecutionSlotsInState is capacity.py:89 Capacity.report.
func (s *Store) ExecutionSlotsInState(ctx context.Context, state string) ([]ExecutionSlotsRow, error) {
	return queryRows(ctx, s, scanExecutionSlots, "SELECT "+executionSlotsColumns+" FROM execution_slots WHERE state = ?"+
		"  ORDER BY reserved_at, slot_id", state)
}

// HighestSlotTenure is capacity.py:368; 0 for a subject never reserved.
func (s *Store) HighestSlotTenure(ctx context.Context, subjectKind, subjectKey string) (int64, error) {
	var highest sql.NullInt64
	err := s.q(ctx).QueryRowContext(ctx, "SELECT MAX(tenure) AS highest FROM execution_slots"+
		"  WHERE subject_kind = ? AND subject_key = ?", subjectKind, subjectKey).Scan(&highest)
	return highest.Int64, err
}

// RunsIn is capacity.py:182 _runs_in: held slots in the store, a project or an initiative.
func (s *Store) RunsIn(ctx context.Context, scopeKind, scopeKey, held string) (int64, error) {
	var tally int64
	var err error
	switch scopeKind {
	case "store":
		err = s.q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) AS tally FROM execution_slots WHERE state = ?", held).Scan(&tally)
	case "project":
		err = s.q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) AS tally FROM execution_slots"+
			"  WHERE state = ? AND project_key = ?", held, scopeKey).Scan(&tally)
	default:
		err = s.q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) AS tally FROM execution_slots"+
			"  WHERE state = ? AND initiative_key = ?", held, scopeKey).Scan(&tally)
	}
	return tally, err
}

// ReleaseExecutionSlot is capacity.py:505; the state guard is in the UPDATE, so two concurrent
// releases cannot both write. It reports whether this call released the slot.
func (s *Store) ReleaseExecutionSlot(ctx context.Context, slotID, released, at, releasedBy, reason, held string) (bool, error) {
	result, err := s.exec(ctx, "UPDATE execution_slots SET state = ?, released_at = ?, released_by = ?,"+
		" release_reason = ? WHERE slot_id = ? AND state = ?", released, at, releasedBy, reason, slotID, held)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

// DeclareExecutionLimit is capacity.py:542: re-declaring updates, never accumulates.
func (s *Store) DeclareExecutionLimit(ctx context.Context, l ExecutionLimitsRow) error {
	_, err := s.exec(ctx, "INSERT INTO execution_limits (limit_id, scope_kind, scope_key, dimension,"+
		" unit, ceiling, enforce, declared_by, source, revision, declared_at,"+
		" updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT (limit_id) DO UPDATE SET unit = excluded.unit,"+
		" ceiling = excluded.ceiling, enforce = excluded.enforce,"+
		" declared_by = excluded.declared_by, source = excluded.source,"+
		" revision = excluded.revision, updated_at = excluded.updated_at",
		l.LimitID, l.ScopeKind, l.ScopeKey, l.Dimension, l.Unit, l.Ceiling, l.Enforce, l.DeclaredBy,
		l.Source, l.Revision, l.DeclaredAt, l.UpdatedAt)
	return err
}

// ExecutionLimit is capacity.py:538.
func (s *Store) ExecutionLimit(ctx context.Context, limitID string) (ExecutionLimitsRow, error) {
	return queryRow(ctx, s, scanExecutionLimits, "SELECT "+executionLimitsColumns+" FROM execution_limits WHERE limit_id = ?", limitID)
}

// ExecutionLimits is capacity.py:129 Capacity.headroom.
func (s *Store) ExecutionLimits(ctx context.Context, scopeKind, scopeKey string) ([]ExecutionLimitsRow, error) {
	return queryRows(ctx, s, scanExecutionLimits, "SELECT "+executionLimitsColumns+" FROM execution_limits"+
		"  WHERE scope_kind = ? AND scope_key = ? ORDER BY dimension", scopeKind, scopeKey)
}

// EnforcedExecutionLimits is capacity.py:404 _ceiling_refusal.
func (s *Store) EnforcedExecutionLimits(ctx context.Context, scopeKind, scopeKey string) ([]ExecutionLimitsRow, error) {
	return queryRows(ctx, s, scanExecutionLimits, "SELECT "+executionLimitsColumns+" FROM execution_limits"+
		"  WHERE scope_kind = ? AND scope_key = ? AND enforce = 1"+
		"  ORDER BY dimension", scopeKind, scopeKey)
}

// ObserveExecutionUsage is capacity.py:585: one current measurement per dimension.
func (s *Store) ObserveExecutionUsage(ctx context.Context, u ExecutionUsageRow) error {
	_, err := s.exec(ctx, "INSERT INTO execution_usage (scope_kind, scope_key, dimension, observed,"+
		" observed_by, method, observed_at) VALUES (?,?,?,?,?,?,?)"+
		" ON CONFLICT (scope_kind, scope_key, dimension) DO UPDATE SET"+
		" observed = excluded.observed, observed_by = excluded.observed_by,"+
		" method = excluded.method, observed_at = excluded.observed_at",
		u.ScopeKind, u.ScopeKey, u.Dimension, u.Observed, u.ObservedBy, u.Method, u.ObservedAt)
	return err
}

// ExecutionUsage is capacity.py:143.
func (s *Store) ExecutionUsage(ctx context.Context, scopeKind, scopeKey, dimension string) (ExecutionUsageRow, error) {
	return queryRow(ctx, s, scanExecutionUsage, "SELECT "+executionUsageColumns+" FROM execution_usage"+
		"  WHERE scope_kind = ? AND scope_key = ? AND dimension = ?", scopeKind, scopeKey, dimension)
}
