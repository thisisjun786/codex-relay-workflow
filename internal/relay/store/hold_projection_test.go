package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// The projection backs up committed content through the store's one connection. Inside a
// transaction of the same store that connection is taken, so it is refused like a nested
// transaction instead of waiting on itself.
func TestProjection_is_refused_inside_the_stores_own_transaction(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "state", "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	projected, release, err := s.Projection(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var version string
	if err = projected.DB.QueryRowContext(ctx, "SELECT value FROM schema_meta WHERE key='version'").Scan(&version); err != nil || version != SchemaVersion {
		t.Fatalf("projected version %q: %v", version, err)
	}
	if err = release(); err != nil {
		t.Fatal(err)
	}
	err = s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		_, _, nested := s.Projection(txCtx)
		return nested
	})
	if !errors.Is(err, ErrNestedTransaction) {
		t.Fatalf("nested projection: %v", err)
	}
}
