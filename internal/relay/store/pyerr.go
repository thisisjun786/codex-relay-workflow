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
	"unicode"
	"unicode/utf8"

	"modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
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
		text += ": " + PathRepr(path.Path)
	}
	var link *os.LinkError
	if errors.As(err, &link) {
		text += ": " + PathRepr(link.Old) + " -> " + PathRepr(link.New)
	}
	return text
}

// FSDecode is os.fsdecode as Go holds the result: the path's bytes, each byte that is not UTF-8
// replaced by the lone surrogate surrogateescape makes of it (U+DC80..U+DCFF) in WTF-8, which
// the JSON writers spell \udcXX as json.dumps does.
func FSDecode(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRuneInString(p[i:])
		if r == utf8.RuneError && size == 1 {
			v := 0xdc00 + rune(p[i])
			b.Write([]byte{0xed, byte(0xa0 | (v>>6)&0x1f), byte(0x80 | v&0x3f)})
			i++
			continue
		}
		b.WriteString(p[i : i+size])
		i += size
	}
	return b.String()
}

// PathRepr is repr() of a filename as Python holds one: os.fsdecode of its bytes, so a byte that
// is not UTF-8 is the lone surrogate surrogateescape makes of it (U+DC80..U+DCFF), and then every
// character str.isprintable refuses (controls, format characters, separators but the space, lone
// surrogates) written as \xNN, \uNNNN or \UNNNNNNNN.
func PathRepr(path string) string {
	quote := "'"
	if strings.Contains(path, "'") && !strings.Contains(path, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for i := 0; i < len(path); {
		r, size := utf8.DecodeRuneInString(path[i:])
		if r == utf8.RuneError && size == 1 {
			r = 0xdc00 + rune(path[i]) // surrogateescape
		}
		i += size
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
		case unicode.IsPrint(r): // str.isprintable's set: L, M, N, P, S and the space (a surrogate is Cs)
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// pythonRepr is repr() of a str (settings.Repr: Python's quote choice and its escapes of every
// character str.isprintable() rejects, a lone surrogate included).
func pythonRepr(text string) string { return settings.Repr(text) }

// PythonRepr is pythonRepr for callers outside the package that echo a Python !r field.
func PythonRepr(text string) string { return pythonRepr(text) }

// pythonHostError is an OS or SQLite failure Python raises out of Store() unhandled: its text is
// Python's host envelope detail, f"{type(error).__name__}: {error}", and the failure itself
// stays reachable through errors.As.
type pythonHostError struct{ cause error }

func (e *pythonHostError) Error() string { return PythonSQLiteError(e.cause) }
func (e *pythonHostError) Unwrap() error { return e.cause }

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
