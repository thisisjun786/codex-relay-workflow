package doctor

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// A shell program (a hook command, a wrapper script, an sh -c string) is parsed with
// mvdan.cc/sh/v3/syntax (docs/port/decisions.md 37) and read for the words the shell would
// execute. The grammar is Bash's: Codex runs a hook command through a shell this scan cannot
// name (dash is /bin/sh on the relay host, bash the login shell), and a wrapper names its own
// interpreter. Bash's grammar is a superset of the POSIX one those shells share, so every
// command either shell would run is read; the constructs only Bash accepts ([[ ]], process
// substitution, arrays, `function`) only add commands to judge, and a program neither accepts
// is a parse error, reported as unreadable.
//
// On top of the syntax tree this file decides the role of each word: a command position (a
// call's first word after its assignments), an argument, or a script a shell reads (sh FILE,
// . FILE). It follows the builtins and programs that run a command they are given (exec,
// command, time, env, nohup, nice, setsid, stdbuf, timeout), reads sh -c strings, eval words,
// trap actions, here-documents fed to a shell, and command and process substitutions as
// programs, and judges a function by its body. Anything it does not model is reported to
// unreadable rather than dropped.

// How a word of a shell program reaches exec.
const (
	roleArgument = iota // an argument some command receives
	roleCommand         // a command position: a builtin, a function, else a PATH lookup
	roleScript          // a file a shell reads as its program (sh FILE, . FILE)
)

// maxShellDepth bounds how far sh -c strings, eval, trap actions and command substitutions are
// followed inside one program.
const maxShellDepth = 4

// shellWord is one word of a shell program as the scan judges it.
type shellWord struct {
	Written string // quotes removed, expansions as written: what a report names
	Value   string // with the expansions the scan makes; complete only when Missing is ""
	Missing string // the first expansion that could not be made, as written
}

// shellBuiltins are the builtins that run no program by name. A builtin is found before PATH,
// so echo, test, true and kill here are never the files of that name.
var shellBuiltins = setOf(": true false cd pwd echo printf test [ export readonly local declare typeset set unset shift exit return break continue read wait umask ulimit alias unalias hash type times getopts jobs bg fg kill shopt pushd popd dirs let history logout disown suspend enable help bind caller compgen complete compopt mapfile readarray fc")

// prefixSpec is a command that runs the command named after its options: a builtin (exec,
// command, builtin) or a program (env, nohup, ...). time is a reserved word the parser reads.
type prefixSpec struct {
	builtin     bool
	flags       map[string]bool // options without a value
	valued      map[string]bool // options whose value is the next word
	joined      []string        // option prefixes with the value attached (-n10, --signal=KILL)
	listOnly    map[string]bool // options after which nothing is run (command -v)
	split       []string        // option prefixes whose value is a command line this reader does not split (env -S)
	numeric     bool            // -N is an option (nice -10)
	operands    int             // words before the command (timeout's duration)
	assignments bool            // NAME=value words before the command (env)
}

var prefixes = map[string]prefixSpec{
	"exec":    {builtin: true, flags: setOf("-c -l"), valued: setOf("-a")},
	"command": {builtin: true, flags: setOf("-p"), listOnly: setOf("-v -V")},
	"builtin": {builtin: true},
	"env":     {flags: setOf("-i - -0 -v --ignore-environment --null --debug"), valued: setOf("-u --unset -C --chdir"), joined: strings.Fields("-u -C --unset= --chdir="), split: strings.Fields("-S --split-string"), assignments: true},
	"nohup":   {},
	"setsid":  {flags: setOf("-c -f -w --ctty --fork --wait")},
	"nice":    {valued: setOf("-n --adjustment"), joined: strings.Fields("-n --adjustment="), numeric: true},
	"stdbuf":  {valued: setOf("-i -o -e --input --output --error"), joined: strings.Fields("-i -o -e --input= --output= --error=")},
	"timeout": {flags: setOf("--foreground --preserve-status -v --verbose"), valued: setOf("-s --signal -k --kill-after"), joined: strings.Fields("-s -k --signal= --kill-after="), operands: 1},
}

func setOf(words string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.Fields(words) {
		set[w] = true
	}
	return set
}

// What one option of a prefix command is.
const (
	optionFlag = iota
	optionValued
	optionListOnly
	optionUnknown
)

func (p prefixSpec) option(word string) int {
	switch {
	case p.listOnly[word]:
		return optionListOnly
	case hasAnyPrefix(word, p.split):
		return optionUnknown
	case p.flags[word]:
		return optionFlag
	case p.valued[word]:
		return optionValued
	case p.numeric && len(word) > 1 && digits(word[1:]):
		return optionFlag
	}
	for _, prefix := range p.joined {
		if strings.HasPrefix(word, prefix) && len(word) > len(prefix) {
			return optionFlag
		}
	}
	return optionUnknown
}

func hasAnyPrefix(word string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if strings.HasPrefix(word, prefix) {
			return true
		}
	}
	return false
}

func digits(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] < '0' || text[i] > '9' {
			return false
		}
	}
	return text != ""
}

// shellWalker reads shell programs and reports each word the shell would pass to exec, with its
// role, to visit (which returns false to stop), and each construct it cannot read to
// unreadable. expand makes a word's expansions. Functions a program defines are remembered
// across the programs it reads.
type shellWalker struct {
	expand     Expander
	visit      func(word shellWord, role int) bool
	unreadable func(value, detail string)
	functions  map[string]bool
	stopped    bool
}

func (w *shellWalker) walk(program string, depth int) {
	if w.stopped {
		return
	}
	if depth > maxShellDepth {
		w.unreadable(snippet(program), nestedTooDeep)
		return
	}
	if w.functions == nil {
		w.functions = map[string]bool{}
	}
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(program), "")
	if err != nil {
		w.unreadable(snippet(program), "a shell program this scan cannot parse ("+err.Error()+"), so its commands cannot be told apart")
		return
	}
	(&shellReader{w: w, src: program, depth: depth}).stmts(file.Stmts)
}

var nestedTooDeep = "a shell program nested more than " + strconv.Itoa(maxShellDepth) + " deep in sh -c strings, eval, trap actions or command substitutions, which this scan does not read"

// snippet is a program as an unreadable entry names it: its first 80 characters.
func snippet(program string) string {
	runes := []rune(strings.TrimSpace(program))
	if len(runes) > 80 {
		return string(runes[:80]) + "..."
	}
	return string(runes)
}

// shellReader reads one parsed program; src is the text its positions index.
type shellReader struct {
	w     *shellWalker
	src   string
	depth int
}

// simple is the simple command being read.
type simple struct {
	name        string
	prefix      *prefixSpec
	prefixName  string
	operands    int
	optionValue bool
	optionsDone bool
	shell       *shellCall
	eval        bool
	evalWords   []string
	trap        bool
	trapDone    bool
	source      bool
}

// shellCall is a shell invoked as a command: whether it was given -c or -s, and whether the
// program it runs has been read.
type shellCall struct {
	c, s, operands, done, optionValue bool
}

func (r *shellReader) source(node syntax.Node) string {
	start, end := int(node.Pos().Offset()), int(node.End().Offset())
	end = min(end, len(r.src))
	if start < 0 || start > end {
		return ""
	}
	return r.src[start:end]
}

func (r *shellReader) stmts(list []*syntax.Stmt) {
	for _, s := range list {
		if r.w.stopped {
			return
		}
		r.stmt(s)
	}
}

func (r *shellReader) stmt(s *syntax.Stmt) {
	if s == nil {
		return
	}
	var stdin *string
	for _, redirect := range s.Redirs {
		switch redirect.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			body := r.heredoc(redirect.Hdoc)
			stdin = &body
		case syntax.WordHdoc:
			text := r.word(redirect.Word).Written
			stdin = &text
		}
	}
	fed := r.command(s.Cmd, stdin)
	for _, redirect := range s.Redirs {
		if fed && (redirect.Op == syntax.Hdoc || redirect.Op == syntax.DashHdoc || redirect.Op == syntax.WordHdoc) {
			continue // read as the shell's program
		}
		r.substitutions(redirect.Word)
		r.substitutions(redirect.Hdoc)
	}
}

// heredoc is a here-document's text: its literal parts as they stand and its expansions as
// written.
func (r *shellReader) heredoc(body *syntax.Word) string {
	if body == nil {
		return ""
	}
	var text strings.Builder
	for _, part := range body.Parts {
		if lit, ok := part.(*syntax.Lit); ok {
			text.WriteString(lit.Value)
		} else {
			text.WriteString(r.source(part))
		}
	}
	return text.String()
}

// command reads one command and returns whether stdin was read as a shell's program.
func (r *shellReader) command(cmd syntax.Command, stdin *string) bool {
	switch c := cmd.(type) {
	case nil:
	case *syntax.CallExpr:
		return r.call(c, stdin)
	case *syntax.IfClause:
		for clause := c; clause != nil; clause = clause.Else {
			r.stmts(clause.Cond)
			r.stmts(clause.Then)
		}
	case *syntax.WhileClause:
		r.stmts(c.Cond)
		r.stmts(c.Do)
	case *syntax.ForClause:
		if loop, ok := c.Loop.(*syntax.WordIter); ok {
			for _, item := range loop.Items {
				r.argument(item)
			}
		} else {
			r.w.unreadable(r.source(c.Loop), "an arithmetic for loop, whose evaluation this scan does not read")
			r.substitutions(c.Loop)
		}
		r.stmts(c.Do)
	case *syntax.CaseClause:
		r.argument(c.Word)
		for _, item := range c.Items {
			for _, pattern := range item.Patterns {
				r.substitutions(pattern)
			}
			r.stmts(item.Stmts)
		}
	case *syntax.Block:
		r.stmts(c.Stmts)
	case *syntax.Subshell:
		r.stmts(c.Stmts)
	case *syntax.BinaryCmd:
		r.stmt(c.X)
		r.stmt(c.Y)
	case *syntax.FuncDecl:
		if c.Name != nil {
			r.w.functions[c.Name.Value] = true
		}
		for _, name := range c.Names {
			r.w.functions[name.Value] = true
		}
		r.stmt(c.Body)
	case *syntax.ArithmCmd, *syntax.LetClause:
		r.w.unreadable(r.source(c), "an arithmetic command, whose evaluation this scan does not read")
		r.substitutions(c)
	case *syntax.TestClause:
		syntax.Walk(c.X, func(n syntax.Node) bool {
			if word, ok := n.(*syntax.Word); ok {
				r.argument(word)
				return false
			}
			return true
		})
	case *syntax.DeclClause:
		for _, assign := range c.Args {
			r.assign(assign)
		}
	case *syntax.TimeClause:
		r.stmt(c.Stmt)
	case *syntax.CoprocClause:
		r.stmt(c.Stmt)
	default:
		r.w.unreadable(r.source(c), fmt.Sprintf("a %T command, which this scan does not read", c))
	}
	return false
}

// substitutions reads the command and process substitutions anywhere under node as programs.
func (r *shellReader) substitutions(node syntax.Node) {
	if word, ok := node.(*syntax.Word); node == nil || (ok && word == nil) {
		return
	}
	syntax.Walk(node, func(n syntax.Node) bool {
		switch n := n.(type) {
		case *syntax.CmdSubst:
			r.nested(n, n.Stmts)
			return false
		case *syntax.ProcSubst:
			r.nested(n, n.Stmts)
			return false
		}
		return true
	})
}

// nested reads statements parsed inside this program one level deeper.
func (r *shellReader) nested(node syntax.Node, stmts []*syntax.Stmt) {
	if r.w.stopped {
		return
	}
	if r.depth+1 > maxShellDepth {
		r.w.unreadable(snippet(r.source(node)), nestedTooDeep)
		return
	}
	(&shellReader{w: r.w, src: r.src, depth: r.depth + 1}).stmts(stmts)
}

func (r *shellReader) visit(word shellWord, role int) {
	if word.Written == "" || r.w.stopped {
		return
	}
	if !r.w.visit(word, role) {
		r.w.stopped = true
	}
}

// argument reads a word some command receives.
func (r *shellReader) argument(word *syntax.Word) {
	if word == nil {
		return
	}
	r.substitutions(word)
	r.visit(r.word(word), roleArgument)
}

func (r *shellReader) assign(a *syntax.Assign) {
	if a.Value != nil {
		r.argument(a.Value)
	}
	if a.Array != nil {
		for _, elem := range a.Array.Elems {
			r.argument(elem.Value)
		}
	}
	if a.Index != nil {
		r.substitutions(a.Index)
	}
}

// call reads a simple command and returns whether stdin was read as a shell's program.
func (r *shellReader) call(call *syntax.CallExpr, stdin *string) bool {
	for _, a := range call.Assigns {
		r.assign(a)
	}
	c := &simple{}
	for i, arg := range call.Args {
		if r.w.stopped {
			return false
		}
		r.substitutions(arg)
		word := r.word(arg)
		if i == 0 {
			c.name = word.Written
			r.commandWord(c, word)
			continue
		}
		r.operand(c, word, assignment(arg))
	}
	switch {
	case c.eval && len(c.evalWords) > 0:
		r.w.walk(strings.Join(c.evalWords, " "), r.depth+1)
	case c.shell != nil && !c.shell.done:
		if stdin != nil {
			r.w.walk(*stdin, r.depth+1)
			return true
		}
		r.w.unreadable(c.name, "a shell that reads its program from its standard input, which this scan cannot see")
	}
	return false
}

// assignment reports whether a word is written NAME=value with an unquoted NAME.
func assignment(word *syntax.Word) bool {
	if len(word.Parts) == 0 {
		return false
	}
	lit, ok := word.Parts[0].(*syntax.Lit)
	if !ok {
		return false
	}
	eq := strings.IndexByte(lit.Value, '=')
	return eq > 0 && validName(lit.Value[:eq])
}

// commandWord dispatches the word the current simple command runs.
func (r *shellReader) commandWord(c *simple, word shellWord) {
	if word.Missing != "" {
		r.visit(word, roleCommand) // what it names is unknown: the visitor reports it
		return
	}
	name := word.Value
	base := filepath.Base(name)
	spec, prefixed := prefixes[name]
	if !prefixed {
		if spec, prefixed = prefixes[base]; prefixed && spec.builtin {
			prefixed = false // a path names a file, never a builtin
		}
	}
	switch {
	case r.w.functions[name]:
		// a function this program defines, read where it was defined
	case name == "eval":
		c.eval = true
	case name == "trap":
		c.trap = true
	case name == "." || name == "source":
		c.source = true
	case prefixed:
		if !spec.builtin {
			r.visit(word, roleCommand)
		}
		c.prefix, c.prefixName, c.operands, c.optionsDone, c.optionValue = &spec, base, spec.operands, false, false
	case shellBuiltins[name]:
		// runs nothing by name
	default:
		r.visit(word, roleCommand)
		if shells[base] {
			c.name, c.shell = word.Written, &shellCall{}
		}
	}
}

// operand reads a word after the command word.
func (r *shellReader) operand(c *simple, word shellWord, assign bool) {
	switch {
	case c.prefix != nil:
		r.prefixWord(c, word, assign)
	case c.eval:
		c.evalWords = append(c.evalWords, word.Written)
	case c.trap && !c.trapDone:
		if word.Written == "--" || (strings.HasPrefix(word.Written, "-") && word.Written != "-") {
			return // -p and -l list traps; -- ends the options
		}
		c.trapDone = true
		if word.Written != "-" {
			r.w.walk(word.Written, r.depth+1)
		}
	case c.source:
		c.source = false
		r.visit(word, roleScript)
	case c.shell != nil && !c.shell.done:
		r.shellWord(c, word)
	default:
		r.visit(word, roleArgument)
	}
}

func (r *shellReader) prefixWord(c *simple, word shellWord, assign bool) {
	spec, text := c.prefix, word.Written
	switch {
	case c.optionValue:
		c.optionValue = false
		r.visit(word, roleArgument)
		return
	case !c.optionsDone && text == "--":
		c.optionsDone = true
		return
	case !c.optionsDone && strings.HasPrefix(text, "-") && (text != "-" || spec.flags["-"]):
		switch spec.option(text) {
		case optionValued:
			c.optionValue = true
		case optionListOnly:
			c.prefix = nil // nothing runs; the rest are arguments
		case optionUnknown:
			r.w.unreadable(text, "an option of "+c.prefixName+" this scan does not read, so the command it runs cannot be told apart")
			c.prefix = nil
		}
		return
	case spec.assignments && assign:
		_, written, _ := strings.Cut(word.Written, "=")
		_, value, _ := strings.Cut(word.Value, "=")
		r.visit(shellWord{Written: written, Value: value, Missing: word.Missing}, roleArgument)
		return
	case c.operands > 0:
		c.operands--
		r.visit(word, roleArgument)
		return
	}
	c.prefix = nil
	r.commandWord(c, word)
}

// shellWord reads the options and operands of a shell run as a command: with -c the first
// operand is the program it runs; without -c or -s it is the script file it reads.
func (r *shellReader) shellWord(c *simple, word shellWord) {
	s, text := c.shell, word.Written
	switch {
	case s.optionValue:
		s.optionValue = false
	case !s.operands && (text == "--" || text == "-"):
		s.operands = true
	case !s.operands && len(text) > 1 && (text[0] == '-' || text[0] == '+'):
		switch {
		case text == "--rcfile" || text == "--init-file":
			s.optionValue = true
		case strings.HasPrefix(text, "--"):
		default:
			for _, letter := range text[1:] {
				switch letter {
				case 'c':
					s.c = true
				case 's':
					s.s = true
				case 'o', 'O':
					s.optionValue = true
				}
			}
		}
	default:
		s.operands = true
		switch {
		case s.c:
			s.done = true
			r.w.walk(text, r.depth+1)
		case s.s:
			r.visit(word, roleArgument)
		default:
			s.done = true
			r.visit(word, roleScript)
		}
	}
}

// word renders a word: as written (quotes removed, each expansion as its source) and with the
// expansions the scan makes: a leading ~ or ~/ (HOME), and $NAME or ${NAME} for the names the
// expander holds. Any other expansion (a command substitution, an arithmetic expansion, a
// parameter expansion with an operator, an unknown or empty name, $'...') is Missing.
func (r *shellReader) word(word *syntax.Word) shellWord {
	var written, value strings.Builder
	missing := ""
	unmade := func(source string) {
		written.WriteString(source)
		if missing == "" {
			missing = source
		}
	}
	var part func(p syntax.WordPart, first, double bool)
	part = func(p syntax.WordPart, first, double bool) {
		switch p := p.(type) {
		case *syntax.Lit:
			text := unquote(p.Value, double)
			written.WriteString(text)
			if first && !double && strings.HasPrefix(text, "~") {
				name, rest, slash := strings.Cut(text[1:], "/")
				home := r.w.expand.Vars["HOME"]
				if name != "" || home == "" {
					if missing == "" {
						missing = "~" + name
					}
					return
				}
				value.WriteString(home)
				if slash {
					value.WriteString("/" + rest)
				}
				return
			}
			value.WriteString(text)
		case *syntax.SglQuoted:
			if p.Dollar {
				unmade(r.source(p))
				return
			}
			written.WriteString(p.Value)
			value.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, inner := range p.Parts {
				part(inner, false, true)
			}
		case *syntax.ParamExp:
			source := r.source(p)
			if plainParameter(p) {
				if v := r.w.expand.Vars[p.Param.Value]; v != "" {
					written.WriteString(source)
					value.WriteString(v)
					return
				}
			}
			unmade(source)
		default:
			unmade(r.source(p))
		}
	}
	if word != nil {
		for i, p := range word.Parts {
			part(p, i == 0, false)
		}
	}
	return shellWord{Written: written.String(), Value: value.String(), Missing: missing}
}

// plainParameter is $NAME or ${NAME}: a name and no operator.
func plainParameter(p *syntax.ParamExp) bool {
	return p.Param != nil && validName(p.Param.Value) && p.Flags == nil && !p.Excl && !p.Length &&
		!p.Width && !p.IsSet && p.NestedParam == nil && p.Index == nil && len(p.Modifiers) == 0 &&
		p.Slice == nil && p.Repl == nil && p.Names == 0 && p.Exp == nil
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

// shellWords is every simple command's words as written, one list per command, in the order
// the program holds them (substitutions after the command they sit in). It is the reader's
// view of how a program splits into words.
func shellWords(program string) ([][]string, error) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(program), "")
	if err != nil {
		return nil, err
	}
	r := &shellReader{w: &shellWalker{}, src: program}
	var out [][]string
	syntax.Walk(file, func(n syntax.Node) bool {
		if call, ok := n.(*syntax.CallExpr); ok {
			var words []string
			for _, arg := range call.Args {
				words = append(words, r.word(arg).Written)
			}
			out = append(out, words)
		}
		return true
	})
	return out, nil
}
