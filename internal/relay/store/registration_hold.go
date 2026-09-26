package store

import (
	"context"
	"database/sql"
	"net/url"
	"time"
)

// RegistrationTimeout is intent.SQLITE_TIMEOUT: the lock wait a registration hold, or a read-only
// open of the relay store by the marker readers, may spend. Declared once, beside the opens.
const RegistrationTimeout = 2 * time.Second

// RegistrationHold is intent.registration_hold: the relay's write lock (BEGIN IMMEDIATE) held
// across a check and the marker publication that depends on it, and never committed. It goes
// through the store's write admission (refuseLiveState, and the ownership fence todo 30 adds to
// the same path) before any connection is made, opens mode=rw and never rwc, so an absent store
// stays absent, and ends in ROLLBACK so it records nothing.
//
// run receives the held connection; why is "" when the hold was taken, else the refusal text
// Python yields beside a None connection, byte for byte.
func RegistrationHold(ctx context.Context, dbPath string, run func(conn *sql.Conn, why string) error) error {
	absolute, err := expandUser(dbPath)
	if err != nil {
		return run(nil, "the relay store path "+pythonRepr(dbPath)+" could not be read as a path")
	}
	if absolute, err = refuseLiveState(absolute); err != nil {
		return run(nil, "the relay store could not be opened for writing: "+err.Error())
	}
	u := url.URL{Scheme: "file", Path: absolute}
	db, err := boundedDB(u.Path, "rw", RegistrationTimeout)
	if err != nil {
		return run(nil, "the relay store could not be opened for writing: "+PythonSQLiteMessage(err))
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return run(nil, "the relay store could not be opened for writing: "+PythonSQLiteMessage(err))
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return run(nil, "the relay store's write lock could not be taken: "+PythonSQLiteMessage(err))
	}
	runErr := run(conn, "")
	// ROLLBACK and never COMMIT: the hold writes nothing of its own. A failed ROLLBACK is
	// Python's `except sqlite3.Error: pass`: closing the connection ends the transaction anyway.
	_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	return runErr
}
