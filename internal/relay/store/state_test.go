package store

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

func TestCanonicalSocket_resolves_symlink_before_parent(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "target", "child")
	if err := os.MkdirAll(actual, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalSocket(root + "/alias/../socket")
	if err != nil || got != filepath.Join(root, "target", "socket") {
		t.Fatalf("socket=%q: %v", got, err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	got, err = canonicalSocket(filepath.Join(root, "dangling", "socket"))
	if err != nil || got != filepath.Join(root, "missing", "socket") {
		t.Fatalf("dangling=%q: %v", got, err)
	}
}
func TestExpandUser_matches_python_home_and_named_user(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ input, want string }{{"~", home}, {"~/socket", filepath.Join(home, "socket")}, {"~" + current.Username, current.HomeDir}} {
		got, err := expandUser(test.input)
		if err != nil || got != test.want {
			t.Errorf("%s: %q want %q: %v", test.input, got, test.want, err)
		}
	}
}
func TestDiscoverStateDir_normalizes_legacy_spelling(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	for _, spelling := range []string{"./socket", "a//b"} {
		legacy := socketHash(filepath.Clean(spelling))
		old := filepath.Join(root, "codex-session-relay", legacy)
		if err := os.MkdirAll(old, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(old, "relay.sqlite3"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		selected, err := DiscoverStateDir(spelling)
		if err != nil || selected.Path != old {
			t.Fatalf("%q: %+v: %v", spelling, selected, err)
		}
	}
}
func TestOpen_refuses_symlink_into_live_state(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("CRW_ALLOW_LIVE_STATE", "")
	live := filepath.Join(root, "codex-session-relay")
	if err := os.MkdirAll(live, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(live, alias); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), filepath.Join(alias, "relay.sqlite3"), "")
	if err != ErrLiveState {
		t.Fatalf("got %v", err)
	}
}
func TestLocate_returns_empty_physical_fields_when_missing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	loc, err := s.Locate(context.Background())
	if err != nil || loc.RealPath != "" || loc.Inode != 0 || loc.StoreID == "" || loc.SchemaVersion != "1" {
		t.Fatalf("location %+v: %v", loc, err)
	}
}
func TestOpen_seeds_identity_once_and_preserves_socket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	ctx := context.Background()
	s, err := Open(ctx, path, "first.sock")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, key := range []string{"store_id", "store_created_at", "socket_path"} {
		var value string
		if err := s.DB.QueryRow("SELECT value FROM schema_meta WHERE key=?", key).Scan(&value); err != nil {
			t.Fatal(err)
		}
		values[key] = value
	}
	s.Close()
	if values["store_id"] == "" || values["store_created_at"] == "" {
		t.Fatal(values)
	}
	s, err = Open(ctx, path, "second.sock")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for key, want := range values {
		var got string
		if err := s.DB.QueryRow("SELECT value FROM schema_meta WHERE key=?", key).Scan(&got); err != nil || got != want {
			t.Fatalf("%s: %q want %q: %v", key, got, want, err)
		}
	}
}
func TestBoundedDB_rw_refuses_missing_database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sqlite3")
	db, err := boundedDB(path, "rw", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err == nil {
		t.Fatal("rw created a missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing database was created: %v", err)
	}
}
