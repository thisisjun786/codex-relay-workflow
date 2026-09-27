package faults

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

var cNames = []string{"fault-show", "fault-next", "fault-retry", "fault-queue", "fault-cancel", "fault-stage"}

func cResponse(w io.Writer, value any, code int) int {
	if e := contract.Emit(w, cOrdered(value, "")); e != nil {
		return 3
	}
	return code
}
func cOrdered(v any, parent string) any {
	if m, ok := v.(map[string]any); ok {
		orders := map[string][]string{"": {"schema", "scopeKey", "faults", "limit", "next", "limits", "publications", "held", "budgets", "budgetsTruncated", "faultId", "found", "error", "reason", "detail", "publicationId", "state", "stage", "ref", "recorded", "remediationId"}, "target": {"team", "projectRef"}, "publicationsFull": {"publication_id", "fault_id", "kind", "trigger_key", "cycle", "tracker_ref", "external_ref", "summary", "identity_digest", "state", "attempts", "next_attempt_at", "lease_owner", "lease_until", "issued_at", "last_error", "external_result", "created_at", "updated_at", "confirmed_at", "payload", "target", "holdReason", "history"}, "faults": {"seq", "fault_id", "product", "fault_class", "component", "severity", "signature", "scope", "scope_key", "state", "cycle", "episode", "occurrence_count", "reopen_count", "detail", "suppression", "external_ref", "first_seen_at", "last_seen_at", "cleared_at", "published_at", "resolved_at", "updated_at", "linkState", "linkedProject", "occurrences", "publicationsTruncated", "publications", "clears"}, "occurrences": {"occurrence_id", "fault_id", "episode", "occurrence_key", "severity", "cleared", "detail", "evidence", "evidence_digest", "truncated", "observed_at", "recorded_at", "recorded_ts"}, "publications": {"publication_id", "kind", "trigger_key", "state", "attempts", "external_ref", "last_error"}, "held": {"publicationId", "kind", "product", "reason"}, "budgets": {"product", "kind", "limit", "window", "used", "remaining", "source"}}
		order := orders[parent]
		if parent == "history" || parent == "attempts" {
			order = []string{"attempt", "owner", "takeover", "claimed_at", "issued_at", "outcome", "error", "ended", "ended_at"}
		}
		if parent == "remediations" {
			order = []string{"remediation_id", "fault_id", "cycle", "kind", "ref", "method", "outcome", "detail", "recorded_at"}
		}
		if parent == "publicationsFull" {
			order = orders["publicationsFull"]
		}
		if parent == "" {
			if _, ok := m["publication_id"]; ok {
				order = orders["publicationsFull"]
			}
			if _, ok := m["fault_id"]; ok {
				if _, yes := m["occurrences"]; yes {
					order = []string{"fault_id", "product", "fault_class", "component", "severity", "signature", "scope", "scope_key", "state", "cycle", "episode", "occurrence_count", "reopen_count", "detail", "suppression", "external_ref", "first_seen_at", "last_seen_at", "cleared_at", "published_at", "resolved_at", "updated_at", "linkState", "linkedProject", "occurrences", "remediations", "publications", "progress"}
				}
			}
			if _, ok := m["readingsTotal"]; ok {
				order = []string{"read", "recorded", "queued", "gaps", "readingsNext", "readingsTotal", "limits"}
			}
			if _, ok := m["awaitingRecord"]; ok {
				order = []string{"publicationId", "kind", "trigger", "queued", "awaitingTarget", "awaitingRecord", "reason"}
			}
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.SliceStable(keys, func(i, j int) bool {
			rank := func(k string) int {
				for x, v := range order {
					if v == k {
						return x
					}
				}
				return len(order)
			}
			a, b := rank(keys[i]), rank(keys[j])
			if a != b {
				return a < b
			}
			return keys[i] < keys[j]
		})
		out := make(contract.OrderedObject, 0, len(keys))
		for _, k := range keys {
			child := k
			if k == "publications" && parent == "" {
				_, isFault := m["fault_id"]
				_, isNext := m["budgets"]
				if isFault || isNext {
					child = "publicationsFull"
				}
			}
			out = append(out, contract.Field{Key: k, Value: cOrdered(m[k], child)})
		}
		return out
	}
	if list, ok := v.([]any); ok {
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = cOrdered(item, parent)
		}
		return out
	}
	return v
}
func cLimit(raw, name string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, e := strconv.Atoi(raw)
	if e != nil || n < 1 {
		return 0, fmt.Errorf("fault_observation_malformed: %s is a positive integer, not %s", name, raw)
	}
	if n > 1000 {
		n = 1000
	}
	return n, nil
}
func cView(r row) map[string]any {
	out := map[string]any{}
	for _, col := range r {
		if col.Name != "claim_token" && col.Name != "fault_product" && col.Name != "seq" && col.Name != "turn" {
			out[col.Name] = col.Value
		}
	}
	return out
}
func cOccurrences(rows []row) []any {
	out := cList(rows)
	for _, entry := range out {
		m := entry.(map[string]any)
		m["cleared"] = m["cleared"] == int64(1)
		m["truncated"] = m["truncated"] == int64(1)
		if raw, ok := m["evidence"].(string); ok {
			m["evidence"], _ = loads(raw)
		}
	}
	return out
}
func cList(rows []row) []any {
	out := make([]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, cView(r))
	}
	return out
}
func cFault(ctx context.Context, l *Ledger, id string) (row, error) {
	r, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=COALESCE((SELECT fault_id FROM fault_aliases WHERE alias_id=?),?)", id, id)
	if e != nil {
		return nil, e
	}
	if r == nil {
		return nil, fmt.Errorf("fault_unknown: no fault %s", f1Repr(id))
	}
	return r, nil
}
func cPublication(ctx context.Context, l *Ledger, id string) (map[string]any, error) {
	r, e := l.one(ctx, "SELECT * FROM fault_publications WHERE publication_id=?", id)
	if e != nil {
		return nil, e
	}
	if r == nil {
		return nil, fmt.Errorf("fault_unknown: no publication '%s'", id)
	}
	out := cView(r)
	extra, e := l.one(ctx, "SELECT * FROM fault_publication_payloads WHERE publication_id=?", id)
	if e != nil {
		return nil, e
	}
	var payload, project, hold any
	if extra != nil {
		project = extra.Get("project_ref")
		hold = extra.Get("hold_reason")
		if extra.Get("payload") != nil {
			payload, _ = loads(text(extra, "payload"))
		}
	}
	out["payload"] = payload
	out["target"] = map[string]any{"team": r.Get("tracker_ref"), "projectRef": project}
	out["holdReason"] = hold
	history, e := l.Store.All(ctx, "SELECT attempt,owner,takeover,claimed_at,issued_at,outcome,error,ended,ended_at FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id DESC LIMIT 3", id)
	if e != nil {
		return nil, e
	}
	slices.Reverse(history)
	out["history"] = cList(history)
	return out, nil
}
func executeC(ctx context.Context, l *Ledger, name string, a map[string]string) (any, error) {
	switch name {
	case "fault-show":
		return cShow(ctx, l, a)
	case "fault-next":
		return cNext(ctx, l, a)
	case "fault-retry", "fault-cancel":
		return cRetryCancel(ctx, l, name, a)
	case "fault-stage":
		return cStage(ctx, l, a)
	case "fault-queue":
		return cQueue(ctx, l, a)
	}
	return nil, fmt.Errorf("fault_observation_malformed: unknown command %s", name)
}

type cMissingFault struct{ id string }

func (e *cMissingFault) Error() string { return "fault not found: " + e.id }
func cShow(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	limit, e := cLimit(a["--limit"], "--limit", 20)
	if e != nil {
		return nil, e
	}
	id, pub := a["--fault"], a["--publication"]
	if id != "" && pub != "" {
		return nil, fmt.Errorf("usage: argument --publication: not allowed with argument --fault")
	}
	if id != "" || pub != "" {
		var ignored []string
		for _, flag := range []string{"--product", "--fault-class", "--scope", "--fault-state", "--after"} {
			if _, ok := a[flag]; ok {
				ignored = append(ignored, flag)
			}
		}
		if len(ignored) > 0 {
			single := "--fault"
			if pub != "" {
				single = "--publication"
			}
			return nil, fmt.Errorf("fault_observation_malformed: %s filter a listing and would be ignored beside %s", strings.Join(ignored, ", "), single)
		}
	}
	if pub != "" {
		view, e := cPublication(ctx, l, pub)
		if e != nil {
			return nil, e
		}
		rows, e := l.Store.All(ctx, "SELECT attempt,owner,takeover,claimed_at,issued_at,outcome,error,ended,ended_at FROM fault_publication_attempts WHERE publication_id=? ORDER BY attempt_id DESC LIMIT ?", pub, limit)
		if e != nil {
			return nil, e
		}
		slices.Reverse(rows)
		view["attempts"] = cList(rows)
		return view, nil
	}
	if id != "" {
		r, e := l.one(ctx, "SELECT * FROM fault_ledger WHERE fault_id=?", id)
		if e != nil {
			return nil, e
		}
		if r == nil {
			return nil, &cMissingFault{id}
		}
		out := cView(r)
		out["linkState"] = "none"
		out["linkedProject"] = nil
		for key, q := range map[string]string{"occurrences": "SELECT * FROM fault_occurrences WHERE fault_id=? ORDER BY rowid DESC LIMIT ?", "remediations": "SELECT * FROM fault_remediations WHERE fault_id=? ORDER BY rowid DESC LIMIT ?", "publications": "SELECT * FROM fault_publications WHERE fault_id=? ORDER BY rowid LIMIT ?"} {
			rows, e := l.Store.All(ctx, q, id, limit)
			if e != nil {
				return nil, e
			}
			if key == "occurrences" {
				out[key] = cOccurrences(rows)
			} else if key == "publications" {
				list := make([]any, 0, len(rows))
				for _, pub := range rows {
					view, e := cPublication(ctx, l, text(pub, "publication_id"))
					if e != nil {
						return nil, e
					}
					list = append(list, view)
				}
				out[key] = list
			} else {
				slices.Reverse(rows)
				out[key] = cList(rows)
			}
		}
		out["progress"] = map[string]any{}
		return out, nil
	}
	var after int
	if raw := a["--after"]; raw != "" {
		after, e = strconv.Atoi(raw)
		if e != nil || after < 0 {
			return nil, fmt.Errorf("fault_observation_malformed: after is a non-negative integer, not %s", raw)
		}
	}
	clauses := []string{"rowid > ?"}
	params := []any{after}
	for _, filter := range []struct{ flag, col string }{{"--product", "product"}, {"--fault-class", "fault_class"}, {"--scope", "scope_key"}, {"--fault-state", "state"}} {
		if value, ok := a[filter.flag]; ok {
			clauses = append(clauses, filter.col+" = ?")
			params = append(params, value)
		}
	}
	params = append(params, limit+1)
	rows, e := l.Store.All(ctx, "SELECT rowid AS seq,* FROM fault_ledger WHERE "+strings.Join(clauses, " AND ")+" ORDER BY rowid LIMIT ?", params...)
	if e != nil {
		return nil, e
	}
	var next any
	if len(rows) > limit {
		next = rows[limit-1].Get("seq")
		rows = rows[:limit]
	}
	faults := make([]any, 0, len(rows))
	for _, r := range rows {
		v := cView(r)
		v["seq"] = r.Get("seq")
		v["linkState"] = "none"
		v["linkedProject"] = nil
		occ, e := l.Store.All(ctx, "SELECT * FROM fault_occurrences WHERE fault_id=? ORDER BY rowid DESC LIMIT 3", text(r, "fault_id"))
		if e != nil {
			return nil, e
		}
		v["occurrences"] = cOccurrences(occ)
		pub, e := l.Store.All(ctx, "SELECT publication_id,kind,trigger_key,state,attempts,external_ref,last_error FROM fault_publications WHERE fault_id=? ORDER BY rowid DESC LIMIT 21", text(r, "fault_id"))
		if e != nil {
			return nil, e
		}
		v["publicationsTruncated"] = len(pub) > 20
		if len(pub) > 20 {
			pub = pub[:20]
		}
		slices.Reverse(pub)
		v["publications"] = cList(pub)
		clear, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_timeline WHERE fault_id=? AND kind='cleared'", text(r, "fault_id"))
		if e != nil {
			return nil, e
		}
		v["clears"] = integer(clear, "n")
		faults = append(faults, v)
	}
	return map[string]any{"schema": "fault-ledger/1", "scopeKey": nil, "faults": faults, "limit": limit, "next": next, "limits": "derived from this store only. A queued publication is not an issue anybody has written, and a confirmed one is not an issue anybody read. Pass next as after to continue; nested lists keep the newest 20 per fault"}, nil
}
func cNext(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	if e := l.expireLeases(ctx); e != nil {
		return nil, e
	}
	limit, e := cLimit(a["--limit"], "--limit", 4)
	if e != nil {
		return nil, e
	}
	var answer any
	e = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		answer, err = cQueueState(ctx, l, limit)
		return err
	})
	return answer, e
}

func cQueueState(ctx context.Context, l *Ledger, limit int) (any, error) {
	selected, e := cReady(ctx, l, limit)
	if e != nil {
		return nil, e
	}
	chosen := map[string]bool{}
	ready := []any{}
	for _, r := range selected {
		id := text(r, "publication_id")
		chosen[id] = true
		view, e := cPublication(ctx, l, id)
		if e != nil {
			return nil, e
		}
		ready = append(ready, view)
	}
	rows, e := l.Store.All(ctx, "SELECT p.*, f.product AS fault_product FROM fault_publications p JOIN fault_ledger f ON f.fault_id=p.fault_id WHERE p.state='pending' ORDER BY p.rowid LIMIT ?", limit*4)
	if e != nil {
		return nil, e
	}
	held := []any{}
	budgets := []any{}
	pairs, e := l.Store.All(ctx, "SELECT DISTINCT f.product,p.kind FROM fault_publications p JOIN fault_ledger f ON f.fault_id=p.fault_id WHERE p.state='pending' LIMIT 21")
	if e != nil {
		return nil, e
	}
	truncated := len(pairs) > 20
	if truncated {
		pairs = pairs[:20]
	}
	for _, pair := range pairs {
		kind, product := text(pair, "kind"), text(pair, "product")
		maximum := int64(20)
		if kind == openRecord {
			maximum = 5
		}
		custom, e := l.one(ctx, "SELECT max_count,window_seconds FROM fault_limits WHERE product=? AND kind=?", product, kind)
		if e != nil {
			return nil, e
		}
		window := 3600.0
		source := "default"
		if custom != nil {
			maximum = integer(custom, "max_count")
			window, _ = custom.Get("window_seconds").(float64)
			source = "override"
		}
		used, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product=? AND kind=? AND used_ts>?", product, kind, l.Clock.Now()-window)
		if e != nil {
			return nil, e
		}
		count := integer(used, "n")
		remaining := maximum - count
		if remaining < 0 {
			remaining = 0
		}
		budgets = append(budgets, map[string]any{"product": product, "kind": kind, "limit": maximum, "window": window, "used": count, "remaining": remaining, "source": source})
	}
	for _, r := range rows {
		id := text(r, "publication_id")
		if chosen[id] {
			continue
		}
		f, e := cFault(ctx, l, text(r, "fault_id"))
		if e != nil {
			return nil, e
		}
		classified := append(row{}, r...)
		classified = append(classified, f1Column("product", f.Get("product")), f1Column("scope_key", f.Get("scope_key")), f1Column("owned_ref", f.Get("external_ref")))
		reason, e := dAttentionReason(ctx, l, classified, l.Clock.Now())
		if e != nil {
			return nil, e
		}
		reasons := map[string]string{"ready": "budget_spent", "held": "budget_spent", "backingOff": "backing_off", "awaitingRecord": "awaiting_record", "awaitingTarget": "awaiting_target", "scopeKeyContested": "scope_key_contested", "kindUnregistered": "kind_unregistered", "issueOwned": "issue_owned"}
		held = append(held, map[string]any{"publicationId": id, "kind": text(r, "kind"), "product": text(r, "fault_product"), "reason": reasons[reason]})
		if len(held) >= limit {
			break
		}
	}
	return map[string]any{"publications": ready, "held": held, "budgets": budgets, "budgetsTruncated": truncated}, nil
}

// cReady excludes spent pairs before the bounded query and interleaves products,
// then spends only a local offer allowance (listing never consumes the budget).
func cReady(ctx context.Context, l *Ledger, limit int) ([]row, error) {
	registered, issue, targeted, projected := []string{}, []string{}, []string{}, []string{}
	for name := range kinds {
		spec, ok := executableKind(name)
		if !ok {
			continue
		}
		registered = append(registered, name)
		if spec.RequiresIssue {
			issue = append(issue, name)
		}
		if spec.Target != "" {
			targeted = append(targeted, name)
		}
		if spec.Target == "team+project" {
			projected = append(projected, name)
		}
	}
	listed := func(names []string) string {
		sort.Strings(names)
		quoted := make([]string, len(names))
		for i, name := range names {
			quoted[i] = "'" + strings.ReplaceAll(name, "'", "''") + "'"
		}
		return "(" + strings.Join(quoted, ",") + ")"
	}
	query := `SELECT * FROM (SELECT p.*,f.product AS fault_product,p.rowid AS seq,
 ROW_NUMBER() OVER (PARTITION BY f.product ORDER BY p.rowid) AS turn
 FROM fault_publications p JOIN fault_ledger f ON f.fault_id=p.fault_id
 LEFT JOIN fault_targets t ON t.scope_key=f.scope_key
 LEFT JOIN fault_target_projects tp ON tp.scope_key=f.scope_key AND tp.product=f.product
 WHERE p.state='pending' AND (p.next_attempt_at IS NULL OR p.next_attempt_at<=?)
 AND p.kind IN ` + listed(registered) + `
 AND (SELECT COUNT(*) FROM fault_budget_uses u WHERE u.product=f.product AND u.kind=p.kind
      AND u.used_ts > ? - COALESCE((SELECT window_seconds FROM fault_limits l WHERE l.product=f.product AND l.kind=p.kind),3600))
     < COALESCE((SELECT max_count FROM fault_limits l WHERE l.product=f.product AND l.kind=p.kind),CASE p.kind WHEN 'open_record' THEN 5 WHEN 'notification' THEN 10 ELSE 20 END)
 AND (p.kind NOT IN ` + listed(issue) + ` OR f.external_ref IS NOT NULL)
 AND (p.kind!='open_record' OR f.external_ref IS NULL)
 AND (p.kind NOT IN ` + listed(targeted) + ` OR (t.scope_key IS NOT NULL AND tp.scope_key IS NOT NULL AND NOT EXISTS(SELECT 1 FROM fault_ledger o WHERE o.scope_key=f.scope_key AND o.product!=f.product)))
 AND (p.kind NOT IN ` + listed(projected) + ` OR tp.project_ref IS NOT NULL)) ORDER BY turn,seq LIMIT ?`
	rows, err := l.Store.All(ctx, query, l.Clock.Now(), l.Clock.Now(), limit*4)
	if err != nil {
		return nil, err
	}
	remaining := map[[2]string]int64{}
	chosen := []row{}
	for _, r := range rows {
		pair := [2]string{text(r, "fault_product"), text(r, "kind")}
		if _, ok := remaining[pair]; !ok {
			maximum := int64(20)
			if pair[1] == openRecord {
				maximum = 5
			}
			window := 3600.0
			custom, e := l.one(ctx, "SELECT max_count,window_seconds FROM fault_limits WHERE product=? AND kind=?", pair[0], pair[1])
			if e != nil {
				return nil, e
			}
			if custom != nil {
				maximum = integer(custom, "max_count")
				window = f1Number(custom.Get("window_seconds"))
			}
			used, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product=? AND kind=? AND used_ts>?", pair[0], pair[1], l.Clock.Now()-window)
			if e != nil {
				return nil, e
			}
			remaining[pair] = maximum - integer(used, "n")
		}
		if remaining[pair] <= 0 {
			continue
		}
		remaining[pair]--
		chosen = append(chosen, r)
		if len(chosen) >= limit {
			break
		}
	}
	return chosen, nil
}
