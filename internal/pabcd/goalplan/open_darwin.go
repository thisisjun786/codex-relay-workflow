package goalplan

import (
	"golang.org/x/sys/unix"
	"os"
	"runtime"
	"strings"
	"unsafe"
)

// Darwin has no O_PATH: search-only ancestors fail closed without read permission.
func directoryOpenFlags() int { return unix.O_RDONLY | unix.O_DIRECTORY }
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
