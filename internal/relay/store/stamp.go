package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// The write fence is a frozen stamp (decision 56). A store is this runtime's to write when
// its schema_meta carries the six ownership keys naming owner go and its schema is the frozen v1
// schema; both are read on the connection the writer opens, before any statement of the open
// writes. The mirror (takeover.json), write-gate.lock and takeover.lock are kept as every store
// has them: a new store is created with all of them, the socket binding republishes the mirror,
// and no writer rewrites or deletes any of them (the Stop hook and the doctor read them, and an
// older runtime admits a store only with all of them). What a writer does no longer depends on
// the mirror: no copy of the database is read, and no transaction rereads the stamp.

// stampOn reads and judges the durable stamp on q, the connection a writer opened: a store
// another runtime owns, or one whose stamp is incomplete or unsupported, is refused as the fence
// refused it (reason store_owned_by_other).
func stampOn(ctx context.Context, q ownership.Queryer) (ownership.Stamp, error) {
	stamp, err := ownership.ReadStamp(ctx, q)
	if err == nil && stamp.Owner != "go" {
		err = &ownership.Refused{Detail: "store belongs to " + stamp.Owner}
	}
	if err != nil {
		return stamp, ownershipRefusal(err)
	}
	return stamp, nil
}

// ownershipRefusal answers an ownership refusal (or a failure to read the stamp) as the fence's
// admission answered it: reason store_owned_by_other, in the fence's words where it has them.
func ownershipRefusal(err error) error {
	var refused *RefusedError
	if errors.As(err, &refused) {
		return err
	}
	return &RefusedError{Reason: "store_owned_by_other", Detail: OwnershipRefusalDetail(err), cause: err}
}

// verifyWritable is a writable open's check on its first connection, before the schema script
// runs: the stamp names this runtime, the frozen schema is whole (a store missing a table fails,
// never repaired), and a socket the open names is the store's. A store not yet bound to
// an App Server socket is bound to the one given (bindSocket). It returns the write gate held
// SH, which the store keeps until it closes.
func verifyWritable(ctx context.Context, db *sql.DB, resolved, socket string) (*os.File, error) {
	stamp, err := stampOn(ctx, db)
	if err != nil {
		return nil, err
	}
	// A missing table or column is the command's host error, as the fence raised it.
	if err = ValidateOwnershipSchema(ctx, db); err != nil {
		return nil, err
	}
	canonical := ""
	if socket != "" {
		if canonical, err = canonicalSocket(socket); err != nil {
			return nil, err
		}
	}
	gate, err := holdGate(ctx, db, resolved, stamp, canonical)
	if err != nil {
		return nil, err
	}
	if canonical != "" {
		var recorded string
		err := db.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='socket_path'").Scan(&recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, errors.Join(err, gate.Close())
		}
		if recorded != canonical {
			// A domain refusal (exit 2), in the fence's words: a bound store is opened only for
			// the socket it records.
			refused := &ownership.Refused{Detail: "requested socket disagrees with ownership record"}
			return nil, errors.Join(&RefusedError{Reason: "store_owned_by_other", Detail: refused.Detail, cause: refused}, gate.Close())
		}
	}
	return gate, nil
}

// holdGate takes the write gate beside resolved SH, without waiting, for the store's lifetime:
// it keeps a writer out of the way of an opener that holds it EX to create the store or to bind
// it. A writable open that names an App Server socket binds the store first (bindSocket) when the
// store records no socket, or records this one under a mirror that names none (the torn binding a
// crash between the binding's commit and its publication leaves), and keeps the gate it held EX,
// downgraded to SH. A gate that is missing, held EX or not trusted refuses the open, as the
// fence's admission refused it.
func holdGate(ctx context.Context, db *sql.DB, resolved string, stamp ownership.Stamp, socket string) (*os.File, error) {
	path := filepath.Join(filepath.Dir(resolved), "write-gate.lock")
	if socket != "" && !ReadOnlyCommand(ctx) && unbound(resolved, stamp, socket) {
		gate, err := ownership.LockWithin(ctx, path, true, "write-gate EX for the socket binding")
		if err != nil {
			var expired *ownership.LockWaitExpired
			if errors.As(err, &expired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, ownershipRefusal(&ownership.Refused{Detail: fmt.Sprintf("write gate: %v", err)})
		}
		if err = bindSocket(ctx, db, resolved, socket); err != nil {
			return nil, errors.Join(err, gate.Close())
		}
		// Downgrade in place: the binder never lets the gate go between the binding and the
		// store's shared hold.
		if err = unix.Flock(int(gate.Fd()), unix.LOCK_SH|unix.LOCK_NB); err != nil {
			return nil, errors.Join(ownershipRefusal(&ownership.Refused{Detail: fmt.Sprintf("write gate: %v", err)}), gate.Close())
		}
		return gate, nil
	}
	gate, err := ownership.Lock(path, false, false)
	if err != nil {
		return nil, ownershipRefusal(&ownership.Refused{Detail: fmt.Sprintf("write gate: %v", err)})
	}
	return gate, nil
}

// unbound is whether a writable open naming socket binds the store: it records no socket, or
// records this one under a mirror that names none.
func unbound(resolved string, stamp ownership.Stamp, socket string) bool {
	switch stamp.SocketPath {
	case "":
		return true
	case socket:
		record, err := ownership.ReadRecord(resolved)
		return err == nil && record.AppServerSocket == nil
	}
	return false
}

// bindSocket is the socket binding (cutover.md Record), under the gate the caller holds EX:
// schema_meta.socket_path is recorded with INSERT OR IGNORE (the first recording wins) and must
// then be socket, and the mirror is republished with appServerSocket and scopeKey, every other
// field as the store's mirror has it. A crash between the two leaves socket_path recorded under a
// mirror that names none, which the next open naming the same socket completes.
func bindSocket(ctx context.Context, db *sql.DB, resolved, socket string) (err error) {
	record, err := ownership.ReadRecord(resolved)
	if err != nil {
		return ownershipRefusal(err)
	}
	key, err := ownership.ScopeKey(socket)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, e := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
			err = errors.Join(err, e)
		}
	}()
	if _, err = conn.ExecContext(ctx, "INSERT OR IGNORE INTO schema_meta VALUES ('socket_path', ?)", socket); err != nil {
		return fmt.Errorf("record socket_path: %w", err)
	}
	var recorded string
	if err = conn.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='socket_path'").Scan(&recorded); err != nil {
		return err
	}
	if recorded != socket {
		refused := &ownership.Refused{Detail: "requested socket disagrees with the recorded store socket"}
		return &RefusedError{Reason: "store_owned_by_other", Detail: refused.Detail, cause: refused}
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	if err = bindFault("committed"); err != nil {
		return err
	}
	record.AppServerSocket, record.ScopeKey = &socket, &key
	return ownership.Publish(record.Database.RealPath, record, nil)
}

// bindFault is a deterministic crash seam for tests; production leaves it inert.
var bindFault = func(string) error { return nil }

// stampInPlace is the durable stamp of the store at dbPath as a lock-free preflight reads it:
// in place, creating no SQLite sidecar and copying nothing (InPlaceRead). fenced is false for an
// absent store and for one with neither an ownership key nor a mirror, which the preflights let
// through (the writable open creates the one and refuses the other).
func stampInPlace(ctx context.Context, dbPath string) (meta map[string]string, fenced bool, err error) {
	meta, err = inPlaceMetadata(ctx, dbPath, ownership.LockWait)
	if err != nil {
		return nil, false, err
	}
	for _, key := range ownership.Keys {
		if _, stamped := meta[key]; stamped {
			fenced = true
		}
	}
	if !fenced {
		// A mirror link naming no file reads as no mirror, as partialStore reads it.
		if _, e := os.Stat(filepath.Join(filepath.Dir(resolveLoosely(dbPath)), "takeover.json")); e == nil {
			fenced = true
		}
	}
	return meta, fenced, nil
}

// stampRefusal is a preflight's judgement of a fenced store's stamp: nil for this runtime's
// store, otherwise the refusal the writable open would give it.
func stampRefusal(meta map[string]string) error {
	if owner := meta["owner"]; owner != "go" {
		if owner == "" {
			return fenceRefused("missing or unsupported writer protocol")
		}
		return fenceRefused("the relay store belongs to another runtime")
	}
	return nil
}

// inPlaceMetadata is schema_meta of the store at dbPath read in place (OpenStopRead's read, which
// creates no sidecar and copies nothing), waiting at most wait for a writer's lock.
func inPlaceMetadata(ctx context.Context, dbPath string, wait time.Duration) (map[string]string, error) {
	return metadataBy(ctx, dbPath, func(resolved string) (queryer, func() error, error) {
		ro, err := OpenStopRead(ctx, resolved, wait)
		if err != nil {
			return nil, nil, err
		}
		return ro, ro.Close, nil
	})
}
