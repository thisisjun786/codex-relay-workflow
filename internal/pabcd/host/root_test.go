package host

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lexicalWorld is the B2-04 reproduction (scratch notes/B2.md, "Wrong DB identity reproduction"): a root
// spelled <lexical>/alias/.., where alias links to <physical>/sub, so the kernel resolves the root to
// <physical> while filepath.Clean names <lexical>. Each directory holds its own databases.
type lexicalWorld struct {
	f                  fixture
	physical, lexical  string
	spelled            string // <lexical>/alias/..
	physicalDB, lexDB  string
	physicalGoal, lexG string
}

func newLexicalWorld(t *testing.T) lexicalWorld {
	t.Helper()
	f := newFixture(t)
	w := lexicalWorld{f: f, physical: filepath.Join(f.root, "path-physical"), lexical: filepath.Join(f.root, "path-lexical")}
	for _, dir := range []string{filepath.Join(w.physical, "sub"), w.lexical} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(w.physical, "sub"), filepath.Join(w.lexical, "alias")); err != nil {
		t.Fatal(err)
	}
	w.spelled = w.lexical + "/alias/.."
	// The physical root, the one the kernel selects, holds the archived row; the lexical one an active row.
	f.home = w.physical
	w.physicalDB = f.db("5", map[string]any{"archived": 1})
	f.home = w.lexical
	w.lexDB = f.db("5", nil)
	for dir, status := range map[string]string{w.physical: "active", w.lexical: "paused"} {
		path := filepath.Join(dir, GoalsDBFilename)
		seed(t, path, goalsSchema)
		seed(t, path, "INSERT INTO thread_goals VALUES (?, 'g', 'o', ?)", child, status)
	}
	w.physicalGoal, w.lexG = filepath.Join(w.physical, GoalsDBFilename), filepath.Join(w.lexical, GoalsDBFilename)
	return w
}

// CRW-1136 criterion 1: relay session current must read the database the kernel selects for the configured
// root, so the archived identity there is refused; the oracle listed the physical directory and opened the
// lexically cleaned one, whose row was live.
func TestNativeSessionReadsTheDatabaseOfThePhysicalRoot(t *testing.T) {
	for _, variable := range []string{"CODEX_SQLITE_HOME", "CODEX_HOME"} {
		w := newLexicalWorld(t)
		delete(w.f.vars, "CODEX_HOME")
		w.f.vars[variable] = w.spelled
		got, err := w.f.resolve(w.f.cwd)
		if err == nil || err.Error() != msgArchived {
			t.Errorf("%s=%s: %+v, %v; want the archived refusal of %s", variable, w.spelled, got, err, w.physicalDB)
		}
	}
	// A live row in the physical database is accepted, and the path reported is the root as spelled, joined
	// without cleaning, so it names the file that was read.
	w := newLexicalWorld(t)
	w.f.home = w.physical
	w.f.db("6", nil)
	w.f.vars["CODEX_SQLITE_HOME"] = w.spelled
	if got := w.f.accepts("live physical row", w.f.cwd); got.DBPath != w.spelled+"/state_6.sqlite" {
		t.Errorf("DBPath %q, want %q", got.DBPath, w.spelled+"/state_6.sqlite")
	}
}

// CRW-1136 criterion 2: the goals database is read under the same physical root as the native database.
func TestGoalsDatabaseIsUnderTheSameRootAsTheNativeDatabase(t *testing.T) {
	w := newLexicalWorld(t)
	w.f.home = w.physical
	w.f.db("6", nil)
	vars := map[string]string{"CODEX_SQLITE_HOME": w.spelled, "CODEX_THREAD_ID": child}
	goals, err := GoalsDBPath(envOf(vars))
	if err != nil {
		t.Fatal(err)
	}
	if status := GoalActiveStatus(child, goals); status != GoalActive {
		t.Errorf("goals %q read %s, want the physical root's active row", goals, status)
	}
	native, err := ResolveNativeSession(w.f.cwd, envOf(vars))
	if err != nil {
		t.Fatal(err)
	}
	if dir := strings.TrimSuffix(goals, GoalsDBFilename); dir != strings.TrimSuffix(native.DBPath, "state_6.sqlite") {
		t.Errorf("goals %q and native %q are under different roots", goals, native.DBPath)
	}
}

// CRW-1136 criterion 3: a root whose spelling holds quotes, a dollar sign, a backtick, a backslash, a
// space or bytes that are not UTF-8 is listed and opened as the same directory, its bytes kept.
func TestNativeDatabaseIsListedAndOpenedUnderOneSpelling(t *testing.T) {
	for _, name := range []string{`q"uote`, "s'ingle", "d$ollar", "b`tick", `b\slash`, "sp ace", "inv-\x80", "inv-\xe2\x82"} {
		f := newFixture(t)
		home := filepath.Join(f.root, name)
		if err := os.Mkdir(home, 0o755); err != nil {
			t.Skipf("%q: %v", name, err)
		}
		want := home + "/state_5.sqlite"
		if err := os.Rename(f.db("5", nil), want); err != nil { // seed would read the name as a DSN
			t.Fatal(err)
		}
		f.vars["CODEX_HOME"] = home
		if got := f.accepts(name, f.cwd); got.DBPath != want {
			t.Errorf("%q: DBPath %q, want %q", name, got.DBPath, want)
		}
		goals, err := GoalsDBPath(envOf(map[string]string{"CODEX_HOME": home}))
		if err != nil || goals != home+"/"+GoalsDBFilename {
			t.Errorf("%q: goals %q, %v", name, goals, err)
		}
	}
}

// The root carries where it came from, keeps the spelling it was given (whitespace and "..", never
// trimmed or cleaned) and notes an empty HOME, which the oracle read as the working directory.
func TestCodexSQLiteRootSourcesAndNotes(t *testing.T) {
	account := func() (string, error) { return "/account", nil }
	for _, c := range []struct {
		vars map[string]string
		want Root
	}{
		{map[string]string{"CODEX_SQLITE_HOME": "/sq/a/..", "CODEX_HOME": "/ch", "HOME": "/h"}, Root{Path: "/sq/a/..", Source: "env", Variable: "CODEX_SQLITE_HOME"}},
		{map[string]string{"CODEX_SQLITE_HOME": "", "CODEX_HOME": "/ch ", "HOME": "/h"}, Root{Path: "/ch ", Source: "env", Variable: "CODEX_HOME"}},
		{map[string]string{"CODEX_HOME": "", "HOME": "/h/"}, Root{Path: "/h/.codex", Source: "default", Variable: "HOME"}},
		{map[string]string{}, Root{Path: "/account/.codex", Source: "default"}},
	} {
		if got, err := codexSQLiteRoot(envOf(c.vars), account); err != nil || got != c.want {
			t.Errorf("%v: %+v, %v, want %+v", c.vars, got, err, c.want)
		}
	}
	got, err := codexSQLiteRoot(envOf(map[string]string{"HOME": ""}), account)
	if err != nil || got.Path != "/account/.codex" || !strings.Contains(got.Note, "HOME is set but empty") {
		t.Errorf("empty HOME: %+v, %v", got, err)
	}
	for _, vars := range []map[string]string{{"CODEX_SQLITE_HOME": "./sq"}, {"CODEX_HOME": " /ch"}, {"HOME": "~"}} {
		var rootErr *RootError
		if _, err := codexSQLiteRoot(envOf(vars), account); !errors.As(err, &rootErr) {
			t.Errorf("%v: %v, want a RootError", vars, err)
		}
	}
	if got := (Root{Path: "/a/link/.."}).Join("state_5.sqlite"); got != "/a/link/../state_5.sqlite" {
		t.Errorf("Join cleaned: %q", got)
	}
}
