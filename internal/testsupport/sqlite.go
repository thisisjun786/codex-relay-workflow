package testsupport

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// FrozenStorePath is the frozen empty relay store the contract keeps
// (contract/fixtures/sqlite-ddl/python-store.sqlite3): the schema the relay's first release left, in WAL
// mode, holding only the schema_meta rows version, store_id, store_created_at and socket_path and
// none of the ownership stamp. It is the store of the version before CRW-301, which added the
// indexes of HistoryIndexes: a test of the upgrade starts from it, and Create adds them to a copy
// so a test that needs a store of this version does not meet them as news. Tests start a store from it
// (Create, a fixture restored row by row) instead of running either runtime's initializer.
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
// without rows maps to an empty list.
//
// The additive DAG zone (store/dag_zone.go) is outside what a golden of the v1 schema holds, so a
// where that does not name it never selects its tables: the selection keeps the frozen v1 schema's
// tables (testsupport.V1TableRows) and drops every table the frozen script
// (contract/schema/relay-sqlite.sql) does not create. Membership is that script's, not a name
// prefix, the way swapgate.zoneObjects derives the zone from the statements an open runs: the
// merge-lane tables the zone appends carry no dag_ prefix and are dropped like the rest. A table a
// test creates on its own is dropped too, and it is neither v1 nor the zone.
//
// A where that names the zone ("dag" or "zone") takes over the selection and excludes nothing, which
// is how a caller asks for every table of the store, the zone included.
func TableRows(t testing.TB, db Querier, where string) map[string][]map[string]any {
	t.Helper()
	query := "SELECT name FROM sqlite_master WHERE type='table'"
	if where != "" {
		query += " AND (" + where + ")"
	}
	selectsZone := strings.Contains(where, "dag") || strings.Contains(where, "zone")
	tables := map[string][]map[string]any{}
	for _, table := range Rows(t, db, query+" ORDER BY name") {
		name := table["name"].(string)
		if !selectsZone && !IsV1Table(t, name) {
			continue
		}
		tables[name] = Rows(t, db, "SELECT * FROM "+QuoteIdent(name)+" ORDER BY rowid")
	}
	return tables
}

// ZoneTableRows are the rows of every table of the additive DAG zone (store/dag_zone.go): the tables
// of db the frozen v1 script does not create, whatever they are named.
func ZoneTableRows(t testing.TB, db Querier) map[string][]map[string]any {
	t.Helper()
	return tableRowsWhere(t, db, func(name string) bool { return !IsV1Table(t, name) })
}

// V1TableRows are the rows of every table of the frozen v1 schema, the zone excluded. It is
// TableRows(t, db, "") spelled out for a caller that wants the intent in the name.
func V1TableRows(t testing.TB, db Querier) map[string][]map[string]any {
	t.Helper()
	return tableRowsWhere(t, db, func(name string) bool { return IsV1Table(t, name) })
}

// AllTableRows are the rows of every table of db, the additive DAG zone included: the oracle of
// "a refusal changed nothing".
func AllTableRows(t testing.TB, db Querier) map[string][]map[string]any {
	t.Helper()
	return tableRowsWhere(t, db, func(string) bool { return true })
}

// tableRowsWhere is the rows of every table of db whose name keep accepts, in name order.
func tableRowsWhere(t testing.TB, db Querier, keep func(string) bool) map[string][]map[string]any {
	t.Helper()
	tables := map[string][]map[string]any{}
	for _, table := range Rows(t, db, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name") {
		name := table["name"].(string)
		if !keep(name) {
			continue
		}
		tables[name] = Rows(t, db, "SELECT * FROM "+QuoteIdent(name)+" ORDER BY rowid")
	}
	return tables
}

// v1TableNames is the set of table names the frozen v1 script creates
// (contract/schema/relay-sqlite.sql), lower-cased and read once.
var v1TableNames = sync.OnceValues(func() (map[string]bool, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(root, "contract", "schema", "relay-sqlite.sql"))
	if err != nil {
		return nil, err
	}
	// Comments are prose and several of them mention CREATE TABLE IF NOT EXISTS.
	code := regexp.MustCompile(`--[^\n]*`).ReplaceAllString(string(raw), "")
	names := map[string]bool{}
	for _, match := range regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([A-Za-z_][A-Za-z0-9_]*)`).FindAllStringSubmatch(code, -1) {
		names[strings.ToLower(match[1])] = true
	}
	return names, nil
})

// IsV1Table is IsV1TableName for a test, failing the test when the frozen script cannot be read.
func IsV1Table(t testing.TB, name string) bool {
	t.Helper()
	ok, err := IsV1TableName(name)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// IsV1TableName is whether name is a table of the frozen v1 script
// (contract/schema/relay-sqlite.sql): the tables a golden of the v1 schema holds. SQLite's own
// sqlite_sequence counts, since the script's AUTOINCREMENT tables create it. Everything else a store
// holds is the additive DAG zone's. The zone is derived this way and not from a name prefix, as
// swapgate.zoneObjects derives it from the statements an open runs: the merge-lane tables the zone
// appends carry no dag_ prefix.
func IsV1TableName(name string) (bool, error) {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, "sqlite_") {
		return true, nil
	}
	names, err := v1TableNames()
	if err != nil {
		return false, err
	}
	return names[lower], nil
}

// V1ObjectPredicate is the SQLite predicate selecting the sqlite_master rows of the frozen v1
// schema. Every sqlite_master object names its table in tbl_name (a table names itself, an index, a
// trigger and SQLite's own sqlite_autoindex_ entries name the table they belong to), so the
// predicate is a list of the v1 table names tested against tbl_name. It is built from the frozen
// script rather than from a name prefix, so a zone table the v1 script does not create is excluded
// whatever it is named. The returned text is a SQL boolean expression; the caller appends it to a
// WHERE clause.
func V1ObjectPredicate() (string, error) {
	names, err := v1TableNames()
	if err != nil {
		return "", err
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	quoted := make([]string, 0, len(sorted)+1)
	for _, name := range sorted {
		quoted = append(quoted, "'"+strings.ReplaceAll(name, "'", "''")+"'")
	}
	// sqlite_sequence is created by the frozen script's AUTOINCREMENT tables, so it is the v1 schema's too.
	quoted = append(quoted, "'sqlite_sequence'")
	return "tbl_name IN (" + strings.Join(quoted, ",") + ")", nil
}

// V1ObjectPredicateFor is V1ObjectPredicate for a test, failing the test when the frozen script
// cannot be read. Callers that already return an error use V1ObjectPredicate directly.
func V1ObjectPredicateFor(t testing.TB) string {
	t.Helper()
	predicate, err := V1ObjectPredicate()
	if err != nil {
		t.Fatal(err)
	}
	return predicate
}

// NonEmpty is tables without the tables that hold no rows.
func NonEmpty(tables map[string][]map[string]any) map[string][]map[string]any {
	maps.DeleteFunc(tables, func(_ string, rows []map[string]any) bool { return len(rows) == 0 })
	return tables
}

// SQLiteCatalog is the schema of the store at path, read through a read-only connection: each object of
// sqlite_master as "type name", mapped to the SQL that created it ("" for an object without any).
func SQLiteCatalog(t testing.TB, path string) map[string]string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT type || ' ' || name, COALESCE(sql, '') FROM sqlite_master")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}
