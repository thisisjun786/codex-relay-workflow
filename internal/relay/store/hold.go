package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// holdStat is registration_hold's resolved.stat(), taken before the fence's Admission: the
// store as Path(db_path).expanduser().absolute() spells it, symlinks followed. Its error names
// that spelling, e.g. "stat <path>: no such file or directory" for a store or directory that
// does not exist.
// It precedes the live-state guard, which protects opens: a failed stat opens nothing.
func holdStat(path string) error {
	spelled, err := absoluteExpanded(path)
	if err != nil {
		return err
	}
	_, err = os.Stat(spelled)
	return err
}

// ReadOnly is a mode=ro connection that never creates a store (intent.read_only_connection).
type ReadOnly struct{ db *sql.DB }

// OpenStopRead is ownership.stop_metadata's read for the read-only Stop path (cutover.md Lock
// order): it creates no SQLite sidecar and copies nothing, under the rule InPlaceRead states. A
// store whose committed state cannot be read that way (ErrWALWithoutIndex) is an error, which
// the Stop owner read answers by asking the owner rather than trusting D.
func OpenStopRead(ctx context.Context, path string, timeout time.Duration) (*ReadOnly, error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	return OpenInPlace(ctx, resolved, timeout)
}

// OpenInPlace opens the store at path read-only under InPlaceRead's rule, taking no lock and
// running no schema script: the file InPlaceRead resolved, with the parameters it gave for that
// file's sidecars. SQLite then has to name that same file as the connection's main database
// (PRAGMA database_list), so a read never pairs one file's sidecars with another file's pages,
// as a link replaced between the examination and the open would.
func OpenInPlace(ctx context.Context, path string, timeout time.Duration) (*ReadOnly, error) {
	resolved, params, err := InPlaceRead(path)
	if err != nil {
		return nil, err
	}
	return openExamined(ctx, resolved, params, timeout)
}

// openExamined opens the file whose sidecars were examined and refuses a connection SQLite made
// to any other file.
func openExamined(ctx context.Context, examined string, params url.Values, timeout time.Duration) (*ReadOnly, error) {
	db, err := boundedURI(examined, params, timeout)
	if err != nil {
		return nil, err
	}
	var opened string
	if err := db.QueryRowContext(ctx, "SELECT file FROM pragma_database_list WHERE name = 'main'").Scan(&opened); err != nil {
		_ = db.Close()
		return nil, err
	}
	if opened != examined {
		_ = db.Close()
		return nil, fmt.Errorf("the store's sidecars were examined beside %s, but SQLite opened %s, so its committed state was not read", pyvalue.StrRepr(examined), pyvalue.StrRepr(opened))
	}
	return &ReadOnly{db: db}, nil
}

// walHeaderSize is a write-ahead log's header; a log no longer than it holds no frame.
const walHeaderSize = 32

// ErrWALWithoutIndex is a store whose write-ahead log holds frames while its shared-memory index
// is missing or unusable, which an unclean shutdown leaves. The frames may hold commits D does
// not, and SQLite reads them only by building the index, which is a sidecar.
var ErrWALWithoutIndex = errors.New("the store's write-ahead log holds frames and its shared-memory index is missing or unusable (an unclean shutdown), so its committed state cannot be read without creating a SQLite sidecar")

// InPlaceRead is the read-only, no-sidecar rule for reading a store's committed state (decision
// 36): the file SQLite opens for path and the URI parameters for it, or why none can be given.
// SQLite's unix VFS resolves a database path one component at a time, symbolic links included,
// and keeps -wal and -shm beside the file it resolved to, so path is resolved the same way
// first (resolvePath) and the sidecars examined are that file's; beside a link to D there are
// none, and reading D immutable there would skip every frame of the WAL beside D. The caller
// opens the returned path, never path itself (OpenInPlace). With D-wal and D-shm both present
// a WAL connection (live or crashed) left SQLite's coordination files, and mode=ro reads the
// committed frames while creating nothing. With no D-wal, or one that holds no frame (empty or
// only its header), every commit is in D (SQLite unlinks -shm before -wal at a checkpointed
// close, and a writer creates -wal before it can commit), so D is read immutable=1, which never
// creates -wal or -shm; a plain mode=ro would create both. A D-wal holding frames beside no
// usable D-shm (ErrWALWithoutIndex), or one that cannot be examined, has no such read: immutable
// would ignore frames that may hold commits, and mode=ro would create the index. A path that
// cannot be resolved has none either. Python's ownership.stop_metadata applies the same rule and
// raises in those states.
func InPlaceRead(path string) (string, url.Values, error) {
	resolved, err := resolvePath(path)
	if err != nil {
		return "", nil, fmt.Errorf("the store's path could not be resolved as SQLite resolves it: %w", err)
	}
	params := url.Values{"mode": {"ro"}}
	wal, walErr := os.Stat(resolved + "-wal")
	shm, shmErr := os.Stat(resolved + "-shm")
	switch {
	case walErr == nil && shmErr == nil && shm.Mode().IsRegular():
		return resolved, params, nil
	case errors.Is(walErr, os.ErrNotExist):
	case walErr != nil:
		return "", nil, fmt.Errorf("the store's write-ahead log could not be examined: %w", walErr)
	case wal.Mode().IsRegular() && wal.Size() <= walHeaderSize:
	default:
		return "", nil, ErrWALWithoutIndex
	}
	params.Set("immutable", "1")
	return resolved, params, nil
}

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

// readOnlyCommandKey marks a context that serves one of cli.py's READ_ONLY_COMMANDS forms.
type readOnlyCommandKey struct{}

// WithReadOnlyCommand is Services.read_only: every Open under ctx then serves a read-only
// command the way cli.py's Services.store does (openForRead).
func WithReadOnlyCommand(ctx context.Context) context.Context {
	return context.WithValue(ctx, readOnlyCommandKey{}, true)
}

// ReadOnlyCommand reports whether ctx serves a read-only command form.
func ReadOnlyCommand(ctx context.Context) bool {
	marked, _ := ctx.Value(readOnlyCommandKey{}).(bool)
	return marked
}

// openForRead is Services.store for a read-only command (cli.py:185-205). A store this
// runtime may write is opened by the admitted opener, so reads under its own active
// ownership keep the writer's semantics; any ownership refusal (another owner, draining,
// starting without a permit, a contended write gate, a disagreeing record) reads through
// Store(read_only=True) instead. It never creates, initializes or binds a store: an absent
// one is refused as Python's Services.store refuses it, reason store_absent (cutover.md Record,
// Read-only clients).
//
// The writer-form open a read command gets takes no SQLite write lock when the store is whole:
// its identity rows, the settlements-backfill marker and its indexes are all there, as in every
// store the relay creates, and seedMetadata then writes nothing, so the read neither waits for
// the daemon's write transaction nor holds up the daemon's next one. The one open that does
// write is the first of a store from before the marker (settlements still to backfill, once) or
// one with an index still to install; both are completed here as they always were.
func openForRead(ctx context.Context, path, socket string) (*Store, error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	if storeAbsent(resolved) {
		return nil, &RefusedError{Reason: ReasonStoreAbsent, Detail: "no relay store exists at " + pathlibSpelling(path) + "; a read-only command never creates one"}
	}
	if err = partialStore(resolved); err != nil {
		return nil, err
	}
	// Another runtime's store is read without its write gate (stampInPlace, which never takes a
	// lock); one this runtime may write is opened as a writer opens it.
	if meta, fenced, err := stampInPlace(ctx, resolved); err == nil && fenced && stampRefusal(meta) != nil {
		return OpenReadOnlyStore(ctx, path)
	}
	s, err := openFenced(ctx, path, socket, OpenOptions{BusyTimeout: 30 * time.Second})
	var denied *RefusedError
	if err != nil && errors.As(err, &denied) {
		return OpenReadOnlyStore(ctx, path)
	}
	return s, err
}

// storeAbsent is the one state a writer, never a reader, may initialize: no database, no
// ownership mirror and no write gate (decision 30).
func storeAbsent(resolved string) bool {
	dir := filepath.Dir(resolved)
	for _, name := range []string{resolved, filepath.Join(dir, "takeover.json"), filepath.Join(dir, "write-gate.lock")} {
		if _, err := os.Lstat(name); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// partialStore is the fence's refusal of a partial store, a write gate or an ownership mirror
// beside a D that is certainly absent (lstat ENOENT), which every form refuses and none reads,
// creates or repairs (decision 30). Its words are the fence writer's, in check_start's order:
// the mirror is read first (ownership.mirror), and a record beside no D is refused by validate
// (the empty stamp has no writer protocol); with no record, Admission refuses the gate it found
// without a database. It takes no lock, so a reader never takes the gate. nil when D is present
// or cannot be examined (the opener reports that); the caller has ruled out an absent store.
func partialStore(resolved string) error {
	if _, err := os.Lstat(resolved); !errors.Is(err, unix.ENOENT) {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(resolved), "takeover.json"))
	switch {
	case errors.Is(err, unix.ENOENT):
		return fenceRefused("partial store: write-gate.lock without a database")
	case err != nil:
		return fenceRefused("takeover record unreadable: " + err.Error())
	}
	if why := MirrorRefusal(raw); why != "" {
		return fenceRefused(why)
	}
	return fenceRefused("missing or unsupported writer protocol")
}

// creating is whether a first opener is creating the store at resolved right now: D and the
// mirror are certainly absent (lstat ENOENT; a mirror link naming no file reads as no mirror,
// as partialStore reads it) and another opener holds the gate EX (gateHeld), as a creator holds
// the gate it placed (placeGate, ownership.py _create_gate) until the store is whole. A
// writer-side preflight waits for such a creator (StartPreflight); a reader never probes the
// gate.
func creating(resolved string) bool {
	if _, err := os.Lstat(resolved); !errors.Is(err, unix.ENOENT) {
		return false
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(resolved), "takeover.json")); !errors.Is(err, unix.ENOENT) {
		return false
	}
	return gateHeld(resolved)
}

// gateHeld is whether another opener holds the gate beside resolved EX, probed once without
// waiting: a shared lock released at once, as ownership.py _creating probes it. A gate that
// cannot be opened or locked is held by nobody, and so is one that is not a regular file,
// which no creator places. The open does not wait either (O_NONBLOCK): a FIFO opened read-only
// would wait for a writer.
func gateHeld(resolved string) bool {
	fd, err := unix.Open(filepath.Join(filepath.Dir(resolved), "write-gate.lock"), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	var gate unix.Stat_t
	if err = unix.Fstat(fd, &gate); err != nil || gate.Mode&unix.S_IFMT != unix.S_IFREG {
		return false
	}
	return errors.Is(unix.Flock(fd, unix.LOCK_SH|unix.LOCK_NB), unix.EWOULDBLOCK)
}

// OpenReadOnlyStore is Store(path, read_only=True): mode=ro with query_only=ON on every
// connection and sqlite3.connect's 5 s timeout, deferred transactions, and no admission,
// write gate, DDL, guard index or initialization row. SQLite may still create its own
// WAL/SHM coordination files, which the client never removes (cutover.md).
func OpenReadOnlyStore(ctx context.Context, path string) (*Store, error) {
	resolved, err := refuseLiveState(path)
	if err != nil {
		return nil, err
	}
	db, err := boundedDB(resolved, "ro", 5*time.Second, "PRAGMA query_only=ON")
	if err != nil {
		return nil, err
	}
	if err = db.PingContext(ctx); err != nil {
		_ = db.Close()
		// The failed connect is the command's host error.
		return nil, &hostError{cause: err}
	}
	return &Store{DB: db, Path: pathlibSpelling(path), readOnly: true}, nil
}

// ReadOnly reports whether s is a read-only Store, which must never be written.
func (s *Store) ReadOnly() bool { return s.readOnly }

// Projection is FaultLedger.readonly_queue_state's private copy: the store's current content,
// backed up through SQLite into a temporary database this process alone may write, so
// housekeeping can be projected without mutating another runtime's store. The caller runs
// release, which closes the copy and removes it.
func (s *Store) Projection(ctx context.Context) (_ *Store, release func() error, err error) {
	// The backup takes the store's one connection; inside a transaction it would wait on
	// itself, so it is refused there like a nested transaction.
	if open, ok := ctx.Value(openTxKey{}).(openTx); ok && open.store == s {
		return nil, nil, ErrNestedTransaction
	}
	dir, err := os.MkdirTemp("", "crw-projection-")
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()
	copyPath := filepath.Join(dir, "relay.sqlite3")
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	err = conn.Raw(func(raw any) error {
		source, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver has no backup API")
		}
		backup, e := source.NewBackup(copyPath)
		if e != nil {
			return e
		}
		_, e = backup.Step(-1)
		return errors.Join(e, backup.Finish())
	})
	if err = errors.Join(err, conn.Close()); err != nil {
		return nil, nil, err
	}
	db, err := boundedDB(copyPath, "rw", 5*time.Second)
	if err != nil {
		return nil, nil, err
	}
	projected := &Store{DB: db, Path: s.Path}
	return projected, func() error { return errors.Join(db.Close(), os.RemoveAll(dir)) }, nil
}
