package goalplan

import (
	"golang.org/x/sys/unix"
	"os"
	"runtime"
	"strings"
	"unsafe"
)

// Darwin has no O_PATH. The goalplan directory is opened search-only: O_EXEC | O_DIRECTORY, which is O_SEARCH on macOS
// (xnu bsd/sys/fcntl.h, the flag internal/pabcd/cli planDirectoryFlags uses). A directory without read permission then still
// opens for the by-name lookups and the writes its mode allows (CRW-581); the O_RDONLY open this replaces refused it with
// EACCES. Compiled and vetted on Linux with GOOS=darwin; not run on macOS.
const darwinOSearch = 0x40000000

func directoryOpenFlags() int { return darwinOSearch | unix.O_DIRECTORY }
func descriptorPath(f *os.File) (string, error) {
	var buf [1024]byte // Darwin MAXPATHLEN, the F_GETPATH buffer size.
	var pin runtime.Pinner
	pin.Pin(&buf[0])
	defer pin.Unpin()
	_, err := unix.FcntlInt(f.Fd(), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0]))))
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(buf[:]), "\x00"), nil
}
