package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	_ "modernc.org/sqlite"
)

func TestOpen_matches_python_schema_when_fresh(t *testing.T) {
	t.Parallel()
	// Given: a temporary state directory.
	ctx := context.Background()
	root := t.TempDir()
	goDB := filepath.Join(root, "go", "relay.sqlite3")
	// When: Go initializes its own database.
	store, err := fixtureOpen(ctx, goDB, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Then: SQLite's persisted CREATE text is the one Python's Store persisted, byte for byte, and the indexes
	// CRW-301 added to it (contract/schema/relay-sqlite-history-indexes.json).
	checkJSON(t, "sqlite_master", master(t, goDB))
}
func TestOpen_preserves_python_database_when_reopened(t *testing.T) {
	t.Parallel()
	// Given: a committed database made by the real Python Store.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	fixture, err := testsupport.FrozenStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	// The fixture is written by hand, unfenced; the Go host owns it as its initializer would.
	testsupport.Fence(t, path, "go")
	before := master(t, path)
	// When: it is reopened.
	reopened, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Then: the schema gains the history indexes (CRW-301) and no other object changes, a second open
	// changes nothing, and integrity holds.
	indexes, err := testsupport.HistoryIndexes()
	if err != nil || len(indexes) == 0 {
		t.Fatalf("the golden delta: %d indexes: %v", len(indexes), err)
	}
	want := append([]string(nil), before...)
	for _, index := range indexes {
		want = append(want, "index\x00"+index.Name+"\x00"+index.Table+"\x00"+index.SQL)
	}
	sort.Strings(want)
	if got := master(t, path); !reflect.DeepEqual(got, want) {
		t.Fatalf("reopen: sqlite_master is %q, want the previous one plus the history indexes %q", got, want)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err = fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := master(t, path); !reflect.DeepEqual(got, want) {
		t.Fatal("a second open changed sqlite_master")
	}
	var integrity string
	if err := reopened.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity: %s: %v", integrity, err)
	}
}
func TestOpen_enforces_foreign_keys_on_every_connection(t *testing.T) {
	t.Parallel()
	// Given: a store whose pool holds one connection (decisions.md section 4), and that
	// connection discarded so the pool must dial a fresh one each round.
	ctx := context.Background()
	store, err := fixtureOpen(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if max := store.DB.Stats().MaxOpenConnections; max != 1 {
		t.Fatalf("max open connections %d", max)
	}
	for round := range 3 {
		// When: a connection is taken; the previous one was closed out of the pool.
		conn, err := store.DB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		// Then: every new connection enforces foreign keys and synchronous durability.
		var foreign, sync, busy int
		var journal string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
			t.Fatalf("connection %d: journal_mode=%s: %v", round, journal, err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreign); err != nil || foreign != 1 {
			t.Fatalf("connection %d: foreign_keys=%d: %v", round, foreign, err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync); err != nil || sync != 2 {
			t.Fatalf("connection %d: synchronous=%d: %v", round, sync, err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil || busy != 30000 {
			t.Fatalf("connection %d: busy_timeout=%d: %v", round, busy, err)
		}
		// Undo the pragma on this connection, then discard it, so the next round can pass only
		// if the connection hook ran again on a new connection.
		if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
			t.Fatal(err)
		}
		// Raw returning ErrBadConn removes the connection from the pool and closes it.
		if err := conn.Raw(func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("discard connection: %v", err)
		}
	}
	if opened := store.DB.Stats().OpenConnections; opened > 1 {
		t.Fatalf("open connections %d", opened)
	}
}
func TestDiscoverStateDir_adopts_legacy_noncanonical_sibling(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	// Given: an old spelling's existing store and a canonical directory without a database.
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	socket := "relative.sock"
	legacy := socketHash(socket)
	old := filepath.Join(root, "codex-session-relay", legacy)
	if err := os.MkdirAll(old, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "relay.sqlite3"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// When: discovering the directory.
	selection, err := DiscoverStateDir(socket)
	// Then: the existing legacy database wins.
	if err != nil || selection.Path != old {
		t.Fatalf("selected %+v: %v", selection, err)
	}
}

// The product opens a store in its live state directory with no variable set: the guard is
// test isolation only (decisions.md 46). Both live roots are opened, the home default and
// $XDG_STATE_HOME's, each made a temporary directory here.
func TestOpen_opens_the_live_state_by_default(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	for _, location := range []struct {
		name string
		path func(home, xdg string) string
	}{
		{"xdg", func(_, xdg string) string {
			return filepath.Join(xdg, "codex-session-relay", "default", "relay.sqlite3")
		}},
		{"home", func(home, _ string) string {
			return filepath.Join(home, ".local", "state", "codex-session-relay", "default", "relay.sqlite3")
		}},
	} {
		t.Run(location.name, func(t *testing.T) {
			// Given: the live state roots are temporary directories, and no refusal is set.
			root := t.TempDir()
			home, xdg := filepath.Join(root, "home"), filepath.Join(root, "xdg")
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", xdg)
			t.Setenv("CRW_REFUSE_LIVE_STATE", "")
			if err := os.Unsetenv("CRW_REFUSE_LIVE_STATE"); err != nil {
				t.Fatal(err)
			}
			path := location.path(home, xdg)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			// When: a store is opened there.
			s, err := fixtureOpen(context.Background(), path, "")
			// Then: it is opened, and it is the store at that path.
			if err != nil {
				t.Fatalf("got %v", err)
			}
			defer s.Close()
			var version string
			if err := s.DB.QueryRow("SELECT value FROM schema_meta WHERE key = 'version'").Scan(&version); err != nil || version != SchemaVersion {
				t.Fatalf("schema version %q: %v", version, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("database not at the live path: %v", err)
			}
		})
	}
}

// Under test isolation (CRW_REFUSE_LIVE_STATE=1, which testsupport sets in this binary) the same
// open is refused, before the database exists.
func TestOpen_refuses_live_state_under_test_isolation(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	// Given: a test-specific live state root.
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("CRW_REFUSE_LIVE_STATE", "1")
	path := filepath.Join(root, "codex-session-relay", "default", "relay.sqlite3")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// When: opening the protected location.
	_, err := fixtureOpen(context.Background(), path, "")
	// Then: it refuses before creating the database.
	if err != ErrLiveState {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("database created: %v", err)
	}
}
func master(t *testing.T, path string) []string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE 'dag\\_%' ESCAPE '\\' AND tbl_name NOT LIKE 'dag\\_%' ESCAPE '\\' ORDER BY type, name, tbl_name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var kind, name, table string
		var text sql.NullString
		if err := rows.Scan(&kind, &name, &table, &text); err != nil {
			t.Fatal(err)
		}
		result = append(result, kind+"\x00"+name+"\x00"+table+"\x00"+text.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
func repositoryRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSuffix(wd, "/internal/relay/store")
}

func TestOpen_refuses_both_live_state_locations_when_xdg_state_home_is_set(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	for _, location := range []struct {
		name string
		path func(home, xdg string) string
	}{
		{"xdg", func(_, xdg string) string { return filepath.Join(xdg, "codex-session-relay", "s", "relay.sqlite3") }},
		{"home", func(home, _ string) string {
			return filepath.Join(home, ".local", "state", "codex-session-relay", "s", "relay.sqlite3")
		}},
	} {
		t.Run(location.name, func(t *testing.T) {
			// Given: HOME and a different XDG_STATE_HOME, both under a temporary root.
			root := t.TempDir()
			home, xdg := filepath.Join(root, "home"), filepath.Join(root, "xdg")
			t.Setenv("HOME", home)
			t.Setenv("XDG_STATE_HOME", xdg)
			t.Setenv("CRW_REFUSE_LIVE_STATE", "1")
			path := location.path(home, xdg)
			// When: a store is opened at that live-state location.
			_, err := fixtureOpen(context.Background(), path, "")
			// Then: it is refused before the database exists.
			if !errors.Is(err, ErrLiveState) {
				t.Fatalf("got %v", err)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("database created: %v", err)
			}
		})
	}
}
