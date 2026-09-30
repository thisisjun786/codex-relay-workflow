package store

import (
	"context"
	"database/sql"
	"testing"
)

func TestTransaction_refuses_immediate_foreign_key_violation(t *testing.T) {
	// Given: an explicitly constrained table in a test-only database.
	store := recordStore(t)
	ctx := context.Background()
	if _, err := store.DB.ExecContext(ctx, `CREATE TABLE immediate_guard (id INTEGER PRIMARY KEY, parent INTEGER REFERENCES immediate_guard(id))`); err != nil {
		t.Fatal(err)
	}
	// When: writing a child without a parent in the transaction.
	err := store.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO immediate_guard (id,parent) VALUES (1,2)`)
		return err
	})
	// Then: the violation is returned and the row remains absent.
	if err == nil {
		t.Fatal("foreign key violation accepted")
	}
	var count int
	if scanErr := store.DB.QueryRowContext(ctx, `SELECT count(*) FROM immediate_guard`).Scan(&count); scanErr != nil || count != 0 {
		t.Fatalf("partial row: %d: %v", count, scanErr)
	}
}
