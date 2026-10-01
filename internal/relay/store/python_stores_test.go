package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// storeRows is every row of a store the retired Python implementation wrote but its schema_meta
// (its runtime's stamp), read as SQLite holds it, in a fixture: a test that read Python's store
// with Go reads these rows in a store Go creates (restoreStore), since a fixture keeps rows rather
// than a database file. A table without an INTEGER PRIMARY KEY was dumped with its rowid first.
type storeRows struct {
	Tables []tableRows `json:"tables"`
}

type tableRows struct {
	Table   string   `json:"table"`
	Columns []string `json:"columns"`
	Rows    [][]cell `json:"rows"`
}

// cell is one SQLite value: null, an integer, a real, text (UTF-8, or base64 when it is not) or
// a blob.
type cell struct {
	Integer *string `json:"i,omitempty"`
	Real    *string `json:"r,omitempty"`
	Text    *string `json:"t,omitempty"`
	RawText *string `json:"tb,omitempty"`
	Blob    *string `json:"b,omitempty"`
}

func (c cell) value() (any, error) {
	switch {
	case c.Integer != nil:
		return strconv.ParseInt(*c.Integer, 10, 64)
	case c.Real != nil:
		f, err := strconv.ParseFloat(*c.Real, 64)
		if err != nil && !math.IsInf(f, 0) {
			return nil, err
		}
		return f, nil
	case c.Text != nil:
		return *c.Text, nil
	case c.RawText != nil:
		raw, err := base64.StdEncoding.DecodeString(*c.RawText)
		return string(raw), err
	case c.Blob != nil:
		return base64.StdEncoding.DecodeString(*c.Blob)
	default:
		return nil, nil
	}
}

// restoreStore creates the Go store at path and gives it exactly the rows of dump, every table
// but schema_meta emptied first.
func restoreStore(t *testing.T, path string, dump storeRows) *Store {
	t.Helper()
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatalf("Go cannot create the store Python's rows go into: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	tables, err := queryStrings(s.DB, "SELECT name FROM sqlite_master WHERE type = 'table' AND name != 'schema_meta' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		for _, table := range tables {
			if _, err := conn.ExecContext(ctx, "DELETE FROM "+testsupport.QuoteIdent(table)); err != nil {
				return err
			}
		}
		for _, table := range dump.Tables {
			names := make([]string, len(table.Columns))
			marks := make([]string, len(table.Columns))
			for i, column := range table.Columns {
				names[i], marks[i] = testsupport.QuoteIdent(column), "?"
			}
			insert := "INSERT INTO " + testsupport.QuoteIdent(table.Table) + " (" + strings.Join(names, ", ") + ") VALUES (" + strings.Join(marks, ", ") + ")"
			for _, row := range table.Rows {
				args := make([]any, len(row))
				for i, c := range row {
					value, err := c.value()
					if err != nil {
						return err
					}
					args[i] = value
				}
				if _, err := conn.ExecContext(ctx, insert, args...); err != nil {
					return fmt.Errorf("%s: %w", table.Table, err)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("restoring Python's rows: %v", err)
	}
	return s
}

func queryStrings(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var value string
		if err = rows.Scan(&value); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, value)
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

// pythonNoise are the values a rerun changes: a digest (of a temporary path, most often), the
// random name of a directory Python's tempfile made, a time read from the clock, and a file's
// inode number.
var pythonNoise = regexp.MustCompile(`[0-9a-f]{12,}|(?:relay-test-|decl-|tmp)[a-z0-9_]{8}|[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]+)?(?:\+00:00|Z)|inode\\*"?: ?[0-9]+`)

// maskNoise spells every pythonNoise value by its kind and length alone.
func maskNoise(text string) string {
	return pythonNoise.ReplaceAllStringFunc(text, func(value string) string { return fmt.Sprintf("<noise%d>", len(value)) })
}
