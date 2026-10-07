//go:build !linux && !darwin

package migrate

import "errors"

// noReplaceSupported says this platform has no rename that refuses to replace, so nothing is published here.
const noReplaceSupported = false

// noReplaceRename has no platform call: the run refuses before writing, and there is no fallback to a replacing rename.
func noReplaceRename(dirfd int, oldName, newName string) error { return errors.ErrUnsupported }

// ownedDirIdentityHandleFlag is unused here: this platform has no handle that pins a directory without
// read permission, so a creation checks the name immediately before a by-name no-follow chmod and the
// two-syscall window that leaves is carried as a residual in the issue's defect record.
const ownedDirIdentityHandleFlag = 0

// ownedDirIdentityHandleOK says this platform cannot pin a directory without read permission.
const ownedDirIdentityHandleOK = false

// ownedDirIdentityFchmodat2 has no call here.
var ownedDirIdentityFchmodat2 = func(fd int, perm uint32) error {
	return errors.ErrUnsupported
}

// ownedDirIdentityFchmodUsesFchmodat2 says the mode call below does not consult ownedDirIdentityFchmodat2.
const ownedDirIdentityFchmodUsesFchmodat2 = false

// ownedDirIdentityFchmod is unreachable: ownedDirIdentityHandleOK is false, so the creation never pins
// a handle to give a mode through.
func ownedDirIdentityFchmod(fd int, perm uint32) error { return errors.ErrUnsupported }
