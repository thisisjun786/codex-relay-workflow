package testsupport

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FrozenStorePath is the frozen empty relay store the contract keeps
// (contract/fixtures/sqlite-ddl/python-store.sqlite3): the schema every relay store has, in WAL
// mode, holding only the schema_meta rows version, store_id, store_created_at and socket_path and
// none of the ownership stamp. Tests start a store from it (Create, a fixture restored row by row)
// instead of running either runtime's initializer.
func FrozenStorePath() string {
	root, err := moduleRoot()
	if err != nil {
		panic(err)
	}
	return filepath.Join(root, "contract", "fixtures", "sqlite-ddl", "python-store.sqlite3")
}

// FrozenStore is the bytes of the frozen store (FrozenStorePath).
func FrozenStore() ([]byte, error) { return os.ReadFile(FrozenStorePath()) }

// QuoteIdent is name as an SQLite identifier: in double quotes, with each double quote doubled.
func QuoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

// Querier is a database Rows and TableRows read: a *sql.DB, a *sql.Conn or a *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// Rows are the rows query returns from db, each a map from column name to the value SQLite holds:
// an INTEGER as int64, a REAL as float64, TEXT and a BLOB as a string, NULL as nil. No rows is an
// empty list, never nil.
func Rows(t testing.TB, db Querier, query string, args ...any) []map[string]any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		row := make(map[string]any, len(columns))
		for i, value := range values {
			if raw, ok := value.([]byte); ok {
				value = string(raw)
			}
			row[columns[i]] = value
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out
}

// TableRows are the rows of each table of db that where selects, by table name: where is a
// condition on sqlite_master's name ("name LIKE 'fault_%'"; "" selects every table of the frozen v1
// schema, schema_meta and sqlite_sequence included), and each table's rows are Rows in rowid order. A table
// without rows maps to an empty list. The additive DAG zone (store/dag_zone.go) is outside what a
// golden of the v1 schema holds: a where that does not name it ("dag") never selects its tables.
func TableRows(t testing.TB, db Querier, where string) map[string][]map[string]any {
	t.Helper()
	query := "SELECT name FROM sqlite_master WHERE type='table'"
	if where != "" {
		query += " AND (" + where + ")"
	}
	if !strings.Contains(where, "dag") {
		query += " AND name NOT LIKE 'dag\\_%' ESCAPE '\\'"
	}
	tables := map[string][]map[string]any{}
	for _, table := range Rows(t, db, query+" ORDER BY name") {
		name := table["name"].(string)
		tables[name] = Rows(t, db, "SELECT * FROM "+QuoteIdent(name)+" ORDER BY rowid")
	}
	return tables
}

// NonEmpty is tables without the tables that hold no rows.
func NonEmpty(tables map[string][]map[string]any) map[string][]map[string]any {
	maps.DeleteFunc(tables, func(_ string, rows []map[string]any) bool { return len(rows) == 0 })
	return tables
}
