package ledger

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (l *Ledger) Save(ctx context.Context, receipt Receipt) (Receipt, error) {
	saved := make(Receipt, len(receipt)+1)
	for k, v := range receipt {
		saved[k] = v
	}
	saved["updatedAt"] = float64(time.Now().UnixNano()) / 1e9
	raw, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}
	_, err = l.db.ExecContext(ctx, `UPDATE operations SET receipt=? WHERE request_id=?`, raw, saved["requestId"])
	if err != nil {
		return nil, fmt.Errorf("save receipt: %w", err)
	}
	return saved, nil
}

func (l *Ledger) Get(ctx context.Context, id string) (Receipt, error) {
	var raw string
	err := l.db.QueryRowContext(ctx, `SELECT receipt FROM operations WHERE request_id=?`, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnknown
	}
	if err != nil {
		return nil, fmt.Errorf("get receipt: %w", err)
	}
	return decode(raw)
}

func decode(raw string) (Receipt, error) {
	var r Receipt
	d := json.NewDecoder(bytes.NewBufferString(raw))
	d.UseNumber()
	if err := d.Decode(&r); err != nil {
		return nil, fmt.Errorf("decode receipt: %w", err)
	}
	return r, nil
}
