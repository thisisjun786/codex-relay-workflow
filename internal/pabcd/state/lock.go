package state

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"time"
)

// WithSessionLock runs fn holding the session's exclusive lock, the file <state file>.lock that holds the pid (withSessionLock).
// There is deliberately no stale-lock breaker: reading, renaming and unlinking by path is racy, and two processes that both
// judge a lock stale can enter together and lose a verdict. Acquisition instead gives up after about 250 ms and returns the
// error of the last create (errors.Is(err, fs.ErrExist)), so a lock left by a dead process costs a denied completion, visible
// and recoverable, and a stale .lock file is removed by hand. The lock is released by path, errors ignored, also when fn panics.
func WithSessionLock(cwd, sessionID string, fn func() error) error {
	return withSessionLock(cwd, sessionID, fn, time.Sleep)
}

func withSessionLock(cwd, sessionID string, fn func() error, sleep func(time.Duration)) error {
	if err := makeSessionsDir(cwd); err != nil {
		return err
	}
	lockPath := StatePath(cwd, sessionID) + ".lock"
	delays := [...]time.Duration{5, 10, 15, 20, 25, 30, 35, 40, 35, 35} // milliseconds: LOCK_RETRY_DELAYS_MS
	for attempt := 0; ; attempt++ {
		err := createExclusive(lockPath, strconv.Itoa(os.Getpid()))
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt >= len(delays) {
			return err
		}
		sleep(delays[attempt] * time.Millisecond)
	}
	defer func() { _ = removeFile(lockPath) }()
	return fn()
}
