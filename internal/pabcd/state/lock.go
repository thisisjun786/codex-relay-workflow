package state

import "time"

// WithSessionLock runs fn holding the session's exclusive lock.
func WithSessionLock(cwd, sessionID string, fn func() error) error { return unimplemented() }

func withSessionLock(cwd, sessionID string, fn func() error, sleep func(time.Duration)) error {
	return unimplemented()
}
