package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func lockedDatabase(t *testing.T) (string, *sql.Conn) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	seed, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.ExecContext(ctx, "CREATE TABLE seed(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	locker, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locker.Close() })
	lockConn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lockConn.Close() })
	if _, err := lockConn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	return path, lockConn
}

func TestOpenWaitsForLockBeforeJournalMode(t *testing.T) {
	ctx := context.Background()
	path, lockConn := lockedDatabase(t)

	started := make(chan struct{})
	retry := make(chan struct{})
	released := make(chan struct{})
	go func() {
		<-started
		<-retry
		_, releaseErr := lockConn.ExecContext(context.Background(), "COMMIT")
		if releaseErr != nil {
			_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
		}
		close(released)
	}()

	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var signalOnce = make(chan struct{}, 1)
	var retryOnce = make(chan struct{}, 1)
	reopened, err := open(openCtx, path, "", OpenOptions{BusyTimeout: 5 * time.Second, OnConnect: func() {
		select {
		case signalOnce <- struct{}{}:
			close(started)
		default:
		}
	}, BusyRetryHook: func() {
		select {
		case retryOnce <- struct{}{}:
			close(retry)
		default:
		}
	}})
	if err != nil {
		t.Fatalf("open while a second connection held a reserved lock: %v", err)
	}
	defer reopened.Close()
	select {
	case <-released:
	case <-openCtx.Done():
		t.Fatal(openCtx.Err())
	}
}

func TestOpenCancellationStopsBusyJournalModeRetry(t *testing.T) {
	path, _ := lockedDatabase(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstRetry := make(chan struct{})
	finished := make(chan error, 1)
	var once = make(chan struct{}, 1)
	go func() {
		_, err := open(ctx, path, "", OpenOptions{BusyTimeout: 30 * time.Second, BusyRetryHook: func() {
			select {
			case once <- struct{}{}:
				close(firstRetry)
			default:
			}
		}})
		finished <- err
	}()
	select {
	case <-firstRetry:
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("connection hook did not report a busy retry")
	}
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "PRAGMA journal_mode=WAL") {
			t.Fatalf("open cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open ignored cancellation after its first busy retry")
	}
}
