package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// PathlibSpelling is str(Path(value)) as the interpreter answers it: empty and "." components
// collapse, ".." stays, exactly two leading slashes remain a root of their own (POSIX leaves "//"
// implementation-defined) and three or more fold to one.
func TestPathlibSpellingIsStrOfPath(t *testing.T) {
	t.Parallel()
	inputs := []string{"//var/x", "///var/x", "////x", "//", "/", "", ".", "./a", "a//b/./c", "//a/../b", "//./x", "x/", "//x/", "/a//b/"}
	var got [][2]string
	for _, input := range inputs {
		got = append(got, [2]string{input, PathlibSpelling(input)})
	}
	checkJSON(t, "str(Path())", got)
}

// A state directory spelled with two leading slashes, by --state, the environment override or
// XDG_STATE_HOME, is the directory Python's resolve_state_dir names, and says so in its detail.
func TestAStateDirectoryKeepsTwoLeadingSlashesAsPythonDoes(t *testing.T) {
	// Serial: sets process environment variables, which every other running test would see.
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("XDG_STATE_HOME", "/"+filepath.Join(root, "xdg"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	scope := defaultScopeSubstitution(t)
	state := "/" + filepath.Join(root, "st")
	flag, err := ResolveStateDir(state, "")
	if err != nil || flag.Path != state {
		t.Errorf("--state %s: %q: %v", state, flag.Path, err)
	}
	discovered, err := DiscoverStateDir("")
	if err != nil || discovered.Path != "/"+filepath.Join(root, "xdg", "codex-session-relay", discovered.SocketScope) {
		t.Errorf("XDG_STATE_HOME %s: %q (%s): %v", os.Getenv("XDG_STATE_HOME"), discovered.Path, discovered.Detail, err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", state)
	env, err := ResolveStateDir("", "")
	if err != nil {
		t.Fatalf("CODEX_SESSION_RELAY_STATE=%s: %v", state, err)
	}
	named := struct{ Flag, Env, Discovered, Detail string }{flag.Path, env.Path, discovered.Path, discovered.Detail}
	checkJSON(t, "resolve_state_dir and discover_state_dir", named, scope, golden.Substitute(root, "<ROOT>"))
}

// A relative --state, override or XDG_STATE_HOME is read against the working directory as the
// kernel names it (os.getcwd), not as $PWD spells it through a symbolic link.
func TestARelativeStateDirectoryIsReadAgainstThePhysicalWorkingDirectory(t *testing.T) {
	// Serial: sets process environment variables and changes the working directory, which every other running test would see.
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
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex-home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("XDG_STATE_HOME", "xdg")
	flag, err := ResolveStateDir("st", "")
	if err != nil || flag.Path != filepath.Join(physical, "st") {
		t.Errorf("--state st: %q: %v", flag.Path, err)
	}
	discovered, err := DiscoverStateDir("")
	if err != nil {
		t.Fatalf("XDG_STATE_HOME=xdg: %v", err)
	}
	t.Setenv("CODEX_SESSION_RELAY_STATE", "st")
	env, err := ResolveStateDir("", "")
	if err != nil {
		t.Fatalf("CODEX_SESSION_RELAY_STATE=st: %v", err)
	}
	named := struct{ Flag, Env, Discovered string }{flag.Path, env.Path, discovered.Path}
	checkJSON(t, "resolve_state_dir and discover_state_dir", named, defaultScopeSubstitution(t), golden.Substitute(root, "<ROOT>"))
}

// ~ is Path.home(): HOME when it is set at all, an empty HOME being the root and a HOME's
// trailing slashes dropped, and the passwd entry when HOME is unset. The default state directory
// is under that home, never under the working directory.
func TestHomeIsPathlibsHome(t *testing.T) {
	// Serial: sets process environment variables and changes the working directory, which every other running test would see.
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("XDG_STATE_HOME", "")
	// Unset, CODEX_HOME is the home's .codex, so the default socket that scopes the state
	// directory follows the same home.
	t.Setenv("CODEX_HOME", "")
	must(t, os.Unsetenv("CODEX_HOME"))
	expansions := func(t *testing.T) [][2]string {
		t.Helper()
		var out [][2]string
		for _, spelled := range []string{"~", "~/x", "~//x/"} {
			got, err := expandUser(spelled)
			if err != nil {
				t.Fatalf("%s: %v", spelled, err)
			}
			out = append(out, [2]string{spelled, pathlibSpelling(got)})
		}
		return out
	}
	for _, c := range []struct{ name, home string }{
		{"empty", ""}, {"root", "/"}, {"trailing-slashes", filepath.Join(root, "user-home") + "//"}, {"two-leading-slashes", "/" + filepath.Join(root, "user-home")},
	} {
		home := c.home
		t.Run("HOME="+c.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			got := expansions(t)
			checkJSON(t, "expanduser", got, golden.Substitute(root, "<ROOT>"))
			selected, err := DiscoverStateDir("")
			if err != nil {
				t.Fatal(err)
			}
			checkJSON(t, "default state", [2]string{selected.Path, selected.Detail}, defaultScopeSubstitution(t), golden.Substitute(root, "<ROOT>"))
		})
	}
	// Unset, HOME is the passwd entry. Only ~ is compared: the default state directory under the
	// passwd home is this account's live state, which a test never reads.
	t.Run("HOME unset", func(t *testing.T) {
		t.Setenv("HOME", "")
		if err := os.Unsetenv("HOME"); err != nil {
			t.Fatal(err)
		}
		got := expansions(t)
		for _, expanded := range got {
			if expanded[1] == root || expanded[1] == "/x" {
				t.Errorf("%s is %q with HOME unset", expanded[0], expanded[1])
			}
		}
		checkJSON(t, "expanduser", got, passwdHomeSubstitution())
	})
}

// PathlibChild and PathlibParent are str(Path(parent) / name) and str(Path(path).parent).
func TestPathlibChildAndParentAreJoinAndParentOfPath(t *testing.T) {
	t.Parallel()
	inputs := []string{"//var/x", "///var/x", "//", "/", ".", "", "a", "..", "a/b/", "//x", "/x", "/a/../b"}
	var got [][3]string
	for _, input := range inputs {
		got = append(got, [3]string{input, PathlibChild(input, "n"), PathlibParent(input)})
	}
	checkJSON(t, "Path(p) / n, Path(p).parent", got)
}

// Every store discovery names besides the canonical one, the legacy store it keeps, the store it
// adopts and the stores it reports as ambiguous or unidentified, is spelled as Python's
// discover_state_dir spells it: under an XDG_STATE_HOME of two leading slashes each keeps both, as
// the canonical directory does. Under a relative XDG_STATE_HOME or HOME the stores are asked
// through the absolute directory the selection names, so a store recording this socket is adopted
// and one recording none is named absolute, in both runtimes.
func TestDiscoverySpellsEveryStoreItNamesAsPythonDoes(t *testing.T) {
	// Serial: sets process environment variables and changes the working directory, which every other running test would see.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
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
	type discovered struct {
		Path, DBPath, Detail, SocketScope string
		Ambiguous, Unidentified           []string
	}
	// compare checks what discovery names for socket, as the key says, with the golden.
	compare := func(t *testing.T, key, socket string) StateSelection {
		t.Helper()
		got, err := DiscoverStateDir(socket)
		if err != nil {
			t.Fatal(err)
		}
		named := discovered{got.Path, got.DBPath(), got.Detail, got.SocketScope, append([]string{}, got.Ambiguous...), append([]string{}, got.Unidentified...)}
		checkJSON(t, key, named, golden.Substitute(root, "<ROOT>"))
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
			// The key is legacy_socket_scope's (the golden), the digest of the socket as spelled.
			legacy := wantText(t, "legacy_socket_scope", func() string { return socketHash(pathlibSpelling("a/../b.sock")) })
			store(filepath.Join(stores, legacy), "")
			if kept := compare(t, "kept", "a/../b.sock"); kept.SocketScope != legacy {
				t.Fatalf("the legacy store was not kept: %+v", kept)
			}
			// Another socket reports that store as one recording no socket.
			if other := compare(t, "unidentified", "/x/other.sock"); len(other.Unidentified) != 1 {
				t.Fatalf("the store recording no socket was not reported: %+v", other)
			}
			// A store recording the socket is adopted, and two of them are ambiguous.
			store(filepath.Join(stores, "adopt-a"), "/x/third.sock")
			if adopted := compare(t, "adopted", "/x/third.sock"); adopted.SocketScope != "adopt-a" {
				t.Fatalf("the store recording this socket was not adopted: %+v", adopted)
			}
			store(filepath.Join(stores, "adopt-b"), "/x/third.sock")
			if ambiguous := compare(t, "ambiguous", "/x/third.sock"); len(ambiguous.Ambiguous) != 2 {
				t.Fatalf("two stores recording this socket were not ambiguous: %+v", ambiguous)
			}
			// A link to a store directory is a directory to Path.is_dir(), which follows it.
			outside := filepath.Join(root, filepath.Base(base.stores)+"-outside")
			store(outside, "")
			if err := os.Symlink(outside, filepath.Join(stores, "linked")); err != nil {
				t.Fatal(err)
			}
			if linked := compare(t, "linked", "/x/fourth.sock"); len(linked.Unidentified) != 2 {
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
	// Serial: changes the process working directory, which every other running test would see.
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
	for _, c := range []struct{ name, cwd string }{{"alias-wd", filepath.Join(root, "alias", "wd")}, {"root", "/"}} {
		cwd := c.cwd
		t.Run("cwd "+c.name, func(t *testing.T) {
			t.Chdir(cwd)
			var named [][6]string
			for _, input := range inputs {
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
				named = append(named, [6]string{input, abspath, Dirname(abspath), directory, absolute, scope})
			}
			checkJSON(t, "input, Abspath, Dirname, StoreDirectory, Absolute, ScopeRoot", named, golden.Substitute(root, "<ROOT>"))
		})
	}
}

// Realpath is os.path.realpath without strict, which Path.resolve() is: a component that cannot
// be examined (under a directory this user may not search, beneath a file, too long) is kept as
// spelled and the walk goes on, a link loop is kept where it is met, a ".." after a link applies
// to the link's target, and a relative path is read against the physical working directory.
func TestRealpathIsPathResolve(t *testing.T) {
	// Serial: changes the process working directory, which every other running test would see.
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
	var resolved [][2]string
	for _, input := range inputs {
		got, err := Realpath(input)
		if err != nil {
			t.Errorf("Realpath(%q): %v", input, err)
		}
		resolved = append(resolved, [2]string{input, got})
	}
	checkJSON(t, "os.path.realpath", resolved, golden.Substitute(root, "<ROOT>"))
	if _, err = Realpath("a\x00b"); err == nil {
		t.Error("Realpath accepted an embedded NUL, which Python's lstat refuses with ValueError")
	}
}
