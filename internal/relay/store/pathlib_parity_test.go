package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// venvPython is the worktree's interpreter, named before a test changes its working directory.
func venvPython(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), ".venv", "bin", "python")
}

// pythonAt runs script with python from dir, in this test process's environment as it stands.
// Its answer is recorded (pythonOracle).
func pythonAt(t *testing.T, python, dir, script string, args ...string) string {
	t.Helper()
	parts := append([]string{"at", python, dir, script}, oracleEnvironmentNow()...)
	raw := pythonOracle(t, append(parts, args...), func() ([]byte, error) {
		command := exec.Command(python, append([]string{"-c", script}, args...)...)
		command.Dir = dir
		command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		raw, err := command.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("python: %v\n%s", err, raw)
		}
		return raw, nil
	})
	return strings.TrimSpace(string(raw))
}

// PathlibSpelling is str(Path(value)) as the interpreter answers it: empty and "." components
// collapse, ".." stays, exactly two leading slashes remain a root of their own (POSIX leaves "//"
// implementation-defined) and three or more fold to one.
func TestPathlibSpellingIsStrOfPath(t *testing.T) {
	inputs := []string{"//var/x", "///var/x", "////x", "//", "/", "", ".", "./a", "a//b/./c", "//a/../b", "//./x", "x/", "//x/", "/a//b/"}
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err = json.Unmarshal([]byte(pythonOutput(t, "import json, sys; from pathlib import Path; print(json.dumps([str(Path(p)) for p in json.loads(sys.argv[1])]))", string(raw))), &want); err != nil {
		t.Fatal(err)
	}
	for i, input := range inputs {
		if got := PathlibSpelling(input); got != want[i] {
			t.Errorf("PathlibSpelling(%q) = %q, Python's str(Path()) is %q", input, got, want[i])
		}
	}
}

// A state directory spelled with two leading slashes, by --state, the environment override or
// XDG_STATE_HOME, is the directory Python's resolve_state_dir names, and says so in its detail.
func TestAStateDirectoryKeepsTwoLeadingSlashesAsPythonDoes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("XDG_STATE_HOME", "/"+filepath.Join(root, "xdg"))
	state := "/" + filepath.Join(root, "st")
	var want struct{ Flag, Env, Discovered, Detail string }
	script := `import json, os, sys
from codex_session_relay.store import discover_state_dir, resolve_state_dir
flag = resolve_state_dir(sys.argv[1]).path
discovered = discover_state_dir(None)
os.environ["CODEX_SESSION_RELAY_STATE"] = sys.argv[1]
print(json.dumps({"Flag": str(flag), "Env": str(resolve_state_dir(None).path), "Discovered": str(discovered.path), "Detail": discovered.detail}))`
	if err := json.Unmarshal([]byte(pythonStoreValue(t, script, state)), &want); err != nil {
		t.Fatal(err)
	}
	if want.Flag != state || want.Discovered != "/"+filepath.Join(root, "xdg", "codex-session-relay", "default") {
		t.Fatalf("Python named %+v", want)
	}
	flag, err := ResolveStateDir(state, "")
	if err != nil || flag.Path != want.Flag {
		t.Errorf("--state %s: %q, Python %q: %v", state, flag.Path, want.Flag, err)
	}
	discovered, err := DiscoverStateDir("")
	if err != nil || discovered.Path != want.Discovered || discovered.Detail != want.Detail {
		t.Errorf("XDG_STATE_HOME %s: %q (%s), Python %q (%s): %v", os.Getenv("XDG_STATE_HOME"), discovered.Path, discovered.Detail, want.Discovered, want.Detail, err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", state)
	env, err := ResolveStateDir("", "")
	if err != nil || env.Path != want.Env {
		t.Errorf("CODEX_SESSION_RELAY_STATE=%s: %q, Python %q: %v", state, env.Path, want.Env, err)
	}
}

// A relative --state, override or XDG_STATE_HOME is read against the working directory as the
// kernel names it (os.getcwd), not as $PWD spells it through a symbolic link.
func TestARelativeStateDirectoryIsReadAgainstThePhysicalWorkingDirectory(t *testing.T) {
	python := venvPython(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	physical := filepath.Join(root, "real", "wd")
	if err = os.MkdirAll(physical, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias", "wd")
	t.Chdir(alias) // sets PWD to the alias, as a shell that cd'd through the link does
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("XDG_STATE_HOME", "xdg")
	var want struct{ Flag, Env, Discovered string }
	script := `import json, os, sys
from codex_session_relay.store import discover_state_dir, resolve_state_dir
flag = resolve_state_dir("st").path
discovered = discover_state_dir(None).path
os.environ["CODEX_SESSION_RELAY_STATE"] = "st"
print(json.dumps({"Flag": str(flag), "Env": str(resolve_state_dir(None).path), "Discovered": str(discovered)}))`
	if err = json.Unmarshal([]byte(pythonAt(t, python, alias, script)), &want); err != nil {
		t.Fatal(err)
	}
	if want.Flag != filepath.Join(physical, "st") {
		t.Fatalf("Python named %+v", want)
	}
	flag, err := ResolveStateDir("st", "")
	if err != nil || flag.Path != want.Flag {
		t.Errorf("--state st: %q, Python %q: %v", flag.Path, want.Flag, err)
	}
	discovered, err := DiscoverStateDir("")
	if err != nil || discovered.Path != want.Discovered {
		t.Errorf("XDG_STATE_HOME=xdg: %q, Python %q: %v", discovered.Path, want.Discovered, err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", "st")
	env, err := ResolveStateDir("", "")
	if err != nil || env.Path != want.Env {
		t.Errorf("CODEX_SESSION_RELAY_STATE=st: %q, Python %q: %v", env.Path, want.Env, err)
	}
}

// ~ is Path.home(): HOME when it is set at all, an empty HOME being the root and a HOME's
// trailing slashes dropped, and the passwd entry when HOME is unset. The default state directory
// is under that home, never under the working directory.
func TestHomeIsPathlibsHome(t *testing.T) {
	python := venvPython(t)
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("XDG_STATE_HOME", "")
	script := `import json
from pathlib import Path
from codex_session_relay.store import discover_state_dir
print(json.dumps([str(Path("~").expanduser()), str(Path("~/x").expanduser()), str(Path("~//x/").expanduser())]))`
	discover := `import json
from codex_session_relay.store import discover_state_dir
selected = discover_state_dir(None)
print(json.dumps([str(selected.path), selected.detail]))`
	for _, c := range []struct{ name, home string }{
		{"empty", ""}, {"root", "/"}, {"trailing-slashes", filepath.Join(root, "user-home") + "//"}, {"two-leading-slashes", "/" + filepath.Join(root, "user-home")},
	} {
		home := c.home
		t.Run("HOME="+c.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			var want []string
			if err := json.Unmarshal([]byte(pythonAt(t, python, root, script)), &want); err != nil {
				t.Fatal(err)
			}
			for i, spelled := range []string{"~", "~/x", "~//x/"} {
				got, err := expandUser(spelled)
				if err == nil {
					got = pathlibSpelling(got)
				}
				if err != nil || got != want[i] {
					t.Errorf("%s: %q, Python %q: %v", spelled, got, want[i], err)
				}
			}
			if err := json.Unmarshal([]byte(pythonAt(t, python, root, discover)), &want); err != nil {
				t.Fatal(err)
			}
			selected, err := DiscoverStateDir("")
			if err != nil || selected.Path != want[0] || selected.Detail != want[1] {
				t.Errorf("default state %q (%s), Python %q (%s): %v", selected.Path, selected.Detail, want[0], want[1], err)
			}
		})
	}
	// Unset, HOME is the passwd entry. Only ~ is compared: the default state directory under the
	// passwd home is this account's live state, which a test never reads.
	t.Run("HOME unset", func(t *testing.T) {
		t.Setenv("HOME", "")
		if err := os.Unsetenv("HOME"); err != nil {
			t.Fatal(err)
		}
		var want []string
		if err := json.Unmarshal([]byte(pythonAt(t, python, root, script)), &want); err != nil {
			t.Fatal(err)
		}
		for i, spelled := range []string{"~", "~/x", "~//x/"} {
			got, err := expandUser(spelled)
			if err == nil {
				got = pathlibSpelling(got)
			}
			if err != nil || got != want[i] || got == root || got == "/x" {
				t.Errorf("%s: %q, Python %q: %v", spelled, got, want[i], err)
			}
		}
	})
}

// PathlibChild and PathlibParent are str(Path(parent) / name) and str(Path(path).parent).
func TestPathlibChildAndParentAreJoinAndParentOfPath(t *testing.T) {
	inputs := []string{"//var/x", "///var/x", "//", "/", ".", "", "a", "..", "a/b/", "//x", "/x", "/a/../b"}
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	var want [][2]string
	script := "import json, sys; from pathlib import Path; print(json.dumps([[str(Path(p) / 'n'), str(Path(p).parent)] for p in json.loads(sys.argv[1])]))"
	if err = json.Unmarshal([]byte(pythonOutput(t, script, string(raw))), &want); err != nil {
		t.Fatal(err)
	}
	for i, input := range inputs {
		if got := PathlibChild(input, "n"); got != want[i][0] {
			t.Errorf("PathlibChild(%q, n) = %q, Python %q", input, got, want[i][0])
		}
		if got := PathlibParent(input); got != want[i][1] {
			t.Errorf("PathlibParent(%q) = %q, Python %q", input, got, want[i][1])
		}
	}
}

// Every store discovery names besides the canonical one, the legacy store it keeps, the store it
// adopts and the stores it reports as ambiguous or unidentified, is spelled as Python's
// discover_state_dir spells it: under an XDG_STATE_HOME of two leading slashes each keeps both, as
// the canonical directory does. Under a relative XDG_STATE_HOME or HOME the stores are asked
// through the absolute directory the selection names, so a store recording this socket is adopted
// and one recording none is named absolute, in both runtimes.
func TestDiscoverySpellsEveryStoreItNamesAsPythonDoes(t *testing.T) {
	python := venvPython(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	discover := `import json, sys
from codex_session_relay.store import discover_state_dir, legacy_socket_scope
if sys.argv[1] == "legacy":
    print(legacy_socket_scope(sys.argv[2]))
else:
    print(json.dumps(discover_state_dir(sys.argv[2]).to_record()))`
	// A store discovery asks about: a schema_meta table, recording socket when one is given. (Python's
	// sqlite3 wrote these until todo 44; the file is the same to either reader.)
	store := func(dir, socket string) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", filepath.Join(dir, "relay.sqlite3"))
		must(t, err)
		_, err = db.Exec("CREATE TABLE schema_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
		if err == nil && socket != "" {
			_, err = db.Exec("INSERT INTO schema_meta VALUES ('socket_path', ?)", socket)
		}
		must(t, errors.Join(err, db.Close()))
	}
	compare := func(t *testing.T, socket string) StateSelection {
		t.Helper()
		var want struct {
			Path, DBPath, Detail, SocketScope string
			Ambiguous, Unidentified           []string
		}
		if err := json.Unmarshal([]byte(pythonAt(t, python, root, discover, "discover", socket)), &want); err != nil {
			t.Fatal(err)
		}
		got, err := DiscoverStateDir(socket)
		if err != nil {
			t.Fatal(err)
		}
		if got.Path != want.Path || got.DBPath() != want.DBPath || got.Detail != want.Detail || got.SocketScope != want.SocketScope ||
			strings.Join(got.Ambiguous, "|") != strings.Join(want.Ambiguous, "|") || strings.Join(got.Unidentified, "|") != strings.Join(want.Unidentified, "|") {
			t.Errorf("socket %s:\n Go     %q %q %q ambiguous %q unidentified %q\n Python %q %q %q ambiguous %q unidentified %q",
				socket, got.Path, got.Detail, got.SocketScope, got.Ambiguous, got.Unidentified,
				want.Path, want.Detail, want.SocketScope, want.Ambiguous, want.Unidentified)
		}
		return got
	}
	for _, base := range []struct{ name, xdg, home, stores string }{
		{"XDG_STATE_HOME=two-slashes", "/" + filepath.Join(root, "two"), "", "two"}, {"XDG_STATE_HOME=absolute", filepath.Join(root, "one"), "", "one"},
		{"XDG_STATE_HOME=rel", "rel", "", "rel"}, {"XDG_STATE_HOME=,HOME=hrel", "", "hrel", "hrel/.local/state"},
	} {
		t.Run(base.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", base.xdg)
			if base.home != "" {
				t.Setenv("HOME", base.home)
			}
			stores := filepath.Join(root, base.stores, "codex-session-relay")
			// A relative socket's old key names a store that records no socket: discovery keeps it.
			legacy := pythonAt(t, python, root, discover, "legacy", "a/../b.sock")
			store(filepath.Join(stores, legacy), "")
			if kept := compare(t, "a/../b.sock"); kept.SocketScope != legacy {
				t.Fatalf("the legacy store was not kept: %+v", kept)
			}
			// Another socket reports that store as one recording no socket.
			if other := compare(t, "/x/other.sock"); len(other.Unidentified) != 1 {
				t.Fatalf("the store recording no socket was not reported: %+v", other)
			}
			// A store recording the socket is adopted, and two of them are ambiguous.
			store(filepath.Join(stores, "adopt-a"), "/x/third.sock")
			if adopted := compare(t, "/x/third.sock"); adopted.SocketScope != "adopt-a" {
				t.Fatalf("the store recording this socket was not adopted: %+v", adopted)
			}
			store(filepath.Join(stores, "adopt-b"), "/x/third.sock")
			if ambiguous := compare(t, "/x/third.sock"); len(ambiguous.Ambiguous) != 2 {
				t.Fatalf("two stores recording this socket were not ambiguous: %+v", ambiguous)
			}
			// A link to a store directory is a directory to Path.is_dir(), which follows it.
			outside := filepath.Join(root, filepath.Base(base.stores)+"-outside")
			store(outside, "")
			if err := os.Symlink(outside, filepath.Join(stores, "linked")); err != nil {
				t.Fatal(err)
			}
			if linked := compare(t, "/x/fourth.sock"); len(linked.Unidentified) != 2 {
				t.Fatalf("the linked store recording no socket was not reported: %+v", linked)
			}
		})
	}
}

// Abspath, Dirname and StoreDirectory are os.path.abspath, os.path.dirname and
// assignment.store_directory, and Absolute and ownership.ScopeRoot are Path(p).absolute() and
// Path(p).expanduser().absolute(). A relative path is read against the working directory the
// kernel names, not $PWD's spelling through a link, and under the root it gains no second slash.
func TestAbsolutePathsAreThePathsPythonNames(t *testing.T) {
	python := venvPython(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "real", "wd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	inputs := []string{"relay.sqlite3", "st/relay.sqlite3", "a/../st//relay.sqlite3", "./st/", "//x/relay.sqlite3", "/x/./relay.sqlite3", "//relay.sqlite3", "/relay.sqlite3", "."}
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	script := `import json, os, sys
from pathlib import Path
from codex_session_relay.assignment import store_directory
class Held:
    def __init__(self, path):
        self.path = Path(path)
print(json.dumps([[os.path.abspath(p), os.path.dirname(os.path.abspath(p)), store_directory(Held(p)), str(Path(p).absolute()), str(Path(p).expanduser().absolute())] for p in json.loads(sys.argv[1])]))`
	for _, c := range []struct{ name, cwd string }{{"alias-wd", filepath.Join(root, "alias", "wd")}, {"root", "/"}} {
		cwd := c.cwd
		t.Run("cwd "+c.name, func(t *testing.T) {
			t.Chdir(cwd)
			var want [][5]string
			if err := json.Unmarshal([]byte(pythonAt(t, python, cwd, script, string(raw))), &want); err != nil {
				t.Fatal(err)
			}
			for i, input := range inputs {
				abspath, err := Abspath(input)
				if err != nil {
					t.Fatal(err)
				}
				directory, err := StoreDirectory(input)
				if err != nil {
					t.Fatal(err)
				}
				absolute, err := Absolute(input)
				if err != nil {
					t.Fatal(err)
				}
				scope, err := ownership.ScopeRoot(input)
				if err != nil {
					t.Fatal(err)
				}
				got := [5]string{abspath, Dirname(abspath), directory, absolute, scope}
				if got != want[i] {
					t.Errorf("%q: Abspath, Dirname, StoreDirectory, Absolute, ScopeRoot\n go     %q\n python %q", input, got, want[i])
				}
			}
		})
	}
}

// Realpath is os.path.realpath without strict, which Path.resolve() is: a component that cannot
// be examined (under a directory this user may not search, beneath a file, too long) is kept as
// spelled and the walk goes on, a link loop is kept where it is met, a ".." after a link applies
// to the link's target, and a relative path is read against the physical working directory.
func TestRealpathIsPathResolve(t *testing.T) {
	python := venvPython(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"locked/inner", "real/wd", "real/deep/er"} {
		if err = os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		"loopa": "loopb", "loopb": "loopa", "self": "self", "alias": filepath.Join(root, "real"),
		"up": "real/deep/er/..", "dangling": "nowhere/x", "chain": "alias/deep", "abs-loop": filepath.Join(root, "abs-loop", "x"),
		"real/wd/back": "../..", "into-locked": "locked/inner",
	} {
		if err = os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Chmod(filepath.Join(root, "locked"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(root, "locked"), 0o700) })
	wd := filepath.Join(root, "alias", "wd")
	t.Chdir(wd)
	inputs := []string{
		root + "/locked/inner/st", root + "/into-locked/st", root + "/loopa/st", "loopa", "../../loopb/x/..", root + "/self/a/../b",
		root + "/abs-loop/y", root + "/alias/wd/../deep", root + "/up/x", root + "/dangling/y", root + "/chain/er/../../wd",
		root + "/file/x/y", root + "/real/wd/back/alias/wd", "st", ".", "", "..", "/", "//x/./y//", "/" + strings.Repeat("n", 300) + "/x",
		root + "/alias/../file",
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	if err = json.Unmarshal([]byte(pythonAt(t, python, wd, "import json, os, sys; print(json.dumps([os.path.realpath(p) for p in json.loads(sys.argv[1])]))", string(raw))), &want); err != nil {
		t.Fatal(err)
	}
	for i, input := range inputs {
		got, err := Realpath(input)
		if err != nil || got != want[i] {
			t.Errorf("Realpath(%q) = %q, %v; Python's os.path.realpath is %q", input, got, err, want[i])
		}
	}
	if _, err = Realpath("a\x00b"); err == nil {
		t.Error("Realpath accepted an embedded NUL, which Python's lstat refuses with ValueError")
	}
}
