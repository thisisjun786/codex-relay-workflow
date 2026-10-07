//go:build darwin

package migrate

import (
	"errors"

	"golang.org/x/sys/unix"
)

// noReplaceSupported says this platform has a rename that refuses to replace.
const noReplaceSupported = true

// noReplaceRename renames oldName to newName inside the directory dirfd names (renameatx_np with RENAME_EXCL): anything already at
// newName, a link included, answers EEXIST and stays. ENOTSUP or EINVAL mean the volume lacks the flag.
func noReplaceRename(dirfd int, oldName, newName string) error {
	return unix.RenameatxNp(dirfd, oldName, dirfd, newName, unix.RENAME_EXCL)
}

// ownedDirIdentityHandleFlag opens a directory as a handle that needs search permission but not read:
// Darwin O_SEARCH, which x/sys does not declare.
const ownedDirIdentityHandleFlag = 0x40000000

// ownedDirIdentityHandleOK says this platform can pin a directory without read permission, so a mode is
// never given to a name a racer could have redirected.
const ownedDirIdentityHandleOK = true

// ownedDirIdentityFchmodat2 has no call here: Darwin has no fchmodat2, and ownedDirIdentityFchmod does
// not need it. It exists so a case can be written once for every platform.
var ownedDirIdentityFchmodat2 = func(fd int, perm uint32) error {
	return errors.ErrUnsupported
}

// ownedDirIdentityFchmod gives the pinned directory exactly perm through its descriptor, with the
// fchmod a descriptor takes. The handle was opened without read permission, which fchmod does not need.
func ownedDirIdentityFchmod(fd int, perm uint32) error {
	return unix.Fchmod(fd, perm)
}
