package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"modernc.org/sqlite"
)

// WriteHold is intent.registration_hold's connection: the store's write lock (BEGIN IMMEDIATE)
// held across a check and the publication that depends on it. It opens with mode=rw, never rwc,
// so an absent store stays absent. Release rolls back: the hold writes nothing itself.
type WriteHold struct {
	db   *sql.DB
	Conn *sql.Conn
}

// HoldForWrite takes the hold within timeout, or returns why it could not, in
// registration_hold's words.
func HoldForWrite(ctx context.Context, path string, timeout time.Duration) (*WriteHold, string) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, "the relay store path " + quoteRepr(path) + " could not be read as a path"
	}
	db, err := boundedDB(resolved, "rw", timeout)
	if err != nil {
		return nil, "the relay store could not be opened for writing: " + err.Error()
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, "the relay store could not be opened for writing: " + err.Error()
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, "the relay store's write lock could not be taken: " + sqliteMessage(err)
	}
	return &WriteHold{db: db, Conn: conn}, ""
}

// Release rolls back and closes the hold.
func (h *WriteHold) Release() error {
	_, rollback := h.Conn.ExecContext(context.Background(), "ROLLBACK")
	return firstErr(rollback, h.Conn.Close(), h.db.Close())
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return fmt.Errorf("release write hold: %w", err)
		}
	}
	return nil
}

// sqliteMessage is str(sqlite3.Error): the engine's message without the driver's code suffix,
// e.g. "database is locked" for SQLITE_BUSY.
func sqliteMessage(err error) string {
	var coded *sqlite.Error
	if errors.As(err, &coded) {
		// sqlite3_errstr of the primary code, which is what Python's sqlite3 carries.
		switch coded.Code() & 0xff {
		case 5:
			return "database is locked"
		case 6:
			return "database table is locked"
		case 8:
			return "attempt to write a readonly database"
		case 14:
			return "unable to open database file"
		}
	}
	return err.Error()
}

func quoteRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// ReadOnly is a mode=ro connection that never creates a store (intent.read_only_connection).
type ReadOnly struct{ db *sql.DB }

// OpenReadOnly opens path read-only with a bounded busy timeout.
func OpenReadOnly(ctx context.Context, path string, timeout time.Duration) (*ReadOnly, error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	db, err := boundedDB(resolved, "ro", timeout)
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &ReadOnly{db: db}, nil
}

func (r *ReadOnly) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return r.db.QueryContext(ctx, query, args...)
}

func (r *ReadOnly) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return r.db.QueryRowContext(ctx, query, args...)
}

func (r *ReadOnly) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return r.db.ExecContext(ctx, query, args...)
}

// Close closes the connection.
func (r *ReadOnly) Close() error { return r.db.Close() }
