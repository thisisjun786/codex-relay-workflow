//go:build linux

package state

import "golang.org/x/sys/unix"

// sessionsDirOpenFlags opens one state directory of the session-state walk. O_PATH asks only for the
// right to name the directory, so a .crw the caller may traverse but not read (mode 0111, another
// user's or a locked-down workspace) still opens — the same choice the goalplan walk makes on Linux.
// O_NOFOLLOW stops at a symbolic link, and the caller refuses it by mode.
func sessionsDirOpenFlags() int {
	return unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}

// stateDirNeedsPathFallback is false on Linux: O_PATH needs no permission on the directory itself, so
// the descriptor open never has to fall back to a pathname open.
func stateDirNeedsPathFallback(error) bool { return false }
