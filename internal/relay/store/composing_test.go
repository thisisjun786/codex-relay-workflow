package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestComposing_python_properties(t *testing.T) {
	t.Run("test_a_joined_scope_commits_nothing_of_its_own", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		err := s.Compose(ctx, func(joined context.Context, _ *sql.Conn) error {
			if err := s.AppendJournal(joined, JournalEntry{At: "t", Kind: "inner", Subject: "s", Detail: "d"}); err != nil {
				return err
			}
			// A reader outside the store's one connection, as Python's durable_kinds().
			if count := durableCount(t, s.Path, `SELECT count(*) FROM journal WHERE kind='inner'`); count != 0 {
				t.Fatalf("joined write was durable before opener committed: %d", count)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		entries, err := s.Journal(ctx, "inner", "s")
		if err != nil || len(entries) != 1 {
			t.Fatalf("committed entries: %+v %v", entries, err)
		}
	})
	t.Run("test_a_raise_inside_a_joined_scope_rolls_back_what_the_opener_wrote", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		failure := errors.New("interrupted")
		err := s.Compose(ctx, func(joined context.Context, _ *sql.Conn) error {
			if err := s.AppendJournal(joined, JournalEntry{At: "t", Kind: "opener", Subject: "s", Detail: "d"}); err != nil {
				return err
			}
			if err := s.AppendJournal(joined, JournalEntry{At: "t", Kind: "inner", Subject: "s", Detail: "d"}); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			t.Fatalf("lost failure: %v", err)
		}
		var count int
		if err := s.DB.QueryRowContext(ctx, `SELECT count(*) FROM journal`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial writes %d: %v", count, err)
		}
	})
	t.Run("test_nesting_without_composing_is_still_the_error_it_always_was", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		// When: a writer opens a transaction inside another one, outside any composing scope.
		err := s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, `INSERT INTO journal (at,kind,subject,detail) VALUES ('t','outer','s','d')`); err != nil {
				return err
			}
			// ctx here is the one Transaction handed the body, so it carries the open transaction.
			return s.Transaction(ctx, func(context.Context, *sql.Conn) error { return nil })
		})
		// Then: Python's error at once, the outer write rolled back, the store still writable.
		if !errors.Is(err, ErrNestedTransaction) {
			t.Fatalf("nested transaction: %v", err)
		}
		if entries, err := s.Journal(ctx, "outer", "s"); err != nil || len(entries) != 0 {
			t.Fatalf("outer write survived: %+v %v", entries, err)
		}
		if err := s.AppendJournal(ctx, JournalEntry{At: "t", Kind: "after", Subject: "s", Detail: "d"}); err != nil {
			t.Fatalf("connection remains locked: %v", err)
		}
	})
	t.Run("test_the_composing_counter_is_released_when_the_body_raises", func(t *testing.T) {
		s := recordStore(t)
		ctx := context.Background()
		failure := errors.New("failed")
		if err := s.Compose(ctx, func(context.Context, *sql.Conn) error { return failure }); !errors.Is(err, failure) {
			t.Fatalf("composition failure: %v", err)
		}
		if err := s.AppendJournal(ctx, JournalEntry{At: "t", Kind: "after", Subject: "s", Detail: "d"}); err != nil {
			t.Fatalf("failed composition retained transaction: %v", err)
		}
		// The composing scope must not outlive the failure: accidental nesting is still an error.
		nested := s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
			return s.Transaction(txCtx, func(context.Context, *sql.Conn) error { return nil })
		})
		if !errors.Is(nested, ErrNestedTransaction) {
			t.Fatalf("nested transaction after a failed composition: %v", nested)
		}
	})
}

// durableCount reads through its own connection, so it sees only what has been committed.
func durableCount(t *testing.T, path, query string) int {
	t.Helper()
	reader, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err := reader.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestTransaction_waits_for_the_one_connection_when_another_goroutine_holds_it(t *testing.T) {
	// Given: one goroutine inside a transaction, holding the store's only connection.
	s := recordStore(t)
	ctx := context.Background()
	inside, release := make(chan struct{}), make(chan struct{})
	outer := make(chan error, 1)
	go func() {
		outer <- s.Transaction(ctx, func(ctx context.Context, conn *sql.Conn) error {
			if _, err := conn.ExecContext(ctx, `INSERT INTO journal (at,kind,subject,detail) VALUES ('t','first','s','d')`); err != nil {
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	// When: a second goroutine starts its own transaction while the first is still open.
	second := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		second <- s.AppendJournal(ctx, JournalEntry{At: "t", Kind: "second", Subject: "s", Detail: "d"})
	}()
	<-started
	select {
	case err := <-second:
		t.Fatalf("second transaction finished while the first held the connection: %v", err)
	default:
	}
	close(release)
	// Then: both commit, one after the other.
	for name, done := range map[string]chan error{"first": outer, "second": second} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s transaction: %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s transaction deadlocked", name)
		}
	}
	for _, kind := range []string{"first", "second"} {
		if entries, err := s.Journal(ctx, kind, "s"); err != nil || len(entries) != 1 {
			t.Fatalf("%s: %+v %v", kind, entries, err)
		}
	}
}

func TestTransaction_refuses_nesting_at_once_when_the_inner_call_runs_on_another_goroutine(t *testing.T) {
	// Given: an open transaction holding the store's one connection.
	s := recordStore(t)
	ctx := context.Background()
	err := s.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		// When: a goroutine the body starts opens a transaction with the body's context.
		inner := make(chan error, 1)
		go func() { inner <- s.Transaction(txCtx, func(context.Context, *sql.Conn) error { return nil }) }()
		// Then: it is refused at once rather than waiting on the connection its caller holds.
		select {
		case err := <-inner:
			if !errors.Is(err, ErrNestedTransaction) {
				t.Errorf("nested transaction on another goroutine: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("nested transaction on another goroutine blocked on the held connection")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
