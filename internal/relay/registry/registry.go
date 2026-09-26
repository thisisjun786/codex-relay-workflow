// Package registry ports registry.py, settings.py and rolepolicy.py: relationships, generations,
// anchors, lifecycle, execution-settings records and the role consistency check on them.
package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const (
	Active        = "active"
	AnchorBound   = "bound"
	AnchorPending = "anchor_pending"
)

var reasons = []string{"initial_assignment", "needs_changes_revision"}

func isLive(status string) bool { return status == "active" || status == "paused" }

// Endpoint is models.Endpoint; empty Cwd/CXCSession are None.
type Endpoint struct {
	TaskID, HostID  string
	Cwd, CXCSession sql.NullString
}

// Registry is registry.Registry over one store and one clock.
type Registry struct {
	Store *store.Store
	// Now is clock.iso(); SystemISO when nil.
	Now func() string
	// Policy is this process's role-policy snapshot (rolepolicy.declared()).
	Policy RolePolicy
}

// SystemISO is SystemClock.iso: UTC, microseconds, "+00:00".
func SystemISO() string { return ISO(time.Now()) }

// ISO renders t as Python's datetime.isoformat(timespec="microseconds") in UTC.
func ISO(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000") + "+00:00" }

func (r *Registry) now() string {
	if r.Now == nil {
		return SystemISO()
	}
	return r.Now()
}

// refuse builds a RelayError-shaped refusal (store.RefusedError).
func refuse(reason contract.RefusalReason, format string, args ...any) error {
	return &store.RefusedError{Reason: string(reason), Detail: fmt.Sprintf(format, args...)}
}

func journal(ctx context.Context, s *store.Store, kind, subject string, detail any, at string) error {
	text, ok := detail.(string)
	if !ok {
		text = pyDumps(detail, false)
	}
	_, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)", at, kind, subject, text)
	if err != nil {
		return fmt.Errorf("journal %s: %w", kind, err)
	}
	return nil
}

// row is one relationships row, every column.
type row struct {
	ID, Issue, Status, ParentTask, ParentHost string
	ParentCwd, ParentCXC                      sql.NullString
	ChildTask, ChildHost                      string
	ChildCwd, ChildCXC                        sql.NullString
	Generation                                int64
	Roots, Recipients                         string
	ScopeRef, Supersedes, SupersededBy        sql.NullString
	CreatedAt, UpdatedAt                      string
}

const rowColumns = "relationship_id, issue_key, status, parent_task_id, parent_host_id, parent_cwd, parent_cxc_session," +
	" child_task_id, child_host_id, child_cwd, child_cxc_session, execution_generation, artifact_roots, allowed_recipients," +
	" scope_ref, supersedes, superseded_by, created_at, updated_at"

func readRow(ctx context.Context, s *store.Store, rid string) (*row, error) {
	var r row
	err := s.Querier(ctx).QueryRowContext(ctx, "SELECT "+rowColumns+" FROM relationships WHERE relationship_id = ?", rid).Scan(
		&r.ID, &r.Issue, &r.Status, &r.ParentTask, &r.ParentHost, &r.ParentCwd, &r.ParentCXC,
		&r.ChildTask, &r.ChildHost, &r.ChildCwd, &r.ChildCXC, &r.Generation, &r.Roots, &r.Recipients,
		&r.ScopeRef, &r.Supersedes, &r.SupersededBy, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("relationship %q: %w", rid, err)
	}
	return &r, nil
}

// Generation is registry._generation_record.
type Generation struct {
	RelationshipID    string
	Number            int64
	DispatchRequestID string
	AnchorState       string
	DispatchTurnID    sql.NullString
	OpenedAt          string
	BoundAt, Reason   sql.NullString
}

// Record is _generation_record as JSON (with relationshipId).
func (g Generation) Record() contract.OrderedObject {
	return append(contract.OrderedObject{{Key: "relationshipId", Value: g.RelationshipID}}, g.contract()...)
}

func (g Generation) contract() contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "executionGeneration", Value: g.Number},
		{Key: "dispatchRequestId", Value: g.DispatchRequestID},
		{Key: "anchorState", Value: g.AnchorState},
		{Key: "dispatchTurnId", Value: nullable(g.DispatchTurnID)},
		{Key: "openedAt", Value: g.OpenedAt},
		{Key: "boundAt", Value: nullable(g.BoundAt)},
		{Key: "reason", Value: nullable(g.Reason)},
	}
}

func nullable(v sql.NullString) any {
	if !v.Valid {
		return nil
	}
	return v.String
}

func text(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }

const generationColumns = "relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, opened_at, bound_at, reason"

func scanGeneration(scan func(...any) error) (Generation, error) {
	var g Generation
	err := scan(&g.RelationshipID, &g.Number, &g.DispatchRequestID, &g.AnchorState, &g.DispatchTurnID, &g.OpenedAt, &g.BoundAt, &g.Reason)
	return g, err
}

// Generations is registry.generations: every generation in order.
func (r *Registry) Generations(ctx context.Context, rid string) ([]Generation, error) {
	rows, err := r.Store.Querier(ctx).QueryContext(ctx, "SELECT "+generationColumns+" FROM generations WHERE relationship_id = ? ORDER BY execution_generation", rid)
	if err != nil {
		return nil, err
	}
	var out []Generation
	for rows.Next() {
		g, err := scanGeneration(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, g)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

// Relationship is registry._row_to_record: the internal record, bindings included.
type Relationship struct {
	ID, IssueKey, Status, CreatedAt string
	Parent, Child                   Endpoint
	Generation                      int64
	Generations                     []Generation
	Roots, Recipients               any
	ScopeRef, Supersedes            sql.NullString
	SupersededBy                    sql.NullString
}

func (r *Registry) toRecord(ctx context.Context, x *row) (Relationship, error) {
	generations, err := r.Generations(ctx, x.ID)
	if err != nil {
		return Relationship{}, err
	}
	roots, err := decodeJSON([]byte(x.Roots))
	if err != nil {
		return Relationship{}, fmt.Errorf("artifact roots: %w", err)
	}
	recipients, err := decodeJSON([]byte(x.Recipients))
	if err != nil {
		return Relationship{}, fmt.Errorf("allowed recipients: %w", err)
	}
	record := Relationship{
		ID: x.ID, IssueKey: x.Issue, Status: x.Status, CreatedAt: x.CreatedAt,
		Parent:     Endpoint{x.ParentTask, x.ParentHost, x.ParentCwd, x.ParentCXC},
		Child:      Endpoint{x.ChildTask, x.ChildHost, x.ChildCwd, x.ChildCXC},
		Generation: x.Generation, Generations: generations, Roots: roots, Recipients: recipients,
		ScopeRef: x.ScopeRef, Supersedes: x.Supersedes, SupersededBy: x.SupersededBy,
	}
	for _, g := range generations {
		if g.Number == x.Generation {
			return record, nil
		}
	}
	return Relationship{}, refuse(contract.RefusalUnknownGeneration, "%s points at generation %d which is not retained", pyStr(x.ID), x.Generation)
}

func endpointRecord(e Endpoint) contract.OrderedObject {
	return contract.OrderedObject{{Key: "taskId", Value: e.TaskID}, {Key: "hostId", Value: e.HostID}, {Key: "cwd", Value: nullable(e.Cwd)}}
}

// ContractRecord is registry.contract_record: the frozen relationship shape.
func (x Relationship) ContractRecord() contract.OrderedObject {
	generations := make([]any, len(x.Generations))
	for i, g := range x.Generations {
		generations[i] = g.contract()
	}
	scope := contract.OrderedObject{{Key: "artifactRoots", Value: x.Roots}, {Key: "allowedRecipients", Value: x.Recipients}}
	if x.ScopeRef.Valid {
		scope = append(scope, contract.Field{Key: "scopeRef", Value: x.ScopeRef.String})
	}
	out := contract.OrderedObject{
		{Key: "relationshipId", Value: x.ID},
		{Key: "parent", Value: endpointRecord(x.Parent)},
		{Key: "child", Value: endpointRecord(x.Child)},
		{Key: "issueKey", Value: x.IssueKey},
		{Key: "status", Value: x.Status},
		{Key: "createdAt", Value: x.CreatedAt},
		{Key: "executionGeneration", Value: x.Generation},
		{Key: "generations", Value: generations},
		{Key: "authorizedScope", Value: scope},
	}
	if x.Supersedes.Valid && x.Supersedes.String != "" {
		out = append(out, contract.Field{Key: "supersedes", Value: x.Supersedes.String})
	}
	return out
}

// Bindings is the record's _bindings block (never in the contract record).
func (x Relationship) Bindings() contract.OrderedObject {
	return contract.OrderedObject{{Key: "parentCxcSession", Value: nullable(x.Parent.CXCSession)},
		{Key: "childCxcSession", Value: nullable(x.Child.CXCSession)}, {Key: "supersededBy", Value: nullable(x.SupersededBy)}}
}

// Get is registry.get.
func (r *Registry) Get(ctx context.Context, rid string) (Relationship, error) {
	x, err := readRow(ctx, r.Store, rid)
	if err != nil {
		return Relationship{}, err
	}
	if x == nil {
		return Relationship{}, refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", pyStr(rid))
	}
	return r.toRecord(ctx, x)
}

// RequireActive is registry.require_active.
func (r *Registry) RequireActive(ctx context.Context, rid string) (Relationship, error) {
	record, err := r.Get(ctx, rid)
	if err != nil {
		return record, err
	}
	if record.Status != Active {
		return record, refuse(contract.RefusalRelationshipNotActive, "relationship %s is %s and is never auto-resumed", pyStr(rid), pyStr(record.Status))
	}
	return record, nil
}

// GenerationOf is registry.generation; ok=false is None.
func (r *Registry) GenerationOf(ctx context.Context, rid string, number int64) (Generation, bool, error) {
	if _, err := r.Get(ctx, rid); err != nil {
		return Generation{}, false, err
	}
	g, err := scanGeneration(r.Store.Querier(ctx).QueryRowContext(ctx, "SELECT "+generationColumns+" FROM generations WHERE relationship_id = ? AND execution_generation = ?", rid, number).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Generation{}, false, nil
	}
	return g, err == nil, err
}

// validatedTurnID is registry.validated_turn_id.
func validatedTurnID(turn sql.NullString) error {
	if turn.Valid && strings.TrimSpace(turn.String) == "" {
		return refuse(contract.RefusalUnboundGeneration, "an anchor needs an exact dispatch turn id, not %s", pyStr(turn.String))
	}
	return nil
}

// Registration is registry.register's arguments.
type Registration struct {
	Parent, Child     Endpoint
	IssueKey          string
	ArtifactRoots     []string
	AllowedRecipients []string
	DispatchRequestID string
	ScopeRef          sql.NullString
	DispatchTurnID    sql.NullString
	Supersedes        string
	ProjectKey        string
}

func anchorFor(turn sql.NullString, at string) (string, sql.NullString) {
	if turn.Valid && turn.String != "" {
		return AnchorBound, sql.NullString{String: at, Valid: true}
	}
	return AnchorPending, sql.NullString{}
}

func sameList(recorded any, want []string) bool { return jsonEqual(recorded, anyStrings(want)) }

// Register is registry.register: deterministic and idempotent.
func (r *Registry) Register(ctx context.Context, in Registration) (Relationship, error) {
	if len(in.ArtifactRoots) == 0 || len(in.AllowedRecipients) == 0 {
		return Relationship{}, refuse(contract.RefusalScopeEscape, "a relationship needs at least one artifact root and one allowed recipient")
	}
	rid, err := store.RelationshipID(in.Parent.TaskID, in.Child.TaskID, in.IssueKey)
	if err != nil {
		return Relationship{}, &HostError{Class: "ValueError", Detail: identityDetail(in)}
	}
	if err := validatedTurnID(in.DispatchTurnID); err != nil {
		return Relationship{}, err
	}
	link := r.linkage()
	if in.ProjectKey == "" && in.Supersedes != "" {
		if in.ProjectKey, err = link.inheritedProject(ctx, rid, in.IssueKey, in.Supersedes); err != nil {
			return Relationship{}, err
		}
	}
	existing, err := readRow(ctx, r.Store, rid)
	if err != nil {
		return Relationship{}, err
	}
	if existing != nil {
		record, err := r.toRecord(ctx, existing)
		if err != nil {
			return record, err
		}
		scopeSame := sameList(record.Roots, in.ArtifactRoots) && sameList(record.Recipients, in.AllowedRecipients)
		same := scopeSame && existing.ParentHost == in.Parent.HostID && existing.ChildHost == in.Child.HostID
		if !same && (isLive(existing.Status) || !scopeSame) {
			return Relationship{}, refuse(contract.RefusalRelationshipConflict, "%s already exists with a different scope or hosts", pyStr(rid))
		}
		if !isLive(existing.Status) {
			returned, err := r.returningTenure(ctx, rid, in)
			if err != nil {
				return Relationship{}, r.recordRaced(ctx, err)
			}
			if returned != nil {
				return *returned, nil
			}
			if record, err = r.Get(ctx, rid); err != nil {
				return record, err
			}
		}
		if in.ProjectKey == "" {
			return record, nil
		}
		return r.attachExisting(ctx, rid, in)
	}
	now := r.now()
	if in.ProjectKey != "" {
		var pending *linkRefusal
		err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
			outgoing, err := link.replaceableChild(ctx, in.Supersedes)
			if err != nil {
				return err
			}
			candidate := candidateRow(rid, in)
			_, pending, err = link.attachRefusal(ctx, candidate, in.ProjectKey, outgoing)
			if err != nil || pending == nil {
				return err
			}
			return link.recordConflict(ctx, pending, now)
		})
		if err != nil {
			return Relationship{}, err
		}
		if pending != nil {
			return Relationship{}, r.contestPending(ctx, pending)
		}
	}
	record, err := r.registerInTransaction(ctx, rid, in, now)
	if err != nil {
		return Relationship{}, r.recordRaced(ctx, err)
	}
	return record, nil
}

func identityDetail(in Registration) string {
	for _, f := range []struct{ name, value string }{{"parent_task_id", in.Parent.TaskID}, {"child_task_id", in.Child.TaskID}, {"issue_key", in.IssueKey}} {
		if strings.TrimSpace(f.value) == "" {
			return f.name + " must be a non-empty string"
		}
		if strings.Contains(f.value, "|") {
			return f.name + " must not contain '|', which is the field separator"
		}
	}
	return "invalid identity"
}

// HostError is an unexpected failure carrying Python's exception class name, so the CLI's host
// envelope reads f"{type(error).__name__}: {error}".
type HostError struct{ Class, Detail string }

func (e *HostError) Error() string { return e.Class + ": " + e.Detail }

func candidateRow(rid string, in Registration) *row {
	return &row{ID: rid, Issue: in.IssueKey, Status: Active, ParentTask: in.Parent.TaskID, ChildTask: in.Child.TaskID,
		ChildHost: in.Child.HostID, ChildCwd: in.Child.Cwd, ChildCXC: in.Child.CXCSession, Supersedes: text(in.Supersedes)}
}

func (r *Registry) attachExisting(ctx context.Context, rid string, in Registration) (Relationship, error) {
	var recorded string
	err := r.Store.Querier(ctx).QueryRowContext(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid).Scan(&recorded)
	if err == nil && recorded != in.ProjectKey {
		return Relationship{}, refuse(contract.RefusalRelationshipConflict, "%s is already scoped to project %s, not %s", pyStr(rid), pyStr(recorded), pyStr(in.ProjectKey))
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Relationship{}, err
	}
	var refusal *linkRefusal
	link := r.linkage()
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		fresh, err := readRow(ctx, r.Store, rid)
		if err != nil {
			return err
		}
		if fresh == nil {
			return refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", pyStr(rid))
		}
		if err := r.guardIssueReservation(ctx, in.IssueKey); err != nil {
			return err
		}
		if refusal, err = link.attachIn(ctx, fresh, in.ProjectKey, r.now(), ""); err != nil || refusal == nil {
			return err
		}
		return link.recordConflict(ctx, refusal, r.now())
	})
	if err != nil {
		return Relationship{}, err
	}
	if refusal != nil {
		return Relationship{}, r.contestPending(ctx, refusal)
	}
	return r.Get(ctx, rid)
}

// guardIssueReservation is _guard_issue_reservation for a caller that names no managed request.
func (r *Registry) guardIssueReservation(ctx context.Context, issue string) error {
	var request, state string
	err := r.Store.Querier(ctx).QueryRowContext(ctx, "SELECT request_id, state FROM managed_start_requests WHERE issue_key = ?"+
		" AND state IN ('reserved','create_armed')", issue).Scan(&request, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return refuse(contract.RefusalDuplicateAssignment, "issue %s is held by managed request %s (%s); raw registration cannot acquire it", pyStr(issue), pyStr(request), state)
}

func (r *Registry) registerInTransaction(ctx context.Context, rid string, in Registration, now string) (Relationship, error) {
	link := r.linkage()
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		if in.Supersedes != "" {
			var named string
			err := q.QueryRowContext(ctx, "SELECT issue_key FROM relationships WHERE relationship_id = ?", in.Supersedes).Scan(&named)
			if errors.Is(err, sql.ErrNoRows) {
				return refuse(contract.RefusalUnregisteredRelationship, "supersedes names %s, which is not registered", pyStr(in.Supersedes))
			}
			if err != nil {
				return err
			}
			if named != in.IssueKey {
				return refuse(contract.RefusalRelationshipConflict, "supersedes names %s, which is assigned to issue %s, not %s; a successor replaces the assignment for its own issue", pyStr(in.Supersedes), pyStr(named), pyStr(in.IssueKey))
			}
		}
		outgoing, err := link.replaceableChild(ctx, in.Supersedes)
		if err != nil {
			return err
		}
		if err := r.guardIssueReservation(ctx, in.IssueKey); err != nil {
			return err
		}
		var rival struct{ id, child, parent, status string }
		err = q.QueryRowContext(ctx, "SELECT relationship_id, child_task_id, parent_task_id, status  FROM relationships"+
			"  WHERE issue_key = ? AND status IN ('active','paused')    AND superseded_by IS NULL AND relationship_id != ?",
			in.IssueKey, rid).Scan(&rival.id, &rival.child, &rival.parent, &rival.status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && rival.id != in.Supersedes {
			return refuse(contract.RefusalDuplicateAssignment, "issue %s is already assigned to child %s under %s (%s, parent %s); reuse that assignment, or pass supersedes to replace it deliberately",
				pyStr(in.IssueKey), pyStr(rival.child), pyStr(rival.id), rival.status, pyStr(rival.parent))
		}
		if in.Supersedes != "" {
			var before sql.NullString
			if err := q.QueryRowContext(ctx, "SELECT status FROM relationships WHERE relationship_id = ?", in.Supersedes).Scan(&before); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if _, err := q.ExecContext(ctx, "UPDATE relationships SET superseded_by = ?, status = 'archived', updated_at = ? WHERE relationship_id = ?", rid, now, in.Supersedes); err != nil {
				return err
			}
			if err := link.applyRelationshipStatus(ctx, in.Supersedes, "archived", before.String, now); err != nil {
				return err
			}
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id,"+
			" parent_host_id, parent_cwd, parent_cxc_session, child_task_id, child_host_id,"+
			" child_cwd, child_cxc_session, execution_generation, artifact_roots,"+
			" allowed_recipients, scope_ref, supersedes, superseded_by, created_at,"+
			" updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
			rid, in.IssueKey, Active, in.Parent.TaskID, in.Parent.HostID, in.Parent.Cwd, in.Parent.CXCSession,
			in.Child.TaskID, in.Child.HostID, in.Child.Cwd, in.Child.CXCSession, 1,
			pyDumps(in.ArtifactRoots, false), pyDumps(in.AllowedRecipients, false), in.ScopeRef, text(in.Supersedes), now, now); err != nil {
			return fmt.Errorf("insert relationship: %w", err)
		}
		anchor, bound := anchorFor(in.DispatchTurnID, now)
		if _, err := q.ExecContext(ctx, "INSERT INTO generations (relationship_id, execution_generation,"+
			" dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at,"+
			" bound_at) VALUES (?,?,?,?,?,?,?,?)", rid, 1, in.DispatchRequestID, anchor, in.DispatchTurnID, "initial_assignment", now, bound); err != nil {
			return fmt.Errorf("insert generation: %w", err)
		}
		if err := journal(ctx, r.Store, "relationship_registered", rid, contract.OrderedObject{{Key: "issueKey", Value: in.IssueKey}}, now); err != nil {
			return err
		}
		if in.ProjectKey == "" {
			return nil
		}
		fresh, err := readRow(ctx, r.Store, rid)
		if err != nil {
			return err
		}
		replacing := ""
		if in.Supersedes != "" {
			replacing = outgoing
		}
		refusal, err := link.attachIn(ctx, fresh, in.ProjectKey, now, replacing)
		if err != nil {
			return err
		}
		if refusal != nil {
			return &racedError{refusal: refusal}
		}
		return nil
	})
	if err != nil {
		return Relationship{}, err
	}
	return r.Get(ctx, rid)
}

// racedError is a refusal whose contest must be re-recorded after its transaction rolled back
// (Python's failure.raced_refusal).
type racedError struct{ refusal *linkRefusal }

func (e *racedError) Error() string { return e.refusal.err().Error() }
func (e *racedError) Unwrap() error { return e.refusal.err() }

// contestPending is _contest_pending: the contest was recorded by a transaction that is ours
// when none is open; inside a composed one it travels on the error.
func (r *Registry) contestPending(ctx context.Context, refusal *linkRefusal) error {
	if r.Store.InTransaction(ctx) {
		return &racedError{refusal: refusal}
	}
	return refusal.err()
}

// recordRaced is record_refusal: re-record a contest its rolled-back transaction took with it.
// It returns the error to raise (the refusal itself).
func (r *Registry) recordRaced(ctx context.Context, failure error) error {
	var raced *racedError
	if !errors.As(failure, &raced) {
		return failure
	}
	if r.Store.InTransaction(ctx) {
		return failure
	}
	at := r.now()
	if err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		return r.linkage().recordConflict(ctx, raced.refusal, at)
	}); err != nil {
		return errors.Join(failure, err)
	}
	return raced.refusal.err()
}

// RecordRefusal is the public record_refusal for a caller that composed the transaction.
func (r *Registry) RecordRefusal(ctx context.Context, failure error) error {
	return r.recordRaced(ctx, failure)
}

func (r *Registry) returningTenure(ctx context.Context, rid string, in Registration) (*Relationship, error) {
	now := r.now()
	var pending *linkRefusal
	live := false
	link := r.linkage()
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		fresh, err := readRow(ctx, r.Store, rid)
		if err != nil {
			return err
		}
		if fresh == nil {
			return refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", pyStr(rid))
		}
		if isLive(fresh.Status) {
			live = true
			return nil
		}
		if in.Supersedes == "" {
			var holder, parent string
			err := q.QueryRowContext(ctx, "SELECT relationship_id, parent_task_id FROM relationships"+
				"  WHERE issue_key = ? AND status IN ('active','paused')    AND superseded_by IS NULL", in.IssueKey).Scan(&holder, &parent)
			tail := "its issue is free, so the way back for this same parent, child and issue is relationship-resume, which restates the generation and the scope it re-authorizes"
			if err == nil {
				tail = "issue " + pyStr(in.IssueKey) + " is held by " + pyStr(holder) + " under parent " + pyStr(parent) + ", so pass supersedes to take that tenure over deliberately"
			} else if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return refuse(contract.RefusalRelationshipConflict, "%s is %s and this registration names no predecessor; %s", pyStr(rid), pyStr(fresh.Status), tail)
		}
		if in.Supersedes == rid {
			return refuse(contract.RefusalRelationshipConflict, "%s cannot supersede itself into a new tenure", pyStr(rid))
		}
		var predIssue, predStatus string
		err = q.QueryRowContext(ctx, "SELECT issue_key, status FROM relationships WHERE relationship_id = ?", in.Supersedes).Scan(&predIssue, &predStatus)
		if errors.Is(err, sql.ErrNoRows) {
			return refuse(contract.RefusalUnregisteredRelationship, "supersedes names %s, which is not registered", pyStr(in.Supersedes))
		}
		if err != nil {
			return err
		}
		if predIssue != in.IssueKey {
			return refuse(contract.RefusalRelationshipConflict, "supersedes names %s, which is assigned to issue %s, not %s; a successor replaces the assignment for its own issue", pyStr(in.Supersedes), pyStr(predIssue), pyStr(in.IssueKey))
		}
		if !isLive(predStatus) {
			return refuse(contract.RefusalRelationshipConflict, "supersedes names %s, which is %s, so there is no tenure for %s to take over; a returning tenure replaces the assignment that holds the issue now", pyStr(in.Supersedes), pyStr(predStatus), pyStr(rid))
		}
		outgoing, err := link.replaceableChild(ctx, in.Supersedes)
		if err != nil {
			return err
		}
		var replayed int64
		err = q.QueryRowContext(ctx, "SELECT execution_generation FROM generations  WHERE relationship_id = ? AND dispatch_request_id = ?", rid, in.DispatchRequestID).Scan(&replayed)
		if err == nil {
			return refuse(contract.RefusalRelationshipConflict, "dispatch request %s already opened generation %d of %s, so it cannot open a returning tenure as well; a new tenure is a new dispatch", pyStr(in.DispatchRequestID), replayed, pyStr(rid))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		project := in.ProjectKey
		if project == "" {
			if project, err = link.inheritedProject(ctx, rid, in.IssueKey, in.Supersedes); err != nil {
				return err
			}
		}
		if err := r.guardIssueReservation(ctx, in.IssueKey); err != nil {
			return err
		}
		var plan *attachPlan
		if project != "" {
			candidate := candidateRow(rid, in)
			var refusal *linkRefusal
			plan, refusal, err = link.attachRefusal(ctx, candidate, project, outgoing)
			if err != nil {
				return err
			}
			if refusal != nil {
				pending = refusal
				return link.recordConflict(ctx, refusal, now)
			}
		}
		if _, err := q.ExecContext(ctx, "UPDATE relationships SET superseded_by = ?, status = 'archived', updated_at = ? WHERE relationship_id = ?", rid, now, in.Supersedes); err != nil {
			return err
		}
		if err := link.applyRelationshipStatus(ctx, in.Supersedes, "archived", predStatus, now); err != nil {
			return err
		}
		generation := fresh.Generation + 1
		if _, err := q.ExecContext(ctx, "UPDATE relationships SET status = ?, superseded_by = NULL, supersedes = ?,"+
			" execution_generation = ?, parent_host_id = ?, parent_cwd = ?,"+
			" parent_cxc_session = ?, child_host_id = ?, child_cwd = ?,"+
			" child_cxc_session = ?, updated_at = ? WHERE relationship_id = ?",
			Active, in.Supersedes, generation, in.Parent.HostID, in.Parent.Cwd, in.Parent.CXCSession,
			in.Child.HostID, in.Child.Cwd, in.Child.CXCSession, now, rid); err != nil {
			return err
		}
		anchor, bound := anchorFor(in.DispatchTurnID, now)
		if _, err := q.ExecContext(ctx, "INSERT INTO generations (relationship_id, execution_generation,"+
			" dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at,"+
			" bound_at) VALUES (?,?,?,?,?,NULL,?,?)", rid, generation, in.DispatchRequestID, anchor, in.DispatchTurnID, now, bound); err != nil {
			return err
		}
		if err := journal(ctx, r.Store, "relationship_tenure_reopened", rid, contract.OrderedObject{
			{Key: "issueKey", Value: in.IssueKey}, {Key: "executionGeneration", Value: generation},
			{Key: "supersedes", Value: in.Supersedes}, {Key: "outgoingChild", Value: nullable(text(outgoing))}}, now); err != nil {
			return err
		}
		if err := supersedeOlderDeliveries(ctx, r.Store, rid, generation, now); err != nil {
			return err
		}
		if plan != nil {
			reborn, err := readRow(ctx, r.Store, rid)
			if err != nil {
				return err
			}
			return link.attachApply(ctx, reborn, project, plan, now)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if live {
		return nil, nil
	}
	if pending != nil {
		return nil, r.contestPending(ctx, pending)
	}
	record, err := r.Get(ctx, rid)
	return &record, err
}

// supersedeOlderDeliveries is _supersede_older_deliveries_in.
func supersedeOlderDeliveries(ctx context.Context, s *store.Store, rid string, number int64, now string) error {
	_, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO delivery_supersession (event_id, reason, noted_at, applied)"+
		" SELECT d.event_id, 'stale_generation', ?, 0 FROM deliveries d  JOIN events e ON e.event_id = d.event_id"+
		" WHERE d.relationship_id = ? AND e.execution_generation < ?   AND e.outcome NOT IN ('merge_turn_grant')"+
		"   AND d.state IN ('queued','sending','held_uncertain','dispatched',                  'deferred_busy','withheld_pre_send','inbox_only')"+
		" ON CONFLICT(event_id) DO NOTHING", now, rid, number)
	return err
}

// OpenGeneration is registry.open_generation.
func (r *Registry) OpenGeneration(ctx context.Context, rid, dispatchRequest, reason string, dispatchTurn sql.NullString) (Generation, error) {
	if !contains(reasons, reason) {
		return Generation{}, refuse(contract.RefusalUnknownGeneration, "bad reason %s", pyStr(reason))
	}
	if err := validatedTurnID(dispatchTurn); err != nil {
		return Generation{}, err
	}
	if _, err := r.RequireActive(ctx, rid); err != nil {
		return Generation{}, err
	}
	replay, err := scanGeneration(r.Store.Querier(ctx).QueryRowContext(ctx, "SELECT "+generationColumns+" FROM generations WHERE relationship_id = ? AND dispatch_request_id = ?", rid, dispatchRequest).Scan)
	if err == nil {
		return replay, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Generation{}, err
	}
	var number int64
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		number, err = r.OpenGenerationIn(ctx, rid, dispatchRequest, reason, dispatchTurn)
		return err
	})
	if err != nil {
		return Generation{}, err
	}
	g, _, err := r.GenerationOf(ctx, rid, number)
	return g, err
}

// OpenGenerationIn is registry.open_generation_in: inside the caller's transaction.
func (r *Registry) OpenGenerationIn(ctx context.Context, rid, dispatchRequest, reason string, dispatchTurn sql.NullString) (int64, error) {
	if !contains(reasons, reason) {
		return 0, refuse(contract.RefusalUnknownGeneration, "bad reason %s", pyStr(reason))
	}
	if err := validatedTurnID(dispatchTurn); err != nil {
		return 0, err
	}
	q := r.Store.Querier(ctx)
	var number int64
	err := q.QueryRowContext(ctx, "SELECT execution_generation FROM generations WHERE relationship_id = ? AND dispatch_request_id = ?", rid, dispatchRequest).Scan(&number)
	if err == nil {
		return number, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	var current int64
	var status string
	var supersededBy sql.NullString
	err = q.QueryRowContext(ctx, "SELECT execution_generation, status, superseded_by FROM relationships WHERE relationship_id = ?", rid).Scan(&current, &status, &supersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", pyStr(rid))
	}
	if err != nil {
		return 0, err
	}
	if status != Active || supersededBy.String != "" {
		return 0, refuse(contract.RefusalRelationshipNotActive, "relationship %s is not active", pyStr(rid))
	}
	number = current + 1
	now := r.now()
	anchor, bound := anchorFor(dispatchTurn, now)
	if _, err := q.ExecContext(ctx, "INSERT INTO generations (relationship_id, execution_generation,"+
		" dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at,"+
		" bound_at) VALUES (?,?,?,?,?,?,?,?)", rid, number, dispatchRequest, anchor, dispatchTurn, reason, now, bound); err != nil {
		return 0, err
	}
	if _, err := q.ExecContext(ctx, "UPDATE relationships SET execution_generation = ?, updated_at = ? WHERE relationship_id = ?", number, now, rid); err != nil {
		return 0, err
	}
	if err := journal(ctx, r.Store, "generation_opened", rid, contract.OrderedObject{{Key: "generation", Value: number}, {Key: "reason", Value: reason}}, now); err != nil {
		return 0, err
	}
	return number, supersedeOlderDeliveries(ctx, r.Store, rid, number, now)
}

// BindAnchor is registry.bind_anchor.
func (r *Registry) BindAnchor(ctx context.Context, rid string, number int64, turn, source string) (Generation, error) {
	if source != "dispatch_receipt" {
		return Generation{}, refuse(contract.RefusalUnboundGeneration, "an anchor binds only from a dispatch receipt, not from %s", pyStr(source))
	}
	if strings.TrimSpace(turn) == "" {
		return Generation{}, refuse(contract.RefusalUnboundGeneration, "an anchor needs an exact dispatch turn id, not %s", pyStr(turn))
	}
	current, ok, err := r.GenerationOf(ctx, rid, number)
	if err != nil {
		return Generation{}, err
	}
	if !ok {
		return Generation{}, refuse(contract.RefusalUnknownGeneration, "%s has no generation %d", pyStr(rid), number)
	}
	if current.AnchorState == AnchorBound {
		if current.DispatchTurnID.String == turn {
			return current, nil
		}
		return Generation{}, refuse(contract.RefusalAnchorAlreadyBound, "generation %d is already bound to %s", number, pyRepr(nullable(current.DispatchTurnID)))
	}
	now := r.now()
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if _, err := r.Store.Querier(ctx).ExecContext(ctx, "UPDATE generations SET anchor_state = ?, dispatch_turn_id = ?, bound_at = ?"+
			" WHERE relationship_id = ? AND execution_generation = ?", AnchorBound, turn, now, rid, number); err != nil {
			return err
		}
		return journal(ctx, r.Store, "anchor_bound", rid, contract.OrderedObject{{Key: "generation", Value: number}}, now)
	})
	if err != nil {
		return Generation{}, err
	}
	g, _, err := r.GenerationOf(ctx, rid, number)
	return g, err
}

// SetStatus is registry.set_status: deactivation only.
func (r *Registry) SetStatus(ctx context.Context, rid, status, actor string) (Relationship, error) {
	if status != "paused" && status != "cancelled" && status != "archived" {
		return Relationship{}, refuse(contract.RefusalRelationshipNotActive, "bad status %s", pyStr(status))
	}
	if _, err := r.Get(ctx, rid); err != nil {
		return Relationship{}, err
	}
	now := r.now()
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		var before sql.NullString
		if err := q.QueryRowContext(ctx, "SELECT status FROM relationships WHERE relationship_id = ?", rid).Scan(&before); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if before.Valid && !isLive(before.String) && isLive(status) {
			return refuse(contract.RefusalRelationshipNotActive, "%s is %s, so %s would bring it back to life. Restoring an assignment restates the generation and the scope it re-authorizes, which is relationship-resume; choosing a different live word does not make those checks optional", pyStr(rid), pyStr(before.String), pyStr(status))
		}
		if _, err := q.ExecContext(ctx, "UPDATE relationships SET status = ?, updated_at = ? WHERE relationship_id = ?", status, now, rid); err != nil {
			return err
		}
		if err := r.linkage().applyRelationshipStatus(ctx, rid, status, before.String, now); err != nil {
			return err
		}
		return journal(ctx, r.Store, "status_changed", rid, contract.OrderedObject{{Key: "status", Value: status}, {Key: "actor", Value: actor}}, now)
	})
	if err != nil {
		return Relationship{}, r.recordRaced(ctx, err)
	}
	return r.Get(ctx, rid)
}

// Resume is registry.resume: restate the generation and the whole scope.
func (r *Registry) Resume(ctx context.Context, rid string, expectGeneration int64, roots, recipients []string, actor string) (Relationship, error) {
	now := r.now()
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		x, err := readRow(ctx, r.Store, rid)
		if err != nil {
			return err
		}
		if x == nil {
			return refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", pyStr(rid))
		}
		var mismatches []string
		if x.Generation != expectGeneration {
			mismatches = append(mismatches, fmt.Sprintf("generation is %d, not %d", x.Generation, expectGeneration))
		}
		recordedRoots, err := decodeJSON([]byte(x.Roots))
		if err != nil {
			return err
		}
		recordedRecipients, err := decodeJSON([]byte(x.Recipients))
		if err != nil {
			return err
		}
		if !sameList(recordedRoots, roots) {
			mismatches = append(mismatches, "artifact roots differ from the restated scope")
		}
		if !sameList(recordedRecipients, recipients) {
			mismatches = append(mismatches, "allowed recipients differ from the restated scope")
		}
		if x.SupersededBy.String != "" {
			mismatches = append(mismatches, "superseded by "+x.SupersededBy.String)
		}
		if len(mismatches) > 0 {
			return refuse(contract.RefusalRelationshipNotActive, "resume refused: %s", strings.Join(mismatches, "; "))
		}
		var owner struct{ id, child, status string }
		err = q.QueryRowContext(ctx, "SELECT relationship_id, child_task_id, status FROM relationships"+
			"  WHERE issue_key = ? AND relationship_id != ?    AND status IN ('active','paused') AND superseded_by IS NULL",
			x.Issue, rid).Scan(&owner.id, &owner.child, &owner.status)
		if err == nil {
			return refuse(contract.RefusalDuplicateAssignment, "issue %s is now assigned to child %s under %s (%s); resuming %s would leave the issue with two owners. Replace that assignment deliberately instead",
				pyStr(x.Issue), pyStr(owner.child), pyStr(owner.id), owner.status, pyStr(rid))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !isLive(x.Status) {
			if err := r.guardIssueReservation(ctx, x.Issue); err != nil {
				return err
			}
		} else if err := r.guardIssueReservation(ctx, x.Issue); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "UPDATE relationships SET status = ?, updated_at = ? WHERE relationship_id = ?", Active, now, rid); err != nil {
			return err
		}
		if err := r.linkage().applyRelationshipStatus(ctx, rid, Active, x.Status, now); err != nil {
			return err
		}
		return journal(ctx, r.Store, "status_changed", rid, contract.OrderedObject{{Key: "status", Value: Active}, {Key: "actor", Value: actor}}, now)
	})
	if err != nil {
		return Relationship{}, r.recordRaced(ctx, err)
	}
	return r.Get(ctx, rid)
}

// Supersede is registry.supersede.
func (r *Registry) Supersede(ctx context.Context, oldRID, newRID string) error {
	if _, err := r.Get(ctx, oldRID); err != nil {
		return err
	}
	now := r.now()
	return r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		var before sql.NullString
		if err := q.QueryRowContext(ctx, "SELECT status FROM relationships WHERE relationship_id = ?", oldRID).Scan(&before); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := q.ExecContext(ctx, "UPDATE relationships SET superseded_by = ?, status = 'archived', updated_at = ? WHERE relationship_id = ?", newRID, now, oldRID); err != nil {
			return err
		}
		if err := r.linkage().applyRelationshipStatus(ctx, oldRID, "archived", before.String, now); err != nil {
			return err
		}
		return journal(ctx, r.Store, "superseded", oldRID, contract.OrderedObject{{Key: "by", Value: newRID}}, now)
	})
}

// FullRecord is registry._row_to_record whole: the contract fields plus _bindings and
// supersededBy, with scopeRef always present, as Python returns it to in-process callers.
func (x Relationship) FullRecord() contract.OrderedObject {
	generations := make([]any, len(x.Generations))
	for i, g := range x.Generations {
		generations[i] = g.contract()
	}
	out := contract.OrderedObject{
		{Key: "relationshipId", Value: x.ID},
		{Key: "parent", Value: endpointRecord(x.Parent)},
		{Key: "child", Value: endpointRecord(x.Child)},
		{Key: "issueKey", Value: x.IssueKey},
		{Key: "status", Value: x.Status},
		{Key: "createdAt", Value: x.CreatedAt},
		{Key: "executionGeneration", Value: x.Generation},
		{Key: "generations", Value: generations},
		{Key: "authorizedScope", Value: contract.OrderedObject{{Key: "artifactRoots", Value: x.Roots},
			{Key: "allowedRecipients", Value: x.Recipients}, {Key: "scopeRef", Value: nullable(x.ScopeRef)}}},
	}
	if x.Supersedes.String != "" {
		out = append(out, contract.Field{Key: "supersedes", Value: x.Supersedes.String})
	}
	return append(out, contract.Field{Key: "_bindings", Value: x.Bindings()}, contract.Field{Key: "supersededBy", Value: nullable(x.SupersededBy)})
}
