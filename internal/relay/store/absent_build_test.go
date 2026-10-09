package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// The build connection is synchronous=OFF (what makes the build fast), the finished file is synced before it
// is linked (the one point where the content must be durable), and a sync that fails leaves nothing published.
func TestAbsentStoreBuildConnectionIsUnsyncedAndTheFileIsSyncedBeforeTheLink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	var order []string
	buildObserved = func(db *sql.DB) error {
		var synchronous int
		if err := db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
			return err
		}
		if synchronous != 0 {
			t.Errorf("build connection synchronous = %d, want 0 (OFF)", synchronous)
		}
		order = append(order, "build")
		return nil
	}
	syncBuilt = func(temp string) error {
		order = append(order, "sync")
		if _, err := os.Lstat(path); err == nil {
			t.Error("the store was linked into place before its file was synced")
		}
		if filepath.Dir(temp) != dir || !strings.HasPrefix(filepath.Base(temp), ".relay-create-") {
			t.Errorf("synced %q, want the temporary build file", temp)
		}
		if _, err := os.Lstat(temp + "-wal"); err == nil {
			t.Error("the build connection was still open (a WAL remained) when the file was synced")
		}
		return syncFile(temp)
	}
	createFault = func(point string) error {
		order = append(order, point)
		return nil
	}
	t.Cleanup(func() {
		buildObserved = func(*sql.DB) error { return nil }
		syncBuilt = syncFile
		createFault = func(string) error { return nil }
	})
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got, want := strings.Join(order, ","), "gate-placed,build,sync,built,linked"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}

	// A sync that fails publishes nothing and leaves no build file behind.
	failing := filepath.Join(dir, "failing", "relay.sqlite3")
	boom := errors.New("the disk refused the sync")
	syncBuilt = func(string) error { return boom }
	if s, err := Open(context.Background(), failing, ""); err == nil || !errors.Is(err, boom) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("Open = %v, want the sync's failure", err)
	}
	if _, err := os.Lstat(failing); err == nil {
		t.Fatal("a store whose build could not be synced was published")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "failing", ".relay-create-*")); len(matches) != 0 {
		t.Fatalf("temporary build files left behind: %v", matches)
	}
}
