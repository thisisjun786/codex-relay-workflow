package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// benchObservations is the number of observation rows of the store the open benchmarks with
// observations hold: a relay that has run for a while and counts its sessions in the hundreds.
const benchObservations = 50000

// BenchmarkOpenWithObservations measures an open and close of an existing store that holds
// observations and the settlements the daemon wrote beside them, in the two forms a command
// opens it: writable, and as a command that declares itself read-only. The first measures the
// fixed cost of every open; the second adds that a read opens the same store.
func BenchmarkOpenWithObservations(b *testing.B) {
	for _, observations := range []int{0, benchObservations} {
		path := benchStoreWithObservations(b, observations)
		for _, form := range []struct {
			name string
			ctx  context.Context
		}{
			{"writable", context.Background()},
			{"read-command", WithReadOnlyCommand(context.Background())},
		} {
			b.Run(fmt.Sprintf("observations=%d/%s", observations, form.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					s, err := Open(form.ctx, path, "")
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
}

// BenchmarkReadCommandLockHold measures how long a read-only command's open holds SQLite's write
// lock, which stops the daemon's and every other command's writes meanwhile. A second connection
// asks for the lock (BEGIN IMMEDIATE without waiting) while the opens run, and every interval in
// which it was turned away counts as time the opens held it; the result is that time per open.
// The probe's own short holds can make an open wait, so the time of an open is read from
// BenchmarkOpenWithObservations, not from here.
func BenchmarkReadCommandLockHold(b *testing.B) {
	path := benchStoreWithObservations(b, benchObservations)
	ctx := WithReadOnlyCommand(context.Background())
	probe := startLockProbe(b, path)
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
	b.StopTimer()
	held, samples := probe.stop(b)
	b.ReportMetric(float64(held.Microseconds())/float64(b.N), "us-lock-held/op")
	b.ReportMetric(float64(samples)/float64(b.N), "probes/op")
}

// lockProbe polls SQLite's write lock from its own connection.
type lockProbe struct {
	quit    chan struct{}
	done    chan struct{}
	once    sync.Once
	held    time.Duration
	samples int
	err     error
}

func startLockProbe(b *testing.B, path string) *lockProbe {
	b.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=rw")
	if err != nil {
		b.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		b.Fatal(err)
	}
	p := &lockProbe{quit: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		defer db.Close()
		defer conn.Close()
		last := time.Now()
		for {
			select {
			case <-p.quit:
				return
			default:
			}
			_, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE")
			now := time.Now()
			p.samples++
			switch {
			case err == nil:
				if _, err = conn.ExecContext(ctx, "ROLLBACK"); err != nil {
					p.err = err
					return
				}
			case strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked"):
				p.held += now.Sub(last)
			default:
				p.err = err
				return
			}
			last = time.Now()
			time.Sleep(20 * time.Microsecond)
		}
	}()
	// A benchmark that fails still ends the probe.
	b.Cleanup(func() { p.close() })
	return p
}

func (p *lockProbe) close() { p.once.Do(func() { close(p.quit) }); <-p.done }

// stop ends the probe and returns the time it saw the lock held and how many times it asked.
func (p *lockProbe) stop(b *testing.B) (time.Duration, int) {
	b.Helper()
	p.close()
	if p.err != nil {
		b.Fatalf("the lock probe failed: %v", p.err)
	}
	return p.held, p.samples
}

// benchStoreWithObservations creates a store and fills it with n observations of assignments
// (one in ten without a relationship) and, as the daemon writes them in the same transaction,
// the settlement of every observation that has one.
func benchStoreWithObservations(b *testing.B, n int) string {
	b.Helper()
	path := filepath.Join(b.TempDir(), "state", "relay.sqlite3")
	ctx := context.Background()
	s, err := Open(ctx, path, "")
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
	}()
	if n == 0 {
		return path
	}
	err = s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
INSERT INTO observations (thread_id, turn_id, terminal_status, relationship_id, classification, event_id, observed_at)
SELECT 'thread-' || (i % 500), 'turn-' || i, 'completed', CASE WHEN i % 10 = 0 THEN NULL ELSE 'rel-' || (i % 500) END, 'settled', NULL, '2026-10-01T00:00:00Z' FROM n`, n); err != nil {
			return err
		}
		_, err := conn.ExecContext(ctx, "INSERT INTO assignment_settlements (relationship_id, thread_id, turn_id, terminal_status, settled_at) SELECT relationship_id, thread_id, turn_id, terminal_status, observed_at FROM observations WHERE relationship_id IS NOT NULL")
		return err
	})
	if err != nil {
		b.Fatal(err)
	}
	return path
}
