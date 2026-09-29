package doctor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// What an executable reference resolves to. Classification is by what the file IS once every
// link (the pointer included) is followed, never by the text of the reference: on the relay
// host `current/bin/codex-session-relay` names no Python while `current` points at a venv.
const (
	KindNative            = "native"             // an ELF or Mach-O binary that is not a Python interpreter
	KindPythonInterpreter = "python-interpreter" // a binary (or link) named python*, or a venv's interpreter
	KindPythonScript      = "python-script"      // a script whose #! names a Python interpreter, or a .py file
	KindScript            = "script"             // a #! script for some other interpreter
	KindPythonVenv        = "python-venv"        // a directory carrying pyvenv.cfg
	KindGoRuntime         = "go-binary"          // a runtime directory whose bin/crw is native
	KindDirectory         = "directory"
	KindMissing           = "missing"
	KindUnreadable        = "unreadable"
	KindOther             = "other"
)

var pythonName = regexp.MustCompile(`^python[0-9.]*$|^pypy[0-9.]*$`)

// PythonName reports whether a basename names a Python interpreter.
func PythonName(name string) bool { return pythonName.MatchString(filepath.Base(name)) }

// Executable is one reference, and what it resolves to.
type Executable struct {
	Value          string
	Resolves       string
	Kind           string
	Python         bool
	ThroughPointer bool
	Detail         string
}

// Object is the reference as a report shows it.
func (e Executable) Object() record.Object {
	return record.Object{
		{Key: "value", Value: e.Value},
		{Key: "resolves", Value: nullable(e.Resolves)},
		{Key: "kind", Value: e.Kind},
		{Key: "python", Value: e.Python},
		{Key: "throughPointer", Value: e.ThroughPointer},
		{Key: "detail", Value: e.Detail},
	}
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// magic reports whether head is a native executable image.
func native(head []byte) bool {
	for _, m := range [][]byte{{0x7f, 'E', 'L', 'F'}, {0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.HasPrefix(head, m) {
			return true
		}
	}
	return false
}

// shebang is the interpreter a #! line names, with `env` skipped.
func shebang(head []byte) (string, bool) {
	if !bytes.HasPrefix(head, []byte("#!")) {
		return "", false
	}
	line, _, _ := bytes.Cut(head[2:], []byte("\n"))
	words := strings.Fields(string(line))
	if len(words) == 0 {
		return "", true
	}
	if filepath.Base(words[0]) == "env" {
		for _, word := range words[1:] {
			if !strings.HasPrefix(word, "-") && !strings.Contains(word, "=") {
				return word, true
			}
		}
		return "", true
	}
	return words[0], true
}

// venvRoot is the nearest ancestor of path (itself included) that carries pyvenv.cfg.
func venvRoot(path string) string {
	for dir := path; ; dir = filepath.Dir(dir) {
		if info, err := os.Stat(filepath.Join(dir, "pyvenv.cfg")); err == nil && info.Mode().IsRegular() {
			return dir
		}
		if parent := filepath.Dir(dir); parent == dir {
			return ""
		}
	}
}

// Classify resolves value (relative values against base) and says what it is. pointerPath,
// when set, marks a reference that reaches its target through the owned pointer.
func Classify(value, base, pointerPath string) Executable {
	e := Executable{Value: value}
	path := value
	if !filepath.IsAbs(path) && base != "" {
		path = base + "/" + path
	}
	if pointerPath != "" && (path == pointerPath || strings.HasPrefix(path, strings.TrimSuffix(pointerPath, "/")+"/")) {
		e.ThroughPointer = true
	}
	if strings.HasSuffix(value, ".py") {
		e.Python = true
	}
	resolved, err := record.Resolve(path)
	if err != nil {
		e.Kind, e.Detail = KindUnreadable, "the reference could not be resolved: "+err.Error()
		return e
	}
	e.Resolves = resolved
	info, err := os.Stat(resolved)
	switch {
	case errors.Is(err, os.ErrNotExist):
		e.Kind, e.Detail = KindMissing, "nothing exists at "+resolved
		if PythonName(value) {
			e.Python = true
		}
		return e
	case err != nil:
		e.Kind, e.Detail = KindUnreadable, "what the reference resolves to could not be read: "+store.PythonOSError(err)
		return e
	case info.IsDir():
		if root := venvRoot(resolved); root == resolved {
			e.Kind, e.Python, e.Detail = KindPythonVenv, true, "a Python virtual environment (pyvenv.cfg)"
			return e
		}
		if isNative(filepath.Join(resolved, "bin", "crw")) {
			e.Kind, e.Detail = KindGoRuntime, "a runtime directory whose bin/crw is a native binary"
			return e
		}
		e.Kind, e.Detail = KindDirectory, "a directory"
		return e
	case !info.Mode().IsRegular():
		e.Kind, e.Detail = KindOther, "not a regular file"
		return e
	}
	head, err := readHead(resolved)
	if err != nil {
		e.Kind, e.Detail = KindUnreadable, "the file could not be read: "+store.PythonOSError(err)
		return e
	}
	switch {
	case native(head):
		if PythonName(value) || PythonName(resolved) {
			e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "a native binary named as a Python interpreter"
		} else if root := venvRoot(filepath.Dir(path)); root != "" && PythonName(path) {
			e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "the interpreter of the virtual environment at "+root
		} else {
			e.Kind, e.Detail = KindNative, "a native binary"
		}
	default:
		if interpreter, ok := shebang(head); ok {
			if PythonName(interpreter) {
				e.Kind, e.Python, e.Detail = KindPythonScript, true, "a script run by "+interpreter
			} else {
				e.Kind, e.Detail = KindScript, "a script run by "+interpreter
			}
		} else if strings.HasSuffix(resolved, ".py") || e.Python {
			e.Kind, e.Python, e.Detail = KindPythonScript, true, "a Python source file"
		} else if PythonName(value) || PythonName(resolved) {
			e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "a file named as a Python interpreter"
		} else {
			e.Kind, e.Detail = KindOther, "neither a native binary nor a #! script"
		}
	}
	return e
}

func readHead(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, 256)
	n, err := io.ReadFull(file, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:n], nil
}

func isNative(path string) bool {
	head, err := readHead(path)
	return err == nil && native(head)
}

// RuntimeKind is what a runtime directory is: a Python venv, a Go runtime, or unknown.
func RuntimeKind(directory string) string {
	if info, err := os.Stat(filepath.Join(directory, "pyvenv.cfg")); err == nil && info.Mode().IsRegular() {
		return KindPythonVenv
	}
	if isNative(filepath.Join(directory, "bin", "crw")) {
		return KindGoRuntime
	}
	return "unknown"
}
