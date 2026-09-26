package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"modernc.org/sqlite"
)

// errnoClass is the OSError subclass Python raises for an errno (PEP 3151).
var errnoClass = map[syscall.Errno]string{
	syscall.ENOENT: "FileNotFoundError", syscall.EACCES: "PermissionError", syscall.EPERM: "PermissionError",
	syscall.EEXIST: "FileExistsError", syscall.ENOTDIR: "NotADirectoryError", syscall.EISDIR: "IsADirectoryError",
	syscall.ECONNREFUSED: "ConnectionRefusedError", syscall.ECONNRESET: "ConnectionResetError",
	syscall.ECONNABORTED: "ConnectionAbortedError", syscall.EPIPE: "BrokenPipeError",
	syscall.ETIMEDOUT: "TimeoutError", syscall.EINTR: "InterruptedError", syscall.ESRCH: "ProcessLookupError",
	syscall.ECHILD: "ChildProcessError", syscall.EAGAIN: "BlockingIOError",
}

// pythonStrerror is os.strerror for the errnos whose glibc text Go spells differently.
func pythonStrerror(errno syscall.Errno) string {
	text := errno.Error()
	if text == "" {
		return "Unknown error " + strconv.Itoa(int(errno))
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// PythonOSError renders err as Python's f"{type(error).__name__}: {error}" for an OSError, so a
// field copied from the Python diagnosis keeps its text. Errors that carry no errno keep Go's text.
func PythonOSError(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	class, known := errnoClass[errno]
	if !known {
		class = "OSError"
	}
	return class + ": " + PythonOSErrorText(err)
}

// PythonOSErrorText is str(error) for an OSError: "[Errno N] text: 'path'".
func PythonOSErrorText(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	text := fmt.Sprintf("[Errno %d] %s", int(errno), pythonStrerror(errno))
	var path *fs.PathError
	if errors.As(err, &path) {
		text += ": " + pythonRepr(path.Path)
	}
	var link *os.LinkError
	if errors.As(err, &link) {
		text += ": " + pythonRepr(link.Old) + " -> " + pythonRepr(link.New)
	}
	return text
}

// pythonRepr is repr() of a str: single quotes unless the text holds one and no double quote.
func pythonRepr(text string) string {
	quote := "'"
	if strings.Contains(text, "'") && !strings.Contains(text, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range text {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == quote:
			b.WriteString(`\` + quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// PythonRepr is pythonRepr for callers outside the package that echo a Python !r field.
func PythonRepr(text string) string { return pythonRepr(text) }

var sqliteCodeSuffix = regexp.MustCompile(` \(\d+\)( \(SQLITE_BUSY\))?$`)

// PythonSQLiteError renders a SQLite failure as Python's f"{type(error).__name__}: {error}"
// for the sqlite3 module: the class chosen from the primary result code, and the message
// SQLite itself reported without the driver's "errstr: " prefix and " (code)" suffix.
func PythonSQLiteError(err error) string {
	var failure *sqlite.Error
	if !errors.As(err, &failure) {
		return PythonOSError(err)
	}
	class := "OperationalError"
	switch failure.Code() & 0xff {
	case 19: // SQLITE_CONSTRAINT
		class = "IntegrityError"
	case 26, 11: // SQLITE_NOTADB, SQLITE_CORRUPT
		class = "DatabaseError"
	case 21: // SQLITE_MISUSE
		class = "ProgrammingError"
	}
	return class + ": " + PythonSQLiteMessage(err)
}

// PythonSQLiteMessage is str(error) for a sqlite3 error: SQLite's own message.
func PythonSQLiteMessage(err error) string {
	var failure *sqlite.Error
	if !errors.As(err, &failure) {
		return err.Error()
	}
	message := sqliteCodeSuffix.ReplaceAllString(failure.Error(), "")
	if _, detail, found := strings.Cut(message, ": "); found {
		message = detail
	}
	return message
}
