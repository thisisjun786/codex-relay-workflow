package supervisor

import (
	"encoding/binary"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// watchDatabaseOpens watches the directory of the relay database and returns a function that reports
// how many times a file of the database (relay.sqlite3, its -wal and -shm) was opened since the last
// call. A separate connection, however it is opened (through the driver registered as "sqlite", through
// store.Open or store.OpenReadOnly, which build drivers of their own) opens the file and shows up; the
// store's one pool connection, which stays open, does not. The second result is false where the host
// cannot watch.
func watchDatabaseOpens(tb testing.TB, dir string) (func() int, bool) {
	tb.Helper()
	fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
	if err != nil {
		return nil, false
	}
	tb.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_OPEN); err != nil {
		return nil, false
	}
	buf := make([]byte, 64*1024)
	return func() int {
		count := 0
		for {
			n, err := unix.Read(fd, buf)
			if err != nil || n <= 0 {
				return count
			}
			// struct inotify_event: wd, mask, cookie, len (four 32-bit fields), then len bytes of name.
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				length := int(binary.NativeEndian.Uint32(buf[off+12 : off+16]))
				end := off + unix.SizeofInotifyEvent + length
				if end > n {
					break
				}
				if strings.HasPrefix(strings.TrimRight(string(buf[off+unix.SizeofInotifyEvent:end]), "\x00"), "relay.sqlite3") {
					count++
				}
				off = end
			}
		}
	}, true
}
