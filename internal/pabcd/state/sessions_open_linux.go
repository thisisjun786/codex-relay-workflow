//go:build linux

package state

import "golang.org/x/sys/unix"

// sessionsDirOpenFlags opens one directory of the session-state walk. O_PATH asks only for the right to
// name the directory, so an ancestor the caller may search but not read (mode 0111, another user's
// directory) still opens — the same choice the goalplan walk makes on Linux. O_NOFOLLOW stops at a
// symbolic link; with O_PATH the link itself is returned, and openDirNoFollow refuses it by mode.
func sessionsDirOpenFlags() int {
	return unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}
