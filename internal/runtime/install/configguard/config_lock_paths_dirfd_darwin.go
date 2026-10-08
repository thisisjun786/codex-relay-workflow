//go:build darwin

package configguard

import "golang.org/x/sys/unix"

// configLockPathsDirFlags opens the pinned directory search-only: O_EXEC | O_DIRECTORY, which is O_SEARCH on macOS
// (xnu bsd/sys/fcntl.h). golang.org/x/sys does not export the constant; the literal is the one internal/pabcd/cli
// planDirectoryFlags uses. A directory without read permission still yields a descriptor for the by-name lookups.
// Compiled and vetted on Linux; not executed on macOS (CRW-993 d2).
const configLockPathsDirFlags = 0x40000000 | unix.O_DIRECTORY | unix.O_CLOEXEC
