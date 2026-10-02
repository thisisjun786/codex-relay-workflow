package host

import (
	"database/sql"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite" // the pure-Go driver the repository already uses: no CGO
)

// GoalStatusQuery is the statement run against the host's goals database (goal-active.ts:70,
// coverage item K11/sql-text), fixed as the oracle writes it. The table is Codex's
// (codex-rs/state): a statement that fails on it makes the database unreadable, it is never adapted.
const GoalStatusQuery = "SELECT status FROM thread_goals WHERE thread_id = ?"

// openReadOnly opens an existing database read-only (SQLITE_OPEN_READONLY, node:sqlite's readOnly:
// true): it never creates the file, no statement run through it can write, and the busy timeout is
// 0 as node:sqlite's is. database/sql opens lazily, so a failure shows at the first query.
func openReadOnly(path string) (*sql.DB, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
