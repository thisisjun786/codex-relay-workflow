package store

import (
	"context"
	"database/sql"
	"errors"
)

// scanner is what a typed row reader needs: a *sql.Row or the current row of a *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// exec runs one statement where s.q(ctx) points, as Python's self.db.execute does: inside the
// transaction ctx carries when there is one, so it joins that commit; otherwise on its own,
// autocommitted (isolation_level=None). It never opens a transaction, so a domain transaction
// body can call it without ErrNestedTransaction. A write that reaches one of the six guard
// indexes fails exactly as it does in Python: the driver's UNIQUE constraint error (extended code
// 2067), never a refusal. Python's writers refuse, queue or replay a second live row before they
// insert it, under the same BEGIN IMMEDIATE, so the index is only reached by a raw write.
func (s *Store) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.q(ctx).ExecContext(ctx, query, args...)
}

// queryRows reads every row of query through the ctx-aware querier and closes the cursor before
// returning, so no *sql.Rows is ever held open while the caller makes another store call.
func queryRows[T any](ctx context.Context, s *Store, scan func(scanner) (T, error), query string, args ...any) (_ []T, err error) {
	rows, err := s.q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var result []T
	for rows.Next() {
		row, scanErr := scan(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// queryRow reads the one row query selects; sql.ErrNoRows when there is none.
func queryRow[T any](ctx context.Context, s *Store, scan func(scanner) (T, error), query string, args ...any) (T, error) {
	return scan(s.q(ctx).QueryRowContext(ctx, query, args...))
}
