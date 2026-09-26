package registry

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The linkage commands (todo 26): supervision, peer links, issue attachment, handover,
// directives and their settlement, on top of the binding plan registration already uses.

const (
	scopeInitiative = "initiative"
	roleSupervisor  = "supervisor"
	linkReference   = "reference"
	linkPeer        = "peer"
	statusArchived  = "archived"
)

// roleScopeOwner is linkage.ROLE_SCOPE_OWNER.
var roleScopeOwner = map[string]string{scopeInitiative: "supervisor", scopeProject: "parent", scopeIssue: "child"}

// peerLinkID is link_id(PEER, ...): symmetric, so both ends converge on one record.
func peerLinkID(left, right string) string {
	low, high := left, right
	if high < low {
		low, high = high, low
	}
	return "lnk-" + sha256Hex(strings.Join([]string{linkPeer, scopeProject, low, scopeProject, high}, "|"))[:32]
}

// LinkID is linkage.link_id.
func LinkID(kind, upperKind, upperKey, lowerKind, lowerKey string) string {
	if kind == linkPeer {
		return peerLinkID(upperKey, lowerKey)
	}
	return linkID(kind, upperKind, upperKey, lowerKind, lowerKey)
}

// BindingID is linkage.binding_id.
func BindingID(role, kind, key, task string) string { return bindingID(role, kind, key, task) }

// directiveID is linkage.directive_id.
func directiveID(kind, key, fromScope, digest string, revision int64) string {
	return "dir-" + sha256Hex(strings.Join([]string{kind, key, fromScope, digest, strconv.FormatInt(revision, 10)}, "|"))[:32]
}

// ------------------------------------------------------------------ records

func bindingRecord(row store.Row) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "bindingId", Value: row.Get("binding_id")}, {Key: "role", Value: row.Get("role")},
		{Key: "scopeKind", Value: row.Get("scope_kind")}, {Key: "scopeKey", Value: row.Get("scope_key")},
		{Key: "taskId", Value: row.Get("task_id")}, {Key: "hostId", Value: row.Get("host_id")}, {Key: "cwd", Value: row.Get("cwd")},
		{Key: "status", Value: row.Get("status")}, {Key: "revision", Value: row.Get("revision")},
		{Key: "supersedes", Value: row.Get("supersedes")}, {Key: "supersededBy", Value: row.Get("superseded_by")},
		{Key: "handoverNote", Value: row.Get("handover_note")}, {Key: "createdAt", Value: row.Get("created_at")},
		{Key: "updatedAt", Value: row.Get("updated_at")},
		{Key: "_bindings", Value: contract.OrderedObject{{Key: "cxcSession", Value: row.Get("cxc_session")}}},
	}
}

func linkRecord(row store.Row) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "linkId", Value: row.Get("link_id")}, {Key: "kind", Value: row.Get("link_kind")},
		{Key: "upper", Value: contract.OrderedObject{{Key: "scopeKind", Value: row.Get("upper_kind")},
			{Key: "scopeKey", Value: row.Get("upper_key")}, {Key: "taskId", Value: row.Get("upper_task_id")}}},
		{Key: "lower", Value: contract.OrderedObject{{Key: "scopeKind", Value: row.Get("lower_kind")},
			{Key: "scopeKey", Value: row.Get("lower_key")}, {Key: "taskId", Value: row.Get("lower_task_id")}}},
		{Key: "status", Value: row.Get("status")}, {Key: "revision", Value: row.Get("revision")},
		{Key: "supersededBy", Value: row.Get("superseded_by")}, {Key: "createdAt", Value: row.Get("created_at")},
		{Key: "updatedAt", Value: row.Get("updated_at")},
	}
}

func directiveRecord(row store.Row) contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "directiveId", Value: row.Get("directive_id")}, {Key: "scopeKind", Value: row.Get("scope_kind")},
		{Key: "scopeKey", Value: row.Get("scope_key")}, {Key: "fromTaskId", Value: row.Get("from_task_id")},
		{Key: "fromScopeKey", Value: row.Get("from_scope_key")}, {Key: "linkId", Value: row.Get("link_id")},
		{Key: "linkKind", Value: row.Get("link_kind")}, {Key: "digest", Value: row.Get("digest")},
		{Key: "reference", Value: row.Get("reference")}, {Key: "revision", Value: row.Get("revision")},
		{Key: "disposition", Value: row.Get("disposition")}, {Key: "decidedBy", Value: row.Get("decided_by")},
		{Key: "decidedAt", Value: row.Get("decided_at")}, {Key: "recordedAt", Value: row.Get("recorded_at")},
	}
}

// field is record[key] as a string ("" for None).
func field(record contract.OrderedObject, key string) string {
	v, _ := getField(record, key)
	s, _ := v.(string)
	return s
}

func sub(record contract.OrderedObject, key string) contract.OrderedObject {
	v, _ := getField(record, key)
	o, _ := v.(contract.OrderedObject)
	return o
}

func revisionOf(record contract.OrderedObject) int64 {
	v, _ := getField(record, "revision")
	n, _ := v.(int64)
	return n
}

// orNone is Python's `value or None` for a text column.
func orNone(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	return v
}

func strList(values []string) []any { return anyStrings(values) }

func sortedSet(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

// ------------------------------------------------------------------ reading

// Binding is Linkage.binding; nil is None.
func (r *Registry) Binding(ctx context.Context, bid string) (contract.OrderedObject, error) {
	return r.linkage().binding(ctx, bid)
}

// Owner is Linkage.owner: the newest live binding of a scope, or nil.
func (r *Registry) Owner(ctx context.Context, kind, key string) (contract.OrderedObject, error) {
	row, err := r.Store.One(ctx, "SELECT * FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL"+
		"  ORDER BY revision DESC LIMIT 1", kind, key)
	if err != nil || row == nil {
		return nil, err
	}
	return bindingRecord(row), nil
}

// Owners is Linkage.owners: every live binding of a scope.
func (r *Registry) Owners(ctx context.Context, kind, key string) ([]contract.OrderedObject, error) {
	rows, err := r.Store.All(ctx, "SELECT * FROM scope_bindings"+
		"  WHERE scope_kind = ? AND scope_key = ? AND status IN ('active','paused')"+
		"    AND superseded_by IS NULL"+
		"  ORDER BY revision DESC, binding_id", kind, key)
	if err != nil {
		return nil, err
	}
	out := make([]contract.OrderedObject, len(rows))
	for i, row := range rows {
		out[i] = bindingRecord(row)
	}
	return out, nil
}

// soleOwner is Linkage._sole_owner: the one live owner, or nil with the contest appended.
func (r *Registry) soleOwner(ctx context.Context, kind, key string, contention *[]any) (any, error) {
	held, err := r.Owners(ctx, kind, key)
	if err != nil {
		return nil, err
	}
	if len(held) > 1 {
		candidates := make([]string, len(held))
		for i, h := range held {
			candidates[i] = field(h, "taskId")
		}
		slices.Sort(candidates)
		*contention = append(*contention, contract.OrderedObject{{Key: "contention", Value: "competing_owners"},
			{Key: "scopeKind", Value: kind}, {Key: "scopeKey", Value: key}, {Key: "candidates", Value: strList(candidates)}})
		return nil, nil
	}
	if len(held) == 0 {
		return nil, nil
	}
	return held[0], nil
}

// Link is Linkage.link; nil is None.
func (r *Registry) Link(ctx context.Context, lid string) (contract.OrderedObject, error) {
	row, err := r.Store.One(ctx, "SELECT * FROM scope_links WHERE link_id = ?", lid)
	if err != nil || row == nil {
		return nil, err
	}
	return linkRecord(row), nil
}

// Conflicts is Linkage.conflicts.
func (r *Registry) Conflicts(ctx context.Context, kind, key string) ([]any, error) {
	rows, err := r.Store.All(ctx, "SELECT * FROM linkage_conflicts  WHERE scope_kind = ? AND scope_key = ? ORDER BY id", kind, key)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, row := range rows {
		out = append(out, contract.OrderedObject{{Key: "at", Value: row.Get("at")}, {Key: "scopeKind", Value: row.Get("scope_kind")},
			{Key: "scopeKey", Value: row.Get("scope_key")}, {Key: "reason", Value: row.Get("reason")},
			{Key: "incumbent", Value: orNone(row.Get("incumbent"))}, {Key: "challenger", Value: orNone(row.Get("challenger"))},
			{Key: "detail", Value: row.Get("detail")}})
	}
	return out, nil
}

// Directives is Linkage.directives.
func (r *Registry) Directives(ctx context.Context, kind, key string) ([]contract.OrderedObject, error) {
	rows, err := r.Store.All(ctx, "SELECT * FROM scope_directives  WHERE scope_kind = ? AND scope_key = ? ORDER BY recorded_at, directive_id", kind, key)
	if err != nil {
		return nil, err
	}
	out := make([]contract.OrderedObject, len(rows))
	for i, row := range rows {
		out[i] = directiveRecord(row)
	}
	return out, nil
}

// ContestedDirectives is Linkage.contested_directives.
func (r *Registry) ContestedDirectives(ctx context.Context, kind, key string) ([]contract.OrderedObject, error) {
	all, err := r.Directives(ctx, kind, key)
	if err != nil {
		return nil, err
	}
	var open []contract.OrderedObject
	for _, d := range all {
		if v, _ := getField(d, "disposition"); v == nil {
			open = append(open, d)
		}
	}
	contesting := map[string]bool{}
	for i, first := range open {
		for _, second := range open[i+1:] {
			a, _ := getField(first, "reference")
			b, _ := getField(second, "reference")
			if directiveContest(field(first, "digest"), a, field(second, "digest"), b) != "" {
				contesting[field(first, "directiveId")] = true
				contesting[field(second, "directiveId")] = true
			}
		}
	}
	out := []contract.OrderedObject{}
	for _, d := range open {
		if contesting[field(d, "directiveId")] {
			out = append(out, d)
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ binding

// BindScopeAs is bind_scope with its status argument.
func (r *Registry) BindScopeAs(ctx context.Context, role, key string, endpoint Endpoint, status string) (contract.OrderedObject, error) {
	kind, known := roleScope[role]
	if !known {
		return nil, refuse(contract.RefusalScopeRoleMismatch, "a role is one of child, parent, supervisor, not %s", pyStr(role))
	}
	if status != Active && status != "paused" && status != "cancelled" && status != statusArchived {
		return nil, refuse(contract.RefusalLinkNotActive, "bad status %s", pyStr(status))
	}
	for _, check := range []struct{ value, what string }{{key, "a scope key"}, {endpoint.TaskID, "a task id"}, {endpoint.HostID, "a host id"}} {
		if err := exact(check.value, check.what); err != nil {
			return nil, err
		}
	}
	l := r.linkage()
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
		return l.applyBindingPlan(ctx, plan, status, now)
	})
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}
	return l.binding(ctx, bindingID(role, kind, key, endpoint.TaskID))
}

func (l linkage) insertBinding(ctx context.Context, bid, role, kind, key string, endpoint Endpoint, status string, revision int64, at string, supersedes, note sql.NullString) error {
	if _, err := l.q(ctx).ExecContext(ctx, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id,"+
		" host_id, cwd, cxc_session, status, revision, supersedes, superseded_by,"+
		" handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?)",
		bid, role, kind, key, endpoint.TaskID, endpoint.HostID, endpoint.Cwd, endpoint.CXCSession, status, revision, supersedes, note, at, at); err != nil {
		return err
	}
	return journal(ctx, l.r.Store, "scope_bound", bid, contract.OrderedObject{{Key: "role", Value: role}, {Key: "scopeKind", Value: kind},
		{Key: "scopeKey", Value: key}, {Key: "taskId", Value: endpoint.TaskID}, {Key: "revision", Value: revision}}, at)
}

type end struct{ kind, key, task string }

func (l linkage) insertLink(ctx context.Context, lid, kind string, upper, lower end, at string) error {
	if _, err := l.q(ctx).ExecContext(ctx, "INSERT INTO scope_links (link_id, link_kind, upper_kind, upper_key,"+
		" upper_task_id, lower_kind, lower_key, lower_task_id, status, revision,"+
		" superseded_by, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,?,?)",
		lid, kind, upper.kind, upper.key, upper.task, lower.kind, lower.key, lower.task, Active, 1, at, at); err != nil {
		return err
	}
	return journal(ctx, l.r.Store, "scope_linked", lid, contract.OrderedObject{{Key: "kind", Value: kind}, {Key: "upper", Value: upper.key},
		{Key: "lower", Value: lower.key}, {Key: "revision", Value: 1}}, at)
}

// ------------------------------------------------------------------ supervision

// RegisterSupervision is Linkage.register_supervision.
func (r *Registry) RegisterSupervision(ctx context.Context, initiative, project string, supervisor, parent Endpoint, kind string) (contract.OrderedObject, error) {
	if kind != linkExec && kind != linkReference {
		return nil, refuse(contract.RefusalScopeRoleMismatch, "a supervision is execution or reference, not %s; a peer link is registered with register_peer", pyStr(kind))
	}
	if err := exact(initiative, "an initiative key"); err != nil {
		return nil, err
	}
	if err := exact(project, "a project key"); err != nil {
		return nil, err
	}
	for _, side := range []struct {
		who      string
		endpoint Endpoint
	}{{"supervisor", supervisor}, {"parent", parent}} {
		if err := exact(side.endpoint.TaskID, "the "+side.who+"'s task id"); err != nil {
			return nil, err
		}
		if err := exact(side.endpoint.HostID, "the "+side.who+"'s host id"); err != nil {
			return nil, err
		}
	}
	lid := linkID(kind, scopeInitiative, initiative, scopeProject, project)
	now := r.now()
	l := r.linkage()
	var refused *linkRefusal
	var replayed contract.OrderedObject
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		replay, err := r.Store.One(ctx, "SELECT * FROM scope_links WHERE link_id = ?", lid)
		if err != nil {
			return err
		}
		var refusal *linkRefusal
		if replay != nil && colString(replay, "lower_task_id") == parent.TaskID && colString(replay, "upper_task_id") == supervisor.TaskID {
			for _, claim := range []struct {
				role, scope string
				endpoint    Endpoint
			}{{roleSupervisor, initiative, supervisor}, {roleParent, project, parent}} {
				_, hostRefusal, err := l.bindingPlan(ctx, claim.role, claim.scope, claim.endpoint, "")
				if err != nil {
					return err
				}
				if hostRefusal != nil {
					if err := l.recordConflict(ctx, hostRefusal, now); err != nil {
						return err
					}
					refusal = hostRefusal
					break
				}
			}
			if refusal == nil {
				replayed = linkRecord(replay)
				return nil
			}
		}
		refusal, err = l.supervisionRefusal(ctx, lid, initiative, project, supervisor, parent, kind, replay)
		if err != nil {
			return err
		}
		var supervisorPlan, parentPlan *bindingPlan
		if refusal == nil {
			if supervisorPlan, refusal, err = l.bindingPlan(ctx, roleSupervisor, initiative, supervisor, ""); err != nil {
				return err
			}
		}
		if refusal == nil {
			if parentPlan, refusal, err = l.bindingPlan(ctx, roleParent, project, parent, ""); err != nil {
				return err
			}
		}
		if refusal != nil {
			refused = refusal
			return l.recordConflict(ctx, refusal, now)
		}
		if err := l.applyBindingPlan(ctx, supervisorPlan, Active, now); err != nil {
			return err
		}
		if err := l.applyBindingPlan(ctx, parentPlan, Active, now); err != nil {
			return err
		}
		return l.insertLink(ctx, lid, kind, end{scopeInitiative, initiative, supervisor.TaskID}, end{scopeProject, project, parent.TaskID}, now)
	})
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}
	if replayed != nil {
		return replayed, nil
	}
	return r.Link(ctx, lid)
}

func (l linkage) supervisionRefusal(ctx context.Context, lid, initiative, project string, supervisor, parent Endpoint, kind string, replay store.Row) (*linkRefusal, error) {
	if supervisor.TaskID == parent.TaskID {
		return &linkRefusal{reason: contract.RefusalScopeCycle, detail: "task " + pyStr(supervisor.TaskID) + " cannot supervise itself",
			scopeKind: scopeProject, scopeKey: project, incumbent: supervisor.TaskID, challenger: parent.TaskID}, nil
	}
	if initiative == project {
		return &linkRefusal{reason: contract.RefusalScopeCycle, detail: "an initiative and a project cannot be the same scope " + pyStr(project),
			scopeKind: scopeProject, scopeKey: project, incumbent: initiative, challenger: project}, nil
	}
	reaches, err := l.reaches(ctx, scopeProject, project, supervisor.TaskID)
	if err != nil {
		return nil, err
	}
	if reaches {
		return &linkRefusal{reason: contract.RefusalScopeCycle,
			detail:    "task " + pyStr(supervisor.TaskID) + " already owns a scope below project " + pyStr(project) + ", so supervising it would close a loop",
			scopeKind: scopeProject, scopeKey: project, incumbent: project, challenger: supervisor.TaskID}, nil
	}
	if replay != nil {
		return &linkRefusal{reason: contract.RefusalLinkConflict,
			detail: lid + " already joins these scopes with different endpoints: " + pyStr(colString(replay, "upper_task_id")) +
				" over " + pyStr(colString(replay, "lower_task_id")),
			scopeKind: scopeProject, scopeKey: project, incumbent: colString(replay, "lower_task_id"), challenger: parent.TaskID}, nil
	}
	owning, err := l.r.Store.All(ctx, "SELECT link_id, upper_key, lower_task_id FROM scope_links"+
		"  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY link_id", scopeProject, project)
	if err != nil {
		return nil, err
	}
	if len(owning) > 1 {
		ids := make([]string, len(owning))
		for i, row := range owning {
			ids[i] = colString(row, "link_id")
		}
		return &linkRefusal{reason: contract.RefusalDuplicateScopeOwner,
			detail: "project " + pyStr(project) + " already has more than one live execution supervision (" + strings.Join(ids, ", ") +
				"), so there is no single supervision to register beside. The reading paths report this as competing_parents; repair the store rather than adding to it",
			scopeKind: scopeProject, scopeKey: project, incumbent: colString(owning[0], "upper_key"), challenger: initiative}, nil
	}
	if len(owning) == 0 {
		if kind == linkReference {
			return &linkRefusal{reason: contract.RefusalUnregisteredScope,
				detail:    "project " + pyStr(project) + " has no execution supervisor yet, so there is no outcome to reference. Register its execution supervision first",
				scopeKind: scopeProject, scopeKey: project, challenger: initiative}, nil
		}
		return nil, nil
	}
	held := owning[0]
	upper, lower, heldID := colString(held, "upper_key"), colString(held, "lower_task_id"), colString(held, "link_id")
	if kind == linkExec {
		return &linkRefusal{reason: contract.RefusalDuplicateScopeOwner,
			detail: "project " + pyStr(project) + " already has its execution supervisor under " + heldID + " from initiative " + pyStr(upper) +
				"; another initiative references it instead of supervising it again",
			scopeKind: scopeProject, scopeKey: project, incumbent: upper, challenger: initiative}, nil
	}
	if upper == initiative {
		return &linkRefusal{reason: contract.RefusalLinkConflict,
			detail: "initiative " + pyStr(initiative) + " already supervises project " + pyStr(project) + " under " + heldID +
				", so it cannot also reference it; a reference is how a DIFFERENT initiative reads this outcome",
			scopeKind: scopeProject, scopeKey: project, incumbent: upper, challenger: initiative}, nil
	}
	if lower != parent.TaskID {
		return &linkRefusal{reason: contract.RefusalDuplicateScopeOwner,
			detail: "project " + pyStr(project) + " is executed by parent " + pyStr(lower) + ", so a reference naming " + pyStr(parent.TaskID) +
				" would clone its execution parent",
			scopeKind: scopeProject, scopeKey: project, incumbent: lower, challenger: parent.TaskID}, nil
	}
	return nil, nil
}

// reaches is Linkage._reaches: does a live execution chain from this scope reach a scope task owns?
func (l linkage) reaches(ctx context.Context, kind, key, task string) (bool, error) {
	var count int64
	if err := l.q(ctx).QueryRowContext(ctx, "SELECT count(*) AS n FROM scope_links WHERE link_kind = 'execution'").Scan(&count); err != nil {
		return false, err
	}
	budget := count + 1
	frontier := [][2]string{{kind, key}}
	seen := map[[2]string]bool{}
	for len(frontier) > 0 && budget > 0 {
		budget--
		at := frontier[len(frontier)-1]
		frontier = frontier[:len(frontier)-1]
		if seen[at] {
			continue
		}
		seen[at] = true
		_, held, err := l.oneString(ctx, "SELECT 1 FROM scope_bindings  WHERE scope_kind = ? AND scope_key = ? AND task_id = ?"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL", at[0], at[1], task)
		if err != nil {
			return false, err
		}
		if held {
			return true, nil
		}
		rows, err := l.r.Store.All(ctx, "SELECT lower_kind, lower_key FROM scope_links"+
			"  WHERE upper_kind = ? AND upper_key = ? AND link_kind = 'execution'"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL", at[0], at[1])
		if err != nil {
			return false, err
		}
		for _, row := range rows {
			frontier = append(frontier, [2]string{colString(row, "lower_kind"), colString(row, "lower_key")})
		}
	}
	return false, nil
}

// ------------------------------------------------------------------ peer

// RegisterPeer is Linkage.register_peer.
func (r *Registry) RegisterPeer(ctx context.Context, leftProject string, leftParent Endpoint, rightProject string, rightParent Endpoint) (contract.OrderedObject, error) {
	if err := exact(leftProject, "a project key"); err != nil {
		return nil, err
	}
	if err := exact(rightProject, "a project key"); err != nil {
		return nil, err
	}
	for _, side := range []struct {
		name     string
		endpoint Endpoint
	}{{"left", leftParent}, {"right", rightParent}} {
		if err := exact(side.endpoint.TaskID, "the "+side.name+" parent's task id"); err != nil {
			return nil, err
		}
		if err := exact(side.endpoint.HostID, "the "+side.name+" parent's host id"); err != nil {
			return nil, err
		}
	}
	if leftProject == rightProject {
		return nil, refuse(contract.RefusalScopeCycle, "project %s is not its own peer", pyStr(leftProject))
	}
	lid := peerLinkID(leftProject, rightProject)
	now := r.now()
	l := r.linkage()
	type side struct {
		project  string
		endpoint Endpoint
	}
	sides := []side{{leftProject, leftParent}, {rightProject, rightParent}}
	if sides[1].project < sides[0].project {
		sides[0], sides[1] = sides[1], sides[0]
	}
	var refused *linkRefusal
	var replayed contract.OrderedObject
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		replay, err := r.Store.One(ctx, "SELECT * FROM scope_links WHERE link_id = ?", lid)
		if err != nil {
			return err
		}
		var refusal *linkRefusal
		for _, s := range sides {
			held, err := l.liveOwners(ctx, scopeProject, s.project, roleParent)
			if err != nil {
				return err
			}
			if len(held) > 1 {
				refusal = contestedOwner(scopeProject, s.project, held, s.endpoint.TaskID)
				break
			}
			if len(held) == 0 || held[0] != s.endpoint.TaskID {
				tail, incumbent := ", which has no registered parent", ""
				if len(held) == 1 {
					tail, incumbent = ", which is held by "+pyStr(held[0]), held[0]
				}
				refusal = &linkRefusal{reason: contract.RefusalScopeRoleMismatch,
					detail:    "task " + pyStr(s.endpoint.TaskID) + " is not the registered parent of project " + pyStr(s.project) + tail + "; a peer link joins two project parents",
					scopeKind: scopeProject, scopeKey: s.project, incumbent: incumbent, challenger: s.endpoint.TaskID}
				break
			}
			_, hostRefusal, err := l.bindingPlan(ctx, roleParent, s.project, s.endpoint, "")
			if err != nil {
				return err
			}
			if hostRefusal != nil {
				refusal = hostRefusal
				break
			}
		}
		if refusal == nil && replay != nil {
			replayed = linkRecord(replay)
			return nil
		}
		if refusal != nil {
			refused = refusal
			return l.recordConflict(ctx, refusal, now)
		}
		return l.insertLink(ctx, lid, linkPeer, end{scopeProject, sides[0].project, sides[0].endpoint.TaskID},
			end{scopeProject, sides[1].project, sides[1].endpoint.TaskID}, now)
	})
	if err != nil {
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}
	if replayed != nil {
		return replayed, nil
	}
	return r.Link(ctx, lid)
}

// ------------------------------------------------------------------ attachment

// AttachIssue is Linkage.attach_issue.
func (r *Registry) AttachIssue(ctx context.Context, rid, project string) (any, error) {
	now := r.now()
	l := r.linkage()
	var refused *linkRefusal
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		x, err := readRow(ctx, r.Store, rid)
		if err != nil {
			return err
		}
		if x == nil {
			return refuse(contract.RefusalUnregisteredRelationship, "no relationship %s", pyStr(rid))
		}
		refusal, err := l.attachIn(ctx, x, project, now, "")
		if err != nil || refusal == nil {
			return err
		}
		refused = refusal
		return l.recordConflict(ctx, refusal, now)
	})
	if err != nil {
		var refusedErr *store.RefusedError
		if errors.As(err, &refusedErr) {
			return nil, refusedErr
		}
		return nil, err
	}
	if refused != nil {
		return nil, refused.err()
	}
	return r.Attachment(ctx, rid)
}

// Attachment is Linkage.attachment: nil (None) when the relationship is unscoped.
func (r *Registry) Attachment(ctx context.Context, rid string) (any, error) {
	scoped, err := r.Store.One(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid)
	if err != nil || scoped == nil {
		return nil, err
	}
	project := colString(scoped, "project_key")
	relationship, err := r.Store.One(ctx, "SELECT issue_key FROM relationships WHERE relationship_id = ?", rid)
	if err != nil {
		return nil, err
	}
	var issue any
	if relationship != nil {
		issue = relationship.Get("issue_key")
	}
	contested := []any{}
	var child, link any
	if issue != nil {
		if child, err = r.soleOwner(ctx, scopeIssue, issue.(string), &contested); err != nil {
			return nil, err
		}
	}
	parentOwner, err := r.soleOwner(ctx, scopeProject, project, &contested)
	if err != nil {
		return nil, err
	}
	if issue != nil {
		found, err := r.Link(ctx, linkID(linkExec, scopeProject, project, scopeIssue, issue.(string)))
		if err != nil {
			return nil, err
		}
		if found != nil {
			link = found
		}
	}
	return contract.OrderedObject{{Key: "relationshipId", Value: rid}, {Key: "projectKey", Value: project}, {Key: "issueKey", Value: issue},
		{Key: "child", Value: child}, {Key: "parent", Value: parentOwner}, {Key: "link", Value: link}, {Key: "contention", Value: contested}}, nil
}

// ------------------------------------------------------------------ handover

// finishedStates is linkage.FINISHED_STATES.
var finishedStates = []string{StateMerged, StateClosed, StateAbandoned}

// Outstanding is Linkage.outstanding; task "" with narrow false is None.
func (r *Registry) Outstanding(ctx context.Context, project string, task sql.NullString) ([]string, error) {
	rows, err := r.Store.All(ctx, "SELECT r.relationship_id AS rid, r.parent_task_id AS parent FROM relationships r"+
		"  JOIN relationship_scope s ON s.relationship_id = r.relationship_id"+
		" WHERE s.project_key = ?"+
		"   AND r.status IN ('active','paused') AND r.superseded_by IS NULL"+
		" ORDER BY r.created_at", project)
	if err != nil {
		return nil, err
	}
	view := NewAssignmentView(r)
	out := []string{}
	for _, row := range rows {
		if task.Valid && colString(row, "parent") != task.String {
			continue
		}
		state, err := view.State(ctx, colString(row, "rid"))
		if err != nil {
			return nil, err
		}
		if !slices.Contains(finishedStates, field(state, "state")) {
			out = append(out, colString(row, "rid"))
		}
	}
	return out, nil
}

// Attached is Linkage.attached.
func (r *Registry) Attached(ctx context.Context, project string, task, otherThan sql.NullString) ([]string, error) {
	rows, err := r.Store.All(ctx, "SELECT r.relationship_id FROM relationships r"+
		"  JOIN relationship_scope s ON s.relationship_id = r.relationship_id"+
		" WHERE s.project_key = ? AND r.status IN ('active','paused')"+
		"   AND r.superseded_by IS NULL"+
		"   AND (? IS NULL OR r.parent_task_id = ?)"+
		"   AND (? IS NULL OR r.parent_task_id != ?)"+
		" ORDER BY r.created_at", project, task, task, otherThan, otherThan)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range rows {
		out = append(out, colString(row, "relationship_id"))
	}
	return out, nil
}

// Handover is Linkage.handover.
func (r *Registry) Handover(ctx context.Context, role, key, expect string, endpoint Endpoint, acknowledged []string, evidence, actor string) (contract.OrderedObject, error) {
	kind, known := roleScope[role]
	if !known {
		return nil, refuse(contract.RefusalScopeRoleMismatch, "unknown role %s", pyStr(role))
	}
	if role == roleChild {
		return nil, refuse(contract.RefusalScopeRoleMismatch, "a child is replaced by registering its successor with supersedes, which moves "+
			"the assignment and its issue scope together; handover covers a supervisor or a parent")
	}
	if strings.TrimSpace(evidence) == "" {
		return nil, refuse(contract.RefusalHandoverUnconfirmed, "a handover carries its evidence")
	}
	for _, check := range []struct{ value, what string }{{key, "a scope key"}, {endpoint.TaskID, "the replacement owner's task id"},
		{endpoint.HostID, "the replacement owner's host id"}} {
		if err := exact(check.value, check.what); err != nil {
			return nil, err
		}
	}
	claimed := sortedSet(acknowledged)
	now := r.now()
	newID := bindingID(role, kind, key, endpoint.TaskID)
	l := r.linkage()
	var refused *linkRefusal
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, err := r.Store.One(ctx, "SELECT * FROM scope_bindings"+
			"  WHERE scope_kind = ? AND scope_key = ? AND role = ?"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL"+
			"  ORDER BY revision DESC LIMIT 1", kind, key, role)
		if err != nil {
			return err
		}
		var refusal *linkRefusal
		var current contract.OrderedObject
		switch {
		case row == nil:
			refusal = &linkRefusal{reason: contract.RefusalUnregisteredScope, detail: kind + " " + pyStr(key) + " has no live owner to replace",
				scopeKind: kind, scopeKey: key, challenger: endpoint.TaskID}
		case colString(row, "task_id") != expect:
			holder := colString(row, "task_id")
			refusal = &linkRefusal{reason: contract.RefusalHandoverUnconfirmed,
				detail: "this handover expects " + pyStr(expect) + " to hold " + pyStr(key) + ", but it is held by " + pyStr(holder) +
					"; re-read the scope before replacing its owner",
				scopeKind: kind, scopeKey: key, incumbent: holder, challenger: endpoint.TaskID}
		case endpoint.TaskID == expect:
			refusal = &linkRefusal{reason: contract.RefusalHandoverUnconfirmed,
				detail:    "task " + pyStr(endpoint.TaskID) + " already holds " + pyStr(key) + "; a handover replaces the owner with a different one",
				scopeKind: kind, scopeKey: key, incumbent: expect, challenger: endpoint.TaskID}
		default:
			current = bindingRecord(row)
			unfinished := []string{}
			if kind == scopeProject {
				if unfinished, err = r.Outstanding(ctx, key, sql.NullString{}); err != nil {
					return err
				}
			}
			unfinished = sortedSet(unfinished)
			if !slices.Equal(claimed, unfinished) {
				refusal = &linkRefusal{reason: contract.RefusalHandoverUnconfirmed,
					detail: "the outstanding work restated by this handover is " + pyRepr(strList(claimed)) + " but the store says it is " +
						pyRepr(strList(unfinished)) + "; a replacement owner confirms the unfinished work it takes on",
					scopeKind: kind, scopeKey: key, incumbent: expect, challenger: endpoint.TaskID}
			} else if kind == scopeProject {
				stillHere, err := r.Attached(ctx, key, sql.NullString{}, text(endpoint.TaskID))
				if err != nil {
					return err
				}
				if len(stillHere) > 0 {
					refusal = &linkRefusal{reason: contract.RefusalHandoverWouldStrand,
						detail: kind + " " + pyStr(key) + " still has unfinished work that this handover cannot move: " + pyRepr(strList(sortedSet(stillHere))) +
							" (of which " + pyRepr(strList(unfinished)) + " is unfinished; a settled one still reopens under the parent named on its own row)" +
							". An assignment's identity and its queued deliveries name its parent, so each one is moved by registering its successor with " +
							"supersedes before the scope changes hands",
						scopeKind: kind, scopeKey: key, incumbent: expect, challenger: endpoint.TaskID}
				}
			}
		}
		var plan *bindingPlan
		if refusal == nil {
			if plan, refusal, err = l.bindingPlan(ctx, role, key, endpoint, expect); err != nil {
				return err
			}
		}
		if refusal != nil {
			refused = refusal
			return l.recordConflict(ctx, refusal, now)
		}
		q := l.q(ctx)
		currentID := field(current, "bindingId")
		next := revisionOf(current) + 1
		if _, err := q.ExecContext(ctx, "UPDATE scope_bindings SET status = ?, superseded_by = ?, updated_at = ?  WHERE binding_id = ?",
			statusArchived, newID, now, currentID); err != nil {
			return err
		}
		if plan.action == "insert" {
			if err := l.insertBinding(ctx, newID, role, kind, key, endpoint, Active, next, now, text(currentID), text(evidence)); err != nil {
				return err
			}
		} else if _, err := q.ExecContext(ctx, "UPDATE scope_bindings SET status = ?, revision = ?, supersedes = ?,"+
			" handover_note = ?, superseded_by = NULL, updated_at = ?,"+
			" host_id = ?, cwd = ?, cxc_session = ?"+
			"  WHERE binding_id = ?", Active, next, currentID, evidence, now, endpoint.HostID, endpoint.Cwd, endpoint.CXCSession, newID); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "UPDATE scope_links SET lower_task_id = ?, revision = revision + 1,"+
			" updated_at = ? WHERE lower_kind = ? AND lower_key = ?"+
			"   AND lower_task_id = ? AND status IN ('active','paused')", endpoint.TaskID, now, kind, key, expect); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "UPDATE scope_links SET upper_task_id = ?, revision = revision + 1,"+
			" updated_at = ? WHERE upper_kind = ? AND upper_key = ?"+
			"   AND upper_task_id = ? AND status IN ('active','paused')", endpoint.TaskID, now, kind, key, expect); err != nil {
			return err
		}
		return journal(ctx, r.Store, "scope_handover", newID, contract.OrderedObject{{Key: "scopeKey", Value: key}, {Key: "from", Value: expect},
			{Key: "to", Value: endpoint.TaskID}, {Key: "actor", Value: actor}, {Key: "acknowledged", Value: strList(claimed)}}, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if refused != nil {
		return nil, refused.err()
	}
	return l.binding(ctx, newID)
}

// unwrapRefusal returns a refusal raised inside a transaction body as itself, not wrapped.
func unwrapRefusal(err error) error {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused
	}
	return err
}

// ------------------------------------------------------------------ directives

// RecordDirective is Linkage.record_directive; reference NULL is None.
func (r *Registry) RecordDirective(ctx context.Context, kind, key, fromTask, fromScope, link, digest string, reference sql.NullString) (contract.OrderedObject, error) {
	if err := exact(digest, "a directive digest"); err != nil {
		return nil, err
	}
	var ref any
	if reference.Valid {
		ref = reference.String
	}
	refText := pyStrOrNone(ref)
	now := r.now()
	l := r.linkage()
	var did string
	var refused *linkRefusal
	var replayed contract.OrderedObject
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, holds, err := l.oneString(ctx, "SELECT task_id FROM scope_bindings"+
			"  WHERE scope_key = ? AND task_id = ? AND status IN ('active','paused')"+
			"    AND superseded_by IS NULL", fromScope, fromTask)
		if err != nil {
			return err
		}
		edge, err := r.Store.One(ctx, "SELECT * FROM scope_links WHERE link_id = ?", link)
		if err != nil {
			return err
		}
		var refusal *linkRefusal
		switch {
		case !holds:
			refusal = &linkRefusal{reason: contract.RefusalScopeRoleMismatch,
				detail:    "task " + pyStr(fromTask) + " does not own scope " + pyStr(fromScope) + ", so it cannot instruct from it",
				scopeKind: kind, scopeKey: key, incumbent: fromScope, challenger: fromTask}
		case edge == nil || !isLive(colString(edge, "status")) || colString(edge, "superseded_by") != "":
			refusal = &linkRefusal{reason: contract.RefusalUnregisteredScope, detail: "link " + pyStr(link) + " is not a live link",
				scopeKind: kind, scopeKey: key, incumbent: link, challenger: fromTask}
		case colString(edge, "link_kind") != linkExec || colString(edge, "upper_key") != fromScope ||
			colString(edge, "lower_key") != key || colString(edge, "lower_kind") != kind:
			refusal = &linkRefusal{reason: contract.RefusalUnregisteredScope,
				detail: "link " + pyStr(link) + " does not join " + pyStr(fromScope) + " down to " + kind + " " + pyStr(key) +
					" by execution. A reference carries no authority to instruct: a " +
					"secondary initiative references a project's outcome instead of issuing it work",
				scopeKind: kind, scopeKey: key, incumbent: link, challenger: fromScope}
		case colString(edge, "upper_task_id") != fromTask:
			refusal = &linkRefusal{reason: contract.RefusalScopeRoleMismatch,
				detail:    "link " + pyStr(link) + " records " + pyStr(colString(edge, "upper_task_id")) + " as its upper endpoint, not " + pyStr(fromTask),
				scopeKind: kind, scopeKey: key, incumbent: colString(edge, "upper_task_id"), challenger: fromTask}
		default:
			contradiction, err := envelopeContradiction(ref, link, digest)
			if err != nil {
				return err
			}
			if contradiction != "" {
				refusal = &linkRefusal{reason: contract.RefusalLinkConflict, detail: contradiction,
					scopeKind: kind, scopeKey: key, incumbent: link, challenger: refText}
			}
		}
		if refusal == nil {
			revision, _ := edge.Get("revision").(int64)
			did = directiveID(kind, key, fromScope, digest, revision)
			replay, err := r.Store.One(ctx, "SELECT * FROM scope_directives WHERE directive_id = ?", did)
			if err != nil {
				return err
			}
			if replay != nil {
				disagreement := pointerDisagreement(replay.Get("reference"), ref)
				if disagreement == "" {
					replayed = directiveRecord(replay)
					return nil
				}
				refusal = &linkRefusal{reason: contract.RefusalLinkConflict, detail: disagreement,
					scopeKind: kind, scopeKey: key, incumbent: pyStrOrNone(replay.Get("reference")), challenger: refText}
			} else {
				if refusal, err = l.competitor(ctx, kind, key, digest, ref, did, revision); err != nil {
					return err
				}
				if refusal == nil {
					if _, err := l.q(ctx).ExecContext(ctx, "INSERT INTO scope_directives (directive_id, scope_kind, scope_key,"+
						" from_task_id, from_scope_key, link_id, link_kind, digest,"+
						" reference, revision, disposition, decided_by, decided_at,"+
						" recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,NULL,NULL,NULL,?)",
						did, kind, key, fromTask, fromScope, link, colString(edge, "link_kind"), digest, reference, revision, now); err != nil {
						return err
					}
					return journal(ctx, r.Store, "directive_recorded", did, contract.OrderedObject{{Key: "scopeKey", Value: key},
						{Key: "fromScopeKey", Value: fromScope}, {Key: "linkKind", Value: colString(edge, "link_kind")}}, now)
				}
			}
		}
		refused = refusal
		return l.recordConflict(ctx, refusal, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if refused != nil {
		return nil, refused.err()
	}
	if replayed != nil {
		return replayed, nil
	}
	row, err := r.Store.One(ctx, "SELECT * FROM scope_directives WHERE directive_id = ?", did)
	if err != nil || row == nil {
		return nil, err
	}
	return directiveRecord(row), nil
}

// pyStrOrNone is str(value) for a nullable text column: "None" for NULL.
func pyStrOrNone(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "None"
}

// competitor is Linkage._competitor_in.
func (l linkage) competitor(ctx context.Context, kind, key, digest string, reference any, challenger string, revision int64) (*linkRefusal, error) {
	here := directivePlace(reference)
	if here == nil {
		return nil, nil
	}
	rows, err := l.r.Store.All(ctx, "SELECT * FROM scope_directives"+
		"  WHERE scope_kind = ? AND scope_key = ? AND disposition IS NULL"+
		"  ORDER BY recorded_at, directive_id", kind, key)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		why := directiveContest(colString(row, "digest"), row.Get("reference"), digest, reference)
		if why == "" {
			if !samePlace(directivePlace(row.Get("reference")), here) {
				continue
			}
			why = "restated"
		}
		held := parseReference(row.Get("reference"))
		revisionText := strconv.FormatInt(row.Get("revision").(int64), 10)
		recorded := "directive " + pyStr(colString(row, "directive_id")) + ", recorded " + pyStrOrNone(row.Get("recorded_at")) +
			" by " + pyStr(colString(row, "from_task_id")) + " on link revision " + revisionText
		answering := ""
		if held != nil && held.hasCorrelation {
			answering = " answering " + pyStr(held.correlation)
		}
		settle := "codex-session-relay linkage-settle --directive " + colString(row, "directive_id") + " --disposition superseded --actor <your task id>"
		if why == "restated" {
			what := kind + " " + pyStr(key) + " already has this same " + held.purpose + answering + " live (" + recorded +
				", the same digest). It stays in force through the link's move to revision " + strconv.FormatInt(revision, 10) +
				", so there is nothing to record, and a second live copy would leave a later replacement settling one of two"
			return &linkRefusal{reason: contract.RefusalLinkConflict,
				detail: what + ". Nothing was recorded and the contest is retained. To re-issue it in your own name, settle it first - " +
					settle + " - then record this one again",
				scopeKind: kind, scopeKey: key, incumbent: colString(row, "directive_id"), challenger: challenger}, nil
		}
		var what string
		switch why {
		case "sole":
			what = kind + " " + pyStr(key) + " already has a live " + held.purpose + answering + " (" + recorded + "). A scope keeps" +
				" one live " + held.purpose + ", and a second would leave the parent two versions of it with no recorded order between them"
		case "answer":
			what = kind + " " + pyStr(key) + " already has a live " + held.purpose + answering + " (" + recorded +
				"). One message takes one answer of each purpose"
		default:
			what = kind + " " + pyStr(key) + " has a live directive whose purpose was never recorded (" + recorded + ", reference " +
				pyRepr(row.Get("reference")) + "), so this one cannot be placed beside it and the two would read as contradictory instructions"
		}
		return &linkRefusal{reason: contract.RefusalLinkConflict,
			detail: what + ". Recorded, the two would hold every report the project owes upward until one was settled, so nothing was recorded" +
				" and the contest is retained. To replace it, settle it first - " + settle +
				" - restating in this instruction whatever of it still applies, then record this one again",
			scopeKind: kind, scopeKey: key, incumbent: colString(row, "directive_id"), challenger: challenger}, nil
	}
	return nil, nil
}

// SettleDirective is Linkage.settle_directive; reason NULL is None.
func (r *Registry) SettleDirective(ctx context.Context, id, disposition, decidedBy string, reason sql.NullString) (contract.OrderedObject, error) {
	if disposition != "chosen" && disposition != "superseded" {
		return nil, refuse(contract.RefusalLinkNotActive, "a disposition is chosen or superseded, not %s", pyStr(disposition))
	}
	now := r.now()
	l := r.linkage()
	var refused *linkRefusal
	var already contract.OrderedObject
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, err := r.Store.One(ctx, "SELECT * FROM scope_directives WHERE directive_id = ?", id)
		if err != nil {
			return err
		}
		if row == nil {
			return refuse(contract.RefusalUnregisteredScope, "no directive %s", pyStr(id))
		}
		if stored := row.Get("disposition"); stored != nil {
			if stored != disposition {
				refused = &linkRefusal{reason: contract.RefusalLinkConflict,
					detail: "directive " + pyStr(id) + " was already settled as " + pyRepr(stored) + " by " + pyRepr(row.Get("decided_by")) +
						" at " + pyStrOrNone(row.Get("decided_at")) + "; the decision stands and a later instruction is recorded as its own directive",
					scopeKind: colString(row, "scope_kind"), scopeKey: colString(row, "scope_key"), incumbent: colString(row, "decided_by"), challenger: decidedBy}
				return l.recordConflict(ctx, refused, now)
			}
			already = directiveRecord(row)
			return nil
		}
		if _, err := l.q(ctx).ExecContext(ctx, "UPDATE scope_directives SET disposition = ?, decided_by = ?,  decided_at = ? WHERE directive_id = ?",
			disposition, decidedBy, now, id); err != nil {
			return err
		}
		return journal(ctx, r.Store, "directive_settled", id, contract.OrderedObject{{Key: "disposition", Value: disposition},
			{Key: "decidedBy", Value: decidedBy}, {Key: "reason", Value: nullable(reason)}}, now)
	})
	if err != nil {
		return nil, unwrapRefusal(err)
	}
	if refused != nil {
		return nil, refused.err()
	}
	if already != nil {
		return already, nil
	}
	row, err := r.Store.One(ctx, "SELECT * FROM scope_directives WHERE directive_id = ?", id)
	if err != nil || row == nil {
		return nil, err
	}
	return directiveRecord(row), nil
}

// SetBeforeRegisterTx installs the hook Register runs between its pre-check and its write
// transaction (tests only; nil removes it). It is where a racing claim is injected so the
// interleaving is decided rather than hoped for.
func (r *Registry) SetBeforeRegisterTx(hook func(Registration)) { r.beforeRegisterTx = hook }

// DirectiveID is linkage.directive_id.
func DirectiveID(kind, key, fromScope, digest string, revision int64) string {
	return directiveID(kind, key, fromScope, digest, revision)
}
