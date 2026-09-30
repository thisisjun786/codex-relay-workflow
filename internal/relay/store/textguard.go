package store

import (
	"database/sql/driver"
	"errors"
	"fmt"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

// UnicodeEncodeError is the UnicodeEncodeError Python raises encoding a str that holds a lone
// surrogate as UTF-8: str.encode("utf-8"), the identity hashes' input, or sqlite3 binding the
// str as TEXT. A Go string holds such a str either as an argv or environment byte that is not
// UTF-8 (Python's surrogate escape of it) or as a "\udXXX" JSON escape kept as WTF-8. Error is
// str() of it; cli.main's host envelope prints HostDetail.
type UnicodeEncodeError struct{ Detail string }

func (e *UnicodeEncodeError) Error() string { return e.Detail }

// HostDetail is cli.main's f"{type(error).__name__}: {error}" for it.
func (e *UnicodeEncodeError) HostDetail() string { return "UnicodeEncodeError: " + e.Detail }

// EncodeUTF8 is str.encode("utf-8")'s refusal of text, or nil when every code point encodes. The
// code points are settings.CodePoint's (a WTF-8 surrogate is one, a byte that is not UTF-8 is its
// surrogate escape), and the refusal names the first run of surrogates as CPython's strict UTF-8
// encoder does: one character by itself, a longer run by its first and last position.
func EncodeUTF8(text string) error {
	position, start := 0, -1
	var first rune
	for i := 0; i < len(text); position++ {
		r, size := settings.CodePoint(text, i)
		i += size
		surrogate := r >= 0xd800 && r <= 0xdfff
		if start >= 0 && !surrogate {
			break
		}
		if start < 0 && surrogate {
			start, first = position, r
		}
	}
	switch {
	case start < 0:
		return nil
	case position-start == 1:
		return &UnicodeEncodeError{fmt.Sprintf("'utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed", first, start)}
	}
	return &UnicodeEncodeError{fmt.Sprintf("'utf-8' codec can't encode characters in position %d-%d: surrogates not allowed", start, position-1)}
}

// EncodeError is the UnicodeEncodeError err carries, or nil.
func EncodeError(err error) *UnicodeEncodeError {
	var refused *UnicodeEncodeError
	if errors.As(err, &refused) {
		return refused
	}
	return nil
}

// textGuard opens the relay store's connections so that a string argument sqlite3 could not bind
// is refused as Python's sqlite3 refuses it (EncodeUTF8), before SQLite sees it. Without it such a
// string is written as TEXT whose bytes are not UTF-8, which Python's sqlite3 cannot decode when it
// reads the row back ("Could not decode to UTF-8 column"), and looked up where Python raises.
type textGuard struct{ *sqlite.Driver }

func (d textGuard) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	sqliteConn, ok := conn.(guardedDriverConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("SQLite driver connection lacks the interfaces the store uses")
	}
	return guardedConn{sqliteConn}, nil
}

// guardedDriverConn is what the store asks of a modernc connection: database/sql's context
// interfaces, and the backup API a raw connection is asked for (Store.Projection, the takeover
// snapshot).
type guardedDriverConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
	NewBackup(string) (*sqlite.Backup, error)
}

type guardedConn struct{ guardedDriverConn }

// CheckNamedValue converts an argument as database/sql's default converter does, then refuses a
// string EncodeUTF8 refuses. database/sql wraps the refusal ("sql: converting argument ..."), so a
// caller reaches it through EncodeError.
func (c guardedConn) CheckNamedValue(value *driver.NamedValue) error {
	converted, err := driver.DefaultParameterConverter.ConvertValue(value.Value)
	if err != nil {
		return err
	}
	if text, ok := converted.(string); ok {
		if err = EncodeUTF8(text); err != nil {
			return err
		}
	}
	value.Value = converted
	return nil
}
