//go:build darwin

package crwdir

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// crwdirSwapExchange exchanges the files at a and b atomically (renamex_np with RENAME_SWAP):
// afterwards a holds what b held and b holds what a held, with no moment at which either path is
// missing. A volume without the flag answers ENOTSUP or EINVAL.
func crwdirSwapExchange(a, b string) error {
	if err := unix.RenamexNp(a, b, unix.RENAME_SWAP); err != nil {
		return &os.LinkError{Op: "rename", Old: a, New: b, Err: err}
	}
	return nil
}

// crwdirSwapExchangeUnsupported reports whether err says this platform cannot exchange two files.
func crwdirSwapExchangeUnsupported(err error) bool {
	for _, errno := range [...]syscall.Errno{syscall.EINVAL, syscall.ENOSYS, syscall.ENOTSUP, syscall.EOPNOTSUPP, syscall.EPERM} {
		if errors.Is(err, errno) {
			return true
		}
	}
	return false
}

// crwdirSwapNoReplace renames oldpath to newpath only if nothing is at newpath (renameatx_np with
// RENAME_EXCL): an existing file, directory or symlink answers EEXIST and stays as it is, so the
// backup never overwrites a file it did not create.
func crwdirSwapNoReplace(oldpath, newpath string) error {
	if err := unix.RenameatxNp(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
