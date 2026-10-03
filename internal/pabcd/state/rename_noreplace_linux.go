//go:build linux

package state

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames oldpath to newpath only if nothing is at newpath (renameat2 with RENAME_NOREPLACE): an existing file,
// directory or symlink, dangling or not, answers EEXIST and stays as it is. A kernel or filesystem without the flag answers
// EINVAL, ENOSYS or ENOTSUP. It belongs beside crwdir.Rename and lives here until another package needs it.
func renameNoReplace(oldpath, newpath string) error {
	if err := unix.Renameat2(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_NOREPLACE); err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
