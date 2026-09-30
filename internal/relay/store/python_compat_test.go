package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func pythonStoreValue(t *testing.T, script string, args ...string) string {
	t.Helper()
	return pythonStoreValueIn(t, repositoryRoot(t), script, args...)
}

// pythonStoreValueIn runs the repository's Python from dir, so package-local test support imports.
// Its answer is recorded (pythonOracle).
func pythonStoreValueIn(t *testing.T, dir, script string, args ...string) string {
	t.Helper()
	return pythonStoreValueWith(t, dir, nil, script, args...)
}

// pythonStoreValueWith is pythonStoreValueIn with pyoracle options for the answer.
func pythonStoreValueWith(t *testing.T, dir string, opts []pyoracle.Option, script string, args ...string) string {
	t.Helper()
	isolated := isolatedEnv(t)
	parts := append([]string{"store-value", dir, script}, keptEnvironment()...)
	output := pythonOracle(t, append(parts, args...), func() ([]byte, error) {
		return runPythonStore(t, isolated, dir, script, args...)
	}, opts...)
	return strings.TrimSpace(string(output))
}

// pythonStoreValueAfter is pythonStoreValue for a Python that needs setup (a takeover of a store Go
// wrote, say) before it runs: setup runs only where the live Python answers.
func pythonStoreValueAfter(t *testing.T, setup func(), script string, args ...string) string {
	t.Helper()
	dir := repositoryRoot(t)
	isolated := isolatedEnv(t)
	parts := append([]string{"store-value", dir, script}, keptEnvironment()...)
	output := pythonOracle(t, append(parts, args...), func() ([]byte, error) {
		setup()
		return runPythonStore(t, isolated, dir, script, args...)
	})
	return strings.TrimSpace(string(output))
}

// storeEnvironment are the caller's variables that reach runPythonStore's Python: its isolated
// HOME, XDG state and temporary root.
var storeEnvironment = []string{"HOME", "XDG_STATE_HOME", "CODEX_SESSION_RELAY_STATE", "TMPDIR"}

// keptEnvironment spells storeEnvironment as it stands, for a question's name.
func keptEnvironment() []string {
	out := make([]string, 0, len(storeEnvironment))
	for _, key := range storeEnvironment {
		out = append(out, spelledVariable(key))
	}
	return out
}

// runPythonStore is the live Python run behind pythonStoreValueIn: isolated is isolatedEnv's
// environment, taken before the question is asked so that a run with and without the live Python
// makes the same temporary directories.
func runPythonStore(t *testing.T, isolated []string, dir, script string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("uv", append([]string{"run", "--no-sync", "python", "-c", script}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(isolated, "PYTHONPATH="+filepath.Join(repositoryRoot(t), "packages/codex-session-relay/src")+":"+dir)
	for _, key := range storeEnvironment {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("Python comparison: %v: %s", err, output)
	}
	return output, nil
}

// takeOverPythonStore hands the store the live Python created at path to Go. On replay nothing
// is there, and Go's own first opener creates the store at the same spelling: the location Go
// names is compared either way.
func takeOverPythonStore(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		testsupport.HandOver(t, path, "go")
	}
}

func TestDiscoverStateDir_adopts_python_legacy_parent_spellings(t *testing.T) {
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
			want := legacyScope(t, spelling)
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

// legacyScope is Python's legacy_socket_scope of a socket spelling. The key is a digest of
// str(Path(socket).expanduser()), which names this run's temporary directory, so what is recorded
// is that string: the digest is taken of it as this run spells it. Where the live Python answers,
// its own digest must be that one.
func legacyScope(t *testing.T, spelling string) string {
	t.Helper()
	var answer [2]string
	// The digest a rerun changes is read past in check mode (sameUpToNoise).
	script := "import json, sys; from pathlib import Path; from codex_session_relay.store import legacy_socket_scope; print(json.dumps([str(Path(sys.argv[1]).expanduser()), legacy_socket_scope(sys.argv[1])]))"
	if err := json.Unmarshal([]byte(pythonStoreValueWith(t, repositoryRoot(t), []pyoracle.Option{sameUpToNoise}, script, spelling)), &answer); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(answer[0]))
	scope := hex.EncodeToString(sum[:])[:16]
	if pyoracle.Live() && answer[1] != scope {
		t.Fatalf("legacy_socket_scope(%q) = %s, not the digest of %q", spelling, answer[1], answer[0])
	}
	return scope
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
			opened, err := fixtureOpen(context.Background(), dbPath, "")
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
	// When: Go takes the stopped store over and opens the same relative spelling from the same
	// directory.
	takeOverPythonStore(t, want)
	t.Chdir(cwd)
	s, err := fixtureOpen(context.Background(), input, "")
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
			takeOverPythonStore(t, expected[1])
			s, err := fixtureOpen(context.Background(), spelling, "")
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
	takeOverPythonStore(t, expected[1])
	t.Chdir(root)
	s, err := fixtureOpen(context.Background(), input, "")
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
	takeOverPythonStore(t, path)
	s, err := fixtureOpen(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Locate(context.Background())
	if err != nil || got.LogName != want {
		t.Fatalf("logName=%q want %q: %v", got.LogName, want, err)
	}
}

func isolatedEnv(t *testing.T) []string {
	t.Helper()
	root := t.TempDir()
	return append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+filepath.Join(root, "state"), "XDG_DATA_HOME="+filepath.Join(root, "data"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"), "CODEX_HOME="+filepath.Join(root, "codex"))
}
