package projectlock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

func TestSharedHoldersShareAndAnExclusiveHolderWaitsForThem(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	first, err := Shared(ctx, path, "P1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Shared(ctx, path, "P1")
	if err != nil {
		t.Fatalf("a second shared holder waited for the first: %v", err)
	}
	saved := ownership.LockWait
	ownership.LockWait = 150 * time.Millisecond
	defer func() { ownership.LockWait = saved }()
	var expired *ownership.LockWaitExpired
	if _, err := Exclusive(ctx, path, "P1"); !errors.As(err, &expired) || expired.What != "the project binding lock" {
		t.Fatalf("an exclusive holder did not wait for shared holders: %v", err)
	}
	// Another project's lock is another file.
	other, err := Exclusive(ctx, path, "P2")
	if err != nil {
		t.Fatalf("a holder of P1 held up P2: %v", err)
	}
	_ = other()
	if err := first(); err != nil {
		t.Fatal(err)
	}
	if _, err := Exclusive(ctx, path, "P1"); !errors.As(err, &expired) {
		t.Fatalf("an exclusive holder got the lock while a shared holder was left: %v", err)
	}
	if err := second(); err != nil {
		t.Fatal(err)
	}
	writer, err := Exclusive(ctx, path, "P1")
	if err != nil {
		t.Fatalf("the lock was not released with its holders: %v", err)
	}
	// A start waits for the writer, for the same bounded time, under its own text.
	if _, err := Shared(ctx, path, "P1"); !errors.As(err, &expired) || expired.What != "the managed-start project lock" {
		t.Fatalf("a shared holder did not wait for the exclusive holder: %v", err)
	}
	if err := writer(); err != nil {
		t.Fatal(err)
	}
}

func TestAWaiterGetsTheLockWhenTheHolderLetsGoAndEndsWithItsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	writer, err := Exclusive(context.Background(), path, "P1")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		held, err := Shared(context.Background(), path, "P1")
		if err == nil {
			err = held()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a start took the lock a writer held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := writer(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the start did not get the lock when the writer let go")
	}
	writer, err = Exclusive(context.Background(), path, "P1")
	if err != nil {
		t.Fatal(err)
	}
	defer writer()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Shared(ctx, path, "P1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the wait outlived its context: %v", err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := Shared(cancelled, path, "P-free"); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled context took a free lock: %v", err)
	}
}

// The file is a sidecar beside the store, named by the key's hash, and apart from every request lock.
func TestThePathIsASidecarBesideTheStore(t *testing.T) {
	state := t.TempDir()
	store := filepath.Join(state, "relay.sqlite3")
	sum := sha256.Sum256([]byte("P1"))
	realState, err := filepath.EvalSymlinks(state)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realState, "managed-start-project-"+hex.EncodeToString(sum[:])+".lock")
	if got, err := Path(store, "P1"); err != nil || got != want {
		t.Fatalf("path %s (%v), want %s", got, err, want)
	}
	held, err := Shared(context.Background(), store, "P1")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	info, err := os.Lstat(want)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("sidecar: %v %v", info, err)
	}
}

func TestAStoreWithoutAPathIsRefused(t *testing.T) {
	for _, take := range []func(context.Context, string, string) (func() error, error){Shared, Exclusive} {
		if _, err := take(context.Background(), "", "P1"); err == nil {
			t.Fatal("a lock was taken for a store with no path")
		}
	}
	if _, err := Path("", "P1"); err == nil {
		t.Fatal("a path was made for a store with no path")
	}
}

// Two spellings of one store name one lock file: a lock held through the store's real path is the lock a
// writer meets when it reaches the same store through a link to the file, or through a linked directory.
func TestTwoSpellingsOfOneStoreShareTheLock(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	real := filepath.Join(state, "relay.sqlite3")
	if err := os.WriteFile(real, nil, 0600); err != nil {
		t.Fatal(err)
	}
	aliases := t.TempDir()
	fileLink := filepath.Join(aliases, "alias.sqlite3")
	if err := os.Symlink(real, fileLink); err != nil {
		t.Fatal(err)
	}
	dirLink := filepath.Join(aliases, "dir")
	if err := os.Symlink(state, dirLink); err != nil {
		t.Fatal(err)
	}
	saved := ownership.LockWait
	ownership.LockWait = 150 * time.Millisecond
	defer func() { ownership.LockWait = saved }()
	held, err := Shared(ctx, real, "P1")
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	var expired *ownership.LockWaitExpired
	for name, spelling := range map[string]string{"a link to the file": fileLink, "a linked directory": filepath.Join(dirLink, "relay.sqlite3")} {
		if _, err := Exclusive(ctx, spelling, "P1"); !errors.As(err, &expired) {
			t.Fatalf("%s did not meet the lock held through the real path: %v", name, err)
		}
	}
}
