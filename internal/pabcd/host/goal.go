package host

import (
	"os"
	"path/filepath"
)

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

// GoalsDBPath is resolveGoalsDbPath: Join cleans a ".." lexically, as path.join does (a known defect, kept).
func GoalsDBPath(env LookupEnv) (string, error) {
	dir, err := CodexSQLiteHome(env)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, GoalsDBFilename), nil
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
