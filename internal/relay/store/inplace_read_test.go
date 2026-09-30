package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	if _, params, err := InPlaceRead(path); err != nil || params.Get("immutable") != "" || params.Get("mode") != "ro" {
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
	if _, _, err := InPlaceRead(crashed); !errors.Is(err, ErrWALWithoutIndex) {
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
		if _, params, err := InPlaceRead(crashed); err != nil || params.Get("immutable") != "1" {
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

// The states test_fence.py test_check_stop_never_judges_a_stop_from_an_owner_its_wal_superseded
// gives Python's stop_metadata beside the plain unclean shutdown: an index that is not a regular
// file is no usable index, so frames beside it are refused as beside none; a -wal that is not a
// regular file is not a header-only log; and a -wal that cannot be examined has no read either.
func TestInPlaceReadRefusesAnUnusableIndexOrAnUnexaminableWAL(t *testing.T) {
	frames := make([]byte, walHeaderSize+1)
	for _, state := range []struct {
		name  string
		build func(t *testing.T, path string)
		want  func(error) bool
	}{
		{"frames beside a directory index", func(t *testing.T, path string) {
			must(t, os.WriteFile(path+"-wal", frames, 0o600))
			must(t, os.Mkdir(path+"-shm", 0o700))
		}, func(err error) bool { return errors.Is(err, ErrWALWithoutIndex) }},
		{"a directory log beside no index", func(t *testing.T, path string) {
			must(t, os.Mkdir(path+"-wal", 0o700))
		}, func(err error) bool { return errors.Is(err, ErrWALWithoutIndex) }},
		{"a log that cannot be examined", func(t *testing.T, path string) {
			must(t, os.Symlink(filepath.Base(path)+"-wal", path+"-wal"))
		}, func(err error) bool {
			return err != nil && strings.HasPrefix(err.Error(), "the store's write-ahead log could not be examined: ")
		}},
	} {
		t.Run(state.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "relay.sqlite3")
			must(t, os.WriteFile(path, nil, 0o600))
			state.build(t, path)
			if _, params, err := InPlaceRead(path); !state.want(err) {
				t.Fatalf("read with %v: %v", params, err)
			}
		})
	}
}

// SQLite resolves a symlinked database and keeps its -wal and -shm beside the file the link
// names, so the rule examines the sidecars there: through a link to a live store the WAL's
// commits are read (not D alone, immutable), through a link to a crashed one the WAL without its
// index is refused, and nothing is created beside the link. A connection SQLite made to a file
// other than the one examined is refused.
func TestInPlaceReadExaminesTheSidecarsOfTheFileALinkNames(t *testing.T) {
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
	real, err := resolvePath(path)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err = os.Symlink(path, linked); err != nil {
		t.Fatal(err)
	}
	resolved, params, err := InPlaceRead(linked)
	if err != nil || resolved != real || params.Get("immutable") != "" || params.Get("mode") != "ro" {
		t.Fatalf("a link to a live store: %q %v %v, want %q read mode=ro", resolved, params, err, real)
	}
	ro, err := OpenInPlace(ctx, linked, 0)
	if err != nil {
		t.Fatal(err)
	}
	var tables int
	err = ro.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE name='only_in_wal'").Scan(&tables)
	_ = ro.Close()
	if err != nil || tables != 1 {
		t.Fatalf("a table committed only to the WAL beside the linked file was not read: %d %v", tables, err)
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
	crashedLink := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err = os.Symlink(crashed, crashedLink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := InPlaceRead(crashedLink); !errors.Is(err, ErrWALWithoutIndex) {
		t.Errorf("a link to a WAL with frames and no index: %v", err)
	}
	if ro, err := OpenInPlace(ctx, crashedLink, 0); !errors.Is(err, ErrWALWithoutIndex) {
		if ro != nil {
			_ = ro.Close()
		}
		t.Errorf("the in-place read through a link to a WAL with frames and no index: %v", err)
	}
	for _, place := range []string{linked, crashedLink} {
		if entries, _ := os.ReadDir(filepath.Dir(place)); len(entries) != 1 {
			t.Errorf("a read created a sidecar beside the link: %v", entries)
		}
	}
	if entries, _ := os.ReadDir(filepath.Dir(crashed)); len(entries) != 2 {
		t.Errorf("a read created a sidecar beside the crashed store: %v", entries)
	}
	if ro, err := openExamined(ctx, linked, url.Values{"mode": {"ro"}}, 0); err == nil || !strings.Contains(err.Error(), "SQLite opened") {
		if ro != nil {
			_ = ro.Close()
		}
		t.Errorf("a connection to a file other than the one examined was kept: %v", err)
	}
}
