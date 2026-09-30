package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// The frozen schema declares no foreign key, so no contract write can violate one; this proves
// only that the PRAGMA each connection sets is enforced inside Store.Transaction.
func TestTransaction_enforces_foreign_keys_on_a_scratch_table(t *testing.T) {
	// Given: scratch tables declaring a foreign key, which the contract tables do not.
	s := recordStore(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE fk_parent (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, `CREATE TABLE fk_child (id TEXT PRIMARY KEY,parent TEXT REFERENCES fk_parent(id))`); err != nil {
		t.Fatal(err)
	}
	// When: the transaction writes a child without its parent.
	err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO fk_child VALUES ('c','missing')`)
		return err
	})
	// Then: the error preserves the Python SQLite reason and no row survives.
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("foreign-key reason: %v", err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM fk_child`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial child: %d %v", count, err)
	}
}
