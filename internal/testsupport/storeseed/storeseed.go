// Package storeseed writes and reads relay store rows the product handles with its own SQL in
// the domain packages, for tests that need a row in place before the code under test runs. It
// is test support only: no product package imports it (decision 51).
package storeseed

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// InsertScopeBinding inserts one live scope_bindings row; superseded_by is always NULL.
func InsertScopeBinding(ctx context.Context, s *store.Store, b store.ScopeBindingsRow) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"+
		" host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"+
		" handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)",
		b.BindingID, b.Role, b.ScopeKind, b.ScopeKey, b.TaskID, b.HostID, b.CWD, b.CXCSession,
		b.Status, b.Revision, b.Supersedes, b.HandoverNote, b.CreatedAt, b.UpdatedAt)
	return err
}

// ArchiveScopeBinding is the outgoing half of a handover.
func ArchiveScopeBinding(ctx context.Context, s *store.Store, bindingID, archived, supersededBy, at string) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "UPDATE scope_bindings SET status = ?, superseded_by = ?, updated_at = ?"+
		"  WHERE binding_id = ?", archived, supersededBy, at, bindingID)
	return err
}

// InsertScopeLink inserts one scope_links row; superseded_by is always NULL.
func InsertScopeLink(ctx context.Context, s *store.Store, l store.ScopeLinksRow) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"+
		" upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"+
		" superseded_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
		l.LinkID, l.LinkKind, l.UpperKind, l.UpperKey, l.UpperTaskID, l.LowerKind, l.LowerKey,
		l.LowerTaskID, l.Status, l.Revision, l.CreatedAt, l.UpdatedAt)
	return err
}

// RepointScopeLink reactivates an edge and repoints both of its tasks.
func RepointScopeLink(ctx context.Context, s *store.Store, linkID, status, lowerTaskID, upperTaskID, at string) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "UPDATE scope_links SET status = ?, lower_task_id = ?, upper_task_id = ?,"+
		" revision = revision + 1, superseded_by = NULL, updated_at = ?"+
		"  WHERE link_id = ?", status, lowerTaskID, upperTaskID, at, linkID)
	return err
}

// SetScopeLinkStatus moves one link to status.
func SetScopeLinkStatus(ctx context.Context, s *store.Store, linkID, status, at string) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "UPDATE scope_links SET status = ?, updated_at = ? WHERE link_id = ?", status, at, linkID)
	return err
}

// RecordLinkageConflict records a contest; a retried loser converges on one row.
func RecordLinkageConflict(ctx context.Context, s *store.Store, c store.LinkageConflictsRow) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO linkage_conflicts (at, scope_kind, scope_key, reason, incumbent,"+
		" challenger, detail) VALUES (?,?,?,?,?,?,?)"+
		" ON CONFLICT(scope_kind, scope_key, reason, incumbent, challenger)"+
		"   DO UPDATE SET at = excluded.at, detail = excluded.detail",
		c.At, c.ScopeKind, c.ScopeKey, c.Reason, c.Incumbent, c.Challenger, c.Detail)
	return err
}

// RecordRelationshipScope attaches a relationship to a project; the first project stays.
func RecordRelationshipScope(ctx context.Context, s *store.Store, relationshipID, projectKey, at string) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at)"+
		" VALUES (?,?,?) ON CONFLICT(relationship_id) DO NOTHING", relationshipID, projectKey, at)
	return err
}

// RecordRelationship creates the assignment and its first generation in one transaction. Before it
// opens the transaction it refuses, as the store's own RecordRelationship did until refactor R1
// moved it here, what the product never writes: a dispatch turn id that is present but blank, and a
// bound generation without a dispatch turn id. Both refusals are store.ReasonUnboundGeneration and
// write nothing, so a test cannot start from a state the product cannot reach.
func RecordRelationship(ctx context.Context, s *store.Store, relationship store.Relationship, generation store.Generation, parentHostID, childHostID string) error {
	if turn := generation.DispatchTurnID; turn.Valid && strings.TrimSpace(turn.String) == "" {
		return unboundGeneration("an anchor needs an exact dispatch turn id, not %q", turn.String)
	}
	if generation.AnchorState == store.AnchorBound && !generation.DispatchTurnID.Valid {
		return unboundGeneration("a bound generation needs its dispatch turn id")
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

// unboundGeneration is the refusal the store's RecordRelationship made for a generation with no
// usable dispatch turn id; store.RefusalReason reads its reason.
func unboundGeneration(format string, args ...any) error {
	return &store.RefusedError{Reason: store.ReasonUnboundGeneration, Detail: fmt.Sprintf(format, args...)}
}

// AppendJournal writes one journal row in its own transaction.
func AppendJournal(ctx context.Context, s *store.Store, entry store.JournalEntry) error {
	return s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO journal (at, kind, subject, detail)
   VALUES (?,?,?,?)`, entry.At, entry.Kind, entry.Subject, entry.Detail)
		return err
	})
}

// AuthorizedSettings is one authorized_settings row.
type AuthorizedSettings struct{ TaskID, Settings, Source, RecordedAt string }

// ReadAuthorizedSettings reads one task's authorized_settings row.
func ReadAuthorizedSettings(ctx context.Context, s *store.Store, taskID string) (AuthorizedSettings, error) {
	var r AuthorizedSettings
	err := s.Querier(ctx).QueryRowContext(ctx, "SELECT task_id, settings, source, recorded_at FROM authorized_settings WHERE task_id = ?", taskID).
		Scan(&r.TaskID, &r.Settings, &r.Source, &r.RecordedAt)
	return r, err
}

// EligibleSupervisorMessages lists the claimable supervisor messages due at now, in staging
// order: queued, deferred_busy or withheld_pre_send, with no hold and no later eligibility.
func EligibleSupervisorMessages(ctx context.Context, s *store.Store, now float64, limit int) ([]store.SupervisorMessagesRow, error) {
	ids, err := s.All(ctx, "SELECT message_id FROM supervisor_messages"+
		" WHERE state IN ('queued','deferred_busy','withheld_pre_send') AND hold_reason IS NULL"+
		"   AND (next_eligible_at IS NULL OR next_eligible_at <= ?)"+
		" ORDER BY staged_at, message_id LIMIT ?", now, limit)
	if err != nil {
		return nil, err
	}
	var rows []store.SupervisorMessagesRow
	for _, id := range ids {
		row, err := s.SupervisorMessage(ctx, id.Get("message_id").(string))
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}
