package store

import (
	"context"
	"database/sql"
)

// Supervisor channel, sync outbox and product routing tables: supervisor_messages,
// supervisor_attempts, supervisor_readbacks, sync_targets, sync_outbox, product_registry,
// product_bindings, routing_policy, incident_routes, route_incidents.

// StageSupervisorMessage is supervisorchannel.py:1126: the first staging of a fact wins. It
// reports whether this call staged the row.
func (s *Store) StageSupervisorMessage(ctx context.Context, m SupervisorMessagesRow) (bool, error) {
	return s.changedOne(ctx, "INSERT OR IGNORE INTO supervisor_messages (message_id, obligation_id,"+
		" obligation_kind, relationship_id, project_key, purpose, kind,"+
		" sender_task_id, recipient_task_id, subject, packet, state,"+
		" attempt_count, next_eligible_at, staged_at, updated_at, event_id,"+
		" submission_no, reading) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,0,NULL,?,?,?,?,?)",
		m.MessageID, m.ObligationID, m.ObligationKind, m.RelationshipID, m.ProjectKey, m.Purpose, m.Kind,
		m.SenderTaskID, m.RecipientTaskID, m.Subject, m.Packet, m.State, m.StagedAt, m.UpdatedAt,
		m.EventID, m.SubmissionNo, m.Reading)
}

// SupervisorMessage is supervisorchannel.py:916 SupervisorChannel.get.
func (s *Store) SupervisorMessage(ctx context.Context, messageID string) (SupervisorMessagesRow, error) {
	return queryRow(ctx, s, scanSupervisorMessages, "SELECT "+supervisorMessagesColumns+" FROM supervisor_messages WHERE message_id = ?", messageID)
}

// SupervisorMessageFor is supervisorchannel.py:1457: the message staged for one obligation.
func (s *Store) SupervisorMessageFor(ctx context.Context, obligationKind, obligationID string) (SupervisorMessagesRow, error) {
	return queryRow(ctx, s, scanSupervisorMessages, "SELECT "+supervisorMessagesColumns+" FROM supervisor_messages WHERE obligation_kind = ? AND obligation_id = ?", obligationKind, obligationID)
}

// ClaimableSupervisorMessages is supervisorchannel.py:1927: eligible rows in staging order.
// states are the three claimable states (queued, deferred_busy, withheld_pre_send).
func (s *Store) ClaimableSupervisorMessages(ctx context.Context, states [3]string, now float64, limit int) ([]SupervisorMessagesRow, error) {
	return queryRows(ctx, s, scanSupervisorMessages, "SELECT "+supervisorMessagesColumns+" FROM supervisor_messages"+
		" WHERE state IN (?,?,?) AND hold_reason IS NULL"+
		"   AND (next_eligible_at IS NULL OR next_eligible_at <= ?)"+
		" ORDER BY staged_at, message_id LIMIT ?", states[0], states[1], states[2], now, limit)
}

// SettleSupervisorMessage is supervisorchannel.py:2571: the message moves only while the claim
// that sent it still holds it. It reports whether it moved.
func (s *Store) SettleSupervisorMessage(ctx context.Context, messageID, state string, nextEligible sql.NullFloat64, hold sql.NullString, at, sending string, attemptNo int64, owner sql.NullString) (bool, error) {
	return s.changedOne(ctx, "UPDATE supervisor_messages SET state = ?, next_eligible_at = ?,"+
		" hold_reason = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?"+
		" WHERE message_id = ? AND state = ? AND attempt_count = ?"+
		"   AND lease_owner IS ?", state, nextEligible, hold, at, messageID, sending, attemptNo, owner)
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

// StartSupervisorTransport is supervisorchannel.py:2266.
func (s *Store) StartSupervisorTransport(ctx context.Context, requestID, at string) error {
	_, err := s.exec(ctx, "UPDATE supervisor_attempts SET transport_started_at = ? WHERE request_id = ?", at, requestID)
	return err
}

// SettleSupervisorAttempt is supervisorchannel.py:2563: the attempt's own receipt, always.
func (s *Store) SettleSupervisorAttempt(ctx context.Context, requestID, state, sendAttempted string, retrySafe int64, turnID sql.NullString, record, at string) error {
	_, err := s.exec(ctx, "UPDATE supervisor_attempts SET state = ?, send_attempted = ?, retry_safe = ?,"+
		" turn_id = ?, record = ?, observed_at = ? WHERE request_id = ?", state, sendAttempted, retrySafe, turnID, record, at, requestID)
	return err
}

// SupervisorAttempts is supervisorchannel.py:3156 SupervisorChannel.show.
func (s *Store) SupervisorAttempts(ctx context.Context, messageID string) ([]SupervisorAttemptsRow, error) {
	return queryRows(ctx, s, scanSupervisorAttempts, "SELECT "+supervisorAttemptsColumns+" FROM supervisor_attempts WHERE message_id = ? ORDER BY attempt_no", messageID)
}

// LatestSupervisorAttempt is supervisorchannel.py:3114.
func (s *Store) LatestSupervisorAttempt(ctx context.Context, messageID string) (SupervisorAttemptsRow, error) {
	return queryRow(ctx, s, scanSupervisorAttempts, "SELECT "+supervisorAttemptsColumns+" FROM supervisor_attempts WHERE message_id = ? ORDER BY attempt_no DESC"+
		" LIMIT 1", messageID)
}

// SupervisorAttemptMessage is supervisorchannel.py:2487: which message a request id belongs to.
func (s *Store) SupervisorAttemptMessage(ctx context.Context, requestID string) (string, error) {
	return queryRow(ctx, s, scanString, "SELECT message_id FROM supervisor_attempts WHERE request_id = ?", requestID)
}

// RecordSupervisorReadback is supervisorchannel.py:2939: the latest readback replaces the prior.
func (s *Store) RecordSupervisorReadback(ctx context.Context, r SupervisorReadbacksRow) error {
	_, err := s.exec(ctx, "INSERT INTO supervisor_readbacks (message_id, read_turn_id, proof, verified,"+
		" request_id, detail, read_at) VALUES (?,?,?,?,?,?,?)"+
		" ON CONFLICT(message_id) DO UPDATE SET read_turn_id = excluded.read_turn_id,"+
		" proof = excluded.proof, verified = excluded.verified,"+
		" request_id = excluded.request_id, detail = excluded.detail,"+
		" read_at = excluded.read_at",
		r.MessageID, r.ReadTurnID, r.Proof, r.Verified, r.RequestID, r.Detail, r.ReadAt)
	return err
}

// SupervisorReadback is supervisorchannel.py:3128.
func (s *Store) SupervisorReadback(ctx context.Context, messageID string) (SupervisorReadbacksRow, error) {
	return queryRow(ctx, s, scanSupervisorReadbacks, "SELECT "+supervisorReadbacksColumns+" FROM supervisor_readbacks WHERE message_id = ?", messageID)
}

// SetSyncTarget is sync.py:373 SyncOutbox.set_target.
func (s *Store) SetSyncTarget(ctx context.Context, relationshipID, target, targetRef, at string) error {
	_, err := s.exec(ctx, "INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at)"+
		" VALUES (?,?,?,?)"+
		" ON CONFLICT(relationship_id, target) DO UPDATE SET"+
		"   target_ref = excluded.target_ref, recorded_at = excluded.recorded_at", relationshipID, target, targetRef, at)
	return err
}

// SyncTarget is sync.py:383 SyncOutbox.target_for.
func (s *Store) SyncTarget(ctx context.Context, relationshipID, target string) (SyncTargetsRow, error) {
	return queryRow(ctx, s, scanSyncTargets, "SELECT "+syncTargetsColumns+" FROM sync_targets WHERE relationship_id = ? AND target = ?", relationshipID, target)
}

// EnqueueSync is sync.py:413 enqueue_in: an identical job is enqueued once. It reports whether
// this call inserted the row.
func (s *Store) EnqueueSync(ctx context.Context, o SyncOutboxRow) (bool, error) {
	return s.changedOne(ctx, "INSERT OR IGNORE INTO sync_outbox (sync_id, relationship_id, issue_key, target,"+
		" target_ref, subject_kind, event_id, execution_generation, revision_hash, verdict,"+
		" identity_digest, summary, state, attempts, next_attempt_at, created_at,"+
		" updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,0,NULL,?,?)",
		o.SyncID, o.RelationshipID, o.IssueKey, o.Target, o.TargetRef, o.SubjectKind, o.EventID,
		o.ExecutionGeneration, o.RevisionHash, o.Verdict, o.IdentityDigest, o.Summary, o.State,
		o.CreatedAt, o.UpdatedAt)
}

// SyncJob is sync.py:467 SyncOutbox.get.
func (s *Store) SyncJob(ctx context.Context, syncID string) (SyncOutboxRow, error) {
	return queryRow(ctx, s, scanSyncOutbox, "SELECT "+syncOutboxColumns+" FROM sync_outbox WHERE sync_id = ?", syncID)
}

// NextSyncJobs is sync.py:480 SyncOutbox.next; states are pending, written, claimed. An empty
// target means any target.
func (s *Store) NextSyncJobs(ctx context.Context, states [3]string, target string, now float64, limit int) ([]SyncOutboxRow, error) {
	query := "SELECT " + syncOutboxColumns + " FROM sync_outbox WHERE state IN (?,?,?)" +
		"   AND (next_attempt_at IS NULL OR next_attempt_at <= ?)" +
		"   AND (lease_until IS NULL OR lease_until <= ?)"
	args := []any{states[0], states[1], states[2], now, now}
	if target != "" {
		query += " AND target = ?"
		args = append(args, target)
	}
	query += " ORDER BY created_at LIMIT ?"
	return queryRows(ctx, s, scanSyncOutbox, query, append(args, limit)...)
}

// SyncSnapshot is sync.py:835 SyncOutbox.snapshot; an empty relationship means every job.
func (s *Store) SyncSnapshot(ctx context.Context, relationshipID string) ([]SyncOutboxRow, error) {
	query := "SELECT " + syncOutboxColumns + " FROM sync_outbox"
	var args []any
	if relationshipID != "" {
		query += " WHERE relationship_id = ?"
		args = append(args, relationshipID)
	}
	return queryRows(ctx, s, scanSyncOutbox, query+" ORDER BY created_at", args...)
}

// ClaimSync is sync.py:530.
func (s *Store) ClaimSync(ctx context.Context, syncID, claimed, owner string, leaseUntil float64, token, at string) error {
	_, err := s.exec(ctx, "UPDATE sync_outbox SET state = ?, lease_owner = ?, lease_until = ?,"+
		" claim_token = ?, updated_at = ? WHERE sync_id = ?", claimed, owner, leaseUntil, token, at, syncID)
	return err
}

// ConfirmSync is sync.py:771: the first write time is kept.
func (s *Store) ConfirmSync(ctx context.Context, syncID, confirmed string, externalRef sql.NullString, readback, at string) error {
	_, err := s.exec(ctx, "UPDATE sync_outbox SET state = ?, external_ref = ?, readback = ?,"+
		" written_at = COALESCE(written_at, ?), confirmed_at = ?, last_error = NULL,"+
		" next_attempt_at = NULL, lease_owner = NULL, lease_until = NULL,"+
		" updated_at = ? WHERE sync_id = ?", confirmed, externalRef, readback, at, at, at, syncID)
	return err
}

// FailSync is sync.py:813.
func (s *Store) FailSync(ctx context.Context, syncID, state string, attempts int64, lastError string, nextAttempt float64, at string) error {
	_, err := s.exec(ctx, "UPDATE sync_outbox SET state = ?, attempts = ?, last_error = ?,"+
		" next_attempt_at = ?, lease_owner = NULL, lease_until = NULL,"+
		" claim_token = NULL, updated_at = ? WHERE sync_id = ?", state, attempts, lastError, nextAttempt, at, syncID)
	return err
}

// RetrySync is sync.py:827: a confirmed job is never reopened.
func (s *Store) RetrySync(ctx context.Context, syncID, pending, at, confirmed string) error {
	_, err := s.exec(ctx, "UPDATE sync_outbox SET state = ?, next_attempt_at = NULL, lease_owner = NULL,"+
		" lease_until = NULL, claim_token = NULL, updated_at = ?"+
		" WHERE sync_id = ? AND state != ?", pending, at, syncID, confirmed)
	return err
}

// RegisterProduct is routing.py:102.
func (s *Store) RegisterProduct(ctx context.Context, productKey, record, at string) error {
	_, err := s.exec(ctx, "INSERT INTO product_registry (product_key, record, recorded_at) VALUES (?,?,?)"+
		" ON CONFLICT(product_key) DO UPDATE SET record = excluded.record,"+
		"   recorded_at = excluded.recorded_at", productKey, record, at)
	return err
}

// ProductRegistry is routing.py:113 ProductRouter.registry (with the key columns).
func (s *Store) ProductRegistry(ctx context.Context, productKey string) (ProductRegistryRow, error) {
	return queryRow(ctx, s, scanProductRegistry, "SELECT "+productRegistryColumns+" FROM product_registry WHERE product_key = ?", productKey)
}

// ProductRegistries is routing.py:119.
func (s *Store) ProductRegistries(ctx context.Context) ([]ProductRegistryRow, error) {
	return queryRows(ctx, s, scanProductRegistry, "SELECT "+productRegistryColumns+" FROM product_registry ORDER BY product_key")
}

// BindProduct is routing.py:150.
func (s *Store) BindProduct(ctx context.Context, b ProductBindingsRow) error {
	_, err := s.exec(ctx, "INSERT INTO product_bindings (product_key, kind, ref, record, observed_at,"+
		"  recorded_at) VALUES (?,?,?,?,?,?)"+
		" ON CONFLICT(product_key, kind, ref) DO UPDATE SET record = excluded.record,"+
		"   observed_at = excluded.observed_at, recorded_at = excluded.recorded_at",
		b.ProductKey, b.Kind, b.Ref, b.Record, b.ObservedAt, b.RecordedAt)
	return err
}

// ProductBindings is routing.py:164.
func (s *Store) ProductBindings(ctx context.Context, productKey string) ([]ProductBindingsRow, error) {
	return queryRows(ctx, s, scanProductBindings, "SELECT "+productBindingsColumns+" FROM product_bindings WHERE product_key = ? ORDER BY kind, ref", productKey)
}

// SetRoutingPolicy is routing.py:178.
func (s *Store) SetRoutingPolicy(ctx context.Context, policyKey, record, basis, at string) error {
	_, err := s.exec(ctx, "INSERT INTO routing_policy (policy_key, record, basis, recorded_at)"+
		" VALUES (?,?,?,?) ON CONFLICT(policy_key) DO UPDATE SET"+
		"   record = excluded.record, basis = excluded.basis,"+
		"   recorded_at = excluded.recorded_at", policyKey, record, basis, at)
	return err
}

// RoutingPolicy is routing.py:188 (with the key columns).
func (s *Store) RoutingPolicy(ctx context.Context, policyKey string) (RoutingPolicyRow, error) {
	return queryRow(ctx, s, scanRoutingPolicy, "SELECT "+routingPolicyColumns+" FROM routing_policy WHERE policy_key = ?", policyKey)
}

// UpsertIncidentRoute is routes.py:135 upsert. replaceGoal is Python's goal-is-not-KEEP: when
// false the stored goal is kept. The claimed severity only rises (notice < degraded < broken),
// and classification and superseded_by keep a prior value when not restated.
func (s *Store) UpsertIncidentRoute(ctx context.Context, r IncidentRoutesRow, replaceGoal bool) error {
	goal := r.Goal
	if !replaceGoal {
		goal = sql.NullString{}
	}
	_, err := s.exec(ctx, "INSERT INTO incident_routes (fault_id, product_key, workspace, disposition, stage,"+
		"  target, origin, claimed_severity, goal, classification, superseded_by, detail,"+
		"  created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT(fault_id) DO UPDATE SET product_key = excluded.product_key,"+
		"   workspace = excluded.workspace, disposition = excluded.disposition,"+
		"   stage = excluded.stage, target = excluded.target, origin = excluded.origin,"+
		"   claimed_severity = CASE"+
		"     WHEN 'broken' IN (excluded.claimed_severity, incident_routes.claimed_severity)"+
		"       THEN 'broken'"+
		"     WHEN 'degraded' IN (excluded.claimed_severity, incident_routes.claimed_severity)"+
		"       THEN 'degraded'"+
		"     ELSE incident_routes.claimed_severity END,"+
		"   goal = CASE WHEN ? THEN excluded.goal ELSE incident_routes.goal END,"+
		"   classification = COALESCE(excluded.classification, incident_routes.classification),"+
		"   superseded_by = COALESCE(excluded.superseded_by, incident_routes.superseded_by),"+
		"   detail = excluded.detail, updated_at = excluded.updated_at",
		r.FaultID, r.ProductKey, r.Workspace, r.Disposition, r.Stage, r.Target, r.Origin,
		r.ClaimedSeverity, goal, r.Classification, r.SupersededBy, r.Detail, r.CreatedAt, r.UpdatedAt,
		boolInt(replaceGoal))
	return err
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

// IncidentRoute is routes.py:50 get.
func (s *Store) IncidentRoute(ctx context.Context, faultID string) (IncidentRoutesRow, error) {
	return queryRow(ctx, s, scanIncidentRoutes, "SELECT "+incidentRoutesColumns+" FROM incident_routes WHERE fault_id = ?", faultID)
}

// SetIncidentTarget is routes.py:159 set_target.
func (s *Store) SetIncidentTarget(ctx context.Context, faultID, target, at string) error {
	_, err := s.exec(ctx, "UPDATE incident_routes SET target = ?, updated_at = ? WHERE fault_id = ?", target, at, faultID)
	return err
}

// SettleIncidentRoute is routes.py:166 settle.
func (s *Store) SettleIncidentRoute(ctx context.Context, faultID, stage, detail, at string) error {
	_, err := s.exec(ctx, "UPDATE incident_routes SET stage = ?, detail = ?, updated_at = ? WHERE fault_id = ?", stage, detail, at, faultID)
	return err
}

// CheckIncidentRoute is routes.py:210 checked: the route goes to the back of the rotation.
func (s *Store) CheckIncidentRoute(ctx context.Context, faultID string) error {
	_, err := s.exec(ctx, "UPDATE incident_routes SET checked_seq = (SELECT COALESCE(MAX(checked_seq), 0) + 1 FROM incident_routes) WHERE fault_id = ?", faultID)
	return err
}

// SetIncidentReported is routes.py:216 set_reported.
func (s *Store) SetIncidentReported(ctx context.Context, faultID, reported, at string) error {
	_, err := s.exec(ctx, "UPDATE incident_routes SET reported = ?, updated_at = ? WHERE fault_id = ?", reported, at, faultID)
	return err
}

// StoreRouteIncident is routes.py:224 store_incident: the next sequence for the fault, the
// incident inserted (replace=false keeps an existing one, replace=true overwrites it), then only
// the newest *keep incidents retained. A nil keep is Python's keep=None and keeps every one; any
// other value is passed to LIMIT as Python passes it, so keep 0 deletes every incident of the fault.
// Python's default is MaxStoredIncidents.
func (s *Store) StoreRouteIncident(ctx context.Context, incidentID, faultID, record, at string, keep *int64, replace bool) error {
	var seq int64
	if err := s.q(ctx).QueryRowContext(ctx, "SELECT COALESCE(MAX(recorded_seq), 0) + 1 AS n FROM route_incidents"+
		" WHERE fault_id = ?", faultID).Scan(&seq); err != nil {
		return err
	}
	conflict := "NOTHING"
	if replace {
		conflict = "UPDATE SET record = excluded.record, recorded_at = excluded.recorded_at," +
			"   recorded_seq = excluded.recorded_seq"
	}
	if _, err := s.exec(ctx, "INSERT INTO route_incidents (incident_id, fault_id, record, recorded_at,"+
		"  recorded_seq) VALUES (?,?,?,?,?) ON CONFLICT(incident_id) DO "+conflict, incidentID, faultID, record, at, seq); err != nil {
		return err
	}
	if keep == nil {
		return nil
	}
	_, err := s.exec(ctx, "DELETE FROM route_incidents WHERE fault_id = ? AND incident_id NOT IN"+
		"  (SELECT incident_id FROM route_incidents WHERE fault_id = ?"+
		"    ORDER BY recorded_seq DESC LIMIT ?)", faultID, faultID, *keep)
	return err
}

// MaxStoredIncidents is routes.py:16 MAX_STORED_INCIDENTS, store_incident's default keep.
const MaxStoredIncidents int64 = 16

// RouteIncidents is routes.py:261 incidents, oldest first.
func (s *Store) RouteIncidents(ctx context.Context, faultID string) ([]RouteIncidentsRow, error) {
	return queryRows(ctx, s, scanRouteIncidents, "SELECT "+routeIncidentsColumns+" FROM route_incidents WHERE fault_id = ? ORDER BY recorded_seq", faultID)
}
