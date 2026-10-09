package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1054: two processes that open a store without the DAG zone at the same moment each create the zone's
// tables, and SQLite answered one of them "database schema has changed (17)" (SQLITE_SCHEMA) in the middle of
// the zone, so its command exited 3. The race sits inside one statement (between its preparation and its run,
// or while it prepares again after a peer's commit), so a test cannot place it by ordering statements; it
// injects the result code the driver gives, at the stage the failure was seen.

// codedError is an error that answers the driver's Code, as *sqlite.Error does.
type codedError struct {
	code int
	text string
}

func (e *codedError) Error() string { return e.text }
func (e *codedError) Code() int     { return e.code }

func schemaChanged() error {
	return &codedError{code: sqliteSchema, text: "database schema has changed (17)"}
}

// attempts counts the calls of the schemaAttempt seam per stage and answers inject(stage, n) for call n.
func attempts(t *testing.T, inject func(stage string, n int) error) map[string]int {
	t.Helper()
	var mu sync.Mutex
	counts := map[string]int{}
	schemaAttempt = func(stage string) error {
		mu.Lock()
		counts[stage]++
		n := counts[stage]
		mu.Unlock()
		return inject(stage, n)
	}
	t.Cleanup(func() { schemaAttempt = func(string) error { return nil } })
	return counts
}

func zoneLessStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	return path
}

func tableCount(t *testing.T, s *Store, like string) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE ?", like).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpenRunsAgainAStatementSQLiteAnsweredSchemaChanged(t *testing.T) {
	for _, stage := range []string{"ownership stamp", "ownership schema", "v1 script", "DAG zone step 1", "DAG zone step 16", fmt.Sprintf("DAG zone step %d", len(dagZone)), "guard index"} {
		t.Run(stage, func(t *testing.T) {
			path := zoneLessStore(t)
			counts := attempts(t, func(at string, n int) error {
				if at == stage && n <= 3 {
					return schemaChanged()
				}
				return nil
			})
			s, err := Open(context.Background(), path, "")
			if err != nil {
				t.Fatalf("Open answered %v, want the zone installed after the statement ran again", err)
			}
			defer s.Close()
			if got := tableCount(t, s, "dag_%"); got < 20 {
				t.Fatalf("%d zone tables after Open, want the whole zone", got)
			}
			var index int
			if err := s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name='dag_decisions_active'").Scan(&index); err != nil || index != 1 {
				t.Fatalf("dag_decisions_active: %d (%v)", index, err)
			}
			if len(s.UnenforcedIndexes) != 0 {
				t.Fatalf("guard indexes left unenforced after the statements ran again: %v", s.UnenforcedIndexes)
			}
			if counts[stage] < 4 {
				t.Fatalf("stage %q ran %d times, want at least the 3 refused and 1 run", stage, counts[stage])
			}
		})
	}
}

func TestOpenGivesUpOnSchemaChangedAfterTheBound(t *testing.T) {
	path := zoneLessStore(t)
	const stage = "DAG zone step 16"
	counts := attempts(t, func(at string, _ int) error {
		if at == stage {
			return schemaChanged()
		}
		return nil
	})
	s, err := Open(context.Background(), path, "")
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded although every attempt of a step was answered SQLITE_SCHEMA")
	}
	if !isSchemaChanged(err) || !strings.Contains(err.Error(), "initialize DAG zone (step 16)") || !strings.Contains(err.Error(), "database schema has changed (17)") {
		t.Fatalf("the refusal lost the step or the driver's words: %v", err)
	}
	if counts[stage] != schemaChangeRetries+1 {
		t.Fatalf("step 16 ran %d times, want the first and %d more", counts[stage], schemaChangeRetries)
	}
}

func TestOpenDoesNotRunAgainAnyOtherStoreError(t *testing.T) {
	path := zoneLessStore(t)
	boom := &codedError{code: 5, text: "database is locked (5)"} // SQLITE_BUSY, which the busy timeout owns
	counts := attempts(t, func(at string, _ int) error {
		if at == "DAG zone step 16" {
			return boom
		}
		return nil
	})
	s, err := Open(context.Background(), path, "")
	if err == nil {
		_ = s.Close()
		t.Fatal("Open succeeded although a step failed")
	}
	if !errors.Is(err, boom) || counts["DAG zone step 16"] != 1 {
		t.Fatalf("err = %v after %d runs, want the one run's own error", err, counts["DAG zone step 16"])
	}
}

func TestIsSchemaChangedReadsTheResultCodeWhateverTheExtendedCode(t *testing.T) {
	for code, want := range map[int]bool{17: true, 17 | 1<<8: true, 5: false, 0: false} {
		if got := isSchemaChanged(fmt.Errorf("wrapped: %w", &codedError{code: code})); got != want {
			t.Errorf("code %d: %v, want %v", code, got, want)
		}
	}
	if isSchemaChanged(errors.New("database schema has changed (17)")) {
		t.Error("an error without a result code is not SQLITE_SCHEMA by its words")
	}
}

func TestDAGZoneStatementsNameWhatTheyCreate(t *testing.T) {
	seen := map[string]bool{}
	for i, statement := range dagZone {
		match := zoneObjectName.FindStringSubmatch(statement)
		if match == nil {
			t.Fatalf("step %d names no object it creates: %.80s", i+1, statement)
		}
		if seen[match[1]] {
			t.Fatalf("step %d creates %s a second time", i+1, match[1])
		}
		seen[match[1]] = true
	}
}

func TestZoneInstallIsOneTransaction(t *testing.T) {
	path := zoneLessStore(t)
	boom := errors.New("the host stopped in the middle of the zone")
	attempts(t, func(at string, _ int) error {
		if at == "DAG zone step 50" {
			return boom
		}
		return nil
	})
	if s, err := Open(context.Background(), path, ""); err == nil || !errors.Is(err, boom) {
		if s != nil {
			_ = s.Close()
		}
		t.Fatalf("Open = %v, want the failure at step 50", err)
	}
	schemaAttempt = func(string) error { return nil }
	raw, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var n int
	if err := raw.QueryRow("SELECT count(*) FROM sqlite_master WHERE name LIKE 'dag%' OR name LIKE 'merge_train%' OR name LIKE 'delivery_wakes%'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d zone objects after a failure at step 50 (%v), want none: the zone arrives whole or not at all", n, err)
	}
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatalf("the next open: %v", err)
	}
	defer s.Close()
	if got := tableCount(t, s, "dag_%"); got < 20 {
		t.Fatalf("%d zone tables after the next open, want the whole zone", got)
	}
}

// A store that holds the whole zone is not written by an open: a writer holding the database's write lock
// does not hold the open up.
func TestWholeZoneOpenTakesNoWriteLock(t *testing.T) {
	path := zoneLessStore(t)
	first, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	holder, err := first.DB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = holder.ExecContext(context.Background(), "ROLLBACK") }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := second.DB.ExecContext(ctx, "PRAGMA busy_timeout=100"); err != nil {
		t.Fatal(err)
	}
	if err := installDAGZone(ctx, second.DB); err != nil {
		t.Fatalf("installDAGZone on a whole zone while another connection holds the write lock: %v", err)
	}
}

// flakyQueryer answers the first `failures` reads of schema_meta with the driver's own SQLITE_SCHEMA error,
// as QueryContext gives it when a peer open changed the schema under the read, and the later ones from db.
type flakyQueryer struct {
	db       *sql.DB
	failures int
	reads    int
}

func (f *flakyQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	f.reads++
	if f.reads <= f.failures {
		return nil, schemaChanged()
	}
	return f.db.QueryContext(ctx, query, args...)
}

// The ownership stamp read turns a failed read into an ownership refusal; the refusal keeps the driver's
// error reachable, so the open's bounded retry sees the SQLITE_SCHEMA that the real QueryContext gave
// (CRW-1054: the retry around the stamp read missed it, and the open answered store_owned_by_other).
func TestStampReadRetriesSchemaChangedFromTheRealRead(t *testing.T) {
	path := zoneLessStore(t)
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := &flakyQueryer{db: db, failures: 3}
	var stamp interface{}
	err = retrySchemaChanged(context.Background(), "ownership stamp", func() error {
		s, e := stampOn(context.Background(), q)
		stamp = s
		return e
	})
	if err != nil {
		t.Fatalf("stamp read answered %v, want it read again after SQLITE_SCHEMA", err)
	}
	if q.reads != 4 || stamp == nil {
		t.Fatalf("%d reads, want the 3 refused and 1 run", q.reads)
	}
}

func TestStampReadGivesUpOnSchemaChangedInTheFenceWords(t *testing.T) {
	path := zoneLessStore(t)
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := &flakyQueryer{db: db, failures: 1 << 30}
	err = retrySchemaChanged(context.Background(), "ownership stamp", func() error {
		_, e := stampOn(context.Background(), q)
		return e
	})
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "store_owned_by_other" {
		t.Fatalf("final failure = %v, want the store_owned_by_other refusal", err)
	}
	if !strings.Contains(err.Error(), "ownership refused: read durable ownership: database schema has changed (17)") {
		t.Fatalf("the refusal changed its words: %v", err)
	}
	if q.reads != schemaChangeRetries+1 {
		t.Fatalf("%d reads, want the first and %d more", q.reads, schemaChangeRetries)
	}
}
