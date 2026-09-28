package faults

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

var dNames = []string{"fault-policy", "fault-limit", "fault-attention", "fault-relink", "fault-notifications", "fault-notification-raise", "fault-notification-reserve", "fault-notification-ack", "fault-notification-fail", "fault-notification-reconcile"}

func dResponse(w io.Writer, value any, code int) int {
	parent := ""
	if code != 0 {
		parent = "refusal"
	}
	if e := contract.Emit(w, dOrdered(value, parent)); e != nil {
		return 3
	}
	return code
}
func dOrdered(value any, parent string) any {
	if m, ok := value.(map[string]any); ok {
		if parent == "" {
			if _, ok := m["faultClass"]; ok {
				parent = "policyChange"
			} else if _, ok := m["maxCount"]; ok {
				parent = "limitChange"
			} else if _, ok := m["notificationId"]; ok {
				parent = "notificationChange"
			}
		}
		keys := map[string][]string{"": {"product", "policies", "limits", "unsent", "unlinked", "notifications", "next", "warning", "relinked", "relinkPending", "backfilled", "backfillPending", "reserved", "withheld", "held", "waiting"}, "policies": {"product", "faultClass", "severity", "threshold", "window", "publish", "clears", "source", "overrideReason"}, "limits": {"product", "kind", "limit", "window", "used", "remaining", "source"}, "unsent": {"ready", "awaitingTarget", "held", "awaitingRecord", "scopeKeyContested", "backingOff", "kindUnregistered", "issueOwned", "unclassified", "claimed", "claimedLapsed", "issued", "issuedLapsed", "failed", "uncertain"}, "notifications": {"pending", "reserved", "uncertain", "reservedLapsed"}, "notificationRows": {"notificationId", "faultId", "product", "kind", "reason", "cycle", "ref", "state", "deliveryKey", "owner", "attempts", "lastError", "createdAt", "deliveredAt", "ackRef", "eligibility"}, "eligibility": {"relationshipId", "parentTaskId", "contact", "eligible", "reason"}, "contact": {"deliverable", "observedAt", "contactable", "ageSeconds", "asked", "reason"}, "reserved": {"notificationId", "faultId", "product", "kind", "reason", "cycle", "ref", "state", "deliveryKey", "owner", "attempts", "lastError", "createdAt", "deliveredAt", "ackRef", "token"}, "previous": {"threshold", "window_seconds", "reason"}, "refusal": {"error", "reason", "detail"}, "policyChange": {"product", "faultClass", "severity", "threshold", "window", "reason", "previous"}, "limitChange": {"product", "kind", "maxCount", "window"}, "notificationChange": {"notificationId", "faultId", "product", "kind", "reason", "cycle", "ref", "state", "deliveryKey", "owner", "attempts", "lastError", "createdAt", "deliveredAt", "ackRef"}}
		ordered := contract.OrderedObject{}
		seen := map[string]bool{}
		for _, key := range keys[parent] {
			if v, ok := m[key]; ok {
				ordered = append(ordered, contract.Field{Key: key, Value: dOrdered(v, key)})
				seen[key] = true
			}
		}
		rest := []string{}
		for key := range m {
			if !seen[key] {
				rest = append(rest, key)
			}
		}
		sort.Strings(rest)
		for _, key := range rest {
			ordered = append(ordered, contract.Field{Key: key, Value: dOrdered(m[key], key)})
		}
		return ordered
	}
	if array, ok := value.([]any); ok {
		out := make([]any, len(array))
		for i, v := range array {
			key := parent
			if parent == "notifications" {
				key = "notificationRows"
			}
			out[i] = dOrdered(v, key)
		}
		return out
	}
	return value
}
func dBound(ctx context.Context, raw, field string, fallback, max int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	name := "--" + strings.TrimPrefix(strings.ReplaceAll(field, "_", "-"), "--")
	n := integerArg(ctx, name, raw)
	if n == nil {
		return 0, fmt.Errorf("fault_observation_malformed: %s is a positive integer, not %s", field, raw)
	}
	if n.Sign() < 1 {
		return 0, fmt.Errorf("fault_observation_malformed: %s is a positive integer, not %s", field, n.String())
	}
	if n.Cmp(big.NewInt(int64(max))) > 0 {
		return max, nil
	}
	return int(n.Int64()), nil
}
func dProduct(p string) error {
	if !productName.MatchString(p) {
		return fmt.Errorf("fault_observation_malformed: product %q is not a plain identifier (letters, digits, '.', '_', '-'); a ':' '@' or '|' would let one product's key read as another's", p)
	}
	return nil
}
func dFloat(ctx context.Context, raw string) (float64, error) {
	var v float64
	var ok bool
	if numbers, present := ctx.Value(numberArgsKey{}).(map[string]any); present {
		v, ok = numbers["window"].(float64)
	} else {
		v, ok = argparse.ParseFloat(raw)
	}
	if !ok {
		return 0, fmt.Errorf("fault_observation_malformed: window is 60..2592000s")
	}
	if !(v >= 60 && v <= 2592000) {
		return 0, fmt.Errorf("fault_observation_malformed: window is 60..2592000s")
	}
	return v, nil
}
func executeD(ctx context.Context, l *Ledger, name string, a map[string]string) (any, error) {
	switch name {
	case "fault-policy":
		if a["--fault-class"] == "" {
			return dPolicies(ctx, l, a)
		}
		if a["--limit"] != "" || a["--after"] != "" {
			flags := []string{}
			for _, f := range []string{"--limit", "--after"} {
				if a[f] != "" {
					flags = append(flags, f)
				}
			}
			return nil, fmt.Errorf("fault_observation_malformed: %s page a listing and would be ignored beside --fault-class", strings.Join(flags, ", "))
		}
		if a["--severity"] == "" || a["--reason"] == "" {
			return nil, fmt.Errorf("usage: changing a policy names --fault-class, --severity and --reason")
		}
		return dSetPolicy(ctx, l, a)
	case "fault-limit":
		if a["--kind"] == "" {
			return dLimits(ctx, l, a)
		}
		if a["--limit"] != "" || a["--after"] != "" {
			flags := []string{}
			for _, f := range []string{"--limit", "--after"} {
				if a[f] != "" {
					flags = append(flags, f)
				}
			}
			return nil, fmt.Errorf("fault_observation_malformed: %s page a listing and would be ignored beside --kind", strings.Join(flags, ", "))
		}
		if a["--max-count"] == "" || a["--window"] == "" {
			return nil, fmt.Errorf("usage: setting a budget names --kind, --max-count and --window")
		}
		return dSetLimit(ctx, l, a)
	case "fault-attention":
		return dAttention(ctx, l)
	case "fault-relink":
		return dRelink(ctx, l, a)
	case "fault-notifications":
		return dNotifications(ctx, l, a)
	case "fault-notification-raise":
		return dRaise(ctx, l, a)
	case "fault-notification-reserve":
		return dReserve(ctx, l, a)
	case "fault-notification-ack", "fault-notification-fail", "fault-notification-reconcile":
		return dSettle(ctx, l, name, a)
	}
	return nil, fmt.Errorf("unknown fault command %q", name)
}
func dSetPolicy(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	p, c, s := a["--product"], a["--fault-class"], a["--severity"]
	if e := dProduct(p); e != nil {
		return nil, e
	}
	if _, ok := classes[c]; !ok || !dPythonClass(c) {
		return nil, fmt.Errorf("fault_class_unregistered: '%s' is not a registered fault class, so nothing declares what would clear it", c)
	}
	if s != Notice && s != Degraded && s != Broken {
		return nil, fmt.Errorf("fault_observation_malformed: severity '%s' is not one of ('notice', 'degraded', 'broken')", s)
	}
	if s != Degraded {
		return nil, fmt.Errorf("fault_policy_fixed: a %s fault's policy is fixed: a broken fault files at once and a notice never files", s)
	}
	var threshold, window any
	if a["--threshold"] != "" {
		v, e := dBound(ctx, a["--threshold"], "threshold", 0, 100)
		if e != nil {
			return nil, e
		}
		threshold = v
	}
	if a["--window"] != "" {
		v, e := dFloat(ctx, a["--window"])
		if e != nil {
			return nil, e
		}
		window = v
	}
	if strings.TrimSpace(a["--reason"]) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: a policy change says why")
	}
	var previous any
	e := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := l.one(ctx, "SELECT threshold,window_seconds,reason FROM fault_policies WHERE product=? AND fault_class=? AND severity=?", p, c, s)
		if e != nil {
			return e
		}
		if r != nil {
			previous = map[string]any{"threshold": r.Get("threshold"), "window_seconds": r.Get("window_seconds"), "reason": r.Get("reason")}
		}
		_, e = l.exec(ctx, "INSERT INTO fault_policies(product,fault_class,severity,threshold,window_seconds,reason,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(product,fault_class,severity) DO UPDATE SET threshold=excluded.threshold,window_seconds=excluded.window_seconds,reason=excluded.reason,updated_at=excluded.updated_at", p, c, s, threshold, window, a["--reason"], l.Clock.ISO())
		if e != nil {
			return e
		}
		prior := "null"
		if r != nil {
			prior = fmt.Sprintf(`{"threshold": %s, "window_seconds": %s, "reason": %s}`, dumps(r.Get("threshold"), false), dumps(r.Get("window_seconds"), false), dumps(r.Get("reason"), false))
		}
		detail := fmt.Sprintf(`{"threshold": %s, "window": %s, "reason": %s, "previous": %s}`, dumps(threshold, false), dumps(window, false), dumps(a["--reason"], false), prior)
		_, e = l.exec(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'fault_policy_set',?,?)", l.Clock.ISO(), p+":"+c+":"+s, detail)
		return e
	})
	return map[string]any{"product": p, "faultClass": c, "severity": s, "threshold": threshold, "window": window, "reason": a["--reason"], "previous": previous}, e
}
func dPythonClass(name string) bool {
	switch name {
	case "completion_mismatch", "completion_unverified", "product_defect", "product_expected", "project_needed", "unclassified_incident":
		return false
	}
	return true
}
func dPolicies(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	p := a["--product"]
	if e := dProduct(p); e != nil {
		return nil, e
	}
	limit, e := dBound(ctx, a["--limit"], "--limit", 20, 1000)
	if e != nil {
		return nil, e
	}
	after := a["--after"]
	if after != "" && strings.TrimSpace(after) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: after is the name the last page returned, not %q", after)
	}
	names := make([]string, 0, len(classes))
	for c := range classes {
		if c > after && dPythonClass(c) {
			names = append(names, c)
		}
	}
	sort.Strings(names)
	var next any
	if len(names) > limit {
		next = names[limit-1]
		names = names[:limit]
	}
	list := []any{}
	for _, c := range names {
		for _, s := range []string{Notice, Degraded, Broken} {
			var threshold any
			switch s {
			case Degraded:
				threshold = int64(3)
			case Broken:
				threshold = int64(1)
			}
			window := any(defaultWindow)
			source := "built-in"
			var reason any
			r, e := l.one(ctx, "SELECT threshold,window_seconds,reason FROM fault_policies WHERE product=? AND fault_class=? AND severity=?", p, c, s)
			if e != nil {
				return nil, e
			}
			if r != nil {
				if r.Get("threshold") != nil {
					threshold = r.Get("threshold")
				}
				if r.Get("window_seconds") != nil {
					window = r.Get("window_seconds")
				}
				source = "override"
				reason = r.Get("reason")
			}
			entry := map[string]any{"product": p, "faultClass": c, "severity": s, "threshold": threshold, "window": window, "publish": threshold != nil, "source": source, "clears": classes[c].clears}
			if source == "override" {
				entry["overrideReason"] = reason
			}
			list = append(list, entry)
		}
	}
	return map[string]any{"product": p, "policies": list, "next": next}, nil
}
func dSetLimit(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	p, k := a["--product"], a["--kind"]
	if e := dProduct(p); e != nil {
		return nil, e
	}
	if strings.TrimSpace(k) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: kind is a name")
	}
	n, e := dBound(ctx, a["--max-count"], "max_count", 0, 10000)
	if e != nil {
		return nil, e
	}
	v, e := dFloat(ctx, a["--window"])
	if e != nil {
		return nil, e
	}
	e = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, e := l.exec(ctx, "INSERT INTO fault_limits(product,kind,max_count,window_seconds,updated_at) VALUES(?,?,?,?,?) ON CONFLICT(product,kind) DO UPDATE SET max_count=excluded.max_count,window_seconds=excluded.window_seconds,updated_at=excluded.updated_at", p, k, n, v, l.Clock.ISO())
		if e != nil {
			return e
		}
		detail := dumps(map[string]any{"maxCount": n, "window": v}, false)
		_, e = l.exec(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'fault_limit_set',?,?)", l.Clock.ISO(), p+":"+k, detail)
		return e
	})
	return map[string]any{"product": p, "kind": k, "maxCount": n, "window": v}, e
}
func dLimits(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	p := a["--product"]
	if e := dProduct(p); e != nil {
		return nil, e
	}
	limit, e := dBound(ctx, a["--limit"], "--limit", 20, 1000)
	if e != nil {
		return nil, e
	}
	after := a["--after"]
	if after != "" && strings.TrimSpace(after) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: after is the name the last page returned, not %q", after)
	}
	names := map[string]bool{"open_record": true, "append_comment": true, "update_record": true, "notification": true}
	for k := range kinds {
		if k != "project_create" {
			names[k] = true
		}
	}
	rows, e := l.Store.All(ctx, "SELECT kind FROM fault_limits WHERE product=? AND kind>? ORDER BY kind LIMIT ?", p, after, limit+1)
	if e != nil {
		return nil, e
	}
	for _, r := range rows {
		names[text(r, "kind")] = true
	}
	keys := []string{}
	for k := range names {
		if k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var next any
	if len(keys) > limit {
		next = keys[limit-1]
		keys = keys[:limit]
	}
	entries := []any{}
	for _, k := range keys {
		n := int64(20)
		if k == "open_record" {
			n = 5
		}
		if k == "notification" {
			n = 10
		}
		window := 3600.0
		r, e := l.one(ctx, "SELECT max_count,window_seconds FROM fault_limits WHERE product=? AND kind=?", p, k)
		if e != nil {
			return nil, e
		}
		if r != nil {
			n = integer(r, "max_count")
			window = r.Get("window_seconds").(float64)
		}
		count, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product=? AND kind=? AND used_ts>?", p, k, l.Clock.Now()-window)
		if e != nil {
			return nil, e
		}
		used := integer(count, "n")
		remaining := n - used
		if remaining < 0 {
			remaining = 0
		}
		source := "default"
		if r != nil {
			source = "override"
		}
		entries = append(entries, map[string]any{"product": p, "kind": k, "limit": n, "window": window, "used": used, "remaining": remaining, "source": source})
	}
	return map[string]any{"product": p, "limits": entries, "next": next}, nil
}
