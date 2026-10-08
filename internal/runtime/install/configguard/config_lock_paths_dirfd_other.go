//go:build !linux

package configguard

import "golang.org/x/sys/unix"

// configLockPathsDirFlags opens the pinned directory for reading. Where a search-only open is not available
// a directory without read permission has no descriptor and the case-only proof refuses (CRW-993 d2).
const configLockPathsDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
