package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

type NonceReading struct {
	Nonce     string
	Found     bool
	Readable  bool
	WrittenBy string
	WrittenAt string
	Device    uint64
	Inode     uint64
	Links     uint64
	LogDevice uint64
	LogInode  uint64
	LogName   string
	Detail    string
}

type ReadResult struct {
	Readable bool
	Rows     []Challenge
	Device   uint64
	Inode    uint64
	Links    uint64
	Detail   string
}

// ReadChallengeRows is read_only_rows over store_challenge.
func ReadChallengeRows(ctx context.Context, selection StateSelection) ReadResult {
	var rows []Challenge
	read := ReadOnlyRows(ctx, selection, "SELECT nonce,written_by,written_at FROM store_challenge ORDER BY nonce", nil, func(r RowScanner) error {
		var row Challenge
		if err := r.Scan(&row.Nonce, &row.WrittenBy, &row.WrittenAt); err != nil {
			return err
		}
		rows = append(rows, row)
		return nil
	})
	if !read.Readable || read.Detail != "" {
		rows = nil
	}
	return ReadResult{Readable: read.Readable, Rows: rows, Device: read.Device, Inode: read.Inode, Links: read.Links, Detail: read.Detail}
}

// RowScanner is the one row ReadOnlyRows hands its callback.
type RowScanner interface{ Scan(dest ...any) error }

// RowsRead is what read_only_rows reports beside its rows: the identity of the HELD file, or
// the reason nothing was read. Readable with a Detail is a readable database whose query failed.
type RowsRead struct {
	Readable bool
	Device   uint64
	Inode    uint64
	Links    uint64
	Detail   string
	// Raised is what read_only_rows does not answer as a reading: the UnicodeEncodeError a
	// parameter sqlite3 cannot bind raises (it catches sqlite3.Error only), for the caller to raise.
	Raised error
}

// ReadOnlyRows is read_only_rows: an answer without creating or migrating a store, whose
// identity is the identity of the file this read HELD OPEN, refused unless that file is still
// the one at this store's pathname. Never an error: failures are fields. each is called per row
// while the statement is open, so it must not call another store method.
func ReadOnlyRows(ctx context.Context, selection StateSelection, query string, args []any, each func(RowScanner) error) RowsRead {
	file, expected, refused := holdDatabase(ctx, selection.DBPath())
	if file == nil {
		return RowsRead{Detail: refused}
	}
	defer file.Close()
	opened, ok := measureHeld(ctx, file)
	if !ok {
		return RowsRead{Detail: "the database could not be identified while it was being read"}
	}
	if moved := relocation(file, expected); moved != "" {
		return RowsRead{Detail: moved}
	}
	conn, err := openHeld(ctx, file, "ro")
	if err != nil {
		return RowsRead{Detail: PythonSQLiteError(err)}
	}
	readErr := func() error {
		defer conn.close()
		if elsewhere := conn.elsewhere(ctx, file, expected); elsewhere != "" {
			return refusedRead(elsewhere)
		}
		return conn.query(ctx, query, args, func(r *sql.Rows) error { return each(r) })
	}()
	var refusal refusedRead
	if errors.As(readErr, &refusal) {
		return RowsRead{Detail: string(refusal)}
	}
	if encode := EncodeError(readErr); encode != nil {
		return RowsRead{Raised: encode}
	}
	if readErr != nil {
		// Ask why before reporting what: a store still moved out from under the read fails the
		// statement too, and that is a refusal rather than a readable database's failed query.
		if moved := relocation(file, expected); moved != "" {
			return RowsRead{Detail: moved}
		}
		return RowsRead{Readable: true, Detail: PythonSQLiteError(readErr)}
	}
	// The closing question: without it a rename during the read that is still in place returns
	// rows and an identity a caller reads as "the store at this path".
	if moved := relocation(file, expected); moved != "" {
		return RowsRead{Detail: moved}
	}
	links := opened.links
	if closed, ok := measureHeld(ctx, file); ok {
		links = max(links, closed.links)
	}
	return RowsRead{Readable: true, Device: opened.device, Inode: opened.inode, Links: links}
}

type refusedRead string

func (r refusedRead) Error() string { return string(r) }

// NonceLookup is nonce_lookup: the only evidence CompareStore grades as proof, so it carries
// the identity and log location of the file it was read from, and an answer that cannot be
// attributed is unreadable rather than absent.
func NonceLookup(ctx context.Context, selection StateSelection, nonce string) NonceReading {
	unreadable := func(detail string) NonceReading { return NonceReading{Nonce: nonce, Detail: detail} }
	file, expected, refused := holdDatabase(ctx, selection.DBPath())
	if file == nil {
		return unreadable(refused)
	}
	defer file.Close()
	opened, ok := measureHeld(ctx, file)
	if !ok {
		return unreadable("the database could not be identified while the nonce was being read")
	}
	if moved := relocation(file, expected); moved != "" {
		return unreadable(moved)
	}
	conn, err := openHeld(ctx, file, "ro")
	if err != nil {
		return unreadable(PythonSQLiteError(err))
	}
	var actor, at string
	readErr := func() error {
		defer conn.close()
		if elsewhere := conn.elsewhere(ctx, file, expected); elsewhere != "" {
			return refusedRead(elsewhere)
		}
		return conn.scanRow(ctx, "SELECT written_by,written_at FROM store_challenge WHERE nonce=?", []any{nonce}, &actor, &at)
	}()
	found := readErr == nil
	var refusal refusedRead
	switch {
	case errors.As(readErr, &refusal):
		return unreadable(string(refusal))
	case readErr != nil && !errors.Is(readErr, sql.ErrNoRows):
		// NOT readable: a database that could not answer must not become "it is not there".
		if moved := relocation(file, expected); moved != "" {
			return unreadable(moved)
		}
		return unreadable(PythonSQLiteError(readErr))
	}
	if moved := relocation(file, expected); moved != "" {
		return unreadable(moved)
	}
	result := NonceReading{Nonce: nonce, Readable: true, Found: found, Device: opened.device, Inode: opened.inode, Links: opened.links}
	if closed, ok := measureHeld(ctx, file); ok {
		result.Links = max(result.Links, closed.links)
	}
	result.LogDevice, result.LogInode, result.LogName, _ = heldLogLocation(file, expected)
	if found {
		result.WrittenBy, result.WrittenAt = actor, at
	}
	return result
}

// OwnershipMetadata is ownership.metadata: schema_meta read from a disposable main-plus-WAL
// copy of the database, so no sidecar is created beside the source. An absent database, or
// one without schema_meta, reads as empty. The error text is Python's str(error): the bare OS
// or SQLite message.
func OwnershipMetadata(ctx context.Context, dbPath string) (map[string]string, error) {
	meta, err := readMetadata(ctx, dbPath)
	if err != nil {
		var failure *sqlite.Error
		if errors.As(err, &failure) {
			return nil, errors.New(PythonSQLiteMessage(err))
		}
		return nil, errors.New(PythonOSErrorText(err))
	}
	return meta, nil
}

// stopMetadata is ownership.stop_metadata: schema_meta read in place for the read-only Stop path
// (OpenStopRead), with no copy and no SQLite sidecar; an absent database or one without
// schema_meta has none.
func stopMetadata(ctx context.Context, dbPath string) (map[string]string, error) {
	return metadataBy(ctx, dbPath, func(resolved string) (queryer, func() error, error) {
		ro, err := OpenStopRead(ctx, resolved, 0)
		if err != nil {
			return nil, nil, err
		}
		return ro, ro.Close, nil
	})
}

// readMetadata is OwnershipMetadata with the underlying OS or SQLite error.
func readMetadata(ctx context.Context, dbPath string) (map[string]string, error) {
	return metadataBy(ctx, dbPath, func(resolved string) (queryer, func() error, error) {
		copied, cleanup, err := ownership.CopySnapshot(resolved)
		if err != nil {
			return nil, nil, err
		}
		db, err := boundedDB(copied, "ro", 0)
		if err != nil {
			return nil, nil, errors.Join(err, cleanup())
		}
		return db, func() error { return errors.Join(db.Close(), cleanup()) }, nil
	})
}

// queryer is the read a metadata reader makes: a copy's *sql.DB or an in-place *ReadOnly.
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// metadataBy is schema_meta of the store at dbPath, read through the connection open makes for
// the resolved database: none for an absent database (Path.exists()) or one without schema_meta.
func metadataBy(ctx context.Context, dbPath string, open func(string) (queryer, func() error, error)) (map[string]string, error) {
	meta := map[string]string{}
	resolved, err := resolvePath(dbPath)
	if err != nil {
		return nil, err
	}
	// Path.exists(): only these errnos mean absent; anything else is raised.
	if _, err = os.Stat(resolved); err != nil {
		for _, absent := range []syscall.Errno{syscall.ENOENT, syscall.ENOTDIR, syscall.EBADF, syscall.ELOOP} {
			if errors.Is(err, absent) {
				return meta, nil
			}
		}
		return nil, err
	}
	db, closeDB, err := open(resolved)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeDB() }()
	var present int
	switch err = db.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE name='schema_meta'").Scan(&present); {
	case errors.Is(err, sql.ErrNoRows):
		return meta, nil
	case err != nil:
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT key,value FROM schema_meta")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		meta[key] = value
	}
	return meta, rows.Err()
}

// OwnershipMirror is the read half of ownership.mirror: takeover.json's bytes beside the
// resolved database, nil when it is absent. A read failure is the fence's refusal text.
func OwnershipMirror(dbPath string) ([]byte, error) {
	resolved, err := resolvePath(dbPath)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(resolved), "takeover.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("store_owned_by_other: takeover record unreadable: " + PythonOSError(err))
	}
	return raw, nil
}
