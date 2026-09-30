package doctor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// A shell program (a hook command, a wrapper script, an sh -c string) is parsed with
// mvdan.cc/sh/v3/syntax (docs/port/decisions.md 37) and judged only as far as it is written in
// a small grammar this file understands completely (docs/runtime-install.md "What remove
// reads"): simple commands joined by ;, &, &&, ||, |, |& and newlines; words that are literal
// once the scan's expansions are made; redirections to such words. Every other construct - a
// function, an assignment, a compound command, a here-document, a substitution, a glob or brace
// pattern - is reported as unreadable, never interpreted: shell semantics read statically have
// no bound, and crw install remove deletes a runtime behind this reading's answer. The grammar
// is Bash's, a superset of the POSIX one the shells that run a hook share, so a program either
// would run parses; one the parser rejects is unreadable.

// maxDepth bounds how far sh -c programs, shell scripts and #! interpreters are followed from
// one reference.
const maxDepth = 4

var nestedTooDeep = "a program nested more than " + strconv.Itoa(maxDepth) + " deep in sh -c strings, shell scripts and #! interpreters, which this scan does not follow"

// shellWord is one word as the scan judges it.
type shellWord struct {
	Written    string // quotes removed, expansions as written: what a report names
	Value      string // with the scan's expansions made; complete only when Missing is ""
	Missing    string // the first part that keeps the word from being literal, as written
	Positional bool   // exactly one positional parameter ($@, $*, $1, ...): the program's own arguments
}

// literal is a word no shell reads (an MCP server's command or argument).
func literal(text string) shellWord { return shellWord{Written: text, Value: text} }

// grammar reads one parsed program: its simple commands, and every construct outside the
// grammar as (source, what it is).
type grammar struct {
	src     string
	x       Expander
	argvs   [][]shellWord
	refused [][2]string
}

func parse(program string, x Expander) ([][]shellWord, [][2]string) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(program), "")
	if err != nil {
		return nil, [][2]string{{snippet(program), "a shell program this scan cannot parse (" + err.Error() + "), so its commands cannot be told apart"}}
	}
	g := &grammar{src: program, x: x}
	for _, s := range file.Stmts {
		g.stmt(s)
	}
	return g.argvs, g.refused
}

// snippet is a program as an unreadable entry names it: its first 80 characters.
func snippet(program string) string {
	runes := []rune(strings.TrimSpace(program))
	if len(runes) > 80 {
		return string(runes[:80]) + "..."
	}
	return string(runes)
}

func (g *grammar) source(node syntax.Node) string {
	start, end := int(node.Pos().Offset()), min(int(node.End().Offset()), len(g.src))
	if start < 0 || start > end {
		return ""
	}
	return g.src[start:end]
}

func (g *grammar) refuse(node syntax.Node, what string) {
	g.refused = append(g.refused, [2]string{snippet(g.source(node)), what + ", which this scan does not read"})
}

func (g *grammar) stmt(s *syntax.Stmt) {
	for _, r := range s.Redirs {
		if r.Hdoc != nil || r.Op == syntax.WordHdoc {
			g.refuse(r, "a here-document, a program's input this scan does not read")
		} else if w := g.word(r.Word); w.Missing != "" {
			g.refuse(r, "a redirection to "+w.Written+", which needs "+w.Missing)
		}
	}
	switch c := s.Cmd.(type) {
	case nil:
	case *syntax.CallExpr:
		if len(c.Assigns) > 0 {
			g.refuse(c.Assigns[0], "an assignment, which changes what later words name")
			return
		}
		var argv []shellWord
		for _, arg := range c.Args {
			argv = append(argv, g.word(arg))
		}
		if argv != nil {
			g.argvs = append(g.argvs, argv)
		}
	case *syntax.BinaryCmd: // &&, ||, | and |& run commands from both sides
		g.stmt(c.X)
		g.stmt(c.Y)
	default: // a function, a compound command (if, case, a loop, a subshell, a { } group), a declaration, arithmetic
		g.refuse(c, "a "+strings.TrimPrefix(fmt.Sprintf("%T", c), "*syntax.")+", a construct outside simple commands, lists and pipelines")
	}
}

// word renders a word: as written (quotes removed, each expansion as its source) and with the
// expansions the scan makes: a leading ~ or ~/ (HOME) and $NAME or ${NAME} for the names the
// expander holds. Anything else (another parameter, an operator, a command, process or
// arithmetic substitution, $'...', an unquoted glob or brace pattern) is Missing, and a
// substitution, which runs a program, is refused besides.
func (g *grammar) word(word *syntax.Word) shellWord {
	var written, value strings.Builder
	missing := ""
	unmade := func(text string) {
		written.WriteString(text)
		if missing == "" {
			missing = text
		}
	}
	var part func(p syntax.WordPart, first, last, quoted bool)
	part = func(p syntax.WordPart, first, last, quoted bool) {
		switch p := p.(type) {
		case *syntax.Lit:
			text := unquote(p.Value, quoted)
			switch {
			case !quoted && pattern(p.Value):
				unmade(text)
			case first && !quoted && strings.HasPrefix(text, "~"):
				name, rest, slash := strings.Cut(text[1:], "/")
				home := g.x.Vars["HOME"]
				if name != "" || home == "" || (!slash && !last) {
					unmade(text)
					return
				}
				written.WriteString(text)
				value.WriteString(home)
				if slash {
					value.WriteString("/" + rest)
				}
			default:
				written.WriteString(text)
				value.WriteString(text)
			}
		case *syntax.SglQuoted:
			if p.Dollar {
				unmade(g.source(p))
				return
			}
			written.WriteString(p.Value)
			value.WriteString(p.Value)
		case *syntax.DblQuoted:
			if p.Dollar {
				unmade(g.source(p))
				return
			}
			for _, inner := range p.Parts {
				part(inner, false, false, true)
			}
		case *syntax.ParamExp:
			if plain(p) && validName(p.Param.Value) && g.x.Vars[p.Param.Value] != "" {
				v := g.x.Vars[p.Param.Value]
				written.WriteString(g.source(p))
				value.WriteString(v)
				return
			}
			unmade(g.source(p))
		case *syntax.CmdSubst, *syntax.ProcSubst:
			g.refuse(p, "a command or process substitution, which runs a program")
			unmade(g.source(p))
		default:
			unmade(g.source(p))
		}
	}
	if word == nil {
		return shellWord{}
	}
	for i, p := range word.Parts {
		part(p, i == 0, i == len(word.Parts)-1, false)
	}
	return shellWord{Written: written.String(), Value: value.String(), Missing: missing, Positional: positional(word)}
}

// pattern reports whether an unquoted literal holds a glob or brace pattern character that a
// backslash does not escape.
func pattern(raw string) bool {
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '\\':
			i++
		case '*', '?', '[', '{':
			return true
		}
	}
	return false
}

// positional reports whether a word is one positional parameter ($@, "$@", $*, $0, $1, ${1}).
// Every index is guarded: an empty quoted word ("", or two single quotes) holds no part.
func positional(word *syntax.Word) bool {
	parts := word.Parts
	if len(parts) == 1 {
		if q, ok := parts[0].(*syntax.DblQuoted); ok && !q.Dollar {
			parts = q.Parts
		}
	}
	if len(parts) != 1 {
		return false
	}
	p, ok := parts[0].(*syntax.ParamExp)
	return ok && plain(p) && strings.Trim(p.Param.Value, "@*0123456789") == ""
}

// plain is $NAME or ${NAME}: a parameter and no operator.
func plain(p *syntax.ParamExp) bool {
	return p.Param != nil && p.Flags == nil && !p.Excl && !p.Length && !p.Width && !p.IsSet &&
		p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 && p.Slice == nil &&
		p.Repl == nil && p.Names == 0 && p.Exp == nil
}

// unquote is a literal's text as the shell reads it: outside quotes a backslash takes the next
// character literally; inside double quotes only before $, `, ", \ or a newline.
func unquote(text string, double bool) string {
	if !strings.Contains(text, `\`) {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) && (!double || strings.IndexByte("$`\"\\\n", text[i+1]) >= 0) {
			i++
		}
		out.WriteByte(text[i])
	}
	return out.String()
}

// argvJudge decides what each command of a program runs. cwd is where a relative word resolves,
// "" when that is unknown (a hook runs in the session's workspace). report receives each
// Python reference (e.Python) and each word or construct that cannot be judged (KindUnreadable),
// with the word it concerns. seen, when set, receives every command judged, with what its
// first word runs.
type argvJudge struct {
	c      Classifier
	cwd    string
	depth  int
	report func(word string, e Executable)
	seen   func(argv []shellWord, command Executable)
}

func (j argvJudge) unreadable(word, detail string) {
	j.report(word, Executable{Value: word, Kind: KindUnreadable, Detail: detail})
}

// program judges every simple command of a shell program.
func (j argvJudge) program(text string) {
	defer j.recovered(text)
	if j.depth > maxDepth {
		j.unreadable(snippet(text), nestedTooDeep)
		return
	}
	argvs, refused := parse(text, j.c.Expand)
	for _, r := range refused {
		j.unreadable(r[0], r[1])
	}
	for _, argv := range argvs {
		j.argv(argv, true)
	}
}

// recovered is deferred around the reading of one program or declaration (text): a failure of
// the scan's own reading of it (a panic) becomes an unreadable entry naming that text, never
// the end of the scan.
func (j argvJudge) recovered(text string) {
	if r := recover(); r != nil {
		j.unreadable(snippet(text), fmt.Sprintf("this scan's reading of it failed (%v), so what it runs is unknown", r))
	}
}

// Shells: those whose -c program or script operand is read (modelled), and those that are not.
var (
	modelledShells = setOf("sh bash dash")
	otherShells    = setOf("zsh ksh mksh ash busybox fish csh tcsh yash posh")
)

func setOf(words string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}

// argv judges one command: argv[0] is what runs, found as a shell (shell) or exec finds it.
// The builtins exit, true and : run nothing; exec runs the command after it; a Python program
// is a reference whatever its arguments; sh, bash and dash run their -c program or the script
// they read; env runs the command after its options; anything else runs what it is, and each
// of its arguments is judged as something it may run (argument).
func (j argvJudge) argv(argv []shellWord, shell bool) {
	if len(argv) == 0 {
		return
	}
	head := argv[0]
	if shell && head.Missing == "" {
		switch head.Value {
		case "exit", "true", ":":
			for _, w := range argv[1:] {
				if w.Missing != "" {
					j.unreadable(w.Written, "an argument of "+head.Value+" that needs "+w.Missing+", which this scan does not make")
				}
			}
			return
		case "exec":
			switch {
			case len(argv) == 1:
			case strings.HasPrefix(argv[1].Value, "-"):
				j.unreadable(argv[1].Written, "an option of exec, which this scan does not read")
			default:
				j.argv(argv[1:], false)
			}
			return
		}
	}
	e, ok := j.command(head)
	if !ok {
		return
	}
	if j.seen != nil {
		j.seen(argv, e)
	}
	names := []string{filepath.Base(head.Value), filepath.Base(e.Resolves)}
	switch {
	case e.Python:
		j.report(head.Written, e)
		for _, w := range argv[1:] {
			j.argument(w, true)
		}
	case e.Kind == KindUnreadable:
		j.report(head.Written, e)
	case e.Kind == KindOther:
		j.unreadable(head.Written, e.Detail+": exec refuses it and a shell runs it as a shell script, which this scan does not read")
	case e.Kind == KindMissing || e.Kind == KindDirectory || e.Kind == KindGoRuntime:
		// nothing runs
	case e.Kind == KindNative && (modelledShells[names[0]] || modelledShells[names[1]]):
		j.shell(head.Written, argv[1:])
	case e.Kind == KindNative && (otherShells[names[0]] || otherShells[names[1]]):
		j.unreadable(head.Written, "a shell whose programs this scan does not read")
	case e.Kind == KindNative && (names[0] == "env" || names[1] == "env"):
		j.env(argv[1:])
	default:
		for _, w := range argv[1:] {
			j.argument(w, false)
		}
	}
}

// command is what a command word runs: a literal absolute path, a relative one where the
// working directory is known, or a bare name found on PATH.
func (j argvJudge) command(w shellWord) (Executable, bool) {
	var path string
	switch {
	case w.Missing != "":
		j.unreadable(w.Written, "a command word that needs "+w.Missing+", an expansion this scan does not make, so what it runs is unknown")
		return Executable{}, false
	case !strings.Contains(w.Value, "/"):
		var found bool
		if path, found = j.lookup(w); path == "" {
			if !found {
				j.unreadable(w.Written, "names a command that no directory on the scan's PATH ("+j.c.Expand.Path+") holds as an executable file, so what it runs is unknown")
			}
			return Executable{}, false
		}
	default:
		var ok bool
		if path, ok = j.path(w, false); !ok {
			return Executable{}, false
		}
	}
	e := j.c.classify(path, "", j.depth)
	e.Value = w.Written
	return e, true
}

// path is where a literal word naming a file resolves. A relative one with no known working
// directory is a Python reference when it names Python and, unless quiet, unreadable.
func (j argvJudge) path(w shellWord, quiet bool) (string, bool) {
	switch {
	case filepath.IsAbs(w.Value):
		return w.Value, true
	case j.cwd != "":
		return filepath.Join(j.cwd, w.Value), true
	case strings.HasSuffix(w.Value, ".py"):
		j.report(w.Written, Executable{Value: w.Written, Kind: KindPythonScript, Python: true, Detail: "names a Python file by a relative path"})
	case PythonName(w.Value):
		j.report(w.Written, Executable{Value: w.Written, Kind: KindPythonInterpreter, Python: true, Detail: "names a Python interpreter by a relative path"})
	case !quiet:
		j.unreadable(w.Written, "a relative path, which resolves in whatever directory the program runs in, not the scan's")
	}
	return "", false
}

// lookup finds a bare name on PATH as the shell, exec or a runner does. A Python name PATH does
// not settle is a reference, and a PATH whose relative or empty directory comes first leaves
// the name unreadable; both return "" and true. A name no directory holds returns "", false.
func (j argvJudge) lookup(w shellWord) (string, bool) {
	found, err := lookPath(w.Value, j.c.Expand.Path)
	switch {
	case err == nil:
		return found, true
	case PythonName(w.Value):
		j.report(w.Written, Executable{Value: w.Written, Kind: KindPythonInterpreter, Python: true, Detail: "names a Python interpreter"})
	case errors.Is(err, errRelativePath):
		j.unreadable(w.Written, "names a program looked up on a PATH ("+j.c.Expand.Path+") whose relative or empty directory comes first, so what it names depends on the working directory")
	default:
		return "", false
	}
	return "", true
}

// programText is what makes a word read as program text rather than a name.
const programText = " \t\n;&|<>()$`\\\"'*?[]{}~#!"

// argument judges a word a program receives as something it may run: sudo, xargs, flock,
// timeout and uv run their arguments, and a wrapper passes "$@" on. A word naming a Python
// program is a reference. Under a program that is not Python, one that names a shell, a
// script this scan cannot read or an executable it cannot place, holds program text, or needs
// an expansion the scan does not make is unreadable; a program already judged Python
// (python) is reported and nothing about its arguments is unreadable. A positional parameter
// is the program's own arguments, judged where the program is run; an option is skipped, the
// value of --opt=VALUE judged.
func (j argvJudge) argument(w shellWord, python bool) {
	if w.Missing == "" && strings.HasPrefix(w.Value, "-") {
		_, value, ok := strings.Cut(w.Value, "=")
		if !ok {
			return
		}
		w = shellWord{Written: w.Written, Value: value}
	}
	text := w.Value
	var path string
	switch {
	case w.Positional:
		return
	case w.Missing != "":
		if strings.HasSuffix(w.Written, ".py") || PythonName(w.Written) {
			j.report(w.Written, Executable{Value: w.Written, Kind: KindPythonScript, Python: true, Detail: "names Python through " + w.Missing + ", an expansion this scan does not make"})
		} else if !python {
			j.unreadable(w.Written, "an argument that needs "+w.Missing+", an expansion this scan does not make, given to a program that may run it")
		}
		return
	case !python && assignment(text):
		j.unreadable(w.Written, "an assignment a program such as env or sudo makes, which changes what the command it runs finds")
		return
	case !python && strings.ContainsAny(text, programText):
		j.unreadable(w.Written, "program text given to a program this scan does not model, which may hand it to a shell")
		return
	case strings.Contains(text, "/") || strings.HasSuffix(text, ".py"):
		var ok bool
		if path, ok = j.path(w, python); !ok {
			return
		}
	case python:
		return
	default:
		if path, _ = j.lookup(w); path == "" {
			return // judged, or no program of that name
		}
	}
	e := j.c.classify(path, "", j.depth)
	e.Value = w.Written
	names := []string{filepath.Base(text), filepath.Base(e.Resolves)}
	switch {
	case e.Python || (e.Kind == KindUnreadable && !python):
		j.report(w.Written, e)
	case python:
	case e.Kind == KindOther && executable(e.Resolves):
		j.unreadable(w.Written, e.Detail+", given to a program that may run it")
	case e.Kind == KindNative && (modelledShells[names[0]] || modelledShells[names[1]] || otherShells[names[0]] || otherShells[names[1]]):
		j.unreadable(w.Written, "a shell given to a program this scan does not model, which may hand it a program")
	}
}

// assignment reports whether a word is NAME=value.
func assignment(text string) bool {
	name, _, ok := strings.Cut(text, "=")
	return ok && validName(name)
}

// executable is whether a program handed path could execute it: a regular file with an execute
// bit. execve(2) refuses anything else with EACCES, and no shell's ENOEXEC fallback reads it, so a
// socket, FIFO or device named as an argument - the App Server socket a bridge record passes with
// --socket, say - is never what the program runs, whatever its mode bits say.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0
}

// shellFlags reports whether an option word sets only flags that change nothing this scan
// judges (-e, -u, -x, -f), and -c where a program operand is allowed. -l and -i read startup
// files, -s reads standard input.
func shellFlags(text string, c bool) bool {
	letters := "euxf"
	if c {
		letters += "c"
	}
	return len(text) > 1 && text[0] == '-' && strings.Trim(text[1:], letters) == ""
}

// shell judges sh, bash or dash run as a command: the program after -c, or the script operand,
// which the shell reads as its program whatever its #! line says; the words after either are
// its positional parameters.
func (j argvJudge) shell(name string, args []shellWord) {
	c, i := false, 0
	for ; i < len(args) && args[i].Missing == "" && (strings.HasPrefix(args[i].Value, "-") || strings.HasPrefix(args[i].Value, "+")); i++ {
		if args[i].Value == "-" || args[i].Value == "--" {
			i++
			break
		}
		if !shellFlags(args[i].Value, true) {
			j.unreadable(args[i].Written, "an option of "+name+" this scan does not read (a login or interactive shell reads startup files, -s reads standard input)")
			return
		}
		c = c || strings.Contains(args[i].Value, "c")
	}
	operands := args[i:]
	switch {
	case len(operands) == 0:
		j.unreadable(name, "a shell given no program or script, which reads its program from standard input, which this scan cannot see")
		return
	case operands[0].Missing != "":
		j.unreadable(operands[0].Written, "a shell program or script that needs "+operands[0].Missing+", which this scan does not make")
	case c:
		next := j
		next.depth++
		next.program(operands[0].Value)
	default:
		if path, ok := j.path(operands[0], false); ok {
			e := j.c.sourced(path, j.depth+1)
			e.Value = operands[0].Written
			if e.Python || e.Kind == KindUnreadable {
				j.report(operands[0].Written, e)
			}
		}
	}
	for _, w := range operands[1:] {
		j.argument(w, false)
	}
}

// env judges env run as a command: after -i and -- it runs the command that follows. An
// assignment or any other option changes what that command finds, so it is unreadable, and
// after -i a bare command is looked up on a default PATH this scan does not model.
func (j argvJudge) env(args []shellWord) {
	cleared := false
	for i, w := range args {
		switch {
		case w.Missing == "" && (w.Value == "-i" || w.Value == "-" || w.Value == "--ignore-environment" || w.Value == "--"):
			cleared = cleared || w.Value != "--"
			continue
		case w.Missing != "" || strings.HasPrefix(w.Value, "-") || strings.Contains(w.Value, "="):
			j.unreadable(w.Written, "an option, assignment or expansion of env, which changes or hides the command it runs")
		case cleared && !strings.Contains(w.Value, "/"):
			j.unreadable(w.Written, "a bare command env -i looks up on a default PATH this scan does not model")
		default:
			j.argv(args[i:], false)
		}
		return
	}
}
