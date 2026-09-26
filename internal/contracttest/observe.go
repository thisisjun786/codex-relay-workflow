package contracttest

import (
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite for given.sql and expect.queries (docs/port/decisions.md 4)
)

// observe reports the fields of contract/README.md "expect.observe" for one path.
func observe(path string) (map[string]any, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"exists": false, "bytes": nil, "size": nil, "mode": nil, "target": nil, "entries": nil}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("observe: %w", err)
	}
	result := map[string]any{"exists": true, "bytes": nil, "size": nil, "mode": float64(info.Mode().Perm()), "target": nil, "entries": nil}
	if target, err := os.Readlink(path); err == nil {
		result["target"] = target
	}
	if stat, err := os.Stat(path); err == nil {
		switch {
		case stat.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("observe: %w", err)
			}
			result["bytes"], result["size"] = hex.EncodeToString(data), float64(len(data))
		case stat.IsDir():
			entries, err := os.ReadDir(path)
			if err != nil {
				return nil, fmt.Errorf("observe: %w", err)
			}
			names := make([]any, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			slices.SortFunc(names, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
			result["entries"] = names
		}
	}
	return result, nil
}

func seedSQL(state, script string) error {
	if err := os.MkdirAll(state, 0o755); err != nil {
		return fmt.Errorf("given.sql: %w", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(state, "relay.sqlite3"))
	if err != nil {
		return fmt.Errorf("given.sql: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(script); err != nil {
		return fmt.Errorf("given.sql: %w", err)
	}
	return nil
}

// querySQL runs expect.queries against the case database; rows become JSON arrays so they
// compare with fixture values.
func querySQL(state string, queries map[string]string) (map[string]any, error) {
	db, err := sql.Open("sqlite", filepath.Join(state, "relay.sqlite3"))
	if err != nil {
		return nil, fmt.Errorf("expect.queries: %w", err)
	}
	defer db.Close()
	out := map[string]any{}
	for label, query := range queries {
		rows, err := queryRows(db, query)
		if err != nil {
			return nil, fmt.Errorf("expect.queries[%s]: %w", label, err)
		}
		out[label] = rows
	}
	return out, nil
}

func queryRows(db *sql.DB, query string) ([]any, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		for i, value := range values {
			switch v := value.(type) {
			case int64:
				values[i] = float64(v)
			case []byte:
				values[i] = string(v)
			}
		}
		out = append(out, values)
	}
	return out, rows.Err()
}
