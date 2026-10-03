package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func TestDiscoverStateDir_adopts_python_legacy_parent_spellings(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "user-home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	if err := os.MkdirAll(filepath.Join(root, "user-home"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// "//"+root keeps its two leading slashes in str(Path()), so its legacy key is its own.
	for _, c := range []struct{ name, spelling string }{
		{"relative", "a/../b.sock"}, {"home", "~/x/../y.sock"},
		{"through-link", filepath.Join(root, "link") + "/../s.sock"}, {"two-slashes", "/" + filepath.Join(root, "sock", "app.sock")},
	} {
		spelling := c.spelling
		t.Run(c.name, func(t *testing.T) {
			// Given: Python's old socket spelling named an existing state database.
			want := legacyScope(t, spelling, root)
			old := filepath.Join(os.Getenv("XDG_STATE_HOME"), "codex-session-relay", want)
			if err := os.MkdirAll(old, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(old, "relay.sqlite3"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			// When: Go discovers the same socket.
			selected, err := DiscoverStateDir(spelling)
			// Then: the Python-owned sibling wins, without hashing a cleaned parent.
			if err != nil || selected.Path != old || selected.SocketScope != want {
				t.Fatalf("%q: %+v: %v", spelling, selected, err)
			}
		})
	}
}

// legacyScope is Python's legacy_socket_scope of a socket spelling: the digest of
// str(Path(socket).expanduser()). That spelling is the golden, with this run's temporary
// directory root as <ROOT>, and the digest is taken of it as this run spells it.
func legacyScope(t *testing.T, spelling, root string) string {
	t.Helper()
	spelled := wantText(t, "str(Path(socket).expanduser())", func() string {
		expanded, err := expandUser(spelling)
		if err != nil {
			t.Fatal(err)
		}
		return pathlibSpelling(expanded)
	}, golden.Substitute(root, "<ROOT>"))
	sum := sha256.Sum256([]byte(spelled))
	return hex.EncodeToString(sum[:])[:16]
}

func TestResolveStateDir_matches_python_absolute_with_symlink_parent(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	link := filepath.Join(root, "link")
	if err := os.MkdirAll(filepath.Join(root, "target", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target", "child"), link); err != nil {
		t.Fatal(err)
	}
	spelling := link + "/../st"
	for _, source := range []string{"flag", "env"} {
		t.Run(source, func(t *testing.T) {
			explicit := spelling
			t.Setenv("CODEX_SESSION_RELAY_STATE", "")
			if source == "env" {
				explicit = ""
				t.Setenv("CODEX_SESSION_RELAY_STATE", spelling)
			}
			selected, err := ResolveStateDir(explicit, "")
			if err != nil {
				t.Fatalf("%s: %v", source, err)
			}
			checkText(t, "resolve_state_dir", selected.Path, golden.Substitute(root, "<ROOT>"))
			// The textual parent remains for the OS, which traverses the symlink first.
			physical, err := resolvePath(selected.Path)
			if err != nil || physical != filepath.Join(root, "target", "st") {
				t.Fatalf("physical=%q: %v", physical, err)
			}
			dbPath := selected.DBPath()
			opened, err := fixtureOpen(context.Background(), dbPath, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			if !exists(filepath.Join(physical, "relay.sqlite3")) {
				t.Fatalf("store did not open at the physical directory %q (selected %q, resolved %q)", physical, dbPath, opened.Path)
			}
		})
	}
}

func TestExpandUser_rejects_unknown_user_like_python_pathlib(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	input := "~crw_user_that_does_not_exist_17/x/../y"
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("CODEX_SESSION_RELAY_STATE", input)
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"expandUser", func() error { _, err := expandUser(input); return err }},
		{"flag", func() error { _, err := ResolveStateDir(input, ""); return err }},
		{"env", func() error { _, err := ResolveStateDir("", ""); return err }},
		{"socket", func() error { _, err := DiscoverStateDir(input); return err }},
		{"xdg", func() error { t.Setenv("XDG_STATE_HOME", input); _, err := DiscoverStateDir(""); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil || !strings.Contains(err.Error(), "crw_user_that_does_not_exist_17") {
				t.Fatalf("Path.expanduser() rejects %q (RuntimeError: Could not determine home directory.); Go returned %v", input, err)
			}
			if _, err := os.Stat(filepath.Join(root, "codex-session-relay")); !os.IsNotExist(err) {
				t.Fatalf("unknown user created state: %v", err)
			}
		})
	}
}

func TestOpen_does_not_expand_tilde_like_python_store(t *testing.T) {
	// Serial: changes the process working directory, which every other running test would see.
	// Given: both implementations run from the same temporary working directory.
	cwd := t.TempDir()
	input := "~/pst/relay.sqlite3"
	// When: Go opens the relative spelling from that directory.
	t.Chdir(cwd)
	s, err := fixtureOpen(context.Background(), input, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Locate(context.Background())
	if err != nil || !exists(got.RealPath) {
		t.Fatalf("realPath=%q: %v", got.RealPath, err)
	}
	// Then: the store is the literal ~ directory under it, as Python's Store names it.
	checkText(t, "realPath", got.RealPath, golden.Substitute(cwd, "<CWD>"))
}

func TestLocate_normalizes_db_path_and_absolutizes_real_path_like_python(t *testing.T) {
	t.Parallel()
	for _, input := range []string{"./x//relay.sqlite3", "./x/../y//relay.sqlite3", "~/literal/relay.sqlite3"} {
		t.Run(input, func(t *testing.T) {
			root := t.TempDir()
			spelling := root + "/" + input
			s, err := fixtureOpen(context.Background(), spelling, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.Locate(context.Background())
			if err != nil || !filepath.IsAbs(got.RealPath) {
				t.Fatalf("location=(%q, %q): %v", got.DBPath, got.RealPath, err)
			}
			checkJSON(t, "dbPath, realPath", [2]string{got.DBPath, got.RealPath}, golden.Substitute(root, "<ROOT>"))
		})
	}
}

func TestLocate_relative_db_path_matches_python(t *testing.T) {
	// Serial: changes the process working directory, which every other running test would see.
	root := t.TempDir()
	input := "./x//relay.sqlite3"
	t.Chdir(root)
	s, err := fixtureOpen(context.Background(), input, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkJSON(t, "dbPath, realPath", [2]string{got.DBPath, got.RealPath}, golden.Substitute(root, "<ROOT>"))
}

func TestLocate_log_name_matches_python(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "relay.sqlite3")
	s, err := fixtureOpen(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Locate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	checkText(t, "logName", got.LogName, golden.Substitute(root, "<ROOT>"))
}
