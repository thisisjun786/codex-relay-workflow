package store

import (
	"context"
	"database/sql"
	"errors"
)

// Fault tables: fault_ledger, fault_aliases, fault_targets, fault_target_projects,
// fault_occurrences, fault_timeline, fault_adoptions, fault_policies, fault_remediations,
// fault_publications, fault_publication_attempts, fault_publication_payloads, fault_links,
// fault_budget_uses, fault_limits, fault_notifications, fault_cursors.

// OpenFaultLedger is faults.py:1175: a fault starts at cycle 1 with no occurrence counted.
func (s *Store) OpenFaultLedger(ctx context.Context, f FaultLedgerRow) error {
	_, err := s.exec(ctx, "INSERT INTO fault_ledger (fault_id, product, fault_class, component,"+
		"  severity, signature, scope, scope_key, state, cycle, occurrence_count,"+
		"  reopen_count, detail, suppression, first_seen_at, last_seen_at,"+
		"  updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,1,0,0,?,?,?,?,?)",
		f.FaultID, f.Product, f.FaultClass, f.Component, f.Severity, f.Signature, f.Scope, f.ScopeKey,
		f.State, f.Detail, f.Suppression, f.FirstSeenAt, f.LastSeenAt, f.UpdatedAt)
	return err
}

// FaultLedger is faults.py:3337 (every column).
func (s *Store) FaultLedger(ctx context.Context, faultID string) (FaultLedgerRow, error) {
	return queryRow(ctx, s, scanFaultLedger, "SELECT "+faultLedgerColumns+" FROM fault_ledger WHERE fault_id = ?", faultID)
}

// ObserveFault is faults.py:1258: an occurrence moves the fault's state and counters.
func (s *Store) ObserveFault(ctx context.Context, faultID, state string, cycle int64, severity string, episode, occurrenceCount, reopened int64, detail sql.NullString, suppression, lastSeenAt string, clearedAt, resolvedAt sql.NullString, at string) error {
	_, err := s.exec(ctx, "UPDATE fault_ledger SET state = ?, cycle = ?, severity = ?, episode = ?,"+
		"  occurrence_count = ?, reopen_count = reopen_count + ?, detail = ?,"+
		"  suppression = ?, last_seen_at = ?, cleared_at = ?, resolved_at = ?,"+
		"  updated_at = ? WHERE fault_id = ?", state, cycle, severity, episode, occurrenceCount, reopened,
		detail, suppression, lastSeenAt, clearedAt, resolvedAt, at, faultID)
	return err
}

// SetFaultState is faults.py:1602.
func (s *Store) SetFaultState(ctx context.Context, faultID, state, at string) error {
	_, err := s.exec(ctx, "UPDATE fault_ledger SET state = ?, updated_at = ? WHERE fault_id = ?", state, at, faultID)
	return err
}

// MaterializeFaultRef is faults.py:1348: a fault keeps the first external reference it gets.
func (s *Store) MaterializeFaultRef(ctx context.Context, faultID, externalRef, at string) error {
	_, err := s.exec(ctx, "UPDATE fault_ledger SET external_ref = ?, updated_at = ?"+
		" WHERE fault_id = ? AND external_ref IS NULL", externalRef, at, faultID)
	return err
}

// FaultAliasTarget is faults.py:766 _canonical: an alias resolved, anything else unchanged.
func (s *Store) FaultAliasTarget(ctx context.Context, identifier string) (string, error) {
	target, err := queryRow(ctx, s, scanString, "SELECT fault_id FROM fault_aliases WHERE alias_id = ?", identifier)
	if errors.Is(err, sql.ErrNoRows) {
		return identifier, nil
	}
	return target, err
}

// RecordFaultAlias is faults.py:807.
func (s *Store) RecordFaultAlias(ctx context.Context, aliasID, faultID, at string) error {
	_, err := s.exec(ctx, "INSERT INTO fault_aliases (alias_id, fault_id, created_at) VALUES (?,?,?)", aliasID, faultID, at)
	return err
}

// FaultAlias reads one alias row.
func (s *Store) FaultAlias(ctx context.Context, aliasID string) (FaultAliasesRow, error) {
	return queryRow(ctx, s, scanFaultAliases, "SELECT "+faultAliasesColumns+" FROM fault_aliases WHERE alias_id = ?", aliasID)
}

// SetFaultTarget is faults.py:892 FaultLedger.set_target: the team and the project together.
func (s *Store) SetFaultTarget(ctx context.Context, scopeKey, trackerRef, product string, projectRef sql.NullString, at string) error {
	if _, err := s.exec(ctx, "INSERT INTO fault_targets (scope_key, tracker_ref, recorded_at) VALUES (?,?,?)"+
		" ON CONFLICT(scope_key) DO UPDATE SET tracker_ref = excluded.tracker_ref,"+
		"   recorded_at = excluded.recorded_at", scopeKey, trackerRef, at); err != nil {
		return err
	}
	_, err := s.exec(ctx, "INSERT INTO fault_target_projects (scope_key, product, project_ref, recorded_at)"+
		" VALUES (?,?,?,?) ON CONFLICT(scope_key) DO UPDATE SET"+
		"   product = excluded.product, project_ref = excluded.project_ref,"+
		"   recorded_at = excluded.recorded_at", scopeKey, product, projectRef, at)
	return err
}

// FaultTarget is faults.py:919 target_for.
type FaultTarget struct {
	ScopeKey   string
	TrackerRef string
	Product    sql.NullString
	ProjectRef sql.NullString
}

func (s *Store) FaultTarget(ctx context.Context, scopeKey string) (FaultTarget, error) {
	return queryRow(ctx, s, func(row scanner) (FaultTarget, error) {
		var t FaultTarget
		return t, row.Scan(&t.ScopeKey, &t.TrackerRef, &t.Product, &t.ProjectRef)
	}, "SELECT t.scope_key, t.tracker_ref, p.product, p.project_ref"+
		" FROM fault_targets t LEFT JOIN fault_target_projects p ON p.scope_key = t.scope_key"+
		" WHERE t.scope_key = ?", scopeKey)
}

// FaultTargetTeam is the fault_targets half of faults.py:837 (every column).
func (s *Store) FaultTargetTeam(ctx context.Context, scopeKey string) (FaultTargetsRow, error) {
	return queryRow(ctx, s, scanFaultTargets, "SELECT "+faultTargetsColumns+" FROM fault_targets WHERE scope_key = ?", scopeKey)
}

// FaultTargetProject is faults.py:3735 (every column).
func (s *Store) FaultTargetProject(ctx context.Context, scopeKey string) (FaultTargetProjectsRow, error) {
	return queryRow(ctx, s, scanFaultTargetProjects, "SELECT "+faultTargetProjectsColumns+" FROM fault_target_projects WHERE scope_key = ?", scopeKey)
}

// RecordFaultOccurrence is faults.py:1222: one occurrence per (fault, episode, key). It reports
// whether this call recorded it.
func (s *Store) RecordFaultOccurrence(ctx context.Context, o FaultOccurrencesRow) (bool, error) {
	return s.changedOne(ctx, "INSERT OR IGNORE INTO fault_occurrences (occurrence_id, fault_id, episode,"+
		"  occurrence_key, severity, cleared, detail, evidence, evidence_digest,"+
		"  truncated, observed_at, recorded_at, recorded_ts)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
		o.OccurrenceID, o.FaultID, o.Episode, o.OccurrenceKey, o.Severity, o.Cleared, o.Detail,
		o.Evidence, o.EvidenceDigest, o.Truncated, o.ObservedAt, o.RecordedAt, o.RecordedTS)
}

// FaultOccurrences is faults.py:1077 occurrences; newest first when newest, else oldest first.
func (s *Store) FaultOccurrences(ctx context.Context, faultID string, limit int, newest bool) ([]FaultOccurrencesRow, error) {
	order := "ASC"
	if newest {
		order = "DESC"
	}
	return queryRows(ctx, s, scanFaultOccurrences, "SELECT "+faultOccurrencesColumns+" FROM fault_occurrences WHERE fault_id = ? ORDER BY rowid "+order+
		" LIMIT ?", faultID, limit)
}

// PruneFaultOccurrences is faults.py:1708: only the newest keep survive. It returns the count removed.
func (s *Store) PruneFaultOccurrences(ctx context.Context, faultID string, keep int) (int64, error) {
	result, err := s.exec(ctx, "DELETE FROM fault_occurrences WHERE fault_id = ? AND rowid NOT IN"+
		"  (SELECT rowid FROM fault_occurrences WHERE fault_id = ?"+
		"    ORDER BY rowid DESC LIMIT ?)", faultID, faultID, keep)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// AppendFaultTimeline is faults.py:1238/1532/1598.
func (s *Store) AppendFaultTimeline(ctx context.Context, t FaultTimelineRow) error {
	_, err := s.exec(ctx, "INSERT INTO fault_timeline (fault_id, cycle, kind, ref_id, detail,"+
		"  recorded_at, recorded_ts) VALUES (?,?,?,?,?,?,?)",
		t.FaultID, t.Cycle, t.Kind, t.RefID, t.Detail, t.RecordedAt, t.RecordedTS)
	return err
}

// FaultTimelineCount is faults.py:1133.
func (s *Store) FaultTimelineCount(ctx context.Context, faultID, kind string) (int64, error) {
	var n int64
	err := s.q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) AS n FROM fault_timeline WHERE fault_id = ? AND kind = ?", faultID, kind).Scan(&n)
	return n, err
}

// FaultTimeline reads a fault's timeline in sequence order.
func (s *Store) FaultTimeline(ctx context.Context, faultID string) ([]FaultTimelineRow, error) {
	return queryRows(ctx, s, scanFaultTimeline, "SELECT "+faultTimelineColumns+" FROM fault_timeline WHERE fault_id = ? ORDER BY seq", faultID)
}

// RecordFaultAdoption is faults.py:1333: the first adoption of a fault stands.
func (s *Store) RecordFaultAdoption(ctx context.Context, a FaultAdoptionsRow) error {
	_, err := s.exec(ctx, "INSERT INTO fault_adoptions (fault_id, external_ref, scope, state, created_at,"+
		"  updated_at) VALUES (?,?,?,?,?,?) ON CONFLICT(fault_id) DO NOTHING",
		a.FaultID, a.ExternalRef, a.Scope, a.State, a.CreatedAt, a.UpdatedAt)
	return err
}

// FaultAdoption is faults.py:1319.
func (s *Store) FaultAdoption(ctx context.Context, faultID string) (FaultAdoptionsRow, error) {
	return queryRow(ctx, s, scanFaultAdoptions, "SELECT "+faultAdoptionsColumns+" FROM fault_adoptions WHERE fault_id = ?", faultID)
}

// MaterializeFaultAdoption is faults.py:1351.
func (s *Store) MaterializeFaultAdoption(ctx context.Context, faultID, at string) error {
	_, err := s.exec(ctx, "UPDATE fault_adoptions SET state = 'materialized', updated_at = ?"+
		" WHERE fault_id = ?", at, faultID)
	return err
}

// SetFaultPolicy is faults.py:1412.
func (s *Store) SetFaultPolicy(ctx context.Context, p FaultPoliciesRow) error {
	_, err := s.exec(ctx, "INSERT INTO fault_policies (product, fault_class, severity, threshold,"+
		"  window_seconds, reason, updated_at) VALUES (?,?,?,?,?,?,?)"+
		" ON CONFLICT(product, fault_class, severity) DO UPDATE SET"+
		"   threshold = excluded.threshold, window_seconds = excluded.window_seconds,"+
		"   reason = excluded.reason, updated_at = excluded.updated_at",
		p.Product, p.FaultClass, p.Severity, p.Threshold, p.WindowSeconds, p.Reason, p.UpdatedAt)
	return err
}

// FaultPolicy is faults.py:1366 (with the key columns).
func (s *Store) FaultPolicy(ctx context.Context, product, faultClass, severity string) (FaultPoliciesRow, error) {
	return queryRow(ctx, s, scanFaultPolicies, "SELECT "+faultPoliciesColumns+" FROM fault_policies"+
		" WHERE product = ? AND fault_class = ? AND severity = ?", product, faultClass, severity)
}

// RecordFaultRemediation is faults.py:1588: one remediation id is recorded once. It reports
// whether this call recorded it.
func (s *Store) RecordFaultRemediation(ctx context.Context, r FaultRemediationsRow) (bool, error) {
	return s.changedOne(ctx, "INSERT OR IGNORE INTO fault_remediations (remediation_id, fault_id, cycle,"+
		"  kind, ref, method, outcome, detail, recorded_at) VALUES (?,?,?,?,?,?,?,?,?)",
		r.RemediationID, r.FaultID, r.Cycle, r.Kind, r.Ref, r.Method, r.Outcome, r.Detail, r.RecordedAt)
}

// FaultRemediations is faults.py:1087 remediations: the newest limit, returned oldest first.
func (s *Store) FaultRemediations(ctx context.Context, faultID string, limit int) ([]FaultRemediationsRow, error) {
	rows, err := queryRows(ctx, s, scanFaultRemediations, "SELECT "+faultRemediationsColumns+" FROM fault_remediations WHERE fault_id = ? ORDER BY rowid DESC LIMIT ?", faultID, limit)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, err
}

// QueueFaultPublication is faults.py:1772: a write is queued with no attempt made.
func (s *Store) QueueFaultPublication(ctx context.Context, p FaultPublicationsRow) error {
	_, err := s.exec(ctx, "INSERT INTO fault_publications (publication_id, fault_id, kind, trigger_key,"+
		"  cycle, tracker_ref, external_ref, summary, identity_digest, state, attempts,"+
		"  created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,0,?,?)",
		p.PublicationID, p.FaultID, p.Kind, p.TriggerKey, p.Cycle, p.TrackerRef, p.ExternalRef,
		p.Summary, p.IdentityDigest, p.State, p.CreatedAt, p.UpdatedAt)
	return err
}

// FaultPublication is faults.py:2953.
func (s *Store) FaultPublication(ctx context.Context, publicationID string) (FaultPublicationsRow, error) {
	return queryRow(ctx, s, scanFaultPublications, "SELECT "+faultPublicationsColumns+" FROM fault_publications WHERE publication_id = ?", publicationID)
}

// FaultPublications is faults.py:2101 publications; an empty kind or state matches any.
func (s *Store) FaultPublications(ctx context.Context, faultID, kind, state string, after int64, limit int) ([]FaultPublicationsRow, error) {
	return queryRows(ctx, s, scanFaultPublications, "SELECT "+faultPublicationsColumns+" FROM fault_publications WHERE fault_id = ?"+
		" AND (? IS NULL OR kind = ?) AND (? IS NULL OR state = ?) AND rowid > ?"+
		" ORDER BY rowid LIMIT ?", faultID, nullText(kind), nullText(kind), nullText(state), nullText(state), after, limit)
}

func nullText(value string) sql.NullString {
	return sql.NullString{String: value, Valid: value != ""}
}

// ClaimFaultPublication is faults.py:2427.
func (s *Store) ClaimFaultPublication(ctx context.Context, publicationID, claimed, token, owner string, leaseUntil float64, attempts int64, at string) error {
	_, err := s.exec(ctx, "UPDATE fault_publications SET state = ?, claim_token = ?, lease_owner = ?,"+
		"  lease_until = ?, attempts = ?, updated_at = ? WHERE publication_id = ?", claimed, token, owner, leaseUntil, attempts, at, publicationID)
	return err
}

// RecordFaultPublicationAttempt is faults.py:2415; it returns the new attempt_id.
func (s *Store) RecordFaultPublicationAttempt(ctx context.Context, a FaultPublicationAttemptsRow) (int64, error) {
	result, err := s.exec(ctx, "INSERT INTO fault_publication_attempts (publication_id, attempt, owner,"+
		"  takeover, claimed_at, claimed_ts) VALUES (?,?,?,?,?,?)",
		a.PublicationID, a.Attempt, a.Owner, a.Takeover, a.ClaimedAt, a.ClaimedTS)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// IssueCurrentFaultAttempt is faults.py:2516 on the publication's newest attempt.
func (s *Store) IssueCurrentFaultAttempt(ctx context.Context, publicationID, at string, moment float64) error {
	_, err := s.exec(ctx, "UPDATE fault_publication_attempts SET issued_at = ?, issued_ts = ?"+
		" WHERE attempt_id = (SELECT MAX(attempt_id) FROM fault_publication_attempts"+
		" WHERE publication_id = ?)", at, moment, publicationID)
	return err
}

// FaultPublicationAttempts is faults.py:2126 _attempts: the newest limit, returned oldest first.
func (s *Store) FaultPublicationAttempts(ctx context.Context, publicationID string, limit int) ([]FaultPublicationAttemptsRow, error) {
	rows, err := queryRows(ctx, s, scanFaultPublicationAttempts, "SELECT "+faultPublicationAttemptsColumns+" FROM fault_publication_attempts WHERE publication_id = ?"+
		" ORDER BY attempt_id DESC LIMIT ?", publicationID, limit)
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, err
}

// WriteFaultPublicationPayload is faults.py:3621: the latest payload replaces every column.
func (s *Store) WriteFaultPublicationPayload(ctx context.Context, p FaultPublicationPayloadsRow) error {
	_, err := s.exec(ctx, "INSERT INTO fault_publication_payloads (publication_id, project_ref, payload,"+
		"  hold_reason, updated_at, target_mode) VALUES (?,?,?,?,?,?)"+
		" ON CONFLICT(publication_id) DO UPDATE"+
		" SET project_ref = excluded.project_ref, payload = excluded.payload,"+
		"   hold_reason = excluded.hold_reason, updated_at = excluded.updated_at,"+
		"   target_mode = excluded.target_mode",
		p.PublicationID, p.ProjectRef, p.Payload, p.HoldReason, p.UpdatedAt, p.TargetMode)
	return err
}

// FaultPublicationPayload is faults.py:3605.
func (s *Store) FaultPublicationPayload(ctx context.Context, publicationID string) (FaultPublicationPayloadsRow, error) {
	return queryRow(ctx, s, scanFaultPublicationPayloads, "SELECT "+faultPublicationPayloadsColumns+" FROM fault_publication_payloads WHERE publication_id = ?", publicationID)
}

// OpenFaultLink is faults.py:1946: a link starts at revision 0.
func (s *Store) OpenFaultLink(ctx context.Context, l FaultLinksRow) error {
	_, err := s.exec(ctx, "INSERT INTO fault_links (fault_id, external_ref, project_ref,"+
		"  observed_project_ref, state, revision, updated_at) VALUES (?,?,?,?,?,0,?)",
		l.FaultID, l.ExternalRef, l.ProjectRef, l.ObservedProjectRef, l.State, l.UpdatedAt)
	return err
}

// FaultLink is faults.py:1047.
func (s *Store) FaultLink(ctx context.Context, faultID string) (FaultLinksRow, error) {
	return queryRow(ctx, s, scanFaultLinks, "SELECT "+faultLinksColumns+" FROM fault_links WHERE fault_id = ?", faultID)
}

// SetFaultLinkProject is faults.py:1955.
func (s *Store) SetFaultLinkProject(ctx context.Context, faultID string, projectRef sql.NullString, state, at string) error {
	_, err := s.exec(ctx, "UPDATE fault_links SET project_ref = ?, state = ?, updated_at = ?"+
		" WHERE fault_id = ?", projectRef, state, at, faultID)
	return err
}

// ConsumeFaultBudget is faults.py:2156; UNIQUE(product, kind, ref) consumes one ref once.
func (s *Store) ConsumeFaultBudget(ctx context.Context, product, kind, ref, at string, moment float64) error {
	_, err := s.exec(ctx, "INSERT INTO fault_budget_uses (product, kind, ref, used_at, used_ts)"+
		" VALUES (?,?,?,?,?)", product, kind, ref, at, moment)
	return err
}

// FaultBudgetUsed is faults.py:2142: units spent after since.
func (s *Store) FaultBudgetUsed(ctx context.Context, product, kind string, since float64) (int64, error) {
	var n int64
	err := s.q(ctx).QueryRowContext(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product = ? AND kind = ?"+
		" AND used_ts > ?", product, kind, since).Scan(&n)
	return n, err
}

// FaultBudgetUse reads the use one ref consumed (faults.py:2148 asks whether it exists).
func (s *Store) FaultBudgetUse(ctx context.Context, product, kind, ref string) (FaultBudgetUsesRow, error) {
	return queryRow(ctx, s, scanFaultBudgetUses, "SELECT "+faultBudgetUsesColumns+" FROM fault_budget_uses WHERE product = ? AND kind = ?"+
		" AND ref = ?", product, kind, ref)
}

// ReturnFaultBudget is faults.py:2874/3301: an unspent reservation gives its unit back.
func (s *Store) ReturnFaultBudget(ctx context.Context, product, kind, ref string) error {
	_, err := s.exec(ctx, "DELETE FROM fault_budget_uses WHERE product = ? AND kind = ?"+
		" AND ref = ?", product, kind, ref)
	return err
}

// SetFaultLimit is faults.py:2188.
func (s *Store) SetFaultLimit(ctx context.Context, product, kind string, maxCount int64, window float64, at string) error {
	_, err := s.exec(ctx, "INSERT INTO fault_limits (product, kind, max_count, window_seconds, updated_at)"+
		" VALUES (?,?,?,?,?) ON CONFLICT(product, kind) DO UPDATE SET"+
		"   max_count = excluded.max_count, window_seconds = excluded.window_seconds,"+
		"   updated_at = excluded.updated_at", product, kind, maxCount, window, at)
	return err
}

// FaultLimit is faults.py:2134 (with the key columns).
func (s *Store) FaultLimit(ctx context.Context, product, kind string) (FaultLimitsRow, error) {
	return queryRow(ctx, s, scanFaultLimits, "SELECT "+faultLimitsColumns+" FROM fault_limits"+
		" WHERE product = ? AND kind = ?", product, kind)
}

// RaiseFaultNotification is faults.py:3048 _notify: a new notification is pending; an existing
// one is raised again only when it was withdrawn, and nothing else about it changes.
func (s *Store) RaiseFaultNotification(ctx context.Context, n FaultNotificationsRow, pending, withdrawn string) error {
	_, err := s.exec(ctx, "INSERT INTO fault_notifications (notification_id, fault_id, product,"+
		"  kind, reason, cycle, ref, state, created_at, updated_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,?)"+
		" ON CONFLICT(notification_id) DO UPDATE SET state = excluded.state,"+
		"   last_error = NULL, updated_at = excluded.updated_at"+
		" WHERE fault_notifications.state = ?",
		n.NotificationID, n.FaultID, n.Product, n.Kind, n.Reason, n.Cycle, n.Ref, pending, n.CreatedAt, n.UpdatedAt, withdrawn)
	return err
}

// FaultNotification is faults.py:3248.
func (s *Store) FaultNotification(ctx context.Context, notificationID string) (FaultNotificationsRow, error) {
	return queryRow(ctx, s, scanFaultNotifications, "SELECT "+faultNotificationsColumns+" FROM fault_notifications WHERE notification_id = ?", notificationID)
}

// FaultNotificationsInState is faults.py:3104 notifications (rowid order, after a rowid).
func (s *Store) FaultNotificationsInState(ctx context.Context, state string, after int64, limit int) ([]FaultNotificationsRow, error) {
	return queryRows(ctx, s, scanFaultNotifications, "SELECT "+faultNotificationsColumns+" FROM fault_notifications WHERE state = ? AND rowid > ?"+
		" ORDER BY rowid LIMIT ?", state, after, limit)
}

// ReserveFaultNotification is faults.py:3193.
func (s *Store) ReserveFaultNotification(ctx context.Context, notificationID, reserved, token, owner string, leaseUntil float64, at string) error {
	_, err := s.exec(ctx, "UPDATE fault_notifications SET state = ?, token = ?, owner = ?,"+
		"  lease_until = ?, attempts = attempts + 1, updated_at = ?"+
		" WHERE notification_id = ?", reserved, token, owner, leaseUntil, at, notificationID)
	return err
}

// DeliverFaultNotification is faults.py:3293.
func (s *Store) DeliverFaultNotification(ctx context.Context, notificationID, delivered, at, ref string) error {
	_, err := s.exec(ctx, "UPDATE fault_notifications SET state = ?, token = NULL,"+
		" delivered_at = ?, ack_ref = ?, updated_at = ?"+
		" WHERE notification_id = ?", delivered, at, ref, at, notificationID)
	return err
}

// LapseFaultNotifications is faults.py:3075: an expired reservation becomes uncertain.
func (s *Store) LapseFaultNotifications(ctx context.Context, uncertain, at, reserved string, moment float64) error {
	_, err := s.exec(ctx, "UPDATE fault_notifications SET state = ?, token = NULL, updated_at = ?"+
		" WHERE state = ? AND lease_until <= ?", uncertain, at, reserved, moment)
	return err
}

// WriteFaultCursor is faultsweep.py:1615 write_cursors, for one source: a moved cursor restarts its page count.
func (s *Store) WriteFaultCursor(ctx context.Context, source string, position sql.NullString, at string) error {
	_, err := s.exec(ctx, "INSERT INTO fault_cursors (source, position, pages, updated_at)"+
		" VALUES (?,?,0,?)"+
		" ON CONFLICT(source) DO UPDATE SET position = excluded.position,"+
		"   pages = 0, updated_at = excluded.updated_at", source, position, at)
	return err
}

// FaultCursors is faultsweep.py:1600 read_cursors (every column).
func (s *Store) FaultCursors(ctx context.Context) ([]FaultCursorsRow, error) {
	return queryRows(ctx, s, scanFaultCursors, "SELECT "+faultCursorsColumns+" FROM fault_cursors")
}
