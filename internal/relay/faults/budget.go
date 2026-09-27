package faults

import (
	"context"
	"database/sql"
	"fmt"
)

// ConsumeBudget enforces one charge per (product,kind,ref) inside a store
// transaction. Product-specific limits override the Python defaults.
func (l *Ledger) ConsumeBudget(ctx context.Context, product, kind, ref string) (map[string]any, error) {
	if !productName.MatchString(product) || ref == "" {
		return nil, fmt.Errorf("fault_observation_malformed: product and ref are required")
	}
	answer := map[string]any{}
	err := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		maximum, window := int64(20), 3600.0
		switch kind {
		case "open_record":
			maximum = 5
		case "notification":
			maximum = 10
		}
		custom, err := l.one(ctx, "SELECT max_count,window_seconds FROM fault_limits WHERE product = ? AND kind = ?", product, kind)
		if err != nil {
			return err
		}
		if custom != nil {
			maximum = integer(custom, "max_count")
			if value, ok := custom.Get("window_seconds").(float64); ok {
				window = value
			}
		}
		spent, err := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product = ? AND kind = ? AND used_ts > ?", product, kind, l.Clock.Now()-window)
		if err != nil {
			return err
		}
		used := integer(spent, "n")
		remaining := maximum - used
		if remaining < 0 {
			remaining = 0
		}
		existing, err := l.one(ctx, "SELECT 1 FROM fault_budget_uses WHERE product = ? AND kind = ? AND ref = ?", product, kind, ref)
		if err != nil {
			return err
		}
		if existing != nil {
			answer = map[string]any{"consumed": true, "remaining": remaining, "reason": "this ref was already consumed"}
			return nil
		}
		if remaining == 0 {
			answer = map[string]any{"consumed": false, "remaining": int64(0), "reason": "budget_spent"}
			return nil
		}
		if _, err = l.exec(ctx, "INSERT INTO fault_budget_uses (product,kind,ref,used_at,used_ts) VALUES (?,?,?,?,?)", product, kind, ref, l.Clock.ISO(), l.Clock.Now()); err != nil {
			return err
		}
		answer = map[string]any{"consumed": true, "remaining": remaining - 1, "reason": "consumed"}
		return nil
	})
	return answer, err
}
