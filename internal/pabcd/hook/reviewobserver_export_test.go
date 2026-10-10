package hook

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"

// SetReviewObserverSessionLock replaces the review observer's session-lock entry for one test and returns the restore. It exists
// so the session-lock interleaving test can learn when the observer is blocked on a held lock and can fix the retry budget itself
// (CRW-564); the production entry is state.WithSessionLock.
func SetReviewObserverSessionLock(lock func(cwd, sessionID string, fn func() error) error) (restore func()) {
	previous := reviewObserverSessionLock
	reviewObserverSessionLock = lock
	return func() { reviewObserverSessionLock = previous }
}

// SetReviewObserverWriteGoalplan replaces the observer's plan write for one test and returns the restore. It exists so a test can
// fail a write before or after the plan is published (CRW-1113); the production write is goalplan.WriteGoalplan.
func SetReviewObserverWriteGoalplan(write func(cwd string, plan *goalplan.Goalplan) error) (restore func()) {
	previous := reviewObserverWriteGoalplan
	reviewObserverWriteGoalplan = write
	return func() { reviewObserverWriteGoalplan = previous }
}

// SetReviewObserverInboxCounted replaces the hook between the inbox's capacity check and the link of an entry for one test and
// returns the restore (CRW-1113): it lets a test hold two invocations at the moment both have passed the check.
func SetReviewObserverInboxCounted(f func()) (restore func()) {
	previous := reviewObserverInboxCounted
	reviewObserverInboxCounted = f
	return func() { reviewObserverInboxCounted = previous }
}
