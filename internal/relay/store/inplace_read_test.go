package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// InPlaceRead reads a store's committed state without creating a sidecar, and refuses where it
// cannot: a WAL holding frames beside no shared-memory index (an unclean shutdown) may commit
// what D does not, and only building the index reads it. OpenStopRead refuses the same store,
// while an empty WAL (or none) leaves every commit in D, read immutable, and both sidecars
// present are read mode=ro.
func TestInPlaceReadRefusesAWALWithoutItsIndex(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	writer, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	writer.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE in_d (x)"} {
		if _, err = writer.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	checkpointed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA wal_autocheckpoint=0", "CREATE TABLE only_in_wal (x)"} {
		if _, err = writer.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if params, err := InPlaceRead(path); err != nil || params.Get("immutable") != "" || params.Get("mode") != "ro" {
		t.Fatalf("both sidecars present: %v %v", params, err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil || len(wal) <= walHeaderSize {
		t.Fatalf("the live WAL holds no frame: %d %v", len(wal), err)
	}
	crashed := filepath.Join(t.TempDir(), "relay.sqlite3")
	for file, content := range map[string][]byte{crashed: checkpointed, crashed + "-wal": wal} {
		if err = os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The frames are a real commit D lacks: a connection that may build the index reads it.
	verify := filepath.Join(t.TempDir(), "relay.sqlite3")
	for file, content := range map[string][]byte{verify: checkpointed, verify + "-wal": wal} {
		if err = os.WriteFile(file, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check, err := sql.Open("sqlite", "file:"+verify)
	if err != nil {
		t.Fatal(err)
	}
	var tables int
	err = check.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='only_in_wal'").Scan(&tables)
	_ = check.Close()
	if err != nil || tables != 1 {
		t.Fatalf("the copied WAL does not commit only_in_wal: %d %v", tables, err)
	}
	if _, err := InPlaceRead(crashed); !errors.Is(err, ErrWALWithoutIndex) {
		t.Errorf("a WAL with frames and no index: %v", err)
	}
	if ro, err := OpenStopRead(ctx, crashed, 0); !errors.Is(err, ErrWALWithoutIndex) {
		if ro != nil {
			_ = ro.Close()
		}
		t.Errorf("the Stop read of a WAL with frames and no index: %v", err)
	}
	for _, size := range []int{0, walHeaderSize} {
		if err = os.WriteFile(crashed+"-wal", wal[:size], 0o600); err != nil {
			t.Fatal(err)
		}
		if params, err := InPlaceRead(crashed); err != nil || params.Get("immutable") != "1" {
			t.Errorf("a WAL of %d bytes holds no frame, so D is read immutable: %v %v", size, params, err)
		}
	}
	if err = os.Remove(crashed + "-wal"); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenStopRead(ctx, crashed, 0)
	if err != nil {
		t.Fatal(err)
	}
	err = ro.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='in_d'").Scan(&tables)
	_ = ro.Close()
	if err != nil || tables != 1 {
		t.Fatalf("D without a WAL is read: %d %v", tables, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(crashed))
	if len(entries) != 1 {
		t.Fatalf("a read created a sidecar: %v", entries)
	}
}
