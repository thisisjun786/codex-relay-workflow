package doctor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
// when set, marks a reference that reaches its target through the owned pointer. A shell
// wrapper's $VAR and ~ words are not expanded here; a Classifier with an Expander does that.
func Classify(value, base, pointerPath string) Executable {
	return Classifier{Pointer: pointerPath}.Classify(value, base)
}

// Classifier classifies the references of one host: its owned pointer, and the expansions a
// shell wrapper's words are made with.
type Classifier struct {
	Pointer string
	Expand  Expander
}

// maxWrapperDepth bounds how many shell wrappers are followed from one reference.
const maxWrapperDepth = 4

// Classify is the package-level Classify with this classifier's pointer and expansions.
func (c Classifier) Classify(value, base string) Executable {
	return c.classify(value, base, 0)
}

func (c Classifier) classify(value, base string, depth int) Executable {
	e := Executable{Value: value}
	path := value
	if !filepath.IsAbs(path) && base != "" {
		path = base + "/" + path
	}
	if pointerPath := c.Pointer; pointerPath != "" && (path == pointerPath || strings.HasPrefix(path, strings.TrimSuffix(pointerPath, "/")+"/")) {
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
	if native(head) {
		if PythonName(value) || PythonName(resolved) {
			e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "a native binary named as a Python interpreter"
		} else if root := venvRoot(filepath.Dir(path)); root != "" && PythonName(path) {
			e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "the interpreter of the virtual environment at "+root
		} else {
			e.Kind, e.Detail = KindNative, "a native binary"
		}
		return e
	}
	interpreter, script := shebang(head)
	root := venvRoot(filepath.Dir(resolved))
	switch {
	case polyglot(head):
		e.Kind, e.Python, e.Detail = KindPythonScript, true, "a #! shell launcher that re-executes itself under Python (the '''exec' form pip and uv write when the interpreter's path is too long for #! or contains a space)"
	case script && PythonName(interpreter):
		e.Kind, e.Python, e.Detail = KindPythonScript, true, "a script run by "+interpreter
	case root != "":
		e.Kind, e.Python, e.Detail = KindPythonScript, true, "a file inside the Python virtual environment at "+root
	case script && shells[filepath.Base(interpreter)]:
		c.wrapper(&e, resolved, interpreter, depth)
	case script:
		e.Kind, e.Detail = KindScript, "a script run by "+interpreter
	case strings.HasSuffix(resolved, ".py") || e.Python:
		e.Kind, e.Python, e.Detail = KindPythonScript, true, "a Python source file"
	case PythonName(value) || PythonName(resolved):
		e.Kind, e.Python, e.Detail = KindPythonInterpreter, true, "a file named as a Python interpreter"
	default:
		e.Kind, e.Detail = KindOther, "neither a native binary nor a #! script"
	}
	return e
}

// polyglot is the exec launcher pip and uv write: a #! line, then a line starting with three
// single quotes and exec, which sh runs as `exec <python> "$0" "$@"` and Python reads as the
// start of a string literal. Only a Python launcher is written so.
func polyglot(head []byte) bool {
	if !bytes.HasPrefix(head, []byte("#!")) {
		return false
	}
	_, rest, _ := bytes.Cut(head, []byte("\n"))
	return bytes.HasPrefix(rest, []byte("'''exec'"))
}

// shells are the interpreters whose scripts are read for what they run.
var shells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ksh": true, "mksh": true, "ash": true}

// wrapperLimit is the most of a shell script read for what it runs; a larger one is not a
// wrapper this scan can judge.
const wrapperLimit = 1 << 20

// wrapper judges a shell script by what it runs, reading it as a shell program (shellWalker):
// a word naming a Python interpreter or a .py file, a command word (found on PATH when it is a
// bare name) or an absolute path that classifies as Python, or a bare argument PATH resolves
// to a Python program. A command word it cannot expand, resolve or find on PATH, a path it
// cannot read, and a construct the reader cannot place leave the script unreadable rather
// than judged.
func (c Classifier) wrapper(e *Executable, path, interpreter string, depth int) {
	e.Kind, e.Detail = KindScript, "a script run by "+interpreter
	if depth >= maxWrapperDepth {
		e.Kind, e.Detail = KindUnreadable, "a shell script reached through "+strconv.Itoa(maxWrapperDepth)+" wrappers, which this scan does not follow further"
		return
	}
	body, err := readLimited(path, wrapperLimit)
	switch {
	case err != nil:
		e.Kind, e.Detail = KindUnreadable, "the shell script could not be read: "+store.PythonOSError(err)
		return
	case len(body) > wrapperLimit:
		e.Kind, e.Detail = KindUnreadable, "a shell script larger than "+strconv.Itoa(wrapperLimit)+" bytes, which this scan does not read through"
		return
	}
	unresolved := ""
	note := func(detail string) {
		if unresolved == "" {
			unresolved = detail
		}
	}
	w := &shellWalker{
		expand: c.Expand,
		unreadable: func(value, detail string) {
			note("a shell script this scan cannot read through at " + strconv.Quote(value) + ": " + detail)
		},
	}
	w.visit = func(sw shellWord, role int) bool {
		word := sw.Written
		if strings.Contains(word, "\n") {
			return true // a program passed to -c
		}
		// A quoted message ("run setup.py") is not a path; a quoted path may hold a space,
		// and "$DIR/x.py" names a .py file whatever DIR is.
		pathLike := !strings.ContainsAny(word, " \t") || strings.ContainsRune("/$~", rune(word[0]))
		if PythonName(word) || (strings.HasSuffix(word, ".py") && pathLike) {
			e.Kind, e.Python, e.Detail = KindPythonScript, true, "a shell script that runs "+word
			return false
		}
		expanded, missing := sw.Value, sw.Missing
		if missing != "" {
			if role != roleArgument {
				note("a shell script that runs " + word + ", which needs " + missing + ", an expansion this scan does not make")
			}
			return true
		}
		var inner Executable
		switch {
		case !strings.Contains(expanded, "/") && role != roleScript:
			found, err := lookPath(expanded, c.Expand.Path)
			if err != nil {
				if role == roleCommand {
					note("a shell script that runs " + word + ", which no directory on the scan's PATH holds as an executable file")
				}
				return true
			}
			inner = c.classify(found, "", depth+1)
			if role == roleArgument && !inner.Python {
				return true
			}
		case !filepath.IsAbs(expanded):
			if role != roleArgument {
				note("a shell script that runs " + word + ", a relative path that resolves in whatever directory the script runs in")
			}
			return true
		case role == roleScript:
			inner = c.sourced(expanded, "", depth+1)
		default:
			inner = c.classify(expanded, "", depth+1)
		}
		switch {
		case inner.Python:
			e.Kind, e.Python, e.Detail = KindPythonScript, true, "a shell script that runs "+word+", "+inner.Detail
			return false
		case inner.Kind == KindUnreadable:
			note("a shell script that runs " + word + ", " + inner.Detail)
		}
		return true
	}
	w.walk(string(body), 0)
	if !e.Python && unresolved != "" {
		e.Kind, e.Detail = KindUnreadable, unresolved
	}
}

// sourced is what a shell reading value as its program (sh FILE, . FILE) runs: the file judged
// as a shell script whatever its #! line names, unless it is Python or not a readable script.
func (c Classifier) sourced(value, base string, depth int) Executable {
	e := c.classify(value, base, depth)
	if e.Python || (e.Kind != KindOther && e.Kind != KindScript) {
		return e
	}
	c.wrapper(&e, e.Resolves, "sh", depth)
	return e
}

func readLimited(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, limit+1))
}

// Expander makes the expansions of a command word that this scan can make as the shell would:
// a leading ~ or ~/ (HOME), and $NAME or ${NAME} for the names in Vars. Path is the PATH a
// bare command name is looked up in.
type Expander struct {
	Vars map[string]string
	Path string
}

// Expand returns word with every expansion made, or word unchanged and the first expansion
// that cannot be made, as written.
func (x Expander) Expand(word string) (string, string) {
	expanded := word
	if strings.HasPrefix(word, "~") {
		name, rest, slash := strings.Cut(word[1:], "/")
		home := x.Vars["HOME"]
		if name != "" || home == "" {
			return word, "~" + name
		}
		expanded = home
		if slash {
			expanded = strings.TrimSuffix(home, "/") + "/" + rest
		}
	}
	if strings.Contains(expanded, "`") {
		return word, "`"
	}
	var out strings.Builder
	for i := 0; i < len(expanded); i++ {
		if expanded[i] != '$' || i+1 == len(expanded) {
			out.WriteByte(expanded[i])
			continue
		}
		var name, written string
		if expanded[i+1] == '{' {
			end := strings.IndexByte(expanded[i:], '}')
			if end < 0 {
				return word, expanded[i:]
			}
			name, written = expanded[i+2:i+end], expanded[i:i+end+1]
			i += end
		} else {
			j := i + 1
			for j < len(expanded) && nameByte(expanded[j], j > i+1) {
				j++
			}
			if j == i+1 {
				return word, expanded[i : i+2]
			}
			name, written = expanded[i+1:j], expanded[i:j]
			i = j - 1
		}
		value := x.Vars[name]
		if value == "" || !validName(name) {
			return word, written
		}
		out.WriteString(value)
	}
	return out.String(), ""
}

func nameByte(b byte, digitAllowed bool) bool {
	return b == '_' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || digitAllowed && b >= '0' && b <= '9'
}

func validName(name string) bool {
	for i := 0; i < len(name); i++ {
		if !nameByte(name[i], i > 0) {
			return false
		}
	}
	return name != ""
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
