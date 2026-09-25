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

// ReadChallengeRows is read_only_rows over store_challenge: an answer without creating or
// migrating a store, whose identity is the identity of the file this read HELD OPEN, refused
// unless that file is still the one at this store's pathname. Never an error: failures are fields.
func ReadChallengeRows(ctx context.Context, selection StateSelection) ReadResult {
	file, expected, refused := holdDatabase(ctx, selection.DBPath())
	if file == nil {
		return ReadResult{Detail: refused}
	}
	defer file.Close()
	opened, ok := measureHeld(ctx, file)
	if !ok {
		return ReadResult{Detail: "the database could not be identified while it was being read"}
	}
	if moved := relocation(file, expected); moved != "" {
		return ReadResult{Detail: moved}
	}
	conn, err := openHeld(ctx, file, "ro")
	if err != nil {
		return ReadResult{Detail: err.Error()}
	}
	var rows []Challenge
	readErr := func() error {
		defer conn.close()
		if elsewhere := conn.elsewhere(ctx, file, expected); elsewhere != "" {
			return refusedRead(elsewhere)
		}
		return conn.query(ctx, "SELECT nonce,written_by,written_at FROM store_challenge ORDER BY nonce", nil, func(r *sql.Rows) error {
			var row Challenge
			if err := r.Scan(&row.Nonce, &row.WrittenBy, &row.WrittenAt); err != nil {
				return err
			}
			rows = append(rows, row)
			return nil
		})
	}()
	var refusal refusedRead
	if errors.As(readErr, &refusal) {
		return ReadResult{Detail: string(refusal)}
	}
	if readErr != nil {
		// Ask why before reporting what: a store still moved out from under the read fails the
		// statement too, and that is a refusal rather than a readable database's failed query.
		if moved := relocation(file, expected); moved != "" {
			return ReadResult{Detail: moved}
		}
		return ReadResult{Readable: true, Detail: readErr.Error()}
	}
	// The closing question: without it a rename during the read that is still in place returns
	// rows and an identity a caller reads as "the store at this path".
	if moved := relocation(file, expected); moved != "" {
		return ReadResult{Detail: moved}
	}
	links := opened.links
	if closed, ok := measureHeld(ctx, file); ok {
		links = max(links, closed.links)
	}
	return ReadResult{Readable: true, Rows: rows, Device: opened.device, Inode: opened.inode, Links: links}
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
		return unreadable(err.Error())
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
		return unreadable(readErr.Error())
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
