//go:build !linux && !darwin

package state

import (
	"os"
	"syscall"
)

// renameNoReplace has no platform call on this system, so the caller falls back to creating the file in place.
func renameNoReplace(oldpath, newpath string) error {
	return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.ENOTSUP}
}
