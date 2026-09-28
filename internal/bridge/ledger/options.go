package ledger

import (
	"context"
	"encoding/json"
)

// Options adds caller-owned clock and ordered serialization seams. Zero options keep
// the bridge's existing clock and JSON encoding exactly unchanged. Encode must retain
// all receipt fields and return a JSON object; relay callers use their shared encoder.
type Options struct {
	Now    func() float64
	Encode func(Receipt) ([]byte, error)
}

func (l *Ledger) encode(receipt Receipt) ([]byte, error) {
	if l.options.Encode != nil {
		return l.options.Encode(receipt)
	}
	return json.Marshal(receipt)
}

// RawReceipt exposes the retained object bytes, including Python insertion order,
// without a map decode/re-encode at the relay's public JSON boundary.
func (l *Ledger) RawReceipt(ctx context.Context, id string) (json.RawMessage, error) {
	var raw string
	err := l.db.QueryRowContext(ctx, `SELECT receipt FROM operations WHERE request_id=?`, id).Scan(&raw)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
