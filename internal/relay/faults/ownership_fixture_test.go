package faults

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Ownership of the test stores.
//
// Every relay store is fenced (docs/port/decisions.md 14 and 30): a runtime writes only a store
// it owns and refuses one another runtime owns. The replay tests start Go from the frozen
// Python-produced empty store (contract/fixtures/sqlite-ddl) fenced for Go, as Go's absent-store
// initializer stamps a store (f1Twin), and read a store without writing it through readStore.

// f1Twin is the frozen empty store in dir, fenced for Go.
func f1Twin(t *testing.T, dir string) {
	t.Helper()
	fixture, err := testsupport.FrozenStore()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "relay.sqlite3")
	if err = os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	testsupport.Fence(t, path, "go")
}

// readStore reads the store at path, whichever runtime owns it, through a read-only snapshot.
func readStore(t *testing.T, ctx context.Context, path string, read func(context.Context, *store.Store) error) {
	t.Helper()
	ro, err := store.OpenReadOnly(ctx, path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	err = ro.ReadSnapshot(ctx, read)
	if closeErr := ro.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}
