package registry

import (
	"context"
	"database/sql"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The linkage readers: down, up and counterpart. Each answers with a record and never raises for
// an absence; a store that did not answer is state unreadable with the fault kept in detail.

func unreadableDetail(err error) string { return store.PythonSQLiteError(err) }

func (r *Registry) contestedRows(ctx context.Context, kind, key string, contention *[]any) error {
	conflicts, err := r.Conflicts(ctx, kind, key)
	if err != nil {
		return err
	}
	*contention = append(*contention, conflicts...)
	directives, err := r.ContestedDirectives(ctx, kind, key)
	if err != nil {
		return err
	}
	for _, d := range directives {
		*contention = append(*contention, contract.OrderedObject{{Key: "contention", Value: "instruction_conflict"},
			{Key: "scopeKind", Value: kind}, {Key: "scopeKey", Value: key}, {Key: "directiveId", Value: field(d, "directiveId")},
			{Key: "fromScopeKey", Value: field(d, "fromScopeKey")}, {Key: "digest", Value: field(d, "digest")}})
	}
	return nil
}

func gap(kind, key string) contract.OrderedObject {
	return contract.OrderedObject{{Key: "gap", Value: kind + "_without_" + roleScopeOwner[kind]}, {Key: "scopeKind", Value: kind}, {Key: "scopeKey", Value: key}}
}

func contentionIs(rows []any, words ...string) bool {
	for _, row := range rows {
		if o, ok := row.(contract.OrderedObject); ok && slices.Contains(words, field(o, "contention")) {
			return true
		}
	}
	return false
}

func drift(lid, recorded, live string) contract.OrderedObject {
	return contract.OrderedObject{{Key: "contention", Value: "owner_drift"}, {Key: "linkId", Value: lid}, {Key: "recorded", Value: recorded}, {Key: "live", Value: live}}
}

// Down is Linkage.down.
func (r *Registry) Down(ctx context.Context, kind, key string) contract.OrderedObject {
	answer, err := r.down(ctx, kind, key)
	if err != nil {
		return contract.OrderedObject{{Key: "state", Value: "unreadable"}, {Key: "readable", Value: false}, {Key: "levels", Value: []any{}},
			{Key: "gaps", Value: []any{}}, {Key: "contention", Value: []any{}}, {Key: "detail", Value: unreadableDetail(err)}}
	}
	return answer
}

func (r *Registry) down(ctx context.Context, kind, key string) (contract.OrderedObject, error) {
	owners, err := r.Owners(ctx, kind, key)
	if err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		linked, err := r.Store.One(ctx, "SELECT 1 FROM scope_links"+
			"  WHERE ((upper_kind = ? AND upper_key = ?) OR (lower_kind = ? AND"+
			"         lower_key = ?)) AND status IN ('active','paused')", kind, key, kind, key)
		if err != nil {
			return nil, err
		}
		if linked == nil {
			conflicts, err := r.Conflicts(ctx, kind, key)
			if err != nil {
				return nil, err
			}
			return contract.OrderedObject{{Key: "state", Value: "unregistered"}, {Key: "readable", Value: true}, {Key: "levels", Value: []any{}},
				{Key: "gaps", Value: []any{gap(kind, key)}}, {Key: "contention", Value: conflicts}}, nil
		}
	}
	levels, gaps, contention := []any{}, []any{}, []any{}
	if err := r.descend(ctx, kind, key, &levels, &gaps, &contention, 0, map[[2]string]bool{}, nil); err != nil {
		return nil, err
	}
	if len(levels) == 0 {
		return contract.OrderedObject{{Key: "state", Value: "unregistered"}, {Key: "readable", Value: true}, {Key: "levels", Value: []any{}},
			{Key: "gaps", Value: gaps}, {Key: "contention", Value: contention}}, nil
	}
	state := "resolved"
	if contentionIs(contention, "competing_parents", "competing_owners") {
		state = "ambiguous"
	}
	return contract.OrderedObject{{Key: "state", Value: state}, {Key: "readable", Value: true}, {Key: "levels", Value: levels},
		{Key: "gaps", Value: gaps}, {Key: "contention", Value: contention}}, nil
}

func (r *Registry) incomingExecution(ctx context.Context, kind, key string) ([]string, error) {
	rows, err := r.Store.All(ctx, "SELECT link_id FROM scope_links"+
		"  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL", kind, key)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, row := range rows {
		out = append(out, colString(row, "link_id"))
	}
	slices.Sort(out)
	return out, nil
}

func (r *Registry) descend(ctx context.Context, kind, key string, levels, gaps, contention *[]any, depth int64, seen map[[2]string]bool, path [][2]string) error {
	here := [2]string{kind, key}
	if slices.Contains(path, here) {
		*contention = append(*contention, contract.OrderedObject{{Key: "contention", Value: "scope_cycle"}, {Key: "scopeKind", Value: kind}, {Key: "scopeKey", Value: key}})
		return nil
	}
	if seen[here] {
		candidates, err := r.incomingExecution(ctx, kind, key)
		if err != nil {
			return err
		}
		*contention = append(*contention, contract.OrderedObject{{Key: "contention", Value: "competing_parents"}, {Key: "scopeKind", Value: kind},
			{Key: "scopeKey", Value: key}, {Key: "candidates", Value: strList(candidates)}})
		return nil
	}
	seen[here] = true
	marked := len(*contention)
	owner, err := r.soleOwner(ctx, kind, key, contention)
	if err != nil {
		return err
	}
	*levels = append(*levels, contract.OrderedObject{{Key: "scopeKind", Value: kind}, {Key: "scopeKey", Value: key}, {Key: "owner", Value: owner}, {Key: "depth", Value: depth}})
	if owner == nil && len(*contention) == marked {
		*gaps = append(*gaps, gap(kind, key))
	}
	if err := r.contestedRows(ctx, kind, key, contention); err != nil {
		return err
	}
	rows, err := r.Store.All(ctx, "SELECT * FROM scope_links"+
		"  WHERE upper_kind = ? AND upper_key = ? AND link_kind = 'execution'"+
		"    AND status IN ('active','paused') AND superseded_by IS NULL"+
		"  ORDER BY lower_key", kind, key)
	if err != nil {
		return err
	}
	for _, row := range rows {
		edge := linkRecord(row)
		lid := field(edge, "linkId")
		upper, lower := sub(edge, "upper"), sub(edge, "lower")
		if o, ok := owner.(contract.OrderedObject); ok && field(o, "taskId") != field(upper, "taskId") {
			*contention = append(*contention, drift(lid, field(upper, "taskId"), field(o, "taskId")))
		}
		live, err := r.Owner(ctx, field(lower, "scopeKind"), field(lower, "scopeKey"))
		if err != nil {
			return err
		}
		if live != nil && field(live, "taskId") != field(lower, "taskId") {
			*contention = append(*contention, drift(lid, field(lower, "taskId"), field(live, "taskId")))
		}
		if err := r.descend(ctx, field(lower, "scopeKind"), field(lower, "scopeKey"), levels, gaps, contention, depth+1, seen,
			append(slices.Clone(path), here)); err != nil {
			return err
		}
	}
	return nil
}

// UpSelector is up()'s keyword arguments; an empty field is None.
type UpSelector struct{ Task, Issue, Relationship, Scope sql.NullString }

// Up is Linkage.up.
func (r *Registry) Up(ctx context.Context, sel UpSelector) contract.OrderedObject {
	answer, err := r.up(ctx, sel)
	if err != nil {
		return contract.OrderedObject{{Key: "state", Value: "unreadable"}, {Key: "readable", Value: false}, {Key: "levels", Value: []any{}},
			{Key: "gaps", Value: []any{}}, {Key: "contention", Value: []any{}}, {Key: "detail", Value: unreadableDetail(err)}}
	}
	return answer
}

func nullableText(v sql.NullString) any { return nullable(v) }

func (r *Registry) up(ctx context.Context, sel UpSelector) (contract.OrderedObject, error) {
	if sel.Task.Valid && !sel.Scope.Valid && !sel.Issue.Valid && !sel.Relationship.Valid {
		bindings, err := r.bindingsFor(ctx, sel.Task.String)
		if err != nil {
			return nil, err
		}
		var keys []string
		for _, b := range bindings {
			if isLive(field(b, "status")) {
				keys = append(keys, field(b, "scopeKey"))
			}
		}
		keys = sortedSet(keys)
		if len(keys) > 1 {
			return contract.OrderedObject{{Key: "state", Value: "ambiguous"}, {Key: "readable", Value: true}, {Key: "levels", Value: []any{}},
				{Key: "gaps", Value: []any{}}, {Key: "contention", Value: []any{contract.OrderedObject{{Key: "contention", Value: "ambiguous_scope"},
					{Key: "taskId", Value: sel.Task.String}, {Key: "candidates", Value: strList(keys)}}}}}, nil
		}
	}
	kind, key, found, err := r.startingScope(ctx, sel)
	if err != nil {
		return nil, err
	}
	if !found {
		return contract.OrderedObject{{Key: "state", Value: "unregistered"}, {Key: "readable", Value: true}, {Key: "levels", Value: []any{}},
			{Key: "gaps", Value: []any{contract.OrderedObject{{Key: "gap", Value: "unscoped_assignment"}, {Key: "relationshipId", Value: nullableText(sel.Relationship)},
				{Key: "issueKey", Value: nullableText(sel.Issue)}, {Key: "taskId", Value: nullableText(sel.Task)}}}},
			{Key: "contention", Value: []any{}}}, nil
	}
	levels, gaps, contention := []any{}, []any{}, []any{}
	seen := map[[2]string]bool{}
	for !seen[[2]string{kind, key}] {
		seen[[2]string{kind, key}] = true
		marked := len(contention)
		owner, err := r.soleOwner(ctx, kind, key, &contention)
		if err != nil {
			return nil, err
		}
		levels = append(levels, contract.OrderedObject{{Key: "scopeKind", Value: kind}, {Key: "scopeKey", Value: key}, {Key: "owner", Value: owner},
			{Key: "depth", Value: int64(len(levels))}})
		if owner == nil && len(contention) == marked {
			gaps = append(gaps, gap(kind, key))
		}
		if err := r.contestedRows(ctx, kind, key, &contention); err != nil {
			return nil, err
		}
		incoming, err := r.Store.All(ctx, "SELECT * FROM scope_links"+
			"  WHERE lower_kind = ? AND lower_key = ? AND link_kind = 'execution'"+
			"    AND status IN ('active','paused') AND superseded_by IS NULL"+
			"  ORDER BY revision DESC, link_id", kind, key)
		if err != nil {
			return nil, err
		}
		if len(incoming) > 1 {
			candidates := make([]string, len(incoming))
			for i, row := range incoming {
				candidates[i] = colString(row, "link_id")
			}
			slices.Sort(candidates)
			contention = append(contention, contract.OrderedObject{{Key: "contention", Value: "competing_parents"}, {Key: "scopeKind", Value: kind},
				{Key: "scopeKey", Value: key}, {Key: "candidates", Value: strList(candidates)}})
			return contract.OrderedObject{{Key: "state", Value: "ambiguous"}, {Key: "readable", Value: true}, {Key: "levels", Value: levels},
				{Key: "gaps", Value: gaps}, {Key: "contention", Value: contention}}, nil
		}
		if len(incoming) == 0 {
			if kind == scopeProject {
				gaps = append(gaps, contract.OrderedObject{{Key: "gap", Value: "no_supervisor"}, {Key: "scopeKind", Value: scopeProject}, {Key: "scopeKey", Value: key}})
			}
			break
		}
		edge := linkRecord(incoming[0])
		lid := field(edge, "linkId")
		upper, lower := sub(edge, "upper"), sub(edge, "lower")
		live, err := r.Owner(ctx, field(lower, "scopeKind"), field(lower, "scopeKey"))
		if err != nil {
			return nil, err
		}
		if live != nil && field(live, "taskId") != field(lower, "taskId") {
			contention = append(contention, drift(lid, field(lower, "taskId"), field(live, "taskId")))
		}
		above, err := r.Owners(ctx, field(upper, "scopeKind"), field(upper, "scopeKey"))
		if err != nil {
			return nil, err
		}
		if len(above) == 1 && field(above[0], "taskId") != field(upper, "taskId") {
			contention = append(contention, drift(lid, field(upper, "taskId"), field(above[0], "taskId")))
		}
		kind, key = field(upper, "scopeKind"), field(upper, "scopeKey")
	}
	state := "resolved"
	if contentionIs(contention, "competing_owners") {
		state = "ambiguous"
	}
	return contract.OrderedObject{{Key: "state", Value: state}, {Key: "readable", Value: true}, {Key: "levels", Value: levels},
		{Key: "gaps", Value: gaps}, {Key: "contention", Value: contention}}, nil
}

func (r *Registry) startingScope(ctx context.Context, sel UpSelector) (string, string, bool, error) {
	if sel.Relationship.Valid {
		row, err := r.Store.One(ctx, "SELECT issue_key FROM relationships WHERE relationship_id = ?", sel.Relationship.String)
		if err != nil || row == nil {
			return "", "", false, err
		}
		scoped, err := r.Store.One(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", sel.Relationship.String)
		if err != nil || scoped == nil {
			return "", "", false, err
		}
		return scopeIssue, colString(row, "issue_key"), true, nil
	}
	if sel.Issue.Valid {
		owner, err := r.Owner(ctx, scopeIssue, sel.Issue.String)
		if err != nil || owner == nil {
			return "", "", false, err
		}
		return scopeIssue, sel.Issue.String, true, nil
	}
	if sel.Task.Valid {
		if sel.Scope.Valid {
			bindings, err := r.bindingsFor(ctx, sel.Task.String)
			if err != nil {
				return "", "", false, err
			}
			for _, b := range bindings {
				if isLive(field(b, "status")) && field(b, "scopeKey") == sel.Scope.String {
					return field(b, "scopeKind"), field(b, "scopeKey"), true, nil
				}
			}
			return "", "", false, nil
		}
		row, err := r.Store.One(ctx, "SELECT * FROM scope_bindings WHERE task_id = ? AND status IN ('active','paused')"+
			"  AND superseded_by IS NULL ORDER BY revision DESC LIMIT 1", sel.Task.String)
		if err != nil || row == nil {
			return "", "", false, err
		}
		return colString(row, "scope_kind"), colString(row, "scope_key"), true, nil
	}
	return "", "", false, nil
}

// bindingsFor is Linkage._bindings_for: live ones first, newest revision first.
func (r *Registry) bindingsFor(ctx context.Context, task string) ([]contract.OrderedObject, error) {
	rows, err := r.Store.All(ctx, "SELECT * FROM scope_bindings WHERE task_id = ?"+
		"  ORDER BY CASE WHEN status IN ('active','paused') THEN 0 ELSE 1 END,"+
		"           revision DESC, scope_key", task)
	if err != nil {
		return nil, err
	}
	out := make([]contract.OrderedObject, len(rows))
	for i, row := range rows {
		out[i] = bindingRecord(row)
	}
	return out, nil
}

func (r *Registry) joiningLinks(ctx context.Context, sender, recipient contract.OrderedObject) ([]contract.OrderedObject, error) {
	sk, sv, rk, rv := field(sender, "scopeKind"), field(sender, "scopeKey"), field(recipient, "scopeKind"), field(recipient, "scopeKey")
	rows, err := r.Store.All(ctx, "SELECT * FROM scope_links"+
		"  WHERE status IN ('active','paused') AND superseded_by IS NULL"+
		"    AND ((upper_kind = ? AND upper_key = ? AND lower_kind = ? AND lower_key = ?)"+
		"      OR (upper_kind = ? AND upper_key = ? AND lower_kind = ? AND lower_key = ?))"+
		"  ORDER BY revision DESC, link_id", sk, sv, rk, rv, rk, rv, sk, sv)
	if err != nil {
		return nil, err
	}
	out := make([]contract.OrderedObject, len(rows))
	for i, row := range rows {
		out[i] = linkRecord(row)
	}
	return out, nil
}

func rolePairIsWrong(sender, recipient contract.OrderedObject) bool {
	pair := [2]string{field(sender, "role"), field(recipient, "role")}
	return !slices.Contains([][2]string{{roleSupervisor, roleParent}, {roleParent, roleSupervisor}, {roleParent, roleChild},
		{roleChild, roleParent}, {roleParent, roleParent}}, pair)
}

// CounterpartQuery is counterpart()'s keyword arguments; invalid is None.
type CounterpartQuery struct {
	QuotedRevision         sql.NullInt64
	QuotedScope, FromScope sql.NullString
}

// Counterpart is Linkage.counterpart.
func (r *Registry) Counterpart(ctx context.Context, from, to string, q CounterpartQuery) contract.OrderedObject {
	answer, err := r.counterpart(ctx, from, to, q)
	if err != nil {
		return contract.OrderedObject{{Key: "state", Value: "unreadable"}, {Key: "readable", Value: false}, {Key: "link", Value: nil},
			{Key: "from", Value: nil}, {Key: "counterpart", Value: nil}, {Key: "currentOwner", Value: nil}, {Key: "findings", Value: []any{}},
			{Key: "detail", Value: unreadableDetail(err)}}
	}
	return answer
}

func objOrNil(o contract.OrderedObject) any {
	if o == nil {
		return nil
	}
	return o
}

func (r *Registry) counterpart(ctx context.Context, from, to string, q CounterpartQuery) (contract.OrderedObject, error) {
	senders, err := r.bindingsFor(ctx, from)
	if err != nil {
		return nil, err
	}
	recipients, err := r.bindingsFor(ctx, to)
	if err != nil {
		return nil, err
	}
	var findings []string
	narrow := func(list []contract.OrderedObject, scope string) []contract.OrderedObject {
		var out []contract.OrderedObject
		for _, b := range list {
			if field(b, "scopeKey") == scope {
				out = append(out, b)
			}
		}
		return out
	}
	if q.FromScope.Valid {
		if narrowed := narrow(senders, q.FromScope.String); len(narrowed) > 0 {
			senders = narrowed
		} else {
			findings = append(findings, "foreign_sender_scope")
			senders = nil
		}
	}
	wrongScope := false
	if q.QuotedScope.Valid {
		if narrowed := narrow(recipients, q.QuotedScope.String); len(narrowed) > 0 {
			recipients = narrowed
		} else {
			wrongScope = true
			recipients = nil
		}
	}
	var sender, recipient, edge contract.OrderedObject
	if len(senders) > 0 {
		sender = senders[0]
	}
	if len(recipients) > 0 {
		recipient = recipients[0]
	}
	type match struct {
		sender, recipient contract.OrderedObject
		joined            []contract.OrderedObject
	}
	var matches []match
	for _, s := range senders {
		for _, rc := range recipients {
			joined, err := r.joiningLinks(ctx, s, rc)
			if err != nil {
				return nil, err
			}
			if len(joined) > 0 {
				matches = append(matches, match{s, rc, joined})
			}
		}
	}
	var contention []any
	if len(matches) == 1 && len(matches[0].joined) == 1 {
		sender, recipient, edge = matches[0].sender, matches[0].recipient, matches[0].joined[0]
	} else if len(matches) > 0 {
		contention = []any{}
		for _, m := range matches {
			for _, record := range m.joined {
				contention = append(contention, contract.OrderedObject{{Key: "linkId", Value: field(record, "linkId")},
					{Key: "kind", Value: field(record, "kind")}, {Key: "scopeKey", Value: field(m.recipient, "scopeKey")}})
			}
		}
	}
	var current any
	stale := func(b contract.OrderedObject) bool {
		v, _ := getField(b, "supersededBy")
		return !isLive(field(b, "status")) || (v != nil && v != "")
	}
	if recipient != nil && stale(recipient) {
		findings = append(findings, "stale_owner")
		held, err := r.Owners(ctx, field(recipient, "scopeKind"), field(recipient, "scopeKey"))
		if err != nil {
			return nil, err
		}
		if len(held) > 1 {
			findings = append(findings, "competing_owners")
		} else if len(held) == 1 {
			current = held[0]
		}
	}
	if sender != nil && stale(sender) {
		findings = append(findings, "stale_sender")
	}
	if wrongScope {
		findings = append(findings, "foreign_scope")
	}
	answer := func(state string, link any, extra ...contract.Field) contract.OrderedObject {
		out := contract.OrderedObject{{Key: "state", Value: state}, {Key: "readable", Value: true}, {Key: "link", Value: link},
			{Key: "from", Value: objOrNil(sender)}, {Key: "counterpart", Value: objOrNil(recipient)}, {Key: "currentOwner", Value: current}}
		out = append(out, extra...)
		return append(out, contract.Field{Key: "findings", Value: strList(sortedSet(append([]string{}, findings...)))})
	}
	if contention != nil {
		findings = append(findings, "link_contention")
		if rolePairIsWrong(sender, recipient) {
			findings = append(findings, "wrong_role")
		}
		return answer("ambiguous", nil, contract.Field{Key: "candidates", Value: contention}), nil
	}
	if sender == nil || recipient == nil {
		if recipient == nil {
			findings = append(findings, "unregistered_link")
		}
		return answer("unlinked", nil), nil
	}
	if edge == nil {
		findings = append(findings, "unregistered_link")
		if rolePairIsWrong(sender, recipient) {
			findings = append(findings, "wrong_role")
		}
		return answer("unlinked", nil), nil
	}
	if rolePairIsWrong(sender, recipient) {
		findings = append(findings, "wrong_role")
	}
	if q.QuotedRevision.Valid && q.QuotedRevision.Int64 != revisionOf(edge) {
		findings = append(findings, "stale_revision")
	}
	for _, side := range []string{"upper", "lower"} {
		end := sub(edge, side)
		held, err := r.Owners(ctx, field(end, "scopeKind"), field(end, "scopeKey"))
		if err != nil {
			return nil, err
		}
		if len(held) > 1 {
			findings = append(findings, "competing_owners")
			continue
		}
		if len(held) == 1 && field(held[0], "taskId") != field(end, "taskId") {
			findings = append(findings, "owner_drift")
		}
	}
	contested, err := r.ContestedDirectives(ctx, field(recipient, "scopeKind"), field(recipient, "scopeKey"))
	if err != nil {
		return nil, err
	}
	if len(contested) > 0 {
		findings = append(findings, "instruction_conflict")
	}
	status, _ := getField(edge, "status")
	return answer("linked", contract.OrderedObject{{Key: "linkId", Value: field(edge, "linkId")}, {Key: "kind", Value: field(edge, "kind")},
		{Key: "revision", Value: revisionOf(edge)}, {Key: "status", Value: status}}), nil
}
