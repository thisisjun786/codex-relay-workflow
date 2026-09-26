package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

const procFD = "/proc/self/fd"

// diagnosticSeams let a test decide WHEN something happens inside a read leg, never WHAT: the
// real open, connect, statements and identity measurements all still run. They mirror the
// points Python's DescriptorIdentity tests patch (os.open, sqlite3.connect, _held_identity,
// _hold_database). Absent from a context, every seam is a no-op.
type diagnosticSeams struct {
	afterOpen      func(path string)
	connect        func(open func() (*heldConn, error)) (*heldConn, error)
	statement      func(query string)
	afterStatement func()
	closed         func()
	identity       func(fd int) (heldIdentity, bool)
	hold           func(path string) (*os.File, string, string)
}

type diagnosticSeamsKey struct{}

func seamsOf(ctx context.Context) diagnosticSeams {
	seams, _ := ctx.Value(diagnosticSeamsKey{}).(diagnosticSeams)
	return seams
}

type heldIdentity struct{ device, inode, links uint64 }

// holdDatabase is _hold_database: the database held open, so an answer can name the file it
// came from, and refused unless the descriptor still names this store. Returns the file, the
// resolved path it must keep naming, or a refusal detail. Never an error: every failure is a field.
func holdDatabase(ctx context.Context, path string) (*os.File, string, string) {
	seams := seamsOf(ctx)
	if seams.hold != nil {
		return seams.hold(path)
	}
	expected, err := resolvePath(path)
	if err != nil {
		return nil, "", "the database path could not be resolved, so a read cannot be bound to it"
	}
	if info, err := os.Stat(procFD); err != nil || !info.IsDir() {
		return nil, "", procFD + " is unavailable, so a read cannot be bound to the database it came from"
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", PythonOSError(err)
	}
	if seams.afterOpen != nil {
		seams.afterOpen(path)
	}
	if moved := relocation(file, expected); moved != "" {
		_ = file.Close()
		return nil, "", moved
	}
	return file, expected, ""
}

// relocation is _relocation: why the held file is no longer this store, or "" while it is.
func relocation(file *os.File, expected string) string {
	actual, err := os.Readlink(procFD + "/" + strconv.FormatUint(uint64(file.Fd()), 10))
	if err != nil {
		return "the database this process holds open cannot be named: " + err.Error()
	}
	if actual != expected {
		return "the database this process holds open is no longer the file at " + expected + ", so nothing was read from it"
	}
	return ""
}

// measureHeld is _held_identity: device, inode and name count of the file we HOLD.
func measureHeld(ctx context.Context, file *os.File) (heldIdentity, bool) {
	fd := int(file.Fd())
	if seams := seamsOf(ctx); seams.identity != nil {
		return seams.identity(fd)
	}
	return fstatIdentity(fd)
}

func fstatIdentity(fd int) (heldIdentity, bool) {
	var info syscall.Stat_t
	if err := syscall.Fstat(fd, &info); err != nil {
		return heldIdentity{}, false
	}
	return heldIdentity{uint64(info.Dev), uint64(info.Ino), uint64(info.Nlink)}, true
}

// heldLogLocation is _held_log_location: the directory entry this file's log is written
// under, bound to the held inode through that entry.
func heldLogLocation(file *os.File, expected string) (device, inode uint64, name string, ok bool) {
	held, measured := fstatIdentity(int(file.Fd()))
	if !measured {
		return 0, 0, "", false
	}
	directory, err := os.Stat(filepath.Dir(expected))
	if err != nil {
		return 0, 0, "", false
	}
	entry, err := os.Lstat(expected)
	if err != nil {
		return 0, 0, "", false
	}
	where, whereOK := directory.Sys().(*syscall.Stat_t)
	at, atOK := entry.Sys().(*syscall.Stat_t)
	if !whereOK || !atOK || uint64(at.Dev) != held.device || uint64(at.Ino) != held.inode {
		return 0, 0, "", false
	}
	return uint64(where.Dev), uint64(where.Ino), filepath.Base(expected), true
}

// heldConn is a connection opened through the held descriptor. Every statement a leg runs goes
// through it, so a test can see exactly which statements reached a connection.
type heldConn struct {
	db    *sql.DB
	conn  *sql.Conn
	seams diagnosticSeams
}

func openHeld(ctx context.Context, file *os.File, mode string) (*heldConn, error) {
	seams := seamsOf(ctx)
	open := func() (*heldConn, error) {
		db, err := boundedDB(procFD+"/"+strconv.FormatUint(uint64(file.Fd()), 10), mode, 5*time.Second)
		if err != nil {
			return nil, err
		}
		conn, err := db.Conn(ctx)
		if err != nil {
			return nil, errors.Join(err, db.Close())
		}
		return &heldConn{db: db, conn: conn, seams: seams}, nil
	}
	if seams.connect != nil {
		return seams.connect(open)
	}
	return open()
}

func (h *heldConn) close() {
	_ = h.conn.Close()
	_ = h.db.Close()
	if h.seams.closed != nil {
		h.seams.closed()
	}
}

func (h *heldConn) around(query string) func() {
	if h.seams.statement != nil {
		h.seams.statement(query)
	}
	return func() {
		if h.seams.afterStatement != nil {
			h.seams.afterStatement()
		}
	}
}

func (h *heldConn) scanRow(ctx context.Context, query string, args []any, dest ...any) error {
	defer h.around(query)()
	return h.conn.QueryRowContext(ctx, query, args...).Scan(dest...)
}

func (h *heldConn) exec(ctx context.Context, query string) error {
	defer h.around(query)()
	_, err := h.conn.ExecContext(ctx, query)
	return err
}

func (h *heldConn) query(ctx context.Context, query string, args []any, each func(*sql.Rows) error) (err error) {
	defer h.around(query)()
	rows, err := h.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// elsewhere is `_relocation(fd, expected) or _opened_elsewhere(connection, expected)`: the
// readlink runs first because it creates nothing on any build, then SQLite is asked which file
// it actually opened.
func (h *heldConn) elsewhere(ctx context.Context, file *os.File, expected string) string {
	if moved := relocation(file, expected); moved != "" {
		return moved
	}
	var opened string
	if err := h.scanRow(ctx, "PRAGMA database_list", nil, new(int), new(string), &opened); err != nil {
		return "the database this connection opened could not be named: " + err.Error()
	}
	if opened != expected {
		return fmt.Sprintf("this connection opened %q rather than the database at %s, so nothing was read from it", opened, expected)
	}
	return ""
}

func (s *Store) WriteChallengeFor(ctx context.Context, actor string) (Challenge, error) {
	bytes, err := randomBytes(16)
	if err != nil {
		return Challenge{}, fmt.Errorf("challenge nonce: %w", err)
	}
	challenge := Challenge{Nonce: fmt.Sprintf("%x", bytes), WrittenBy: actor, WrittenAt: time.Now().UTC().Format("2006-01-02T15:04:05Z")}
	if err := s.WriteChallenge(ctx, challenge); err != nil {
		return Challenge{}, err
	}
	return challenge, nil
}

func (s *Store) ReadChallenge(ctx context.Context, nonce string) (NonceReading, error) {
	value, err := s.Challenge(ctx, nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return NonceReading{Nonce: nonce, Readable: true}, nil
	}
	if err != nil {
		return NonceReading{}, err
	}
	return NonceReading{Nonce: nonce, Readable: true, Found: true, WrittenBy: value.WrittenBy, WrittenAt: value.WrittenAt}, nil
}
