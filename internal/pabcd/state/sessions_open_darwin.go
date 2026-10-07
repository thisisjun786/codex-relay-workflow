//go:build darwin

package state

import (
	"errors"
	"io/fs"

	"golang.org/x/sys/unix"
)

// sessionsDirOpenFlags opens one state directory of the session-state walk. Darwin has no O_PATH, so the
// open asks for read access on that one component: .crw and .crw/sessions themselves, not cwd or its
// ancestors. That is the same limitation the goalplan walk documents for this platform.
func sessionsDirOpenFlags() int {
	return unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}

// stateDirNeedsPathFallback reports whether a descriptor open of a state directory should fall back to a
// pathname open. On Darwin it does for a permission refusal: the platform has no O_PATH, so an existing
// .crw the caller may traverse but not read would otherwise regress the callers dev allowed (EnsureState,
// WriteState, WithSessionLock). The pathname fallback still refuses a symbolic link through Lstat.
func stateDirNeedsPathFallback(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}
