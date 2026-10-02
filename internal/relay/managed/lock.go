package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

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
			return closeOnError(fmt.Errorf("this managed request is already being advanced"))
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

// projectLockPath is the project's lock file: a sidecar beside the store, named by the project key's hash
// and apart from every request lock (managed-start-<hash>.lock).
func projectLockPath(storePath, project string) string {
	digest := sha256.Sum256([]byte(project))
	return filepath.Join(filepath.Dir(storePath), "managed-start-project-"+hex.EncodeToString(digest[:])+".lock")
}

// LockProject takes the project's lock shared, for the span from a managed start's last project-scope
// ask to its CreateThread. The lock is the protocol that makes the span one step against anyone who
// changes the project's parent binding: such a writer takes the same file exclusively, so it waits for
// every start inside the span, and a start waits for it before it asks about the binding at all. Starts
// share the lock because they do not disturb each other's scope; they are never held up by one another,
// and a start of another project, which uses another file, is never held up by anything here.
//
// The wait is bounded as every fence lock's is (ownership.LockWait, 30 s): a holder that never lets go
// ends the wait with the retryable LockWaitExpired host error after nothing has changed, the request
// stays armed and a retry with the same request id continues it. A cancelled ctx ends it at once.
//
// Nothing but managed-start takes this lock yet. The registry's writers of a project's parent binding
// (BindScopeAs, Handover) run outside this package and still change a binding without waiting, so a
// binding removed by one of them between the last ask and the host's thread/start is not prevented; the
// thread is then refused by registration as before, and a retry with the same request id continues it.
//
// The sidecar is created once and never removed or replaced, as the request lock's is, so two holders
// can never lock different inodes.
func LockProject(ctx context.Context, storePath, project string) (func() error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := projectLockPath(storePath, project)
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Close(fd); err != nil {
		return nil, err
	}
	held, err := ownership.LockWithin(ctx, path, false, "the managed-start project lock")
	if err != nil {
		return nil, err
	}
	return held.Close, nil
}
