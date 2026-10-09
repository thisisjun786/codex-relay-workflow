package doctor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// harnessReadLimit is how many bytes the harness reads from one file it does not control (a
// session slot, a plugin manifest, a hook or MCP document, the install manifest). The files the
// checks read are a few kilobytes; a file past the limit is reported, never read whole and never
// taken for an absent one (CRW-1152).
const harnessReadLimit = 4 << 20

// errHarnessTooLarge is the reason a file past harnessReadLimit is refused. errNotRegular
// (executable.go) is the reason for a FIFO, a device or any other file that is not regular.
var errHarnessTooLarge = errors.New("larger than the 4 MiB diagnostic read limit")

// harnessOpenBounded opens path for a bounded read of the file it names. The open does not
// block: a FIFO opened for reading waits for a writer, and a hook or session record that is a
// FIFO must be reported inside the report's budget. The type is judged on the descriptor that was
// opened, so a file swapped in after a stat cannot be read. A link is followed, as ReadFile did.
func harnessOpenBounded(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
}

// harnessReadBounded is os.ReadFile(path) for an untrusted record: it never blocks on a FIFO, it
// refuses a file that is not regular, and it stops at harnessReadLimit.
func harnessReadBounded(path string) ([]byte, error) {
	file, err := harnessOpenBounded(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return harnessReadHandle(file, path)
}

// harnessReadHandle reads an opened file within the bound. A directory is EISDIR, as the read of
// one is; any other file that is not regular is errNotRegular; more than harnessReadLimit bytes
// is errHarnessTooLarge. Both refusals are fs.PathErrors for the read of path.
func harnessReadHandle(file *os.File, path string) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case info.IsDir():
		return nil, &fs.PathError{Op: "read", Path: path, Err: syscall.EISDIR}
	case !info.Mode().IsRegular():
		return nil, &fs.PathError{Op: "read", Path: path, Err: errNotRegular}
	}
	data, err := io.ReadAll(io.LimitReader(file, harnessReadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > harnessReadLimit {
		return nil, &fs.PathError{Op: "read", Path: path, Err: errHarnessTooLarge}
	}
	return data, nil
}

// errHarnessTooDeep is the reason a document nested past pyjson.MaxDepth is refused.
var errHarnessTooDeep = errors.New("nested deeper than the 10000 container diagnostic limit")

// harnessParseBounded reads a JSON document within the depth bound: pyjson.Loads without Deep
// refuses a container past pyjson.MaxDepth, and that refusal is errHarnessTooDeep here. The reader
// answers one syntax error for both a depth breach and a malformed document, so a refusal is
// errHarnessTooDeep only when the depth is all that stands against the document: it opens a
// container past the limit and the same reader with Deep set, which reads past it, accepts it. A
// document with any syntax error (before the deep containers or after them) keeps the reader's own
// error. The Deep reading runs only for such a refusal and is iterative, within the read limit.
func harnessParseBounded(doc string, options pyjson.LoadOptions) (any, error) {
	options.Deep = false
	value, err := pyjson.Loads(doc, options)
	if err != nil && harnessNestsPastLimit(doc) {
		deep := options
		deep.Deep = true
		if _, deepErr := pyjson.Loads(doc, deep); deepErr == nil {
			return nil, errHarnessTooDeep
		}
	}
	return value, err
}

// harnessNestsPastLimit reports whether doc opens a container past pyjson.MaxDepth, string
// contents aside. It only prefilters a refusal; the readers decide the document.
func harnessNestsPastLimit(doc string) bool {
	depth, inString, escaped := 0, false, false
	for i := 0; i < len(doc); i++ {
		c := doc[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '[' || c == '{':
			depth++
			if depth > pyjson.MaxDepth {
				return true
			}
		case c == ']' || c == '}':
			depth--
		}
	}
	return false
}

// harnessReadRefused reports a file the harness will not read for what it is (not regular, or too
// large), as against one that is absent or fails for another reason.
func harnessReadRefused(err error) bool {
	return errors.Is(err, errNotRegular) || errors.Is(err, errHarnessTooLarge)
}

// harnessRunGuard runs one step of the report and answers its checks. A step that panics is one
// WARN naming it, so the checks that ran before it and the ones after it stay in the report: an
// agent or an installer reads the whole report, and one unreadable directory must not take it
// away (CRW-1152). The oracle's uncaught throw is the defect this replaces.
func harnessRunGuard(name string, run func() []HarnessCheck) (checks []HarnessCheck) {
	defer func() {
		if value := recover(); value != nil {
			message := fmt.Sprint(value)
			if thrown, ok := value.(error); ok {
				message = thrown.Error()
			}
			checks = []HarnessCheck{{Name: name, Severity: HarnessWarn, Evidence: "check skipped: " + message}}
		}
	}()
	return run()
}

// harnessRunGuardOne is harnessRunGuard for a step that answers one check.
func harnessRunGuardOne(name string, run func() HarnessCheck) HarnessCheck {
	return harnessRunGuard(name, func() []HarnessCheck { return []HarnessCheck{run()} })[0]
}

// harnessRunMetadata reads one optional report field. A read that panics answers nil, the value of
// a field that could not be determined, so the checks already made are not lost with it
// (CRW-1152, port: fixed).
func harnessRunMetadata(read func() *string) (value *string) {
	defer func() {
		if recover() != nil {
			value = nil
		}
	}()
	return read()
}
