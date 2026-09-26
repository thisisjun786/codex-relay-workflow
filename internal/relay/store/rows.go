package store

import (
	"context"
	"errors"
	"fmt"
)

// Column is one named value of a row, in the order the statement produced it.
type Column struct {
	Name  string
	Value any // nil, int64, float64, string or []byte, as SQLite stored it
}

// Row is Python's dict(sqlite3.Row): the columns in statement order.
type Row []Column

// Get is row[name]; absent columns read as nil.
func (r Row) Get(name string) any {
	for _, column := range r {
		if column.Name == name {
			return column.Value
		}
	}
	return nil
}

// All is Store.all: every row of a read, through the querier the context selects, so a read
// inside a transaction body sees that transaction's writes. The rows are closed before return.
func (s *Store) All(ctx context.Context, query string, args ...any) (_ []Row, err error) {
	rows, err := s.q(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	names, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("columns: %w", err)
	}
	var out []Row
	for rows.Next() {
		values := make([]any, len(names))
		pointers := make([]any, len(names))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		row := make(Row, len(names))
		for i, name := range names {
			row[i] = Column{Name: name, Value: values[i]}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// One is Store.one: the first row, or nil when there is none.
func (s *Store) One(ctx context.Context, query string, args ...any) (Row, error) {
	rows, err := s.All(ctx, query, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return rows[0], nil
}
