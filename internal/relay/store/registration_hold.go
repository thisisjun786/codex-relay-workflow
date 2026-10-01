package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// RegistrationTimeout is intent.SQLITE_TIMEOUT: the lock wait a registration hold, or a read-only
// open of the relay store by the marker readers, may spend. Declared once, beside the opens.
const RegistrationTimeout = 2 * time.Second

// RegistrationHold is intent.registration_hold: the relay's write lock (BEGIN IMMEDIATE) held
// across a check and the marker publication that depends on it, and never committed. Like the
// fence release, it stats the store first, so a store or directory that does not exist is
// answered with the stat's error, then goes through the store's write admission
// (refuseLiveState, then the write gate held SH) before any connection is made, and the stamp
// is judged on the hold's own connection (decision 56). It opens mode=rw
// and never rwc, so an absent store stays absent, and ends in ROLLBACK so it records nothing.
//
// run receives the held connection; why is "" when the hold was taken, else why it was not,
// naming the failure in Go's words. A write admission refusal is returned
// instead, without calling run: registration_hold yields the fence's OwnershipRefused itself
// and register_relationship raises it, reason store_owned_by_other.
func RegistrationHold(ctx context.Context, dbPath string, run func(conn *sql.Conn, why string) error) error {
	absolute, err := expandUser(dbPath)
	if err != nil {
		return run(nil, "the relay store path "+pyvalue.StrRepr(dbPath)+" could not be read as a path")
	}
	if err = holdStat(dbPath); err != nil {
		return run(nil, "the relay store could not be opened for writing: "+err.Error())
	}
	if absolute, err = refuseLiveState(absolute); err != nil {
		return run(nil, "the relay store could not be opened for writing: "+err.Error())
	}
	gate, err := ownership.Lock(filepath.Join(filepath.Dir(absolute), "write-gate.lock"), false, false)
	if err != nil {
		// The fence does not yield its Admission's OwnershipRefused as an unheld hold:
		// register_relationship raises it, so the caller answers store_owned_by_other.
		return ownershipRefusal(&ownership.Refused{Detail: fmt.Sprintf("write gate: %v", err)})
	}
	defer gate.Close()
	u := url.URL{Scheme: "file", Path: absolute}
	db, err := boundedDB(u.Path, "rw", RegistrationTimeout)
	if err != nil {
		return run(nil, "the relay store could not be opened for writing: "+err.Error())
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return run(nil, "the relay store could not be opened for writing: "+err.Error())
	}
	defer conn.Close()
	if _, err = stampOn(ctx, conn); err != nil {
		// The stamp's refusal is raised as the fence's admission raised it.
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return run(nil, "the relay store's write lock could not be taken: "+err.Error())
	}
	runErr := run(conn, "")
	// ROLLBACK and never COMMIT: the hold writes nothing of its own. A failed ROLLBACK is
	// Python's `except sqlite3.Error: pass`: closing the connection ends the transaction anyway.
	_, _ = conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
	return runErr
}
