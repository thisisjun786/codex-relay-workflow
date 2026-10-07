//go:build darwin

package state

import "golang.org/x/sys/unix"

// sessionsDirOpenFlags opens one directory of the session-state walk. Darwin has no O_PATH, so the open
// asks for read access: a search-only ancestor (mode 0111, another user's directory) fails closed here,
// the same limitation the goalplan walk documents for this platform.
func sessionsDirOpenFlags() int {
	return unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}
