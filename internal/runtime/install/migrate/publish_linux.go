//go:build linux

package migrate

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// noReplaceSupported says this platform has a rename that refuses to replace.
const noReplaceSupported = true

// noReplaceRename renames oldName to newName inside the directory dirfd names (renameat2 with RENAME_NOREPLACE): anything already at
// newName, a link included, answers EEXIST and stays. EINVAL, ENOSYS or ENOTSUP mean the kernel or filesystem lacks the flag.
func noReplaceRename(dirfd int, oldName, newName string) error {
	return unix.Renameat2(dirfd, oldName, dirfd, newName, unix.RENAME_NOREPLACE)
}

// ownedDirIdentityHandleFlag opens a directory as a handle without permission on the directory itself:
// Linux O_PATH.
const ownedDirIdentityHandleFlag = 0x200000

// ownedDirIdentityHandleOK says this platform can pin a directory without read permission, so a mode is
// never given to a name a racer could have redirected.
const ownedDirIdentityHandleOK = true

// ownedDirIdentityFchmodat2 changes a directory's mode through its descriptor with fchmodat2 and
// AT_EMPTY_PATH. It is a variable so a case can model a kernel that does not have fchmodat2 (Linux
// before 6.5 answers ENOSYS, which the runtime call reports as EOPNOTSUPP); no other code replaces it.
var ownedDirIdentityFchmodat2 = func(fd int, perm uint32) error {
	return unix.Fchmodat(fd, "", perm, 0x1000)
}

// ownedDirIdentityFchmod gives the pinned directory exactly perm through its descriptor. fchmodat2 is
// used where the kernel has it (6.5 and later); any other error it answers is tried on the
// /proc/self/fd path, which is the same call a libc fchmod makes on a descriptor, so a kernel or
// filesystem without the flag is still served and its own failure is reported.
func ownedDirIdentityFchmod(fd int, perm uint32) error {
	if err := ownedDirIdentityFchmodat2(fd, perm); err == nil {
		return nil
	}
	return unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), perm)
}
