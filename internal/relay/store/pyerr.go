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

// strerror is os.strerror for the errnos whose glibc text Go spells differently.
func strerror(errno syscall.Errno) string {
	text := errno.Error()
	if text == "" {
		return "Unknown error " + strconv.Itoa(int(errno))
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// StoredOSError is an OS failure as the stored rows word it: Python's
// f"{type(error).__name__}: {error}" for an OSError, the wording the Python relay stored and a
// later reader meets. Errors that carry no errno keep Go's text. The relay words a failure this
// way, and StoredOSErrorText's way, only where the text is stored: the Stop journal's rows (whose
// guard_unreachable detail the hook's own reader parses back, hook.NativePrescanUnreachable), the
// guard answers and faults the hook journals, and the intent records' store_unreadable detail.
// Every message that is only shown names a failure in Go's words (decision R3S-1).
func StoredOSError(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	return osErrorClass(err) + ": " + StoredOSErrorText(err)
}

// osErrorClass is type(error).__name__ for the OSError Python raises for err's errno.
func osErrorClass(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if class, known := errnoClass[errno]; known {
			return class
		}
	}
	return "OSError"
}

// StoredOSErrorText is the stored wording's text without its class: str(error) for an OSError,
// "[Errno N] text: 'path'", the path as PathRepr spells it.
func StoredOSErrorText(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	text := fmt.Sprintf("[Errno %d] %s", int(errno), strerror(errno))
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

// PathRepr is repr() of a filename as Python holds one: os.fsdecode of its bytes, so a byte that
// is not UTF-8 is the lone surrogate surrogateescape makes of it (U+DC80..U+DCFF), and then every
// character str.isprintable refuses (controls, format characters, separators but the space, lone
// surrogates, and what CPython 3.14's Unicode database leaves unassigned) written as \xNN,
// \uNNNN or \UNNNNNNNN.
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
		case settings.Printable(r): // str.isprintable as CPython 3.14 answers it (a surrogate is Cs)
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

var sqliteCodeSuffix = regexp.MustCompile(` \(\d+\)( \(SQLITE_BUSY\))?$`)

// StoredSQLiteError is a SQLite failure as the stored records word it: Python's
// f"{type(error).__name__}: {error}" for the sqlite3 module, the class chosen from the primary
// result code and the message SQLite itself reported without the driver's "errstr: " prefix and
// " (code)" suffix. It words the intent records' store_unopenable and store_write_failed details
// and the linkage readings' detail, which the supervisor stores with the reading; an OS failure
// is StoredOSError's.
func StoredSQLiteError(err error) string {
	if encode := EncodeError(err); encode != nil {
		return encode.HostDetail()
	}
	var failure *sqlite.Error
	if !errors.As(err, &failure) {
		return StoredOSError(err)
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
	return class + ": " + sqliteMessage(err)
}

// sqliteMessage is str(error) for a sqlite3 error: SQLite's own message.
func sqliteMessage(err error) string {
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
