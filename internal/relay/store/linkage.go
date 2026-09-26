package store

import (
	"context"
	"database/sql"
)

// Linkage tables: scope_bindings, scope_links, scope_directives, linkage_conflicts,
// relationship_scope, and the coordination_conflicts table the merge-turn, capacity and
// edit-region modules share. Each query is the statement named beside it, with its columns,
// ordering and conflict clause unchanged.

// InsertScopeBinding is linkage.py:565 _insert_binding; superseded_by is always NULL.
func (s *Store) InsertScopeBinding(ctx context.Context, b ScopeBindingsRow) error {
	_, err := s.exec(ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"+
		" host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"+
		" handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)",
		b.BindingID, b.Role, b.ScopeKind, b.ScopeKey, b.TaskID, b.HostID, b.CWD, b.CXCSession,
		b.Status, b.Revision, b.Supersedes, b.HandoverNote, b.CreatedAt, b.UpdatedAt)
	return err
}

// ScopeBinding is linkage.py:220 Linkage.binding.
func (s *Store) ScopeBinding(ctx context.Context, bindingID string) (ScopeBindingsRow, error) {
	return queryRow(ctx, s, scanScopeBindings, "SELECT "+scopeBindingsColumns+" FROM scope_bindings WHERE binding_id = ?", bindingID)
}

// ScopeOwner is linkage.py:226 Linkage.owner: the newest live binding of a scope.
func (s *Store) ScopeOwner(ctx context.Context, scopeKind, scopeKey string) (ScopeBindingsRow, error) {
	return queryRow(ctx, s, scanScopeBindings, "SELECT "+scopeBindingsColumns+" FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL"+
		"  ORDER BY revision DESC LIMIT 1", scopeKind, scopeKey)
}

// ScopeOwners is linkage.py:245 Linkage.owners: every live binding, so a contest is visible.
func (s *Store) ScopeOwners(ctx context.Context, scopeKind, scopeKey string) ([]ScopeBindingsRow, error) {
	return queryRows(ctx, s, scanScopeBindings, "SELECT "+scopeBindingsColumns+" FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL"+
		"  ORDER BY revision DESC, binding_id", scopeKind, scopeKey)
}

// LiveOwnerTasks is linkage.py:262 _live_owners_in: the writer's view of a scope's owners.
func (s *Store) LiveOwnerTasks(ctx context.Context, scopeKind, scopeKey, role string) ([]string, error) {
	return queryRows(ctx, s, scanString, "SELECT task_id FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY task_id", scopeKind, scopeKey, role)
}

// RivalOwner is the rival read of linkage.py:505 _binding_refusal. replacing names the outgoing
// owner of a handover, or is NULL.
type RivalOwner struct{ BindingID, TaskID, Status string }

func (s *Store) RivalOwner(ctx context.Context, scopeKind, scopeKey, role, taskID string, replacing sql.NullString) (RivalOwner, error) {
	return queryRow(ctx, s, func(row scanner) (RivalOwner, error) {
		var r RivalOwner
		return r, row.Scan(&r.BindingID, &r.TaskID, &r.Status)
	}, "SELECT binding_id, task_id, status FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"    AND task_id != ? AND task_id IS NOT ?", scopeKind, scopeKey, role, taskID, replacing)
}

// BindingsForTask is linkage.py:1853 _bindings_for: live ones first, newest revision first.
func (s *Store) BindingsForTask(ctx context.Context, taskID string) ([]ScopeBindingsRow, error) {
	return queryRows(ctx, s, scanScopeBindings, "SELECT "+scopeBindingsColumns+" FROM scope_bindings WHERE task_id = ?"+
		"  ORDER BY CASE WHEN status IN ('active','paused') THEN 0 ELSE 1 END,"+
		"           revision DESC, scope_key", taskID)
}

// ReactivateScopeBinding is linkage.py:477: the endpoint comes back with the status.
func (s *Store) ReactivateScopeBinding(ctx context.Context, bindingID, status, at, hostID string, cwd, cxcSession sql.NullString) error {
	_, err := s.exec(ctx, "UPDATE scope_bindings SET status = ?, updated_at = ?, host_id = ?,"+
		"  cwd = ?, cxc_session = ? WHERE binding_id = ?", status, at, hostID, cwd, cxcSession, bindingID)
	return err
}

// SetScopeBindingStatus is linkage.py:1055; it leaves a binding already in status untouched.
func (s *Store) SetScopeBindingStatus(ctx context.Context, bindingID, status, at string) error {
	_, err := s.exec(ctx, "UPDATE scope_bindings SET status = ?, updated_at = ?"+
		"  WHERE binding_id = ? AND status != ?", status, at, bindingID, status)
	return err
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

// ScopeLink is linkage.py:312 Linkage.link.
func (s *Store) ScopeLink(ctx context.Context, linkID string) (ScopeLinksRow, error) {
	return queryRow(ctx, s, scanScopeLinks, "SELECT "+scopeLinksColumns+" FROM scope_links WHERE link_id = ?", linkID)
}

// JoiningLinks is linkage.py:1873 _joining_links: every live link between two scopes.
func (s *Store) JoiningLinks(ctx context.Context, senderKind, senderKey, recipientKind, recipientKey string) ([]ScopeLinksRow, error) {
	return queryRows(ctx, s, scanScopeLinks, "SELECT "+scopeLinksColumns+" FROM scope_links"+
		"  WHERE status IN ('active','paused') AND superseded_by IS NULL"+
		"    AND ((upper_kind = ? AND upper_key = ? AND lower_kind = ? AND lower_key = ?)"+
		"      OR (upper_kind = ? AND upper_key = ? AND lower_kind = ? AND lower_key = ?))"+
		"  ORDER BY revision DESC, link_id",
		senderKind, senderKey, recipientKind, recipientKey, recipientKind, recipientKey, senderKind, senderKey)
}

// ExecutionLinksBelow is linkage.py:1992: the live execution edges out of a scope.
func (s *Store) ExecutionLinksBelow(ctx context.Context, upperKind, upperKey string) ([]ScopeLinksRow, error) {
	return queryRows(ctx, s, scanScopeLinks, "SELECT "+scopeLinksColumns+" FROM scope_links"+
		"  WHERE upper_kind = ? AND upper_key = ? AND link_kind = 'execution'"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY lower_key", upperKind, upperKey)
}

// ExecutionLinksAbove is linkage.py:2069: the live execution edges into a scope.
func (s *Store) ExecutionLinksAbove(ctx context.Context, lowerKind, lowerKey string) ([]ScopeLinksRow, error) {
	return queryRows(ctx, s, scanScopeLinks, "SELECT "+scopeLinksColumns+" FROM scope_links"+
		"  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY revision DESC, link_id", lowerKind, lowerKey)
}

// RepointScopeLink is linkage.py:1075: reactivate an edge and repoint both of its tasks.
func (s *Store) RepointScopeLink(ctx context.Context, linkID, status, lowerTaskID, upperTaskID, at string) error {
	_, err := s.exec(ctx, "UPDATE scope_links SET status = ?, lower_task_id = ?, upper_task_id = ?,"+
		" revision = revision + 1, superseded_by = NULL, updated_at = ?"+
		"  WHERE link_id = ?", status, lowerTaskID, upperTaskID, at, linkID)
	return err
}

// SetScopeLinkStatus is linkage.py:1296.
func (s *Store) SetScopeLinkStatus(ctx context.Context, linkID, status, at string) error {
	_, err := s.exec(ctx, "UPDATE scope_links SET status = ?, updated_at = ? WHERE link_id = ?", status, at, linkID)
	return err
}

// InsertScopeDirective is linkage.py:1423; a directive is recorded undecided.
func (s *Store) InsertScopeDirective(ctx context.Context, d ScopeDirectivesRow) error {
	_, err := s.exec(ctx, "INSERT INTO scope_directives (directive_id, scope_kind, scope_key,"+
		" from_task_id, from_scope_key, link_id, link_kind, digest,"+
		" reference, revision, disposition, decided_by, decided_at,"+
		" recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,NULL,NULL,?)",
		d.DirectiveID, d.ScopeKind, d.ScopeKey, d.FromTaskID, d.FromScopeKey, d.LinkID, d.LinkKind,
		d.Digest, d.Reference, d.Revision, d.RecordedAt)
	return err
}

// ScopeDirective is linkage.py:1441.
func (s *Store) ScopeDirective(ctx context.Context, directiveID string) (ScopeDirectivesRow, error) {
	return queryRow(ctx, s, scanScopeDirectives, "SELECT "+scopeDirectivesColumns+" FROM scope_directives WHERE directive_id = ?", directiveID)
}

// ScopeDirectives is linkage.py:332 Linkage.directives.
func (s *Store) ScopeDirectives(ctx context.Context, scopeKind, scopeKey string) ([]ScopeDirectivesRow, error) {
	return queryRows(ctx, s, scanScopeDirectives, "SELECT "+scopeDirectivesColumns+" FROM scope_directives"+
		"  WHERE scope_kind = ? AND scope_key = ? ORDER BY recorded_at, directive_id", scopeKind, scopeKey)
}

// UndecidedDirectives is linkage.py:1469 _competitor_in.
func (s *Store) UndecidedDirectives(ctx context.Context, scopeKind, scopeKey string) ([]ScopeDirectivesRow, error) {
	return queryRows(ctx, s, scanScopeDirectives, "SELECT "+scopeDirectivesColumns+" FROM scope_directives"+
		"  WHERE scope_kind = ? AND scope_key = ? AND disposition IS NULL"+
		"  ORDER BY recorded_at, directive_id", scopeKind, scopeKey)
}

// SettleScopeDirective is linkage.py:1575.
func (s *Store) SettleScopeDirective(ctx context.Context, directiveID, disposition, decidedBy, at string) error {
	_, err := s.exec(ctx, "UPDATE scope_directives SET disposition = ?, decided_by = ?,"+
		"  decided_at = ? WHERE directive_id = ?", disposition, decidedBy, at, directiveID)
	return err
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

// LinkageConflicts is linkage.py:324 Linkage.conflicts.
func (s *Store) LinkageConflicts(ctx context.Context, scopeKind, scopeKey string) ([]LinkageConflictsRow, error) {
	return queryRows(ctx, s, scanLinkageConflicts, "SELECT "+linkageConflictsColumns+" FROM linkage_conflicts"+
		"  WHERE scope_kind = ? AND scope_key = ? ORDER BY id", scopeKind, scopeKey)
}

// RecordRelationshipScope is linkage.py:1061; the first project recorded for a relationship stays.
func (s *Store) RecordRelationshipScope(ctx context.Context, relationshipID, projectKey, at string) error {
	_, err := s.exec(ctx, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"+
		" VALUES (?,?,?) ON CONFLICT(relationship_id) DO NOTHING", relationshipID, projectKey, at)
	return err
}

// RelationshipScope is assignment.py:706: the project a relationship was attached to.
func (s *Store) RelationshipScope(ctx context.Context, relationshipID string) (RelationshipScopeRow, error) {
	return queryRow(ctx, s, scanRelationshipScope, "SELECT "+relationshipScopeColumns+" FROM relationship_scope WHERE relationship_id = ?", relationshipID)
}

// ScopedRelationships is omitted.py:640: every relationship attached to a project, oldest first.
func (s *Store) ScopedRelationships(ctx context.Context, projectKey string) ([]string, error) {
	return queryRows(ctx, s, scanString, "SELECT r.relationship_id FROM relationships r"+
		" JOIN relationship_scope s ON s.relationship_id = r.relationship_id"+
		" WHERE s.project_key = ? ORDER BY r.created_at", projectKey)
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

func scanString(row scanner) (string, error) {
	var value string
	return value, row.Scan(&value)
}
