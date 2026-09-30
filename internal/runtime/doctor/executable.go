package doctor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// What an executable reference resolves to. Classification is by what the file IS once every
// link (the pointer included) is followed, never by the text of the reference.
const (
	KindNative     = "native"    // an ELF or Mach-O binary
	KindScript     = "script"    // a shell script that runs nothing this reading cannot judge, or one that cannot run
	KindGoRuntime  = "go-binary" // a runtime directory whose bin/crw is native
	KindDirectory  = "directory"
	KindMissing    = "missing"
	KindUnreadable = "unreadable"
	KindOther      = "other"
)

// Executable is one reference, and what it resolves to.
type Executable struct {
	Value          string
	Resolves       string
	Kind           string
	ThroughPointer bool
	Detail         string
}

// Object is the reference as a report shows it.
func (e Executable) Object() record.Object {
	return record.Object{
		{Key: "value", Value: e.Value},
		{Key: "resolves", Value: nullable(e.Resolves)},
		{Key: "kind", Value: e.Kind},
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

// native reports whether head is a native executable image.
func native(head []byte) bool {
	for _, m := range [][]byte{{0x7f, 'E', 'L', 'F'}, {0xcf, 0xfa, 0xed, 0xfe}, {0xce, 0xfa, 0xed, 0xfe}, {0xca, 0xfe, 0xba, 0xbe}} {
		if bytes.HasPrefix(head, m) {
			return true
		}
	}
	return false
}

// shebang is a #! line as the kernel reads it: the interpreter and its one optional argument
// (the rest of the line, one word whatever it holds). complete is false when the line does not
// end within the bytes the kernel reads.
func shebang(head []byte) (interpreter, arg string, script, complete bool) {
	if !bytes.HasPrefix(head, []byte("#!")) {
		return "", "", false, true
	}
	line, _, found := bytes.Cut(head[2:], []byte("\n"))
	text := strings.Trim(string(line), " \t")
	interpreter = text
	if i := strings.IndexAny(text, " \t"); i >= 0 {
		interpreter, arg = text[:i], strings.Trim(text[i:], " \t")
	}
	return interpreter, arg, true, found || len(head) < headSize
}

// Classify resolves value (relative values against base) and says what it is. pointerPath,
// when set, marks a reference that reaches its target through the owned pointer. A shell
// script's words are expanded, and its bare commands found, with a Classifier's Expander.
func Classify(value, base, pointerPath string) Executable {
	return Classifier{Pointer: pointerPath}.Classify(value, base)
}

// Classifier classifies the references of one host: its owned pointer, and the expansions and
// PATH a shell script's words are made with. Observe, when set, receives every path classified
// (the reference as an absolute path, before any link is followed) with what it resolves to,
// including each interpreter and script a reference is followed through.
type Classifier struct {
	Pointer string
	Expand  Expander
	Observe func(path string, e Executable)
}

// Classify is the package-level Classify with this classifier's pointer and expansions.
func (c Classifier) Classify(value, base string) Executable {
	return c.classify(value, base, 0)
}

func (c Classifier) classify(value, base string, depth int) (e Executable) {
	e = Executable{Value: value}
	path := value
	if !filepath.IsAbs(path) && base != "" {
		path = base + "/" + path
	}
	if c.Observe != nil {
		defer func() { c.Observe(path, e) }()
	}
	if pointerPath := c.Pointer; pointerPath != "" && (path == pointerPath || strings.HasPrefix(path, strings.TrimSuffix(pointerPath, "/")+"/")) {
		e.ThroughPointer = true
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
		return e
	case err != nil:
		e.Kind, e.Detail = KindUnreadable, "what the reference resolves to could not be read: "+store.PythonOSError(err)
		return e
	case info.IsDir():
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
		e.Kind, e.Detail = KindNative, "a native binary"
		return e
	}
	interpreter, arg, script, complete := shebang(head)
	switch {
	case script && !complete:
		e.Kind, e.Detail = KindUnreadable, "a #! line longer than the "+strconv.Itoa(headSize)+" bytes the kernel reads"
	case script:
		c.interpreted(&e, resolved, interpreter, arg, depth)
	default:
		e.Kind, e.Detail = KindOther, "neither a native binary nor a #! script"
	}
	return e
}

// interpreted judges a #! script by the interpreter the kernel runs it with, resolved and
// classified like any reference: a shell (sh, bash, dash) is read for what the script runs, env
// runs the one program the rest of the line names (the kernel passes it as one word, so an option
// or assignment is no name), found on PATH when bare, and any other interpreter (python, perl,
// node, busybox, a relative path, an unreadable one) is unreadable.
func (c Classifier) interpreted(e *Executable, path, interpreter, arg string, depth int) {
	by := "a script run by " + strings.TrimSpace(interpreter+" "+arg)
	switch {
	case !filepath.IsAbs(interpreter):
		e.Kind, e.Detail = KindUnreadable, by+", a relative interpreter the kernel resolves in whatever directory the script is run from"
		return
	case depth >= maxDepth:
		e.Kind, e.Detail = KindUnreadable, nestedTooDeep
		return
	}
	i := c.classify(interpreter, "", depth+1)
	names := []string{filepath.Base(interpreter), filepath.Base(i.Resolves)}
	named, err := arg, error(nil)
	if !strings.Contains(arg, "/") {
		named, err = lookPath(arg, c.Expand.Path)
	}
	switch env := i.Kind == KindNative && (names[0] == "env" || names[1] == "env"); {
	case i.Kind == KindMissing:
		e.Kind, e.Detail = KindScript, by+", which does not exist, so the script cannot run"
	case i.Kind == KindNative && (modelledShells[names[0]] || modelledShells[names[1]]) && (arg == "" || shellFlags(arg, false)):
		c.wrapper(e, path, interpreter, depth)
	case env && (arg == "" || strings.HasPrefix(arg, "-") || strings.ContainsAny(arg, "= \t")):
		e.Kind, e.Detail = KindUnreadable, by+", whose argument this scan does not read as one program name"
	case env && err == nil:
		c.interpreted(e, path, named, "", depth+1)
	case env:
		e.Kind, e.Detail = KindUnreadable, by+", a name the scan's PATH ("+c.Expand.Path+") does not settle: "+err.Error()
	default:
		e.Kind, e.Detail = KindUnreadable, by+", an interpreter (or option) this scan does not read"
	}
}

// wrapperLimit is the most of a shell script read for what it runs; a larger one is not a
// wrapper this scan can judge.
const wrapperLimit = 1 << 20

// wrapper judges a shell script by what it runs, reading it as a program under the grammar
// shell.go allows (judge): it is unreadable when any word or construct in it cannot be judged. It
// runs where its caller runs, so a relative word in it is unreadable, and its positional
// parameters are the arguments its caller passed.
func (c Classifier) wrapper(e *Executable, path, interpreter string, depth int) {
	e.Kind, e.Detail = KindScript, "a shell script run by "+interpreter+" that runs nothing this scan cannot judge"
	body, err := readLimited(path, wrapperLimit)
	switch {
	case depth >= maxDepth:
		e.Kind, e.Detail = KindUnreadable, nestedTooDeep
		return
	case err != nil:
		e.Kind, e.Detail = KindUnreadable, "the shell script could not be read: "+store.PythonOSError(err)
		return
	case len(body) > wrapperLimit:
		e.Kind, e.Detail = KindUnreadable, "a shell script larger than "+strconv.Itoa(wrapperLimit)+" bytes, which this scan does not read through"
		return
	}
	unresolved := ""
	argvJudge{c: c, depth: depth + 1, report: func(word string, inner Executable) {
		if inner.Kind == KindUnreadable && unresolved == "" {
			unresolved = "a shell script this scan cannot read through at " + strconv.Quote(word) + ": " + inner.Detail
		}
	}}.program(string(body))
	if unresolved != "" {
		e.Kind, e.Detail = KindUnreadable, unresolved
	}
}

// sourced is what a shell reading path as its program (sh FILE) runs: the file judged as a
// shell script whatever its #! line names, unless it is not a script at all.
func (c Classifier) sourced(path string, depth int) Executable {
	e := c.classify(path, "", depth)
	head, err := readHead(e.Resolves)
	if err != nil || native(head) || e.Kind == KindMissing || e.Kind == KindDirectory || e.Kind == KindGoRuntime {
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

// Expander is what a shell program's words are made with: a leading ~ or ~/ (HOME) and $NAME or
// ${NAME} for the names in Vars (shell.go). Path is the PATH a bare command is looked up in.
type Expander struct {
	Vars map[string]string
	Path string
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

// headSize is how much of a file the kernel reads for its #! line (BINPRM_BUF_SIZE).
const headSize = 256

func readHead(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head := make([]byte, headSize)
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

// RuntimeKind is what a runtime directory is: a Go runtime, or unknown.
func RuntimeKind(directory string) string {
	if isNative(filepath.Join(directory, "bin", "crw")) {
		return KindGoRuntime
	}
	return "unknown"
}
