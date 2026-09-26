package store

import (
	"context"
	"database/sql"
	"errors"
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
