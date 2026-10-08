//go:build !linux && !darwin

package configguard

import "golang.org/x/sys/unix"

// configLockPathsDirFlags opens the pinned directory for reading. Where no search-only open is known, a directory
// without read permission has no descriptor and the case-only proof refuses (CRW-993 d2).
const configLockPathsDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
