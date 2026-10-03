package swapgate

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// createIndex is the one statement shape that is executed, matched on the raw text because Normalised rewrites a quoted
// name (CREATE INDEX"name").
var createIndex = regexp.MustCompile(`(?i)^\s*create\s+index\b`)

// indexClass says whether the whole difference between the store's schema and the candidate's is ordinary indexes on tables
// both declare (CRW-472), and which way: ExtendsIndex when any index arrives (with or without departures), NarrowsIndex when
// indexes only leave. ok is false for anything else, and the difference then keeps the answer it always had. The caller
// guarantees that no object both readings hold is defined differently.
//
// An ordinary index is non-unique and calls no SQL function, and that is derived, not parsed: every table both readings
// declare is built in an in-memory database from its own statement, and each index statement is executed against them, so a
// statement SQLite does not take, a unique index, an index on a view or on a table that is not shared is never classified.
func indexClass(held, declared map[string]any, lost, added []string) (answer string, ok bool) {
	if len(lost)+len(added) == 0 {
		return "", false
	}
	for _, key := range append(append([]string(nil), lost...), added...) {
		if !strings.HasPrefix(key, "index ") {
			return "", false
		}
	}
	db, tables, err := sharedTables(held, declared)
	if err != nil {
		return "", false
	}
	defer db.Close()
	for _, key := range lost {
		if !ordinaryIndex(db, tables, key, held[key]) {
			return "", false
		}
	}
	for _, key := range added {
		if !ordinaryIndex(db, tables, key, declared[key]) {
			return "", false
		}
	}
	if len(added) > 0 {
		return ExtendsIndex, true
	}
	return NarrowsIndex, true
}

// sharedTables is an in-memory database holding every table both readings declare, built from the store's statement for it,
// and the lower-cased names of the tables it holds. A table whose statement SQLite does not take is left out, so the indexes
// on it are not classified.
func sharedTables(held, declared map[string]any) (*sql.DB, map[string]bool, error) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(1)
	var keys []string
	for key := range held {
		if _, shared := declared[key]; shared && strings.HasPrefix(key, "table ") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	tables := map[string]bool{}
	for _, key := range keys {
		if statement, ok := held[key].(string); ok {
			if _, err := db.Exec(statement); err == nil {
				tables[strings.ToLower(strings.TrimPrefix(key, "table "))] = true
			}
		}
	}
	return db, tables, nil
}

// ordinaryIndex is whether statement, the CREATE statement of index key, makes in the shared tables an index of that name that
// is not unique and calls no SQL function, on a shared table. Only a single CREATE INDEX statement is executed (a semicolon,
// even inside a literal, is not classified), in a transaction that is rolled back so the next one meets the same tables. The
// shared tables' own sqlite_autoindex_ entries are not counted: the index is looked up by name.
func ordinaryIndex(db *sql.DB, tables map[string]bool, key string, statement any) bool {
	text, ok := statement.(string)
	if !ok || !createIndex.MatchString(text) || strings.Contains(text, ";") {
		return false
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	if callsFunction(ctx, tx, text) {
		return false
	}
	if _, err := tx.ExecContext(ctx, text); err != nil {
		return false
	}
	var table string
	var unique int
	err = tx.QueryRowContext(ctx, "SELECT m.tbl_name, l.\"unique\" FROM sqlite_master AS m JOIN pragma_index_list(m.tbl_name) AS l ON l.name = m.name WHERE m.type = 'index' AND m.name = ?", strings.TrimPrefix(key, "index ")).Scan(&table, &unique)
	return err == nil && unique == 0 && tables[strings.ToLower(table)]
}

// callsFunction is whether SQLite's own program for the statement calls a SQL function, in the index's key or in a partial
// index's WHERE. Such an index is not ordinary: json_extract(reason, ...) fails for text that is not JSON, so it can fail the
// build over existing rows and a write an older runtime, which does not know the index, makes. A statement that cannot be
// explained counts as calling one. The program's opcodes are SQLite's and not a stable interface (Func and PureFunc today), so
// the tests keep json_extract as a control that turns a driver upgrade red.
func callsFunction(ctx context.Context, tx *sql.Tx, statement string) bool {
	rows, err := tx.QueryContext(ctx, "EXPLAIN "+statement)
	if err != nil {
		return true
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil || len(columns) < 2 {
		return true
	}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if rows.Scan(pointers...) != nil || strings.Contains(strings.ToLower(fmt.Sprint(values[1])), "func") {
			return true
		}
	}
	return rows.Err() != nil
}
