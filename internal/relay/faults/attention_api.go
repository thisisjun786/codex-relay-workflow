package faults

import "context"

// NotificationEligibility rechecks the reservation's user intent at transport start.
func (l *Ledger) NotificationEligibility(ctx context.Context, id string, now float64) (map[string]any, error) {
	return dEligibility(ctx, l, id, now)
}

// Attention is the same bounded fault-write reading exposed by fault-attention.
func (l *Ledger) Attention(ctx context.Context) (map[string]any, error) {
	value, err := dAttention(ctx, l)
	if err != nil {
		return nil, err
	}
	return value.(map[string]any), nil
}
