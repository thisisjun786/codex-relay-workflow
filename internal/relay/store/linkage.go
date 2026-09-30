package store

import (
	"context"
)

// Linkage reads over scope_bindings, scope_links, linkage_conflicts and relationship_scope. The
// linkage writes are the registry's own SQL. Each query is the statement named beside it, with
// its columns and ordering unchanged.

// ScopeOwners is linkage.py:245 Linkage.owners: every live binding, so a contest is visible.
func (s *Store) ScopeOwners(ctx context.Context, scopeKind, scopeKey string) ([]ScopeBindingsRow, error) {
	return queryRows(ctx, s, scanScopeBindings, "SELECT "+scopeBindingsColumns+" FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL"+
		"  ORDER BY revision DESC, binding_id", scopeKind, scopeKey)
}

// ExecutionLinksAbove is linkage.py:2069: the live execution edges into a scope.
func (s *Store) ExecutionLinksAbove(ctx context.Context, lowerKind, lowerKey string) ([]ScopeLinksRow, error) {
	return queryRows(ctx, s, scanScopeLinks, "SELECT "+scopeLinksColumns+" FROM scope_links"+
		"  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY revision DESC, link_id", lowerKind, lowerKey)
}

// LinkageConflicts is linkage.py:324 Linkage.conflicts.
func (s *Store) LinkageConflicts(ctx context.Context, scopeKind, scopeKey string) ([]LinkageConflictsRow, error) {
	return queryRows(ctx, s, scanLinkageConflicts, "SELECT "+linkageConflictsColumns+" FROM linkage_conflicts"+
		"  WHERE scope_kind = ? AND scope_key = ? ORDER BY id", scopeKind, scopeKey)
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

func scanString(row scanner) (string, error) {
	var value string
	return value, row.Scan(&value)
}
