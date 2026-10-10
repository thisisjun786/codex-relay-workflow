package host

import "os"

// GoalsDBFilename is GOALS_DB_FILENAME (codex-rs/state/src/lib.rs:82). The goal table, thread_goals,
// is keyed by thread_id, which is the hook payload's session_id.
const GoalsDBFilename = "goals_1.sqlite"

// GoalStatus is whether the host's goal mode owns a thread.
type GoalStatus string

const (
	GoalActive     GoalStatus = "active"     // the thread has a goal row whose status is 'active'
	GoalInactive   GoalStatus = "inactive"   // no database, no row, or any other status
	GoalUnreadable GoalStatus = "unreadable" // the database is there but cannot answer: a caller fails closed
)

// GoalsDBPath is resolveGoalsDbPath under CodexSQLiteRoot, the root the native session database is
// read under: the file name is joined without cleaning, so a root ending in <symlink>/.. names the
// goals database of the directory the kernel resolves it to. The oracle's path.join removed the ".."
// lexically and read another directory's goals (CRW-1136 fixed that). An error names the variable to
// set, and the caller decides (a hook reads it as unreadable and fails closed).
func GoalsDBPath(env LookupEnv) (string, error) { return goalsDBPath(env, accountHome) }

func goalsDBPath(env LookupEnv, account func() (string, error)) (string, error) {
	root, err := codexSQLiteRoot(env, account)
	if err != nil {
		return "", err
	}
	return root.Join(GoalsDBFilename), nil
}

// GoalActiveStatus looks up threadID in the goals database at dbPath, read-only
// (getGoalActiveStatus). A missing database, or any error while checking that it exists, means
// Codex is not using goals.
func GoalActiveStatus(threadID, dbPath string) GoalStatus {
	if threadID == "" {
		return GoalInactive
	}
	if _, err := os.Stat(dbPath); err != nil {
		return GoalInactive
	}
	db, err := openReadOnly(dbPath)
	if err != nil {
		return GoalUnreadable
	}
	defer db.Close()
	rows, err := db.Query(GoalStatusQuery, threadID)
	if err != nil {
		return GoalUnreadable
	}
	defer rows.Close()
	if !rows.Next() {
		if rows.Err() != nil { // a lock, or a malformed file
			return GoalUnreadable
		}
		return GoalInactive
	}
	names, err := rows.Columns()
	var status any
	if err != nil || rows.Scan(&status) != nil {
		return GoalUnreadable
	}
	// JavaScript reads row.status, and SQLite keeps the column name the table declares, so a column
	// declared STATUS or Status leaves the property undefined: unreadable, whatever the row holds.
	text, isText := status.(string)
	switch {
	case len(names) != 1 || names[0] != "status" || !isText: // schema drift, or a status that is not text
		return GoalUnreadable
	case text == "active":
		return GoalActive
	}
	return GoalInactive
}

// SuppressesInterview is true for an active goal and for an unreadable database (fail closed).
func SuppressesInterview(status GoalStatus) bool {
	return status == GoalActive || status == GoalUnreadable
}
