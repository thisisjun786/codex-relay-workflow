package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/projectlock"
)

// ErrBusy is the answer of a start whose request another caller is advancing: nothing was changed and the same request can be tried again. It is a value so a caller
// that runs the same request concurrently (a scheduler waking twice) can tell it from a failure; its text is the one the lock has always answered with.
var ErrBusy = errors.New("this managed request is already being advanced")

// Lock acquires managed.py:311-333's nonblocking per-request flock. The sidecar is
// never removed: replacing a lock inode would allow two independent holders.
func Lock(storePath, requestID string) (func() error, error) {
	digest := sha256.Sum256([]byte(requestID))
	path := filepath.Join(filepath.Dir(storePath), "managed-start-"+hex.EncodeToString(digest[:])+".lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (func() error, error) { _ = unix.Close(fd); return nil, err }
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		return closeOnError(err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
		return closeOnError(fmt.Errorf("managed request lock is not an owned regular file"))
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
			return closeOnError(ErrBusy)
		}
		return closeOnError(err)
	}
	return func() error {
		err := unix.Flock(fd, unix.LOCK_UN)
		return joinClose(err, unix.Close(fd))
	}, nil
}
func joinClose(first, second error) error {
	if first != nil {
		return first
	}
	return second
}

// LockProject is the project's lock held shared by a managed start (internal/relay/store/projectlock): from
// before its last project-scope ask until its child is registered. Every writer of the project's parent
// binding takes the same file exclusively, so a writer waits for every start in that span and a start waits
// for a writer before it asks about the binding; starts do not hold each other up, and nothing of another
// project is held up by it. The wait is bounded at 30 s: past it the retryable LockWaitExpired host error
// answers, nothing has changed, and a retry with the same request id continues the request.
func LockProject(ctx context.Context, storePath, project string) (func() error, error) {
	return projectlock.Shared(ctx, storePath, project)
}
