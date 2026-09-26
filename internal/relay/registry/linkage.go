package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The part of linkage.py registration needs: issue attachment under a project, the child
// binding it writes, the lifecycle that moves with an assignment, and the contest record.
// The linkage commands themselves are todo 26.

const (
	scopeIssue   = "issue"
	scopeProject = "project"
	roleChild    = "child"
	roleParent   = "parent"
	linkExec     = "execution"
)

// linkRefusal is linkage._Refusal: decided, then recorded in the transaction that decided it.
type linkRefusal struct {
	reason                      contract.RefusalReason
	detail, scopeKind, scopeKey string
	incumbent, challenger       string
}

func (l *linkRefusal) err() error {
	return &store.RefusedError{Reason: string(l.reason), Detail: l.detail}
}

type bindingPlan struct {
	id, action, role, scopeKind, scopeKey string
	endpoint                              Endpoint
}

type attachPlan struct {
	binding         bindingPlan
	scopeRowMissing bool
}

type linkage struct{ r *Registry }

func (r *Registry) linkage() linkage { return linkage{r} }

func (l linkage) q(ctx context.Context) store.Querier { return l.r.Store.Querier(ctx) }

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func bindingID(role, kind, key, task string) string {
	return "bnd-" + sha256Hex(strings.Join([]string{role, kind, key, task}, "|"))[:32]
}

func linkID(kind, upperKind, upperKey, lowerKind, lowerKey string) string {
	return "lnk-" + sha256Hex(strings.Join([]string{kind, upperKind, upperKey, lowerKind, lowerKey}, "|"))[:32]
}

func exact(value, what string) error {
	if strings.TrimSpace(value) == "" {
		return refuse(contract.RefusalUnregisteredScope, "%s must be a non-empty string, not %s", what, pyStr(value))
	}
	if strings.Contains(value, "|") {
		return refuse(contract.RefusalUnregisteredScope, "%s must not contain '|', which is the field separator", what)
	}
	return nil
}

func (l linkage) oneString(ctx context.Context, query string, args ...any) (string, bool, error) {
	var out sql.NullString
	err := l.q(ctx).QueryRowContext(ctx, query, args...).Scan(&out)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return out.String, err == nil, err
}

// inheritedProject is Registry._inherited_project; "" is None.
func (l linkage) inheritedProject(ctx context.Context, rid, issue, supersedes string) (string, error) {
	found, ok, err := l.oneString(ctx, "SELECT s.project_key AS project_key FROM relationship_scope s"+
		"  JOIN relationships r ON r.relationship_id = s.relationship_id WHERE s.relationship_id = ? AND r.issue_key = ?", supersedes, issue)
	if err != nil || ok {
		return found, err
	}
	found, _, err = l.oneString(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid)
	return found, err
}

// replaceableChild is linkage.replaceable_child_in; "" is None.
func (l linkage) replaceableChild(ctx context.Context, supersedes string) (string, error) {
	if supersedes == "" {
		return "", nil
	}
	child, _, err := l.oneString(ctx, "SELECT r.child_task_id AS child_task_id FROM relationships r"+
		"  JOIN scope_bindings b ON b.scope_kind = ? AND b.scope_key = r.issue_key   AND b.role = ? AND b.task_id = r.child_task_id"+
		" WHERE r.relationship_id = ? AND r.status IN ('active','paused')   AND r.superseded_by IS NULL"+
		"   AND b.status IN ('active','paused') AND b.superseded_by IS NULL", scopeIssue, roleChild, supersedes)
	return child, err
}

func (l linkage) strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := l.q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, rows.Err()
}

func (l linkage) liveOwners(ctx context.Context, kind, key, role string) ([]string, error) {
	return l.strings(ctx, "SELECT task_id FROM scope_bindings  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL  ORDER BY task_id", kind, key, role)
}

func contestedOwner(kind, key string, owners []string, challenger string) *linkRefusal {
	quoted := make([]string, len(owners))
	for i, o := range owners {
		quoted[i] = pyStr(o)
	}
	return &linkRefusal{reason: contract.RefusalDuplicateScopeOwner,
		detail: kind + " " + pyStr(key) + " has more than one live owner (" + strings.Join(quoted, ", ") +
			"), so there is no owner to write under. The reading paths report this and the" +
			" writing paths refuse it; repair the store rather than letting one of them win",
		scopeKind: kind, scopeKey: key, incumbent: owners[0], challenger: challenger}
}

// attachRefusal is linkage.attach_refusal: pure reads.
func (l linkage) attachRefusal(ctx context.Context, x *row, project, replacing string) (*attachPlan, *linkRefusal, error) {
	if err := exact(project, "a project key"); err != nil {
		return nil, nil, err
	}
	if !isLive(x.Status) || x.SupersededBy.String != "" {
		return nil, &linkRefusal{reason: contract.RefusalRelationshipNotActive,
			detail:    "relationship " + pyStr(x.ID) + " is " + pyStr(x.Status) + ", so its issue cannot be attached to a project",
			scopeKind: scopeProject, scopeKey: project, incumbent: x.ID, challenger: project}, nil
	}
	if x.ParentTask == x.ChildTask {
		return nil, &linkRefusal{reason: contract.RefusalScopeCycle,
			detail:    "relationship " + pyStr(x.ID) + " has the same task as parent and child, which is a self-link rather than a level",
			scopeKind: scopeIssue, scopeKey: x.Issue, incumbent: x.ParentTask, challenger: x.ChildTask}, nil
	}
	owners, err := l.liveOwners(ctx, scopeProject, project, roleParent)
	if err != nil {
		return nil, nil, err
	}
	if len(owners) > 1 {
		return nil, contestedOwner(scopeProject, project, owners, x.ParentTask), nil
	}
	if len(owners) == 0 {
		return nil, &linkRefusal{reason: contract.RefusalUnregisteredScope,
			detail:    "project " + pyStr(project) + " has no registered parent, so an issue cannot be attached to it yet",
			scopeKind: scopeProject, scopeKey: project, challenger: x.ParentTask}, nil
	}
	holder := owners[0]
	movingWithin := false
	if x.Supersedes.String != "" {
		_, taken, err := l.oneString(ctx, "SELECT 1 FROM relationships WHERE relationship_id = ? AND superseded_by = ?", x.Supersedes.String, x.ID)
		if err != nil {
			return nil, nil, err
		}
		child, err := l.replaceableChild(ctx, x.Supersedes.String)
		if err != nil {
			return nil, nil, err
		}
		if taken || child != "" {
			_, movingWithin, err = l.oneString(ctx, "SELECT 1 FROM relationship_scope s  JOIN relationships r ON r.relationship_id = s.relationship_id"+
				" WHERE s.relationship_id = ? AND s.project_key = ? AND r.issue_key = ?", x.Supersedes.String, project, x.Issue)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	if holder != x.ParentTask && !movingWithin {
		return nil, &linkRefusal{reason: contract.RefusalForeignScope,
			detail: "issue " + pyStr(x.Issue) + " is assigned under parent " + pyStr(x.ParentTask) + ", but project " + pyStr(project) +
				" is executed by " + pyStr(holder) + "; an issue belongs to its own project",
			scopeKind: scopeProject, scopeKey: project, incumbent: holder, challenger: x.ParentTask}, nil
	}
	recorded, hasRecord, err := l.oneString(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", x.ID)
	if err != nil {
		return nil, nil, err
	}
	if hasRecord && recorded != project {
		return nil, &linkRefusal{reason: contract.RefusalForeignScope,
			detail:    "issue " + pyStr(x.Issue) + " is already scoped to project " + pyStr(recorded) + ", not " + pyStr(project),
			scopeKind: scopeIssue, scopeKey: x.Issue, incumbent: recorded, challenger: project}, nil
	}
	elsewhere, found, err := l.oneString(ctx, "SELECT s.project_key FROM relationship_scope s  JOIN relationships r ON r.relationship_id = s.relationship_id"+
		" WHERE r.issue_key = ? AND s.project_key != ? AND s.relationship_id != ? LIMIT 1", x.Issue, project, x.ID)
	if err != nil {
		return nil, nil, err
	}
	if found {
		return nil, &linkRefusal{reason: contract.RefusalForeignScope,
			detail: "issue " + pyStr(x.Issue) + " is already scoped to project " + pyStr(elsewhere) +
				" through another assignment, so it cannot also belong to " + pyStr(project),
			scopeKind: scopeIssue, scopeKey: x.Issue, incumbent: elsewhere, challenger: project}, nil
	}
	if err := exact(x.ChildTask, "the child task id"); err != nil {
		return nil, nil, err
	}
	if err := exact(x.ChildHost, "the child host id"); err != nil {
		return nil, nil, err
	}
	plan, refusal, err := l.bindingPlan(ctx, roleChild, x.Issue, Endpoint{x.ChildTask, x.ChildHost, x.ChildCwd, x.ChildCXC}, replacing)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	return &attachPlan{binding: *plan, scopeRowMissing: !hasRecord}, nil, nil
}

// bindingPlan is linkage.binding_plan.
func (l linkage) bindingPlan(ctx context.Context, role, key string, endpoint Endpoint, replacing string) (*bindingPlan, *linkRefusal, error) {
	kind := roleScope[role]
	bid := bindingID(role, kind, key, endpoint.TaskID)
	var status, host, task string
	err := l.q(ctx).QueryRowContext(ctx, "SELECT status, host_id, task_id FROM scope_bindings WHERE binding_id = ?", bid).Scan(&status, &host, &task)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	action := "insert"
	if err == nil {
		if isLive(status) {
			if host != endpoint.HostID {
				return nil, &linkRefusal{reason: contract.RefusalLinkConflict, detail: bid + " is already bound on another host",
					scopeKind: kind, scopeKey: key, incumbent: task, challenger: endpoint.TaskID}, nil
			}
			rivals, err := l.liveOwners(ctx, kind, key, role)
			if err != nil {
				return nil, nil, err
			}
			if len(rivals) > 1 {
				return nil, contestedOwner(kind, key, rivals, endpoint.TaskID), nil
			}
			return &bindingPlan{bid, "present", role, kind, key, endpoint}, nil, nil
		}
		action = "reactivate"
	}
	refusal, err := l.bindingRefusal(ctx, role, kind, key, endpoint, replacing)
	if err != nil || refusal != nil {
		return nil, refusal, err
	}
	return &bindingPlan{bid, action, role, kind, key, endpoint}, nil, nil
}

// rolePolicyFinding is Linkage._role_policy_finding.
func (l linkage) rolePolicyFinding(ctx context.Context, role, task string) (contract.OrderedObject, error) {
	raw, ok, err := l.oneString(ctx, "SELECT settings FROM authorized_settings WHERE task_id = ?", task)
	if err != nil || !ok {
		return nil, err
	}
	decoded, err := decodeJSON([]byte(raw))
	if err != nil {
		return nil, err
	}
	settings, _ := decoded.(contract.OrderedObject)
	return CheckBinding(citedRole(settings), role, settings, l.r.Policy), nil
}

func findingText(finding contract.OrderedObject, key string) string {
	v, _ := getField(finding, key)
	s, _ := v.(string)
	return s
}

func (l linkage) bindingRefusal(ctx context.Context, role, kind, key string, endpoint Endpoint, replacing string) (*linkRefusal, error) {
	finding, err := l.rolePolicyFinding(ctx, role, endpoint.TaskID)
	if err != nil {
		return nil, err
	}
	if finding != nil {
		return &linkRefusal{reason: contract.RefusalReason(findingText(finding, "code")), detail: findingText(finding, "detail"),
			scopeKind: kind, scopeKey: key, incumbent: findingText(finding, "citedRole"), challenger: endpoint.TaskID}, nil
	}
	var rival struct{ id, task, status string }
	err = l.q(ctx).QueryRowContext(ctx, "SELECT binding_id, task_id, status FROM scope_bindings  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL    AND task_id != ? AND task_id IS NOT ?",
		kind, key, role, endpoint.TaskID, text(replacing)).Scan(&rival.id, &rival.task, &rival.status)
	if err == nil {
		return &linkRefusal{reason: contract.RefusalDuplicateScopeOwner,
			detail: kind + " " + pyStr(key) + " is already owned by " + pyStr(rival.task) + " under " + rival.id + " (" + rival.status +
				"); hand it over deliberately instead of opening a second owner",
			scopeKind: kind, scopeKey: key, incumbent: rival.task, challenger: endpoint.TaskID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var other struct{ role, kind, key string }
	err = l.q(ctx).QueryRowContext(ctx, "SELECT role, scope_kind, scope_key FROM scope_bindings  WHERE task_id = ? AND role != ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL", endpoint.TaskID, role).Scan(&other.role, &other.kind, &other.key)
	if err == nil {
		return &linkRefusal{reason: contract.RefusalScopeRoleMismatch,
			detail: "task " + pyStr(endpoint.TaskID) + " is already the " + other.role + " of " + other.kind + " " + pyStr(other.key) +
				", so it cannot also be a " + role,
			scopeKind: kind, scopeKey: key, incumbent: other.key, challenger: endpoint.TaskID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	err = l.q(ctx).QueryRowContext(ctx, "SELECT scope_kind, scope_key FROM scope_bindings  WHERE task_id = ? AND role = ? AND scope_key != ?"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL", endpoint.TaskID, role, key).Scan(&other.kind, &other.key)
	if err == nil {
		return &linkRefusal{reason: contract.RefusalRoleAlreadyBound,
			detail: "task " + pyStr(endpoint.TaskID) + " is already the " + role + " of " + other.kind + " " + pyStr(other.key) +
				"; one task is bound to one Linear level, so a second " + kind + " needs its own " + role,
			scopeKind: kind, scopeKey: key, incumbent: other.key, challenger: endpoint.TaskID}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return nil, nil
}

// attachIn is linkage.attach_in.
func (l linkage) attachIn(ctx context.Context, x *row, project, at, replacing string) (*linkRefusal, error) {
	plan, refusal, err := l.attachRefusal(ctx, x, project, replacing)
	if err != nil || refusal != nil {
		return refusal, err
	}
	return nil, l.attachApply(ctx, x, project, plan, at)
}

// attachApply is linkage.attach_apply.
func (l linkage) attachApply(ctx context.Context, x *row, project string, plan *attachPlan, at string) error {
	q := l.q(ctx)
	lower := Active
	if isLive(x.Status) {
		lower = x.Status
	}
	b := plan.binding
	switch b.action {
	case "insert":
		if _, err := q.ExecContext(ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"+
			" host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"+
			" handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)",
			b.id, b.role, b.scopeKind, b.scopeKey, b.endpoint.TaskID, b.endpoint.HostID, b.endpoint.Cwd, b.endpoint.CXCSession,
			lower, 1, nil, nil, at, at); err != nil {
			return err
		}
		if err := journal(ctx, l.r.Store, "scope_bound", b.id, contract.OrderedObject{{Key: "role", Value: b.role}, {Key: "scopeKind", Value: b.scopeKind},
			{Key: "scopeKey", Value: b.scopeKey}, {Key: "taskId", Value: b.endpoint.TaskID}, {Key: "revision", Value: 1}}, at); err != nil {
			return err
		}
	case "reactivate":
		if _, err := q.ExecContext(ctx, "UPDATE scope_bindings SET status = ?, updated_at = ?, host_id = ?,  cwd = ?, cxc_session = ? WHERE binding_id = ?",
			lower, at, b.endpoint.HostID, b.endpoint.Cwd, b.endpoint.CXCSession, b.id); err != nil {
			return err
		}
		if err := journal(ctx, l.r.Store, "scope_rebound", b.id, contract.OrderedObject{{Key: "scopeKey", Value: b.scopeKey}}, at); err != nil {
			return err
		}
	}
	if _, err := q.ExecContext(ctx, "UPDATE scope_bindings SET status = ?, updated_at = ?  WHERE binding_id = ? AND status != ?", lower, at, b.id, lower); err != nil {
		return err
	}
	if plan.scopeRowMissing {
		if _, err := q.ExecContext(ctx, "INSERT INTO relationship_scope (relationship_id, project_key, recorded_at) VALUES (?,?,?) ON CONFLICT(relationship_id) DO NOTHING", x.ID, project, at); err != nil {
			return err
		}
	}
	lid := linkID(linkExec, scopeProject, project, scopeIssue, x.Issue)
	var status, lowerTask string
	err := q.QueryRowContext(ctx, "SELECT status, lower_task_id FROM scope_links WHERE link_id = ?", lid).Scan(&status, &lowerTask)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := q.ExecContext(ctx, "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"+
			" upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"+
			" superseded_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
			lid, linkExec, scopeProject, project, x.ParentTask, scopeIssue, x.Issue, x.ChildTask, lower, 1, at, at); err != nil {
			return err
		}
		return journal(ctx, l.r.Store, "scope_linked", lid, contract.OrderedObject{{Key: "kind", Value: linkExec}, {Key: "upper", Value: project},
			{Key: "lower", Value: x.Issue}, {Key: "revision", Value: 1}}, at)
	case err != nil:
		return err
	case !isLive(status) || lowerTask != x.ChildTask:
		if _, err := q.ExecContext(ctx, "UPDATE scope_links SET status = ?, lower_task_id = ?, upper_task_id = ?,"+
			" revision = revision + 1, superseded_by = NULL, updated_at = ?  WHERE link_id = ?", lower, x.ChildTask, x.ParentTask, at, lid); err != nil {
			return err
		}
		return journal(ctx, l.r.Store, "scope_link_repointed", lid, contract.OrderedObject{{Key: "lowerTaskId", Value: x.ChildTask}}, at)
	}
	return nil
}

// recordConflict is linkage._record_conflict_in.
func (l linkage) recordConflict(ctx context.Context, refusal *linkRefusal, at string) error {
	if _, err := l.q(ctx).ExecContext(ctx, "INSERT INTO linkage_conflicts (at, scope_kind, scope_key, reason, incumbent,"+
		" challenger, detail) VALUES (?,?,?,?,?,?,?) ON CONFLICT(scope_kind, scope_key, reason, incumbent, challenger)"+
		"   DO UPDATE SET at = excluded.at, detail = excluded.detail",
		at, refusal.scopeKind, refusal.scopeKey, string(refusal.reason), refusal.incumbent, refusal.challenger, refusal.detail); err != nil {
		return err
	}
	return journal(ctx, l.r.Store, "linkage_refused", refusal.scopeKey, contract.OrderedObject{{Key: "reason", Value: string(refusal.reason)},
		{Key: "scopeKind", Value: refusal.scopeKind}, {Key: "incumbent", Value: refusal.incumbent}, {Key: "challenger", Value: refusal.challenger}}, at)
}

func refusing(reason contract.RefusalReason, detail, kind, key, incumbent, challenger string) error {
	return &racedError{refusal: &linkRefusal{reason: reason, detail: detail, scopeKind: kind, scopeKey: key, incumbent: incumbent, challenger: challenger}}
}

// applyRelationshipStatus is linkage.apply_relationship_status_in; previous "" is None.
func (l linkage) applyRelationshipStatus(ctx context.Context, rid, status, previous, _ string) error {
	project, scoped, err := l.oneString(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid)
	if err != nil || !scoped {
		return err
	}
	var x struct{ issue, child, childHost, parent string }
	err = l.q(ctx).QueryRowContext(ctx, "SELECT issue_key, child_task_id, child_host_id, parent_task_id FROM relationships  WHERE relationship_id = ?", rid).
		Scan(&x.issue, &x.child, &x.childHost, &x.parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	at := l.r.now()
	lower := "archived"
	if isLive(status) {
		lower = status
	}
	if !isLive(lower) && previous != "" && !isLive(previous) {
		return nil
	}
	if lower != Active {
		live, found, err := l.oneString(ctx, "SELECT relationship_id FROM relationships  WHERE issue_key = ? AND status IN ('active','paused')"+
			"    AND superseded_by IS NULL  ORDER BY created_at DESC LIMIT 1", x.issue)
		if err != nil {
			return err
		}
		if found && live != rid {
			return nil
		}
	}
	if isLive(lower) {
		holder, found, err := l.oneString(ctx, "SELECT task_id FROM scope_bindings  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL  ORDER BY revision DESC LIMIT 1", scopeProject, project, roleParent)
		if err != nil {
			return err
		}
		if !found || holder != x.parent {
			shown := "nobody"
			if found {
				shown = pyStr(holder)
			}
			return refusing(contract.RefusalForeignScope, "project "+pyStr(project)+" is now parented by "+shown+", not by "+pyStr(x.parent)+
				", so restoring "+pyStr(rid)+" would reattach its issue under an owner the project no longer has; re-register the assignment under the current parent instead",
				scopeProject, project, holder, x.parent)
		}
		rival, found, err := l.oneString(ctx, "SELECT task_id FROM scope_bindings  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL    AND task_id != ?", scopeIssue, x.issue, roleChild, x.child)
		if err != nil {
			return err
		}
		if found {
			return refusing(contract.RefusalDuplicateScopeOwner, "issue "+pyStr(x.issue)+" is now held by "+pyStr(rival)+", so restoring "+
				pyStr(x.child)+" would leave it with two owners", scopeIssue, x.issue, rival, x.child)
		}
		host, found, err := l.oneString(ctx, "SELECT host_id FROM scope_bindings  WHERE scope_kind = ? AND scope_key = ? AND role = ? AND task_id = ?"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL", scopeIssue, x.issue, roleChild, x.child)
		if err != nil {
			return err
		}
		if found && host != x.childHost {
			return refusing(contract.RefusalLinkConflict, "child "+pyStr(x.child)+" holds issue "+pyStr(x.issue)+" on host "+pyStr(host)+", but "+
				pyStr(rid)+" records "+pyStr(x.childHost)+"; restoring it would reactivate an assignment whose routing endpoint is not where the child is",
				scopeIssue, x.issue, host, x.childHost)
		}
		var b struct{ role, kind, key string }
		err = l.q(ctx).QueryRowContext(ctx, "SELECT role, scope_kind, scope_key FROM scope_bindings  WHERE task_id = ? AND status IN ('active','paused')"+
			"    AND superseded_by IS NULL    AND (role != ? OR scope_key != ?)", x.child, roleChild, x.issue).Scan(&b.role, &b.kind, &b.key)
		if err == nil {
			reason := contract.RefusalScopeRoleMismatch
			if b.role == roleChild {
				reason = contract.RefusalRoleAlreadyBound
			}
			return refusing(reason, "task "+pyStr(x.child)+" has since become the "+b.role+" of "+b.kind+" "+pyStr(b.key)+
				", so it cannot be restored as the child of issue "+pyStr(x.issue)+" as well", scopeIssue, x.issue, b.key, x.child)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		finding, err := l.rolePolicyFinding(ctx, roleChild, x.child)
		if err != nil {
			return err
		}
		if finding != nil {
			return refusing(contract.RefusalReason(findingText(finding, "code")), findingText(finding, "detail"), scopeIssue, x.issue, findingText(finding, "citedRole"), x.child)
		}
	}
	q := l.q(ctx)
	if _, err := q.ExecContext(ctx, "UPDATE scope_bindings SET status = ?, updated_at = ?  WHERE scope_kind = ? AND scope_key = ? AND role = ? AND task_id = ?",
		lower, at, scopeIssue, x.issue, roleChild, x.child); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, "UPDATE scope_links SET status = ?, updated_at = ? WHERE link_id = ?", lower, at, linkID(linkExec, scopeProject, project, scopeIssue, x.issue)); err != nil {
		return err
	}
	return journal(ctx, l.r.Store, "scope_lifecycle", rid, contract.OrderedObject{{Key: "status", Value: status}, {Key: "lower", Value: lower}, {Key: "issueKey", Value: x.issue}}, at)
}

// BindScope is linkage.bind_scope: claim a scope for a task, deterministic and idempotent on
// the same claim. A refusal is recorded in the transaction that decided it, then returned.
func (r *Registry) BindScope(ctx context.Context, role, key string, endpoint Endpoint) (contract.OrderedObject, error) {
	kind, known := roleScope[role]
	if !known {
		return nil, refuse(contract.RefusalScopeRoleMismatch, "a role is one of child, parent, supervisor, not %s", pyStr(role))
	}
	for _, check := range []struct{ value, what string }{{key, "a scope key"}, {endpoint.TaskID, "a task id"}, {endpoint.HostID, "a host id"}} {
		if err := exact(check.value, check.what); err != nil {
			return nil, err
		}
	}
	l := r.linkage()
	bid := bindingID(role, kind, key, endpoint.TaskID)
	now := r.now()
	var refused *linkRefusal
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		plan, refusal, err := l.bindingPlan(ctx, role, key, endpoint, "")
		if err != nil {
			return err
		}
		if refusal != nil {
			refused = refusal
			return l.recordConflict(ctx, refusal, now)
		}
		return l.applyBindingPlan(ctx, plan, Active, now)
	})
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}
	return l.binding(ctx, bid)
}

// applyBindingPlan is linkage.apply_binding_plan.
func (l linkage) applyBindingPlan(ctx context.Context, b *bindingPlan, status, at string) error {
	q := l.q(ctx)
	switch b.action {
	case "insert":
		if _, err := q.ExecContext(ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"+
			" host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"+
			" handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)",
			b.id, b.role, b.scopeKind, b.scopeKey, b.endpoint.TaskID, b.endpoint.HostID, b.endpoint.Cwd, b.endpoint.CXCSession,
			status, 1, nil, nil, at, at); err != nil {
			return err
		}
		return journal(ctx, l.r.Store, "scope_bound", b.id, contract.OrderedObject{{Key: "role", Value: b.role}, {Key: "scopeKind", Value: b.scopeKind},
			{Key: "scopeKey", Value: b.scopeKey}, {Key: "taskId", Value: b.endpoint.TaskID}, {Key: "revision", Value: 1}}, at)
	case "reactivate":
		if _, err := q.ExecContext(ctx, "UPDATE scope_bindings SET status = ?, updated_at = ?, host_id = ?,  cwd = ?, cxc_session = ? WHERE binding_id = ?",
			status, at, b.endpoint.HostID, b.endpoint.Cwd, b.endpoint.CXCSession, b.id); err != nil {
			return err
		}
		return journal(ctx, l.r.Store, "scope_rebound", b.id, contract.OrderedObject{{Key: "scopeKey", Value: b.scopeKey}}, at)
	}
	return nil
}

// binding is linkage.binding: the _binding_record of one row, or nil.
func (l linkage) binding(ctx context.Context, bid string) (contract.OrderedObject, error) {
	row, err := l.r.Store.One(ctx, "SELECT * FROM scope_bindings WHERE binding_id = ?", bid)
	if err != nil || row == nil {
		return nil, err
	}
	return contract.OrderedObject{
		{Key: "bindingId", Value: row.Get("binding_id")}, {Key: "role", Value: row.Get("role")},
		{Key: "scopeKind", Value: row.Get("scope_kind")}, {Key: "scopeKey", Value: row.Get("scope_key")},
		{Key: "taskId", Value: row.Get("task_id")}, {Key: "hostId", Value: row.Get("host_id")}, {Key: "cwd", Value: row.Get("cwd")},
		{Key: "status", Value: row.Get("status")}, {Key: "revision", Value: row.Get("revision")},
		{Key: "supersedes", Value: row.Get("supersedes")}, {Key: "supersededBy", Value: row.Get("superseded_by")},
		{Key: "handoverNote", Value: row.Get("handover_note")}, {Key: "createdAt", Value: row.Get("created_at")},
		{Key: "updatedAt", Value: row.Get("updated_at")},
		{Key: "_bindings", Value: contract.OrderedObject{{Key: "cxcSession", Value: row.Get("cxc_session")}}},
	}, nil
}
