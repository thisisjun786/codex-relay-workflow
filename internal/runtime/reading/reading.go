// Package reading is scripts/crw_runtime/reading.py's four-state partition: what a reading of a
// record observed, including what it could not observe. Absent means a clean host, unreadable
// means something is there whose shape cannot be read, an access error means the question
// could not be asked at all, and present means it was read. Collapsing any pair of them turns a
// permission problem into a malformed record or a misconfigured path into a clean host.
//
// Only the four states and the refusal shape are ported. Python's exception lattice and its
// traceback locator are not: a Go refusal names the Python exception class a caller reading the
// report would have seen (so "exception" keeps its meaning), and raisedAt is always null.
package reading

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The four answers a reading gives.
const (
	Absent      = "ABSENT"
	Present     = "PRESENT"
	Unreadable  = "UNREADABLE"
	AccessError = "ACCESS_ERROR"
)

// States is every answer, in reading.py's order.
var States = []string{Absent, Present, Unreadable, AccessError}

// Usable is the partition a consumer asks: read, or established to be absent.
func Usable(state string) bool { return state == Present || state == Absent }

// Unusable is the other half: nothing can be concluded from a reading in this state.
func Unusable(state string) bool { return state == Unreadable || state == AccessError }

// Identity is what the kernel calls an object: its device and inode.
type Identity struct{ Dev, Ino uint64 }

// Reading is a value and the state of the attempt that produced it.
type Reading struct {
	Value     any
	State     string
	Exception string
	Source    string
	Field     string
	Detail    string
	// Identity is taken from the descriptor the bytes were read through; nil where it was not
	// established, and a nil identity never compares equal to anything.
	Identity *Identity
}

// OK reports that the record was read. An existing record with nothing in it is still read.
func (r Reading) OK() bool { return r.State == Present }

// Usable reports read, or established absent.
func (r Reading) Usable() bool { return Usable(r.State) }

// Refusal is what a refusal reports: never the record's contents, only where and what failed.
// The key order is reading.py's refusal().
func (r Reading) Refusal() contract.OrderedObject {
	return contract.OrderedObject{
		{Key: "state", Value: r.State},
		{Key: "source", Value: nullable(r.Source)},
		{Key: "exception", Value: nullable(r.Exception)},
		{Key: "raisedAt", Value: nil},
		{Key: "field", Value: nullable(r.Field)},
		{Key: "detail", Value: nullable(r.Detail)},
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Failure is a shape rejection or a decoding failure carrying the Python exception class a
// reader of the refusal would have seen (TypeError, ValueError, JSONDecodeError, ...).
type Failure struct{ Class, Message string }

func (f *Failure) Error() string { return f.Class + ": " + f.Message }

// Fail is a shape rejection of the given Python class.
func Fail(class, message string) error { return &Failure{Class: class, Message: message} }

// classOf is type(error).__name__ and str(error) for an error a read produced.
func classOf(err error) (class, said string) {
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Class, failure.Message
	}
	text := store.PythonOSError(err)
	if name, rest, ok := strings.Cut(text, ": "); ok && strings.HasSuffix(name, "Error") && !strings.ContainsAny(name, " /") {
		return name, rest
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return "OSError", text
	}
	return "OSError", err.Error()
}

// isOS reports whether err is an OSError (an access error) rather than a shape failure.
func isOS(err error) bool {
	var failure *Failure
	return !errors.As(err, &failure)
}

// failed is reading.failure: an OSError established nothing; the rest read something and
// could not make sense of it.
func failed(err error, source, what, detail string) Reading {
	class, said := classOf(err)
	state := Unreadable
	if isOS(err) {
		state = AccessError
	}
	if detail == "" {
		detail = "could not read " + what
	}
	return Reading{State: state, Exception: class, Source: source, Detail: detail + " (" + class + ": " + said + ")"}
}

// Kind names a non-regular file type the way reading._kind does.
func Kind(mode os.FileMode) string {
	switch {
	case mode.IsDir():
		return "directory"
	case mode&os.ModeSocket != 0:
		return "socket"
	case mode&os.ModeNamedPipe != 0:
		return "named pipe"
	case mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0:
		return "block device"
	case mode&os.ModeCharDevice != 0:
		return "character device"
	}
	return "not a regular file"
}

// Observe is steps 1 and 2 of the ordered partition: what is at this path. It returns a
// settled reading, or nil when a regular file is there to be read. Absent is only returned
// for established absence.
func Observe(path, what string) *Reading {
	if strings.ContainsRune(path, 0) {
		// A NUL-bearing string cannot name a path at all (Python's ValueError).
		r := failed(&Failure{Class: "ValueError", Message: "embedded null byte"}, path, what, "this path cannot name a file")
		return &r
	}
	found, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Reading{State: Absent, Source: path, Detail: "nothing exists at " + path}
	}
	if err != nil {
		r := failed(err, path, what, "whether anything exists at this path could not be established")
		return &r
	}
	if found.Mode()&os.ModeSymlink != 0 {
		found, err = os.Stat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return &Reading{State: Unreadable, Exception: "FileNotFoundError", Source: path, Detail: "a symbolic link whose target does not exist"}
		case errors.Is(err, syscall.ELOOP):
			return &Reading{State: Unreadable, Exception: "OSError", Source: path, Detail: "a symbolic link that loops"}
		case err != nil:
			r := failed(err, path, what, "a symbolic link whose target could not be resolved")
			return &r
		}
	}
	if !found.Mode().IsRegular() {
		return &Reading{State: Unreadable, Source: path, Detail: "this path is a " + Kind(found.Mode()) + ", not a regular file"}
	}
	return nil
}

// OpenRegular is reading.open_regular: a descriptor on the regular file at path, opened
// without blocking and judged by the descriptor, so a pipe swapped in after a look cannot hold
// the open.
func OpenRegular(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = &os.PathError{Op: "open", Path: path, Err: syscall.EINVAL}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// identityOf is the device and inode of an open file.
func identityOf(file *os.File) *Identity {
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &Identity{Dev: uint64(sys.Dev), Ino: sys.Ino}
}

// universalNewlines is what Python's text mode does before json.loads sees the text.
func universalNewlines(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// Decode is json.loads over bytes read in text mode: strict UTF-8, universal newlines, then
// Python's JSON language (object order, NaN and the infinities, a repeated key keeping its last
// value). The error is a *Failure naming the Python exception.
func Decode(raw []byte) (any, error) {
	text, err := store.DecodeUTF8(raw)
	if err != nil {
		return nil, &Failure{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	text = universalNewlines(text)
	if message := store.PythonJSONError(text); message != "" {
		return nil, &Failure{Class: "JSONDecodeError", Message: message}
	}
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Constants: true, Surrogates: true, RawSurrogates: true, Numbers: pyjson.Int64Numbers})
	if err != nil {
		return nil, &Failure{Class: "ValueError", Message: err.Error()}
	}
	return value, nil
}

// ReadJSON reads one JSON record as a Reading rather than a sentinel. absent supplies the value
// an established absence carries; shape may reject a parsed value the caller cannot use.
func ReadJSON(path, what string, absent func() any, shape func(any) error) Reading {
	if settled := Observe(path, what); settled != nil {
		if settled.State == Absent && absent != nil {
			settled.Value = absent()
		}
		return *settled
	}
	file, err := OpenRegular(path)
	if err != nil {
		return failed(err, path, what, "")
	}
	defer file.Close()
	identity := identityOf(file)
	raw, err := io.ReadAll(file)
	if err != nil {
		r := failed(err, path, what, "")
		r.Identity = identity
		return r
	}
	value, err := Decode(raw)
	if err == nil && shape != nil {
		err = shape(value)
	}
	if err != nil {
		r := failed(err, path, what, "")
		r.Identity = identity
		return r
	}
	return Reading{Value: value, State: Present, Source: path, Identity: identity}
}

// ReadText reads one text file through the same partition (reading.read_text).
func ReadText(path, what string) Reading {
	if settled := Observe(path, what); settled != nil {
		if settled.State == Absent {
			settled.Value = ""
		}
		return *settled
	}
	file, err := OpenRegular(path)
	if err != nil {
		return failed(err, path, what, "")
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return failed(err, path, what, "")
	}
	text, err := store.DecodeUTF8(raw)
	if err != nil {
		return failed(&Failure{Class: "UnicodeDecodeError", Message: err.Error()}, path, what, "")
	}
	return Reading{Value: universalNewlines(text), State: Present, Source: path}
}

// SameDirectory is reading.same_directory: whether two spellings name the same object, asked
// of the filesystem. A failure establishes nothing and is answered "different".
func SameDirectory(one, other string) bool {
	a, err := os.Stat(one)
	if err != nil {
		return false
	}
	b, err := os.Stat(other)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// PathIdentity is reading.path_identity: the identity the kernel gives a path now, or false.
func PathIdentity(path string) (Identity, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return Identity{}, false
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Identity{}, false
	}
	return Identity{Dev: uint64(sys.Dev), Ino: sys.Ino}, true
}
