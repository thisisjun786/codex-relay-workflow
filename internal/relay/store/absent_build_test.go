package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// CRW-1054: the absent-store initializer builds the store on a temporary database nobody can name yet, with
// synchronous=OFF (every statement of the schema script and of the zone was a commit that waited for the
// disk, about 4 s per new store on a loaded host), and syncs the finished file before it links it into
// place. The store a command then works on is opened the usual way: synchronous=FULL.
func TestAbsentStoreIsBuiltUnsyncedAndOpenedSynchronousFull(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	var built bool
	createFault = func(point string) error {
		if point == "built" {
			built = true
			if _, err := os.Lstat(path); err == nil {
				t.Error("the store is linked into place before the build is synced and finished")
			}
		}
		return nil
	}
	t.Cleanup(func() { createFault = func(string) error { return nil } })
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !built {
		t.Fatal("the store was not built by this open")
	}
	var synchronous int
	if err := s.DB.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("synchronous = %d (%v), want 2 (FULL) on the store a command works on", synchronous, err)
	}
	var count int
	if err := s.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE name = 'dag_plans'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("zone table dag_plans: %d (%v), want the zone built with the store", count, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".relay-create-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("temporary build files left behind: %v (%v)", matches, err)
	}
}
