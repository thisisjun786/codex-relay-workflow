//go:build darwin

package state

import (
	"os"

	"golang.org/x/sys/unix"
)

// renameNoReplace renames oldpath to newpath only if nothing is at newpath (renamex_np with RENAME_EXCL): an existing file,
// directory or symlink answers EEXIST and stays as it is. A volume without the flag answers ENOTSUP or EINVAL. It belongs beside
// crwdir.Rename and lives here until another package needs it.
func renameNoReplace(oldpath, newpath string) error {
	if err := unix.RenameatxNp(unix.AT_FDCWD, oldpath, unix.AT_FDCWD, newpath, unix.RENAME_EXCL); err != nil {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: err}
	}
	return nil
}
