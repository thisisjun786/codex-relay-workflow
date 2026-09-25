// Package pyerr renders an errno-carrying Go error the way CPython renders the OSError it
// raises for the same errno on Linux, so caller-visible receipts and refusals keep Python's
// bytes.
package pyerr

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
)

// names are the OSError subclasses CPython raises for an errno (PEP 3151), read from
// type(OSError(errno, "")).__name__ for errno 1-133 on Linux. Any other errno is OSError.
var names = map[syscall.Errno]string{
	syscall.EPERM: "PermissionError", syscall.ENOENT: "FileNotFoundError",
	syscall.ESRCH: "ProcessLookupError", syscall.EINTR: "InterruptedError",
	syscall.ECHILD: "ChildProcessError", syscall.EAGAIN: "BlockingIOError",
	syscall.EACCES: "PermissionError", syscall.EEXIST: "FileExistsError",
	syscall.ENOTDIR: "NotADirectoryError", syscall.EISDIR: "IsADirectoryError",
	syscall.EPIPE: "BrokenPipeError", syscall.ECONNABORTED: "ConnectionAbortedError",
	syscall.ECONNRESET: "ConnectionResetError", syscall.ESHUTDOWN: "BrokenPipeError",
	syscall.ETIMEDOUT: "TimeoutError", syscall.ECONNREFUSED: "ConnectionRefusedError",
	syscall.EALREADY: "BlockingIOError", syscall.EINPROGRESS: "BlockingIOError",
}

// strerrors are the errnos whose os.strerror text is not Go's Errno.Error() text with its
// first letter capitalised (compared over errno 1-133 on Linux).
var strerrors = map[syscall.Errno]string{
	41:  "Unknown error 41",
	58:  "Unknown error 58",
	133: "Memory page has hardware error",
}

// Strerror is os.strerror(errno).
func Strerror(errno syscall.Errno) string {
	if text, ok := strerrors[errno]; ok {
		return text
	}
	text := errno.Error()
	if text == "" {
		return text
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// OSError reports err's errno as CPython's exception class name and str(): for a socket that
// does not exist, "FileNotFoundError" and "[Errno 2] No such file or directory". When err is a
// *fs.PathError the message gains Python's ": '<filename>'" suffix, as open() raises it; a
// socket connect carries no filename, so a dial error does not.
func OSError(err error) (name, message string, ok bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return "", "", false
	}
	name, known := names[errno]
	if !known {
		name = "OSError"
	}
	message = fmt.Sprintf("[Errno %d] %s", int(errno), Strerror(errno))
	var path *fs.PathError
	if errors.As(err, &path) {
		message += ": " + settings.Repr(path.Path)
	}
	return name, message, true
}
