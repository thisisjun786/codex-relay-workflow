//go:build linux

package migrate

import "golang.org/x/sys/unix"

// noReplaceSupported says this platform has a rename that refuses to replace.
const noReplaceSupported = true

// noReplaceRename renames oldName to newName inside the directory dirfd names (renameat2 with RENAME_NOREPLACE): anything already at
// newName, a link included, answers EEXIST and stays. EINVAL, ENOSYS or ENOTSUP mean the kernel or filesystem lacks the flag.
func noReplaceRename(dirfd int, oldName, newName string) error {
	return unix.Renameat2(dirfd, oldName, dirfd, newName, unix.RENAME_NOREPLACE)
}
