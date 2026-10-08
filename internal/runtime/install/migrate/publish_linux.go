//go:build linux

package migrate

import (
	"errors"
	"fmt"
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

// ownedDirIdentityFchmodUsesFchmodat2 says the mode call above consults ownedDirIdentityFchmodat2, so a
// case can replace that seam here and expect it to run.
const ownedDirIdentityFchmodUsesFchmodat2 = true

// migrateFollowupProcChmod is the /proc/self/fd path of the descriptor chmod. It is a variable so a case can model a
// kernel whose /proc does not offer the descriptor; no other code replaces it.
var migrateFollowupProcChmod = func(fd int, perm uint32) error {
	return unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), perm)
}

// ownedDirIdentityFchmod gives the pinned directory exactly perm through its descriptor. fchmodat2 is
// used where the kernel has it (6.5 and later); only an answer that the call is absent (ENOSYS or EOPNOTSUPP) is tried on the
// /proc/self/fd path, which is the same call a libc fchmod makes on a descriptor, so a kernel or
// filesystem without the flag is still served; any other answer is a failure of that mechanism and is reported with its errno.
func ownedDirIdentityFchmod(fd int, perm uint32) error {
	first := ownedDirIdentityFchmodat2(fd, perm)
	if first == nil {
		return nil
	}
	if !errors.Is(first, unix.ENOSYS) && !errors.Is(first, unix.EOPNOTSUPP) {
		// fchmodat2 exists and refused this mode, a genuine failure that the /proc path must not get past.
		return first
	}
	second := migrateFollowupProcChmod(fd, perm)
	if second == nil {
		return nil
	}
	// Neither mechanism exists when fchmodat2 answers ENOSYS or EOPNOTSUPP and the /proc path answers ENOENT, EACCES or
	// ENOTDIR: that is a kernel the mode cannot be set on, refused as unsupported with both answers. Any other answer is a
	// genuine failure of a mechanism that exists, and its own errno is kept first in the error.
	firstAbsent := errors.Is(first, unix.ENOSYS) || errors.Is(first, unix.EOPNOTSUPP)
	secondAbsent := errors.Is(second, unix.ENOENT) || errors.Is(second, unix.EACCES) || errors.Is(second, unix.ENOTDIR)
	if firstAbsent && secondAbsent {
		return migrateFollowupChmodUnsupported{fchmodat2: first, proc: second}
	}
	if firstAbsent {
		return fmt.Errorf("%w; fchmodat2: %v", second, first)
	}
	return fmt.Errorf("%w; /proc/self/fd: %v", first, second)
}
