package testsupport

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

// DamageTable makes the next statement that reads or writes the b-tree of table fail as SQLITE_CORRUPT
// (result code 11, the class CRW-848 halts on), and leaves every other table readable and writable. It
// folds the write-ahead log into the database file and drops the connection's page cache, then overwrites
// the table's root page on disk, so only a statement that has to open that root meets the damage. The
// tables the tests damage are a few rows long, so the root page is the whole table. path is the
// database file of db. Tests only: the store must be a temporary one.
func DamageTable(t testing.TB, db *sql.DB, path, table string) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var root, pageSize int64
	if err = conn.QueryRowContext(ctx, "SELECT rootpage FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&root); err != nil {
		t.Fatalf("the root page of %s: %v", table, err)
	}
	if err = conn.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA shrink_memory"} {
		rows, e := conn.QueryContext(ctx, statement)
		if e != nil {
			t.Fatal(e)
		}
		_ = rows.Close()
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	garbage := make([]byte, pageSize)
	for i := range garbage {
		garbage[i] = 0xCD
	}
	if _, err = file.WriteAt(garbage, (root-1)*pageSize); err != nil {
		t.Fatal(err)
	}
	if err = file.Sync(); err != nil {
		t.Fatal(err)
	}
}
