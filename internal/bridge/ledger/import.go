package ledger

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
)

// ImportLegacy copies the supplied alias ledger atomically without replacing conflicts. It runs on every start through
// the alias while the alias ledger exists, so a row canonical already holds is compared, not copied:
//   - an equal fingerprint and an equal receipt is the row already imported;
//   - an equal fingerprint whose alias receipt is not_attempted is no conflict either, and canonical's row is kept as
//     it is: an alias row that never dispatched cannot conflict with canonical's later state of the same request (a
//     Retire of the imported reservation, or a Begin retry of it) (CRW-1561);
//   - every other difference (another fingerprint, or an alias row past not_attempted whose receipt differs) is the
//     refused conflict, and nothing of the import is kept.
func (l *Ledger) ImportLegacy(ctx context.Context, path string) error {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	source, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fmt.Errorf("open legacy ledger: %w", err)
	}
	defer source.Close()
	rows, err := source.QueryContext(ctx, `SELECT request_id,fingerprint,receipt FROM operations`)
	if err != nil {
		return fmt.Errorf("read legacy ledger: %w", err)
	}
	defer rows.Close()
	type row struct{ id, fingerprint, receipt string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.fingerprint, &r.receipt); err != nil {
			return err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range all {
		var fp, raw string
		err := tx.QueryRowContext(ctx, `SELECT fingerprint,receipt FROM operations WHERE request_id=?`, r.id).Scan(&fp, &raw)
		if err == sql.ErrNoRows {
			if _, err := tx.ExecContext(ctx, `INSERT INTO operations VALUES (?,?,?)`, r.id, r.fingerprint, r.receipt); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var a, b any
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(r.receipt), &b); err != nil {
			return err
		}
		equal, err := json.Marshal(a)
		if err != nil {
			return err
		}
		other, err := json.Marshal(b)
		if err != nil {
			return err
		}
		if fp == r.fingerprint && string(equal) != string(other) {
			if alias, ok := b.(map[string]any); ok && alias["status"] == "not_attempted" {
				continue
			}
		}
		if fp != r.fingerprint || string(equal) != string(other) {
			//lint:ignore ST1005 a caller-visible message that is a sentence
			return fmt.Errorf("Conflicting retained request %q in legacy socket ledger %s; no requests will be dispatched. Preserve both ledgers.", r.id, path)
		}
	}
	return tx.Commit()
}
