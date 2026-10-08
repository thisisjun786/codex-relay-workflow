//go:build linux

package configguard

import "golang.org/x/sys/unix"

// configLockPathsDirFlags opens the pinned directory for searching only. A directory the process may search
// but not read still yields the descriptor the by-name lookups use (CRW-993 d2).
const configLockPathsDirFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC
