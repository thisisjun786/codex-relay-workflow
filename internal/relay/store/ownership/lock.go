package ownership

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// LockWait is the one bound on a fence lock wait that another writer can hold for an
// unbounded time (Python ownership.LOCK_WAIT_SECONDS, cutover.md Lock order): the socket
// binding's write-gate EX, which every admitted connection's lifetime SH excludes. It is
// the store's SQLite busy timeout and Go's default OpenOptions.BusyTimeout.
var LockWait = 30 * time.Second

// LockWaitExpired is Python's LockWaitExpired: a bounded fence lock wait ran out. It is a
// retryable host error that changed nothing, and its text is the host detail Python prints.
type LockWaitExpired struct {
	What  string
	Bound time.Duration
}

func (e *LockWaitExpired) Error() string {
	// Python formats the bound with {bound:g}: 30.0 is "30", 0.3 is "0.3".
	return "LockWaitExpired: " + e.What + " was not acquired within " + strconv.FormatFloat(e.Bound.Seconds(), 'g', -1, 64) + "s; retry"
}

// LockWithin takes the existing lock at path, waiting at most LockWait for a holder to
// release it (Python ownership.flock_within); a lock that cannot be opened or is not
// trusted fails at once, as Lock does.
func LockWithin(ctx context.Context, path string, exclusive bool, what string) (*os.File, error) {
	bound := LockWait
	deadline := time.Now().Add(bound)
	for {
		f, err := Lock(path, exclusive, false)
		if err == nil || !errors.Is(err, unix.EWOULDBLOCK) {
			return f, err
		}
		if !time.Now().Before(deadline) {
			return nil, &LockWaitExpired{What: what, Bound: bound}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Lock always uses the existing rendezvous inode. Contention bounded-fails
// immediately; callers can explicitly resume, never bypass a holder on age/PID.
func Lock(path string, exclusive, create bool) (*os.File, error) {
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	if err = lockFileSafe(fd, path); err != nil {
		err = errors.Join(refuse("unsafe lock file %s", path), err)
	}
	if err == nil {
		kind := unix.LOCK_SH
		if exclusive {
			kind = unix.LOCK_EX
		}
		err = unix.Flock(fd, kind|unix.LOCK_NB)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// lockFileSafe trusts a lock inode that is a regular file owned by this user and
// either grants no group or other access (the 0600 mode Go creates, trusted in any
// directory as Python and every earlier Go build trust it) or sits in an owner-only
// directory: a lock another runtime created under umask 002 is 0664 inside a 0700 S,
// and no other user can reach it there (decision D3). flock does not depend on the
// file mode, and a lock is never chmodded, replaced or unlinked (cutover.md: every
// lock file keeps its inode). A lock Lock or serviceLock creates is 0600, so neither
// ever leaves behind a lock file that it then refuses.
func lockFileSafe(fd int, path string) error {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	euid := uint32(os.Geteuid())
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Uid != euid {
		return refuse("lock is not a regular file owned by this user")
	}
	if info.Mode&0077 == 0 {
		return nil
	}
	var dir unix.Stat_t
	if err := unix.Stat(LexicalDir(path), &dir); err != nil {
		return err
	}
	if dir.Mode&unix.S_IFMT != unix.S_IFDIR || dir.Uid != euid || dir.Mode&0022 != 0 {
		return refuse("lock grants group or other access and its directory is not owned by this user or is group/world writable")
	}
	return nil
}

// LexicalDir is the directory the kernel resolved when it opened path: everything
// before the last slash, never cleaned, so a '..' after a symlink names the same
// directory in the stat as in the open (filepath.Dir would clean it lexically).
func LexicalDir(path string) string {
	switch i := strings.LastIndex(path, "/"); {
	case i < 0:
		return "."
	case i == 0:
		return "/"
	default:
		return path[:i]
	}
}
