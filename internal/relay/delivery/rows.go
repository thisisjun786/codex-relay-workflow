package delivery

import (
	"context"
	"database/sql"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Row is one SQLite row by column name, as Python's sqlite3.Row is read.
type Row map[string]any

func (r Row) S(column string) string {
	switch v := r[column].(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

// N reports whether column is NULL.
func (r Row) N(column string) bool { return r[column] == nil }

func (r Row) I(column string) int64 {
	switch v := r[column].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return 0
}

func (r Row) F(column string) float64 {
	switch v := r[column].(type) {
	case int64:
		return float64(v)
	case float64:
		return v
	}
	return 0
}

// Opt is the column as a JSON value: nil for NULL.
func (r Row) Opt(column string) any {
	if b, ok := r[column].([]byte); ok {
		return string(b)
	}
	return r[column]
}

// all reads every row through the ctx-aware querier and closes the cursor before returning.
func all(ctx context.Context, s *store.Store, query string, args ...any) (_ []Row, err error) {
	rows, err := s.Q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []Row
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := Row{}
		for i, c := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			row[c] = values[i]
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// one reads the first row, or nil.
func one(ctx context.Context, s *store.Store, query string, args ...any) (Row, error) {
	rows, err := all(ctx, s, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}

func execSQL(ctx context.Context, s *store.Store, query string, args ...any) (int64, error) {
	result, err := s.Q(ctx).ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func nullable(v any) any {
	if s, ok := v.(string); ok && s == "" {
		return nil
	}
	return v
}

// journal is store.journal inside the caller's transaction.
func journal(ctx context.Context, s *store.Store, kind, subject string, detail any, at string) error {
	text, ok := detail.(string)
	if !ok {
		text = dumps(detail)
	}
	_, err := s.Q(ctx).ExecContext(ctx, `INSERT INTO journal (at, kind, subject, detail) VALUES (?,?,?,?)`, at, kind, subject, text)
	return err
}

type sqlConn = sql.Conn
