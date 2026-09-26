package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpen_matches_python_schema_when_fresh(t *testing.T) {
	// Given: separate temporary state directories and the shipped Python Store.
	ctx := context.Background()
	root := t.TempDir()
	pythonDB := filepath.Join(root, "python", "relay.sqlite3")
	goDB := filepath.Join(root, "go", "relay.sqlite3")
	cmd := exec.Command("uv", "run", "--no-sync", "python", "-c", "from codex_session_relay.store import Store; import sys; Store(sys.argv[1])", pythonDB)
	cmd.Dir = repositoryRoot(t)
	cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+filepath.Join(root, "xdg"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "CODEX_HOME="+filepath.Join(root, "codex"), "PYTHONPATH="+filepath.Join(repositoryRoot(t), "packages/codex-session-relay/src"))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("python Store: %v: %s", err, output)
	}
	// When: Go initializes its own database.
	store, err := Open(ctx, goDB, "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Then: SQLite's persisted CREATE text is byte-identical.
	want := master(t, pythonDB)
	got := master(t, goDB)
	if !reflect.DeepEqual(got, want) {
		for i := range got {
			if i >= len(want) || got[i] != want[i] {
				t.Fatalf("sqlite_master object %d: Go %q, Python %q", i, got[i], want[i])
			}
		}
		t.Fatalf("object counts: Go %d Python %d", len(got), len(want))
	}
}
func TestOpen_preserves_python_database_when_reopened(t *testing.T) {
	// Given: a committed database made by the real Python Store.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	fixture, err := os.ReadFile(filepath.Join(repositoryRoot(t), "contract/fixtures/sqlite-ddl/python-store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	before := master(t, path)
	// When: it is reopened.
	reopened, err := Open(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	// Then: schema remains untouched and integrity holds.
	if got := master(t, path); !reflect.DeepEqual(got, before) {
		t.Fatal("reopen changed sqlite_master")
	}
	var integrity string
	if err := reopened.DB.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity: %s: %v", integrity, err)
	}
}
func TestOpen_enforces_foreign_keys_on_every_connection(t *testing.T) {
	// Given: a store whose pool holds one connection (decisions.md section 4), and that
	// connection discarded so the pool must dial a fresh one each round.
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
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
func TestOpen_refuses_live_state_without_override(t *testing.T) {
	// Given: a test-specific live state root.
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("CRW_ALLOW_LIVE_STATE", "")
	path := filepath.Join(root, "codex-session-relay", "default", "relay.sqlite3")
	// When: opening the protected location.
	_, err := Open(context.Background(), path, "")
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
	rows, err := db.Query("SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name, tbl_name")
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
			t.Setenv("CRW_ALLOW_LIVE_STATE", "")
			path := location.path(home, xdg)
			// When: a store is opened at that live-state location.
			_, err := Open(context.Background(), path, "")
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
