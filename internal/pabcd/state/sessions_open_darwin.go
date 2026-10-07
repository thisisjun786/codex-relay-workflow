//go:build darwin

package state

import "golang.org/x/sys/unix"

// sessionsDirOpenFlags opens one state directory of the session-state walk. Darwin has no O_PATH, so the
// open asks for read access on that one component: .crw and .crw/sessions themselves, not cwd or its
// ancestors. That is the same limitation the goalplan walk documents for this platform.
func sessionsDirOpenFlags() int {
	return unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
}
