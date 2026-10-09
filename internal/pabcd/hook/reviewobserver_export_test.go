package hook

// SetReviewObserverSessionLock replaces the review observer's session-lock entry for one test and returns the restore. It exists
// so the session-lock interleaving test can learn when the observer is blocked on a held lock and can fix the retry budget itself
// (CRW-564); the production entry is state.WithSessionLock.
func SetReviewObserverSessionLock(lock func(cwd, sessionID string, fn func() error) error) (restore func()) {
	previous := reviewObserverSessionLock
	reviewObserverSessionLock = lock
	return func() { reviewObserverSessionLock = previous }
}
