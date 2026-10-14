package hook

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// CRW-1398 item 5: controlPathNote judges a path against the sockaddr_un limit of the system it runs on (Linux 108, darwin 104), not against
// the smaller of the two everywhere. A dial that failed with EINVAL on a path the system holds is not blamed on the path's length.
func TestControlPathNoteUsesTheSystemsSockaddrLimit(t *testing.T) {
	path := func(n int) string { return "/" + strings.Repeat("p", n-1) }
	if got := controlPathNote(path(sockaddrUnPathLimit-1), unix.EINVAL); got != "" {
		t.Errorf("a path of %d bytes fits this system (%d) and got the note %q", sockaddrUnPathLimit-1, sockaddrUnPathLimit, got)
	}
	got := controlPathNote(path(sockaddrUnPathLimit), unix.EINVAL)
	for _, want := range []string{"control socket path is longer than a unix socket address holds", "at most 103 bytes on darwin and 107 on Linux"} {
		if !strings.Contains(got, want) {
			t.Errorf("a path of %d bytes got %q; want %q", sockaddrUnPathLimit, got, want)
		}
	}
	if got := controlPathNote(path(sockaddrUnPathLimit+20), errors.New("connection refused")); got != "" {
		t.Errorf("a dial that failed for another reason got %q", got)
	}
}
