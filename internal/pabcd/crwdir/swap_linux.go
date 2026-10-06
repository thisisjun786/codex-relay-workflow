//go:build linux

package crwdir

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// crwdirSwapExchange exchanges the files at a and b atomically (renameat2 with RENAME_EXCHANGE):
// afterwards a holds what b held and b holds what a held, with no moment at which either path is
// missing. A kernel or filesystem without the flag answers EINVAL, ENOSYS or ENOTSUP.
func crwdirSwapExchange(a, b string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE); err != nil {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: err}
	}
	return nil
}

// crwdirSwapExchangeUnsupported reports whether err says this platform cannot exchange two files:
// the kernel lacks the syscall (ENOSYS) or the filesystem lacks the flag (EINVAL, ENOTSUP,
// EOPNOTSUPP). EPERM is deliberately not in the set: a permission failure at the exchange is a real
// refusal of this publication, not a filesystem without exchange, and reporting it as the latter
// would tell the operator to give up on a filesystem that works.
func crwdirSwapExchangeUnsupported(err error) bool {
	for _, errno := range [...]syscall.Errno{syscall.EINVAL, syscall.ENOSYS, syscall.ENOTSUP, syscall.EOPNOTSUPP} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// crwdirSwapNoReplace renames oldpath to newpath only if nothing is at newpath (renameat2 with
// RENAME_NOREPLACE): an existing file, directory or symlink answers EEXIST and stays as it is, so
// the backup never overwrites a file it did not create.
func crwdirSwapNoReplace(oldpath, newpath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
