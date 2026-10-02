package host

import (
	"database/sql"
	"errors"
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

// GoalsDBPath is resolveGoalsDbPath.
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
	var status any
	err = db.QueryRow(GoalStatusQuery, threadID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return GoalInactive
	}
	text, isText := status.(string)
	switch {
	case err != nil || !isText: // schema drift, a lock, or a status that is not text
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
