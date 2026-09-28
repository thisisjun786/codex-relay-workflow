package store

import (
	"context"
	"errors"
)

// ReadSnapshot exposes the existing domain queries on the read-only connection without
// opening, creating or migrating a Store. All readers share one deferred snapshot.
func (r *ReadOnly) ReadSnapshot(ctx context.Context, run func(context.Context, *Store) error) (err error) {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	defer func() { _, e := conn.ExecContext(ctx, "ROLLBACK"); err = errors.Join(err, e) }()
	s := &Store{DB: r.db}
	return run(context.WithValue(ctx, openTxKey{}, openTx{store: s, conn: conn}), s)
}
