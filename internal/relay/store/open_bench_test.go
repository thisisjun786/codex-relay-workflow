package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkWritableOpen measures what a writable open and close of an existing store costs, on
// an empty store and on one holding about 20 MB of journal rows (decision 56 compares the
// fence's copies of the store against the stamp read on the open connection).
func BenchmarkWritableOpen(b *testing.B) {
	for _, megabytes := range []int{0, 20} {
		b.Run(fmt.Sprintf("store=%dMB", megabytes), func(b *testing.B) {
			path := benchStore(b, megabytes)
			ctx := context.Background()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				s, err := Open(ctx, path, "")
				if err != nil {
					b.Fatal(err)
				}
				if err = s.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkTransaction measures one small write transaction on an open store.
func BenchmarkTransaction(b *testing.B) {
	path := benchStore(b, 0)
	ctx := context.Background()
	s, err := Open(ctx, path, "")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
			_, err := conn.ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES ('t', 'bench', 's', '{}')")
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// benchStore creates a store and fills its journal with about megabytes of rows.
func benchStore(b *testing.B, megabytes int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "state", "relay.sqlite3")
	ctx := context.Background()
	s, err := Open(ctx, path, "")
	if err != nil {
		b.Fatal(err)
	}
	detail := `"` + strings.Repeat("x", 4000) + `"`
	for written := 0; written < megabytes<<20; written += 64 * len(detail) {
		err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
			for j := 0; j < 64; j++ {
				if _, err := conn.ExecContext(ctx, "INSERT INTO journal (at, kind, subject, detail) VALUES ('t', 'bench', 's', ?)", detail); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	if err = s.Close(); err != nil {
		b.Fatal(err)
	}
	return path
}
