package pyerr

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The class name and text CPython 3.13 gives on Linux, read from
// type(OSError(n, "")).__name__ and os.strerror(n).
func Test_errno_classes_and_texts_are_pythons(t *testing.T) {
	for _, tc := range []struct {
		errno      syscall.Errno
		name, text string
	}{
		{syscall.ENOENT, "FileNotFoundError", "No such file or directory"},
		{syscall.ECONNREFUSED, "ConnectionRefusedError", "Connection refused"},
		{syscall.ESHUTDOWN, "BrokenPipeError", "Cannot send after transport endpoint shutdown"},
		{syscall.EALREADY, "BlockingIOError", "Operation already in progress"},
		{syscall.EINPROGRESS, "BlockingIOError", "Operation now in progress"},
		{syscall.EAGAIN, "BlockingIOError", "Resource temporarily unavailable"},
		{syscall.ENOTSOCK, "OSError", "Socket operation on non-socket"},
		{41, "OSError", "Unknown error 41"},
		{58, "OSError", "Unknown error 58"},
		{133, "OSError", "Memory page has hardware error"},
	} {
		name, message, ok := OSError(fmt.Errorf("dial: %w", tc.errno))
		want := fmt.Sprintf("[Errno %d] %s", int(tc.errno), tc.text)
		if !ok || name != tc.name || message != want {
			t.Errorf("errno %d: %q %q, want %q %q", int(tc.errno), name, message, tc.name, want)
		}
	}
}

func Test_a_path_error_carries_pythons_filename_suffix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "it's missing.json")
	_, err := os.Open(path)
	_, message, ok := OSError(err)
	if want := `[Errno 2] No such file or directory: "` + path + `"`; !ok || message != want {
		t.Fatalf("%q, want %q", message, want)
	}
}
