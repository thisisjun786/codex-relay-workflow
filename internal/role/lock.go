package role

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// lockStore takes the store's advisory lock and returns what releases it. The lock is the file <store>.lock, created exclusively and
// holding the pid, so two writers cannot hold it at once: SetRole and ResetRole read the store, change it in memory and publish it whole,
// and without the lock the writer that publishes last discards the update of the one that published first (the oracle has no lock;
// known-defects). A lock held by another writer is retried on the schedule of the session state lock (state.WithSessionLock: ten
// retries, 5 to 40 ms) and then refused, with the error of the last create (errors.Is(err, fs.ErrExist)).
//
// There is deliberately no stale-lock breaker, as in the state lock: a lock left by a dead process refuses every later write until
// someone removes the file by hand, which is visible and recoverable, where two writers that both judge a lock stale can enter
// together. The release removes the file by path and ignores the error, so it is attempted on every path, a panic included.
func lockStore(path string, sleep func(time.Duration)) (release func(), err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, refused(err)
	}
	lock := path + ".lock"
	delays := [...]time.Duration{5, 10, 15, 20, 25, 30, 35, 40, 35, 35} // milliseconds, the state lock's LOCK_RETRY_DELAYS_MS
	for attempt := 0; ; attempt++ {
		f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // an existing symbolic link is refused, never followed
		if err == nil {
			release = func() { _ = os.Remove(lock) }
			if err = writePid(f); err != nil {
				release()
				return nil, refused(err)
			}
			return release, nil
		}
		if !errors.Is(err, fs.ErrExist) || attempt >= len(delays) {
			return nil, refused(err)
		}
		sleep(delays[attempt] * time.Millisecond)
	}
}

// writePid writes the pid into the lock file and closes it, so the file is closed before any release.
func writePid(f *os.File) error {
	_, err := f.WriteString(strconv.Itoa(os.Getpid()))
	return errors.Join(err, f.Close())
}

// refused is how a write that cannot start reads, as the oracle's refusal of a store it cannot read does.
func refused(err error) error { return fmt.Errorf("cannot update subagent config: %w", err) }
