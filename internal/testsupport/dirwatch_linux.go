package testsupport

import (
	"encoding/binary"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// DirEvent is one thing that happened to a name inside a watched directory.
type DirEvent struct {
	Name string
	// Op is "create", "open", "delete" or "move".
	Op string
}

// DirWatch records what a process does to the names in one directory, as strace shows it for
// a command run by hand, but from inside a test and without a tracer: the kernel queues each
// event as the system call happens, so what a call or a child process did is complete the
// moment it has returned.
type DirWatch struct {
	t  testing.TB
	fd int
}

// WatchDir starts recording creations, opens, removals and renames of the names in dir.
func WatchDir(t testing.TB, dir string) *DirWatch {
	t.Helper()
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		t.Fatalf("inotify: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	if _, err := unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_OPEN|unix.IN_DELETE|unix.IN_MOVED_FROM|unix.IN_MOVED_TO); err != nil {
		t.Fatalf("watch %s: %v", dir, err)
	}
	return &DirWatch{t: t, fd: fd}
}

// Drain returns every event recorded since the watch started or Drain last ran, in order.
func (w *DirWatch) Drain() []DirEvent {
	w.t.Helper()
	var events []DirEvent
	buf := make([]byte, 64*1024)
	for {
		n, err := unix.Read(w.fd, buf)
		if err == unix.EAGAIN {
			return events
		}
		if err != nil {
			w.t.Fatalf("inotify read: %v", err)
		}
		if n <= 0 {
			return events
		}
		// struct inotify_event: int wd, uint32 mask, uint32 cookie, uint32 len, then len bytes of name.
		for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
			mask := binary.NativeEndian.Uint32(buf[offset+4:])
			length := int(binary.NativeEndian.Uint32(buf[offset+12:]))
			name := strings.TrimRight(string(buf[offset+unix.SizeofInotifyEvent:offset+unix.SizeofInotifyEvent+length]), "\x00")
			offset += unix.SizeofInotifyEvent + length
			if mask&unix.IN_Q_OVERFLOW != 0 {
				w.t.Fatalf("the inotify queue overflowed: events were lost, so a watch that saw none proves nothing")
			}
			var op string
			switch {
			case mask&unix.IN_CREATE != 0:
				op = "create"
			case mask&unix.IN_OPEN != 0:
				op = "open"
			case mask&unix.IN_DELETE != 0:
				op = "delete"
			case mask&(unix.IN_MOVED_FROM|unix.IN_MOVED_TO) != 0:
				op = "move"
			default:
				continue
			}
			events = append(events, DirEvent{Name: name, Op: op})
		}
	}
}

// Names returns the names the events with the given op touched, in the order they first appear.
func Names(events []DirEvent, op string) []string {
	var names []string
	seen := map[string]bool{}
	for _, event := range events {
		if event.Op == op && !seen[event.Name] {
			seen[event.Name] = true
			names = append(names, event.Name)
		}
	}
	return names
}
