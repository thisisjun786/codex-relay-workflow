package host

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const goalsSchema = "CREATE TABLE thread_goals (thread_id TEXT PRIMARY KEY NOT NULL, goal_id TEXT NOT NULL, objective TEXT NOT NULL, status TEXT NOT NULL)"

func goalsDB(t *testing.T, rows map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), GoalsDBFilename)
	seed(t, path, goalsSchema)
	for thread, status := range rows {
		seed(t, path, "INSERT INTO thread_goals VALUES (?, 'g-'||?, 'obj', ?)", thread, thread, status)
	}
	return path
}

// goal-active.test.ts "resolveGoalsDbPath honors CODEX_SQLITE_HOME > CODEX_HOME"
func TestGoalsDBPathHonorsSQLiteHomeOverCodexHome(t *testing.T) {
	for env, want := range map[string]string{"CODEX_SQLITE_HOME": "/sq/goals_1.sqlite", "CODEX_HOME": "/ch/goals_1.sqlite"} {
		if got, err := GoalsDBPath(envOf(map[string]string{env: want[:3]})); err != nil || got != want {
			t.Errorf("%s: %q, %v", env, got, err)
		}
	}
	if got, _ := GoalsDBPath(envOf(map[string]string{"CODEX_SQLITE_HOME": "/sq", "CODEX_HOME": "/ch"})); got != "/sq/goals_1.sqlite" {
		t.Errorf("both set: %q", got)
	}
}

// "missing DB -> inactive (codex not using goals)" and "Active row -> active; Complete/absent -> inactive"
func TestGoalActiveStatusRows(t *testing.T) {
	path := goalsDB(t, map[string]string{"sess-active": "active", "sess-complete": "complete", "sess-paused": "paused", "sess-upper": "ACTIVE"})
	for thread, want := range map[string]GoalStatus{"sess-active": GoalActive, "sess-complete": GoalInactive, "sess-paused": GoalInactive, "sess-upper": GoalInactive, "sess-missing": GoalInactive, "": GoalInactive} {
		if got := GoalActiveStatus(thread, path); got != want {
			t.Errorf("%q: %s, want %s", thread, got, want)
		}
	}
	if got := GoalActiveStatus("t1", filepath.Join(t.TempDir(), "nope.sqlite")); got != GoalInactive {
		t.Errorf("a missing database is %s", got)
	}
}

// "present-but-unparsable row -> unreadable (fail closed)": no thread_goals table, a status that is
// not text, a file that is not a database, a directory.
func TestGoalActiveStatusUnreadableFailsClosed(t *testing.T) {
	typed := func(value string) string {
		path := filepath.Join(t.TempDir(), GoalsDBFilename)
		seed(t, path, "CREATE TABLE thread_goals (thread_id TEXT, status)")
		seed(t, path, "INSERT INTO thread_goals VALUES ('t', "+value+")")
		return path
	}
	noTable, garbage := filepath.Join(t.TempDir(), GoalsDBFilename), filepath.Join(t.TempDir(), GoalsDBFilename)
	seed(t, noTable, "CREATE TABLE other (x INTEGER)")
	if err := os.WriteFile(garbage, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"no table": noTable, "integer": typed("7"), "null": typed("NULL"), "blob": typed("x'6163'"), "not a database": garbage, "a directory": t.TempDir()} {
		if got := GoalActiveStatus("t", path); got != GoalUnreadable {
			t.Errorf("%s: %s", name, got)
		}
	}
}

// A writer holding the lock decides as it does for node:sqlite readOnly: true (busy timeout 0): a
// rollback-journal database is unreadable at once, a WAL database still serves its committed rows.
func TestGoalActiveStatusUnderAWriterLock(t *testing.T) {
	for mode, want := range map[string]GoalStatus{"delete": GoalUnreadable, "wal": GoalActive} {
		path := filepath.Join(t.TempDir(), GoalsDBFilename)
		writer, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		writer.SetMaxOpenConns(1)
		for _, statement := range []string{"PRAGMA journal_mode=" + mode, goalsSchema, "INSERT INTO thread_goals VALUES ('t', 'g', 'o', 'active')", "BEGIN EXCLUSIVE", "INSERT INTO thread_goals VALUES ('u', 'g', 'o', 'paused')"} {
			if _, err := writer.Exec(statement); err != nil {
				t.Fatal(mode, statement, err)
			}
		}
		start := time.Now()
		if got := GoalActiveStatus("t", path); got != want || time.Since(start) > 2*time.Second {
			t.Errorf("%s: %s after %v, want %s at once", mode, got, time.Since(start), want)
		}
		writer.Close()
	}
}

// The path reaches SQLite as written: a ".." after a symlink is resolved by the OS, as node:sqlite
// does, and not cleaned away first (which would open the other database).
func TestGoalsDatabaseKeepsDotDotAfterASymlink(t *testing.T) {
	root := t.TempDir()
	for dir, status := range map[string]string{root: "paused", filepath.Join(root, "real"): "active"} {
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		seed(t, filepath.Join(dir, GoalsDBFilename), goalsSchema)
		seed(t, filepath.Join(dir, GoalsDBFilename), "INSERT INTO thread_goals VALUES ('t', 'g', 'o', ?)", status)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "real", "sub"), alias); err != nil {
		t.Fatal(err)
	}
	if got := GoalActiveStatus("t", alias+"/../"+GoalsDBFilename); got != GoalActive { // real/, not root
		t.Errorf("the lexically cleaned path was opened: %s", got)
	}
}

// "suppressesInterview is true for active AND unreadable (fail-closed), false for inactive"
func TestSuppressesInterview(t *testing.T) {
	for status, want := range map[GoalStatus]bool{GoalActive: true, GoalUnreadable: true, GoalInactive: false} {
		if got := SuppressesInterview(status); got != want {
			t.Errorf("%s: %v", status, got)
		}
	}
}

// The open is read-only: the file is byte-identical afterwards and a write through the same kind of
// open is refused.
func TestGoalsDatabaseIsOpenedReadOnly(t *testing.T) {
	path := goalsDB(t, map[string]string{"t": "active"})
	before, _ := os.ReadFile(path)
	if GoalActiveStatus("t", path) != GoalActive {
		t.Fatal("not active")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Error("the database changed")
	}
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM thread_goals"); err == nil {
		t.Error("a write through a read-only open succeeded")
	}
}
