package store

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// venvPython is the worktree's interpreter, named before a test changes its working directory.
func venvPython(t *testing.T) string {
	t.Helper()
	return filepath.Join(repositoryRoot(t), ".venv", "bin", "python")
}

// pythonAt runs script with python from dir, in this test process's environment as it stands.
func pythonAt(t *testing.T, python, dir, script string) string {
	t.Helper()
	command := exec.Command(python, "-c", script)
	command.Dir = dir
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, raw)
	}
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
	for _, home := range []string{"", "/", filepath.Join(root, "home") + "//", "/" + filepath.Join(root, "home")} {
		t.Run("HOME="+home, func(t *testing.T) {
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
