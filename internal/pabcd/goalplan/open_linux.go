package goalplan

import (
	"golang.org/x/sys/unix"
	"os"
	"strconv"
)

func directoryOpenFlags() int { return unix.O_PATH | unix.O_DIRECTORY }
func descriptorPath(f *os.File) (string, error) {
	return os.Readlink("/proc/self/fd/" + strconv.Itoa(int(f.Fd())))
}
