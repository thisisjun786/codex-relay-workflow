package host

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // the pure-Go driver the repository already uses: no CGO
)

// GoalStatusQuery is the statement run against the host's goals database (goal-active.ts:70,
// coverage item K11/sql-text), fixed as the oracle writes it. The table is Codex's
// (codex-rs/state): a statement that fails on it makes the database unreadable, it is never adapted.
const GoalStatusQuery = "SELECT status FROM thread_goals WHERE thread_id = ?"

// ThreadQuery is the statement run against the newest state_<N>.sqlite (session-binding.ts:81,
// coverage item K11/sql-text), fixed as the oracle writes it. The table is Codex's, so a statement
// that fails on it makes the database unreadable and is never adapted.
const ThreadQuery = "SELECT id, cwd, archived, source FROM threads WHERE id = ?"

// openReadOnly opens an existing database read-only (SQLITE_OPEN_READONLY, node:sqlite's readOnly:
// true): it never creates the file, no statement run through it can write, and the busy timeout is
// 0 as node:sqlite's is. database/sql opens lazily, so a failure shows at the first query. The path
// reaches SQLite as written: a relative one only gains the working directory, because cleaning it
// (filepath.Abs does) would drop a ".." that the OS resolves after a symlink.
func openReadOnly(path string) (*sql.DB, error) {
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		path = wd + string(filepath.Separator) + path
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
