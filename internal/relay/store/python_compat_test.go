package store

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func pythonStoreValue(t *testing.T, script string, args ...string) string {
	t.Helper()
	return pythonStoreValueIn(t, repositoryRoot(t), script, args...)
}

// pythonStoreValueIn runs the repository's Python from dir, so package-local test support imports.
func pythonStoreValueIn(t *testing.T, dir, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", "-c", script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(isolatedEnv(t), "PYTHONPATH="+filepath.Join(repositoryRoot(t), "packages/codex-session-relay/src")+":"+dir)
	// Preserve the caller's isolated HOME, XDG state and temporary root for the Python comparison.
	for _, key := range []string{"HOME", "XDG_STATE_HOME", "CODEX_SESSION_RELAY_STATE", "TMPDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Python comparison: %v: %s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestDiscoverStateDir_adopts_python_legacy_parent_spellings(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	if err := os.MkdirAll(filepath.Join(root, "home"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{"a/../b.sock", "~/x/../y.sock", filepath.Join(root, "link") + "/../s.sock"} {
		t.Run(spelling, func(t *testing.T) {
			// Given: Python's old socket spelling named an existing state database.
			want := pythonStoreValue(t, "import sys; from codex_session_relay.store import legacy_socket_scope; print(legacy_socket_scope(sys.argv[1]))", spelling)
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

func TestResolveStateDir_matches_python_absolute_with_symlink_parent(t *testing.T) {
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
			want := pythonStoreValue(t, "import sys; from codex_session_relay.store import resolve_state_dir; print(resolve_state_dir(sys.argv[1] or None).path)", explicit)
			selected, err := ResolveStateDir(explicit, "")
			if err != nil || selected.Path != want {
				t.Fatalf("%s: %q want %q: %v", source, selected.Path, want, err)
			}
			// The textual parent remains for the OS, which traverses the symlink first.
			physical, err := resolvePath(selected.Path)
			if err != nil || physical != filepath.Join(root, "target", "st") {
				t.Fatalf("physical=%q: %v", physical, err)
			}
			dbPath := selected.DBPath()
			opened, err := Open(context.Background(), dbPath, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := opened.Close(); err != nil {
				t.Fatal(err)
			}
			if !exists(filepath.Join(physical, "relay.sqlite3")) {
				t.Fatalf("store did not open at Python's physical directory %q (selected %q, resolved %q)", physical, dbPath, opened.Path)
			}
		})
	}
}

func TestExpandUser_rejects_unknown_user_like_python_pathlib(t *testing.T) {
	input := "~crw_user_that_does_not_exist_17/x/../y"
	want := pythonStoreValue(t, "from pathlib import Path; import sys;\ntry: Path(sys.argv[1]).expanduser()\nexcept RuntimeError as error: print(type(error).__name__ + ': ' + str(error))", input)
	if want != "RuntimeError: Could not determine home directory." {
		t.Fatalf("unexpected Python outcome: %q", want)
	}
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
				t.Fatalf("Python rejected %q with %s; Go returned %v", input, want, err)
			}
			if _, err := os.Stat(filepath.Join(root, "codex-session-relay")); !os.IsNotExist(err) {
				t.Fatalf("unknown user created state: %v", err)
			}
		})
	}
}

func TestOpen_does_not_expand_tilde_like_python_store(t *testing.T) {
	// Given: both implementations run from the same temporary working directory.
	cwd := t.TempDir()
	input := "~/pst/relay.sqlite3"
	want := pythonStoreValue(t, "import os, sys; from codex_session_relay.store import Store; os.chdir(sys.argv[1]); s=Store(sys.argv[2]); print(s.locate()['realPath'])", cwd, input)
	// When: Go opens the same relative spelling from the same directory.
	t.Chdir(cwd)
	s, err := Open(context.Background(), input, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Locate(context.Background())
	if err != nil || got.RealPath != want || !exists(want) {
		t.Fatalf("realPath=%q want %q: %v", got.RealPath, want, err)
	}
}

func TestLocate_normalizes_db_path_and_absolutizes_real_path_like_python(t *testing.T) {
	for _, input := range []string{"./x//relay.sqlite3", "./x/../y//relay.sqlite3", "~/literal/relay.sqlite3"} {
		t.Run(input, func(t *testing.T) {
			root := t.TempDir()
			spelling := root + "/" + input
			want := pythonStoreValue(t, "import sys, json; from codex_session_relay.store import Store; location=Store(sys.argv[1]).locate(); print(json.dumps([location['dbPath'], location['realPath']]))", spelling)
			var expected [2]string
			if err := json.Unmarshal([]byte(want), &expected); err != nil {
				t.Fatal(err)
			}
			s, err := Open(context.Background(), spelling, "")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			got, err := s.Locate(context.Background())
			if err != nil || got.DBPath != expected[0] || got.RealPath != expected[1] || !filepath.IsAbs(got.RealPath) {
				t.Fatalf("location=(%q, %q), Python=(%q, %q): %v", got.DBPath, got.RealPath, expected[0], expected[1], err)
			}
		})
	}
}

func TestLocate_relative_db_path_matches_python(t *testing.T) {
	root := t.TempDir()
	input := "./x//relay.sqlite3"
	want := pythonStoreValue(t, "import os, sys, json; from codex_session_relay.store import Store; os.chdir(sys.argv[1]); location=Store(sys.argv[2]).locate(); print(json.dumps([location['dbPath'], location['realPath']]))", root, input)
	var expected [2]string
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	s, err := Open(context.Background(), input, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Locate(context.Background())
	if err != nil || got.DBPath != expected[0] || got.RealPath != expected[1] {
		t.Fatalf("location=(%q, %q), Python=(%q, %q): %v", got.DBPath, got.RealPath, expected[0], expected[1], err)
	}
}

func TestLocate_log_name_matches_python(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "relay.sqlite3")
	want := pythonStoreValue(t, "import sys; from codex_session_relay.store import Store; s=Store(sys.argv[1]); print(s.locate()['logName'])", path)
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Locate(context.Background())
	if err != nil || got.LogName != want {
		t.Fatalf("logName=%q want %q: %v", got.LogName, want, err)
	}
}
