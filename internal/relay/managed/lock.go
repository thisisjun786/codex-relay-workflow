package managed

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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
