package guidancerecord

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// lockName is the lock file beside a session's records. Every change to a resume mark is made holding it, together with the record a
// resume or a whole output writes: a prompt that counts against a pair (NoteUserPrompt), a compact that takes it (TakePair) and a start
// that ends it (Record, ClearResume) run one after the other across the hook processes, so a mark that was taken or ended is never
// written back by a prompt that read it before (CRW-1180). The name starts with a dot, so it is neither a leg nor a mark.
const lockName = ".lock"

// lockWait bounds how long a hook waits for another hook of the same session; a hook that cannot take the lock in time ends the pairs
// it would have changed instead, which only makes a later start say more.
const lockWait = 2 * time.Second

// lockSession takes the lock of the session whose records are in dir, creating dir first when create is set. It answers false when the
// directory is missing (no record, so no mark either) or the lock cannot be taken within lockWait.
func lockSession(dir string, create bool) (unlock func(), ok bool) {
	if create && os.MkdirAll(dir, 0o700) != nil {
		return nil, false
	}
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, false
	}
	for deadline := time.Now().Add(lockWait); ; time.Sleep(2 * time.Millisecond) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, true
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) || time.Now().After(deadline) {
			_ = f.Close()
			return nil, false
		}
	}
}
