package faults

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

var cNames = []string{"fault-show", "fault-next", "fault-retry", "fault-queue", "fault-cancel", "fault-stage"}

func cLimit(ctx context.Context, raw, name string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	n, ok := integerArg(ctx, name, raw)
	if !ok || n < 1 {
		return 0, fmt.Errorf("fault_observation_malformed: %s is a positive integer, not %s", name, raw)
	}
	if n > 1000 {
		return 1000, nil
	}
	return int(n), nil
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
		return nil, fmt.Errorf("fault_unknown: no fault %s", quote.Value(id))
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
			payload, _ = loads(extra.Text("payload"))
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
	case "fault-retry":
		return cRetryCancel(ctx, l, name, a)
	case "fault-cancel":
		return l.Cancel(ctx, a["--publication"], a["--reason"])
	case "fault-stage":
		return cStage(ctx, l, a)
	case "fault-queue":
		return cQueue(ctx, l, a)
	}
	return nil, fmt.Errorf("fault_observation_malformed: unknown command %s", name)
}

type cMissingFault struct{ id string }

func (e *cMissingFault) Error() string { return "fault not found: " + e.id }

// Only these checks precede services.faults (and its lazy store) in Python.
func validateShow(ctx context.Context, a map[string]string) error {
	if raw := a["--limit"]; raw != "" {
		if n, ok := integerArg(ctx, "--limit", raw); !ok || n < 1 {
			return fmt.Errorf("fault_observation_malformed: --limit is a positive integer, not %s", raw)
		}
	}
	id, pub := a["--fault"], a["--publication"]
	if id != "" && pub != "" {
		return fmt.Errorf("usage: argument --publication: not allowed with argument --fault")
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
			return fmt.Errorf("fault_observation_malformed: %s filter a listing and would be ignored beside %s", strings.Join(ignored, ", "), single)
		}
	}
	return nil
}

func cShow(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	if e := validateShow(ctx, a); e != nil {
		return nil, e
	}
	limit, e := cLimit(ctx, a["--limit"], "--limit", 20)
	if e != nil {
		return nil, e
	}
	id, pub := a["--fault"], a["--publication"]
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
					view, e := cPublication(ctx, l, pub.Text("publication_id"))
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
	var after int64
	if raw := a["--after"]; raw != "" {
		n, ok := integerArg(ctx, "--after", raw)
		if !ok || n < 0 {
			return nil, fmt.Errorf("fault_observation_malformed: after is a non-negative integer, not %s", raw)
		}
		after = n
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
		occ, e := l.Store.All(ctx, "SELECT * FROM fault_occurrences WHERE fault_id=? ORDER BY rowid DESC LIMIT 3", r.Text("fault_id"))
		if e != nil {
			return nil, e
		}
		v["occurrences"] = cOccurrences(occ)
		pub, e := l.Store.All(ctx, "SELECT publication_id,kind,trigger_key,state,attempts,external_ref,last_error FROM fault_publications WHERE fault_id=? ORDER BY rowid DESC LIMIT 21", r.Text("fault_id"))
		if e != nil {
			return nil, e
		}
		v["publicationsTruncated"] = len(pub) > 20
		if len(pub) > 20 {
			pub = pub[:20]
		}
		slices.Reverse(pub)
		v["publications"] = cList(pub)
		clear, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_timeline WHERE fault_id=? AND kind='cleared'", r.Text("fault_id"))
		if e != nil {
			return nil, e
		}
		v["clears"] = integer(clear, "n")
		faults = append(faults, v)
	}
	return map[string]any{"schema": "fault-ledger/1", "scopeKey": nil, "faults": faults, "limit": limit, "next": next, "limits": "derived from this store only. A queued publication is not an issue anybody has written, and a confirmed one is not an issue anybody read. Pass next as after to continue; nested lists keep the newest 20 per fault"}, nil
}
func cNext(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	limit, e := cLimit(ctx, a["--limit"], "--limit", 4)
	if e != nil {
		return nil, e
	}
	if l.Store.ReadOnly() {
		// cmd_fault_next branches on services.store.read_only: a store this runtime may not
		// write (cutover.md, Read-only clients under a foreign owner) gets lease expiry
		// projected on a private copy (readonly_queue_state). The owner's own fault-next
		// persists it, so a lapsed claim is offered again and claim() takes it.
		projected, release, e := l.Store.Projection(ctx)
		if e != nil {
			return nil, e
		}
		defer release()
		l = &Ledger{Store: projected, Clock: l.Clock}
	}
	if e := l.expireLeases(ctx); e != nil {
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
		id := r.Text("publication_id")
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
		kind, product := pair.Text("kind"), pair.Text("product")
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
		id := r.Text("publication_id")
		if chosen[id] {
			continue
		}
		f, e := cFault(ctx, l, r.Text("fault_id"))
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
		held = append(held, map[string]any{"publicationId": id, "kind": r.Text("kind"), "product": r.Text("fault_product"), "reason": reasons[reason]})
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
		pair := [2]string{r.Text("fault_product"), r.Text("kind")}
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
