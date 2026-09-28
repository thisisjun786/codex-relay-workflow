package faults

import "context"

func dAttentionReason(ctx context.Context, l *Ledger, r row, moment float64) (string, error) {
	kind := text(r, "kind")
	spec, ok := executableKind(kind)
	if !ok {
		return "kindUnregistered", nil
	}
	if n, ok := r.Get("next_attempt_at").(float64); ok && n > moment {
		return "backingOff", nil
	}
	if kind == "open_record" && r.Get("owned_ref") != nil {
		return "issueOwned", nil
	}
	if spec.RequiresIssue && r.Get("owned_ref") == nil {
		return "awaitingRecord", nil
	}
	if spec.Target != "" {
		target, e := l.one(ctx, "SELECT t.scope_key,tp.product,tp.project_ref FROM fault_targets t LEFT JOIN fault_target_projects tp ON tp.scope_key=t.scope_key WHERE t.scope_key=?", text(r, "scope_key"))
		if e != nil {
			return "", e
		}
		if target == nil || target.Get("product") == nil {
			return "awaitingTarget", nil
		}
		if text(target, "product") != text(r, "product") {
			return "scopeKeyContested", nil
		}
		contested, e := l.one(ctx, "SELECT 1 FROM fault_ledger WHERE scope_key=? AND product!=? LIMIT 1", text(r, "scope_key"), text(r, "product"))
		if e != nil {
			return "", e
		}
		if contested != nil {
			return "scopeKeyContested", nil
		}
		if spec.Target == "team+project" && target.Get("project_ref") == nil {
			return "awaitingTarget", nil
		}
	}
	maximum := int64(20)
	if kind == "open_record" {
		maximum = 5
	}
	if kind == "notification" {
		maximum = 10
	}
	window := 3600.0
	budget, e := l.one(ctx, "SELECT max_count,window_seconds FROM fault_limits WHERE product=? AND kind=?", text(r, "product"), kind)
	if e != nil {
		return "", e
	}
	if budget != nil {
		maximum = integer(budget, "max_count")
		window = budget.Get("window_seconds").(float64)
	}
	used, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product=? AND kind=? AND used_ts>?", text(r, "product"), kind, moment-window)
	if e != nil {
		return "", e
	}
	if integer(used, "n") >= maximum {
		return "held", nil
	}
	return "ready", nil
}
