package host

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	child       = "019a0000-0000-7000-8000-000000000001"
	parent      = "019a0000-0000-7000-8000-000000000002"
	msgAbsent   = "CODEX_THREAD_ID is absent. Run this command inside the native Codex session."
	msgInvalid  = "CODEX_THREAD_ID must be an unmodified native UUID."
	msgWorkdir  = "Cannot resolve the working directory. Run from the native session's directory."
	msgNoRow    = "Native session row is missing. Run inside the intended Codex session."
	msgArchived = "Native session is archived or has an invalid archive flag."
	msgSource   = "Native source is not a supported root session; subagent, unknown and malformed sources cannot bind."
	msgRelative = "Native session has an invalid working directory."
	msgCwd      = "Cannot resolve the native session's working directory."
	msgMismatch = "Working directory does not match the native session. Run from its exact directory."
	msgReadDB   = "Cannot read the newest native state database or its threads schema. Check database access and Node SQLite support."
	msgNoFile   = "Native state database is missing. Check CODEX_SQLITE_HOME or CODEX_HOME."
	msgNoHome   = "Cannot locate the native state database. Check CODEX_SQLITE_HOME or CODEX_HOME."
	msgNotFile  = "Newest native state database must be a regular file, not a symlink or directory."

	// columns are untyped, as a column with no affinity keeps a stored INTEGER an INTEGER; id matches NOCASE.
	columns = "id TEXT COLLATE NOCASE PRIMARY KEY, cwd, archived, source, title TEXT"
)

// fixture is a native home beside a working directory, both under one real path.
type fixture struct {
	t               *testing.T
	root, cwd, home string
	vars            map[string]string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{t, root, filepath.Join(root, "work"), filepath.Join(root, "native"), nil}
	for _, dir := range []string{f.cwd, f.home} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.vars = map[string]string{"CODEX_THREAD_ID": child, "CODEX_HOME": f.home}
	return f
}

// db writes state_<version>.sqlite with one threads row; override replaces a column's value.
func (f fixture) db(version string, override map[string]any) string {
	return f.dbWith(version, columns, override)
}

func (f fixture) dbWith(version, schema string, override map[string]any) string {
	row := map[string]any{"id": child, "cwd": f.cwd, "archived": 0, "source": "vscode"}
	for column, value := range override {
		row[column] = value
	}
	path := filepath.Join(f.home, "state_"+version+".sqlite")
	seed(f.t, path, "CREATE TABLE threads ("+schema+")")
	seed(f.t, path, "INSERT INTO threads VALUES (?, ?, ?, ?, 'PRIVATE_TRANSCRIPT')", row["id"], row["cwd"], row["archived"], row["source"])
	return path
}

// dbIn writes a database with the default row into dir, which must exist.
func (f fixture) dbIn(dir string) string {
	g := f
	g.home = dir
	return g.db("5", nil)
}

func (f fixture) resolve(cwd string) (NativeSession, error) {
	return ResolveNativeSession(cwd, envOf(f.vars))
}

func (f fixture) refuses(name, cwd, want string) {
	f.t.Helper()
	if _, err := f.resolve(cwd); err == nil || err.Error() != want || strings.Contains(err.Error(), "PRIVATE_TRANSCRIPT") {
		f.t.Errorf("%s: %v, want %q", name, err, want)
	}
}

func (f fixture) accepts(name, cwd string) NativeSession {
	f.t.Helper()
	got, err := f.resolve(cwd)
	if err != nil {
		f.t.Errorf("%s: %v", name, err)
	}
	return got
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range list {
		names = append(names, entry.Name())
	}
	return names
}

func TestResolveNativeSessionAcceptsRootSourcesCwdAliasesAndRealZero(t *testing.T) {
	for _, c := range []map[string]any{{"source": "cli"}, {"source": "vscode"}, {"source": "exec"}, {"source": "mcp"}, {"archived": 0.0}} {
		f := newFixture(t)
		alias := filepath.Join(f.root, "alias")
		if err := os.Symlink(f.cwd, alias); err != nil {
			t.Fatal(err)
		}
		c["cwd"] = alias
		dbPath := f.db("5", c)
		before, _ := os.ReadFile(dbPath)
		files := entries(t, f.home)
		if got, err := f.resolve(alias); err != nil || got != (NativeSession{SessionID: child, Cwd: f.cwd, DBPath: dbPath}) {
			t.Errorf("%v: %+v, %v", c, got, err)
		}
		if after, _ := os.ReadFile(dbPath); !bytes.Equal(before, after) || !slices.Equal(files, entries(t, f.home)) {
			t.Errorf("%v: the native home changed", c)
		}
	}
}

func TestResolveNativeSessionAcceptsAnUppercaseUUID(t *testing.T) {
	f := newFixture(t)
	upper := strings.ToUpper(child)
	f.vars["CODEX_THREAD_ID"] = upper
	f.db("5", map[string]any{"id": upper})
	if got := f.accepts("uppercase", f.cwd); got.SessionID != upper {
		t.Errorf("%+v", got)
	}
}

func TestResolveNativeSessionRefusesRowsWithTheOraclesMessages(t *testing.T) {
	const big = int64(1) << 53
	for _, c := range []struct {
		name     string
		override map[string]any
		want     string
	}{
		{"missing row", map[string]any{"id": parent}, msgNoRow}, {"id differing in case under NOCASE", map[string]any{"id": strings.ToUpper(child)}, msgNoRow},
		{"id stored as a BLOB", map[string]any{"id": []byte(child)}, msgNoRow},
		{"archived", map[string]any{"archived": 1}, msgArchived}, {"invalid archive flag", map[string]any{"archived": 2}, msgArchived}, {"null archive flag", map[string]any{"archived": nil}, msgArchived},
		{"text zero in a column without affinity", map[string]any{"archived": "0"}, msgArchived}, {"BLOB archive flag", map[string]any{"archived": []byte{0}}, msgArchived},
		{"INTEGER 2^53-1 reads as a number", map[string]any{"archived": big - 1}, msgArchived}, {"REAL 2^53 is a number too", map[string]any{"archived": float64(big)}, msgArchived},
		{"INTEGER 2^53 cannot be a JavaScript number", map[string]any{"archived": big}, msgReadDB}, {"INTEGER -2^53 likewise", map[string]any{"archived": -big}, msgReadDB},
		{"a big INTEGER in cwd beats the archive flag", map[string]any{"archived": 1, "cwd": big}, msgReadDB}, {"a big INTEGER in source", map[string]any{"source": big}, msgReadDB},
		{"missing cwd", map[string]any{"cwd": "/does-not-exist-cxc"}, msgCwd}, {"relative cwd", map[string]any{"cwd": "."}, msgRelative},
		{"stored cwd with a NUL", map[string]any{"cwd": "/tmp\x00x"}, msgCwd}, {"null cwd", map[string]any{"cwd": nil}, msgRelative}, {"BLOB cwd", map[string]any{"cwd": []byte("/tmp")}, msgRelative},
		{"unknown source", map[string]any{"source": "unknown"}, msgSource}, {"null source", map[string]any{"source": nil}, msgSource}, {"BLOB source", map[string]any{"source": []byte("cli")}, msgSource},
		{"malformed JSON source", map[string]any{"source": `{"private":"PRIVATE_TRANSCRIPT"`}, msgSource}, {"JSON root string", map[string]any{"source": `"cli"`}, msgSource},
		{"custom source", map[string]any{"source": `{"custom":"cli"}`}, msgSource}, {"internal source", map[string]any{"source": `{"internal":"guardian"}`}, msgSource},
		{"subagent", map[string]any{"source": `{"subagent":{"thread_spawn":{"agent_role":null}}}`}, msgSource}, {"subagent review", map[string]any{"source": `{"subagent":"review"}`}, msgSource},
		{"archived beats the source", map[string]any{"archived": 1, "source": "unknown"}, msgArchived},
	} {
		f := newFixture(t)
		f.db("5", c.override)
		f.refuses(c.name, f.cwd, c.want)
	}
	f := newFixture(t)
	f.db("5", map[string]any{"cwd": f.root}) // the parent directory is not the working directory
	f.refuses("parent directory", f.cwd, msgMismatch)
}

// node:sqlite hands JavaScript the stored value as it is, and so should this port: the driver's own
// conversion of a column declared DATE, DATETIME or TIMESTAMP must not turn text into something the
// checks see differently, and affinity (not the port) decides what '0' in an INTEGER column is.
func TestResolveNativeSessionReadsDeclaredColumnTypes(t *testing.T) {
	const dated = "id TEXT, cwd DATETIME, archived DATE, source TIMESTAMP, title TEXT"
	f := newFixture(t)
	f.dbWith("5", dated, nil)
	if got := f.accepts("integer and text in date-typed columns", f.cwd); got.SessionID != child {
		t.Errorf("%+v", got)
	}
	for name, c := range map[string]struct {
		override map[string]any
		want     string
	}{
		"date text in cwd": {map[string]any{"cwd": "2026-01-01 00:00:00"}, msgRelative}, "date text in archived": {map[string]any{"archived": "2026-01-01 00:00:00"}, msgArchived},
		"date text in source": {map[string]any{"source": "2026-01-01T00:00:00Z"}, msgSource},
	} {
		f := newFixture(t)
		f.dbWith("5", dated, c.override)
		f.refuses(name, f.cwd, c.want)
	}
	typed := newFixture(t)
	typed.dbWith("5", "id TEXT, cwd TEXT, archived INTEGER, source TEXT, title TEXT", map[string]any{"archived": "0"})
	typed.accepts("INTEGER affinity turns '0' into 0", typed.cwd)
}

func TestResolveNativeSessionRefusesTheCaller(t *testing.T) {
	for _, c := range []struct {
		id, want string
		set      bool
	}{
		{"", msgAbsent, false}, {"", msgInvalid, true}, {"invalid-private-id", msgInvalid, true}, {child + "\n", msgInvalid, true}, {" " + child, msgInvalid, true}, {"../other", msgInvalid, true},
	} {
		f := newFixture(t)
		f.db("5", nil)
		delete(f.vars, "CODEX_THREAD_ID")
		if c.set {
			f.vars["CODEX_THREAD_ID"] = c.id
		}
		f.refuses(c.id, f.cwd, c.want)
		f.refuses(c.id+" with a bad cwd", "", c.want) // the identity is judged before the directory
	}
	f := newFixture(t)
	f.db("5", nil)
	file := filepath.Join(f.root, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, cwd := range map[string]string{"empty": "", "missing": filepath.Join(f.root, "absent"), "a file": file, "a file and a separator": file + "/"} {
		f.refuses(name+" call cwd", cwd, msgWorkdir)
	}
}

// The highest numeric state_<N> wins and is never skipped for an older one, equal numbers go by
// name, names that are not state_<digits>.sqlite are ignored, CODEX_SQLITE_HOME beats CODEX_HOME.
func TestResolveNativeSessionChoosesTheNewestDatabase(t *testing.T) {
	f := newFixture(t)
	f.db("9", map[string]any{"archived": 1})
	if got, err := f.resolve(f.cwd); err == nil {
		t.Fatalf("an older database answered: %+v", got)
	}
	if newest := f.db("10", nil); true {
		if got, err := f.resolve(f.cwd); err != nil || got.DBPath != newest {
			t.Fatalf("%+v, %v want %s", got, err, newest)
		}
	}
	huge := newFixture(t) // numbers beyond 64 bits still order numerically
	huge.db("5", map[string]any{"archived": 1})
	huge.db("99999999999999999998", map[string]any{"archived": 1})
	if want := huge.db("99999999999999999999", nil); true {
		if got := huge.accepts("huge numbers", huge.cwd); got.DBPath != want {
			t.Errorf("%+v, want %s", got, want)
		}
	}
	tie := newFixture(t) // equal numbers go to the smaller name
	tie.db("7", map[string]any{"archived": 1})
	if smaller := tie.db("007", nil); true {
		if got, err := tie.resolve(tie.cwd); err != nil || got.DBPath != smaller {
			t.Fatalf("%+v, %v want %s", got, err, smaller)
		}
	}
	other := filepath.Join(f.root, "override")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	f.home = other
	f.db("1", map[string]any{"archived": 1})
	f.vars["CODEX_SQLITE_HOME"] = other
	f.refuses("CODEX_SQLITE_HOME", f.cwd, msgArchived)
}

func TestResolveNativeSessionDatabaseFailuresDoNotFallBack(t *testing.T) {
	for _, c := range []struct {
		name, want string
		older      bool // an older, valid database that must not be used exists
		setup      func(f fixture, newest string)
	}{
		{"no database", msgNoFile, false, nil},
		{"only unrelated names", msgNoFile, false, func(f fixture, _ string) {
			for _, name := range []string{"state_5.sqlite.bak", "State_5.sqlite", "state_.sqlite", "state_+5.sqlite", "state_-1.sqlite", "state_0x10.sqlite", "state_５.sqlite"} {
				os.WriteFile(filepath.Join(f.home, name), nil, 0o600)
			}
		}},
		{"home missing", msgNoHome, false, func(f fixture, _ string) { f.vars["CODEX_HOME"] = filepath.Join(f.root, "absent") }},
		{"threads table missing", msgReadDB, true, func(f fixture, newest string) { seed(f.t, newest, "CREATE TABLE other (id TEXT)") }},
		{"column missing", msgReadDB, true, func(f fixture, newest string) {
			seed(f.t, newest, "CREATE TABLE threads (id TEXT, cwd TEXT, archived)")
		}},
		{"malformed", msgReadDB, true, func(_ fixture, newest string) { os.WriteFile(newest, []byte("PRIVATE_TRANSCRIPT"), 0o600) }},
		{"directory", msgNotFile, true, func(_ fixture, newest string) { os.Mkdir(newest, 0o755) }},
		{"symlink", msgNotFile, true, func(f fixture, newest string) { os.Symlink(filepath.Join(f.home, "state_5.sqlite"), newest) }},
	} {
		f := newFixture(t)
		if c.older {
			f.db("5", nil)
		}
		if c.setup != nil {
			c.setup(f, filepath.Join(f.home, "state_10.sqlite"))
		}
		f.refuses(c.name, f.cwd, c.want)
	}
}

// The database path reaches SQLite as a file: URI, so a home whose name is special in one must still open.
func TestResolveNativeSessionOpensAHomeWhoseNameIsSpecialInURIs(t *testing.T) {
	for _, name := range []string{"h %?# x", "h%41", "h?x", "h#x", "h%zz"} {
		f := newFixture(t)
		special := filepath.Join(f.root, name)
		if err := os.Mkdir(special, 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(special, "state_5.sqlite")
		if err := os.Rename(f.db("5", nil), want); err != nil { // seed would read the name as a DSN
			t.Fatal(err)
		}
		f.vars["CODEX_HOME"] = special
		if got := f.accepts(name, f.cwd); got.DBPath != want {
			t.Errorf("%s: %+v, want %s", name, got, want)
		}
	}
}

// A writer holding the lock decides as it does for node:sqlite readOnly: true (busy timeout 0): a
// rollback-journal database is unreadable at once, a WAL database still serves its committed rows.
func TestResolveNativeSessionUnderAWriterLock(t *testing.T) {
	for mode, wantRefused := range map[string]bool{"delete": true, "wal": false} {
		f := newFixture(t)
		path := filepath.Join(f.home, "state_5.sqlite")
		writer, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		writer.SetMaxOpenConns(1)
		for _, statement := range []string{"PRAGMA journal_mode=" + mode, "CREATE TABLE threads (" + columns + ")", "INSERT INTO threads VALUES ('" + child + "', '" + f.cwd + "', 0, 'vscode', 'x')", "BEGIN EXCLUSIVE", "INSERT INTO threads VALUES ('other', '/', 0, 'cli', 'y')"} {
			if _, err := writer.Exec(statement); err != nil {
				t.Fatal(mode, statement, err)
			}
		}
		start := time.Now()
		got, err := f.resolve(f.cwd)
		if refused := err != nil; refused != wantRefused || time.Since(start) > 2*time.Second || refused && err.Error() != msgReadDB {
			t.Errorf("%s: %+v, %v after %v", mode, got, err, time.Since(start))
		}
		writer.Close()
	}
}

// Reading a WAL database read-only still lets SQLite create the -wal and -shm beside it, as the
// oracle's readOnly does (a known defect): the database file itself is untouched.
func TestResolveNativeSessionOfAWALDatabaseLeavesSidecars(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.home, "state_5.sqlite")
	seed(t, path, "PRAGMA journal_mode=WAL")
	seed(t, path, "CREATE TABLE threads ("+columns+")")
	seed(t, path, "INSERT INTO threads VALUES (?, ?, 0, 'vscode', 'x')", child, f.cwd)
	before, _ := os.ReadFile(path)
	if names := entries(t, f.home); !slices.Equal(names, []string{"state_5.sqlite"}) {
		t.Fatalf("a closed WAL database has sidecars: %v", names)
	}
	f.accepts("WAL database", f.cwd)
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the database file changed")
	}
	if names := entries(t, f.home); !slices.Equal(names, []string{"state_5.sqlite", "state_5.sqlite-shm", "state_5.sqlite-wal"}) {
		t.Errorf("entries after the read: %v", names)
	}
}

// The call cwd may be relative, as may CODEX_HOME and an empty HOME that falls back to .codex: all
// resolve against getcwd, as process.cwd() does, and not against $PWD.
func TestResolveNativeSessionResolvesRelativePathsAgainstTheWorkingDirectory(t *testing.T) {
	f := newFixture(t)
	dbPath := f.db("5", nil)
	t.Chdir(f.cwd)
	if got := f.accepts("dot", "."); got.Cwd != f.cwd {
		t.Errorf("%+v", got)
	}
	cwdAlias := filepath.Join(f.root, "cwdalias")
	if err := os.Symlink(f.cwd, cwdAlias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", cwdAlias) // spelled through a link while the process is in f.cwd
	if got := f.accepts("dot with PWD spelled through a link to it", "."); got.Cwd != f.cwd {
		t.Errorf("%+v, want %s", got, f.cwd)
	}
	t.Chdir(f.root)
	if got := f.accepts("relative", "work"); got.Cwd != f.cwd {
		t.Errorf("%+v", got)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(f.root, alias); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PWD", alias) // spelled through a symlink while the process is in f.root
	f.vars["CODEX_HOME"] = "native"
	if got := f.accepts("relative home", f.cwd); got.DBPath != dbPath {
		t.Errorf("%+v, want %s", got, dbPath)
	}
	home := newFixture(t) // an empty HOME makes ~/.codex a path relative to the working directory
	home.home = filepath.Join(home.root, ".codex")
	if err := os.Mkdir(home.home, 0o755); err != nil {
		t.Fatal(err)
	}
	want := home.db("5", nil)
	delete(home.vars, "CODEX_HOME")
	home.vars["HOME"] = ""
	t.Chdir(home.root)
	if got := home.accepts("empty HOME", home.cwd); got.DBPath != want {
		t.Errorf("%+v, want %s", got, want)
	}
}

// realpath(3) stops after 40 links in one path (glibc), counted over the whole path; the standard
// library follows 255. A file followed by a separator is not a directory.
func TestResolveNativeSessionFollowsAtMostFortyLinks(t *testing.T) {
	chain := func(f fixture, prefix string, from string, n int) string {
		target := from
		for i := 1; i <= n; i++ {
			link := filepath.Join(f.root, prefix+strconv.Itoa(i))
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			target = link
		}
		return target
	}
	for n, accepted := range map[int]bool{40: true, 41: false} {
		f := newFixture(t)
		end := chain(f, "l", f.cwd, n)
		f.db("5", map[string]any{"cwd": end})
		if accepted {
			f.accepts("stored cwd", f.cwd)
			f.db("6", nil)
			f.accepts("call cwd", end)
		} else {
			f.refuses("stored cwd", f.cwd, msgCwd)
			f.db("6", nil)
			f.refuses("call cwd", end, msgWorkdir)
		}
	}
	rel := newFixture(t) // a relative link target is read from the directory that holds the link
	rel.db("5", nil)
	if err := os.Mkdir(filepath.Join(rel.root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../work", filepath.Join(rel.root, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	rel.accepts("relative link", filepath.Join(rel.root, "sub", "link"))
	f := newFixture(t) // 25 links on the way to the directory and 25 inside it: each alone is fine, together they are not
	f.db("5", nil)
	through := chain(f, "a", f.root, 25)
	chain(f, "b", f.cwd, 25)
	f.accepts("25 links", through+"/work")
	f.accepts("25 links more", filepath.Join(f.root, "b25"))
	f.refuses("50 links", through+"/b25", msgWorkdir)
	file := filepath.Join(f.root, "afile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for cwd, want := range map[string]string{file: msgMismatch, file + "/": msgCwd, file + "/.": msgCwd} {
		g := newFixture(t)
		g.db("5", map[string]any{"cwd": cwd})
		g.refuses("stored cwd "+cwd, g.cwd, want)
	}
}

// Node holds argv, the environment, getcwd, realpath answers and TEXT as strings decoded from UTF-8
// with U+FFFD for each invalid sequence, so the oracle names a path with invalid bytes only by its
// replacement spelling, whichever way the path reached it (a known defect, kept).
func TestResolveNativeSessionSeesPathsAsNodeDoes(t *testing.T) {
	for _, raw := range [][]byte{{0x80}, {0xe2, 0x82}} {
		replacement := decodeUTF8(raw)
		mkdir := func(path string) {
			t.Helper()
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Skipf("this file system refuses a name that is not UTF-8: %v", err)
			}
		}
		f := newFixture(t) // a stored cwd
		stored := f.root + "/s-" + string(raw)
		mkdir(stored)
		f.db("5", map[string]any{"cwd": stored})
		f.refuses("stored cwd: only the stored bytes exist", f.cwd, msgCwd)
		mkdir(f.root + "/s-" + replacement)
		f.accepts("stored cwd: the replacement spelling exists", f.root+"/s-"+replacement)
		f.refuses("stored cwd: it is not the working directory", f.cwd, msgMismatch)

		g := newFixture(t) // a link to a directory named with invalid bytes, as realpath answers
		mkdir(g.root + "/a-" + string(raw))
		if err := os.Symlink(g.root+"/a-"+string(raw), g.root+"/ln"); err != nil {
			t.Fatal(err)
		}
		g.db("5", map[string]any{"cwd": g.root + "/ln"})
		g.refuses("link: the answer is the replacement spelling, which is absent", g.root+"/ln", msgWorkdir)
		g.refuses("link: the stored cwd resolves through the raw name", g.cwd, msgMismatch)
		mkdir(g.root + "/a-" + replacement)
		g.accepts("link: the replacement spelling exists", g.root+"/a-"+replacement)

		h := newFixture(t) // a working directory named with invalid bytes
		wd := h.root + "/w-" + string(raw)
		mkdir(wd)
		h.db("5", nil)
		t.Chdir(wd)
		h.refuses("getcwd: dot answers with the absent replacement spelling", ".", msgWorkdir)
		mkdir(h.root + "/w-" + replacement)
		h.db("6", map[string]any{"cwd": h.root + "/w-" + replacement})
		h.accepts("getcwd: the replacement spelling is the stored cwd", ".")

		rel := newFixture(t) // a relative call cwd is looked up under the raw working directory, not its replacement spelling
		rawWD, repWD := rel.root+"/x-"+string(raw), rel.root+"/x-"+replacement
		mkdir(rawWD)
		mkdir(repWD)
		os.Mkdir(repWD+"/sub", 0o755)
		rel.db("5", map[string]any{"cwd": repWD + "/sub"})
		t.Chdir(rawWD)
		rel.refuses("relative call cwd: sub exists only under the replacement spelling", "sub", msgWorkdir)

		r := newFixture(t) // a relative CODEX_HOME resolves against the decoded getcwd answer
		os.Mkdir(r.root+"/native2", 0o755)
		t.Chdir(wd)
		r.vars["CODEX_HOME"] = "native2"
		os.Mkdir(wd+"/native2", 0o755)
		os.Rename(r.dbIn(r.root+"/native2"), wd+"/native2/state_5.sqlite")
		r.refuses("relative CODEX_HOME: the decoded working directory is absent", r.cwd, msgNoHome)
		os.Mkdir(h.root+"/w-"+replacement+"/native2", 0o755)
		r.home = h.root + "/w-" + replacement + "/native2"
		r.db("5", map[string]any{"archived": 1})
		r.refuses("relative CODEX_HOME: the replacement spelling holds another database", r.cwd, msgArchived)

		c := newFixture(t) // a call cwd given as raw bytes names its replacement spelling, whatever the raw name links to
		rep := c.root + "/c-" + replacement
		mkdir(rep)
		if err := os.Symlink(c.cwd, c.root+"/c-"+string(raw)); err != nil {
			t.Fatal(err)
		}
		c.db("5", map[string]any{"cwd": rep})
		c.accepts("call cwd: the replacement spelling is the stored cwd", c.root+"/c-"+string(raw))

		e := newFixture(t) // CODEX_HOME given as raw bytes
		home := e.root + "/h-" + string(raw)
		mkdir(home)
		os.Rename(e.db("5", nil), home+"/state_5.sqlite")
		e.vars["CODEX_HOME"] = home
		e.refuses("CODEX_HOME: only the raw bytes exist", e.cwd, msgNoHome)
		mkdir(e.root + "/h-" + replacement)
		e.home = e.root + "/h-" + replacement
		e.db("5", map[string]any{"archived": 1})
		e.refuses("CODEX_HOME: the replacement spelling holds another database", e.cwd, msgArchived)
	}
}
