//go:build darwin

package migrate

import "golang.org/x/sys/unix"

// noReplaceSupported says this platform has a rename that refuses to replace.
const noReplaceSupported = true

// noReplaceRename renames oldName to newName inside the directory dirfd names (renameatx_np with RENAME_EXCL): anything already at
// newName, a link included, answers EEXIST and stays. ENOTSUP or EINVAL mean the volume lacks the flag.
func noReplaceRename(dirfd int, oldName, newName string) error {
	return unix.RenameatxNp(dirfd, oldName, dirfd, newName, unix.RENAME_EXCL)
}
