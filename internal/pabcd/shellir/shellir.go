// Package shellir reads one shell command text and lists the program
// executions it shows: where each program comes from, what its words evaluate
// to, and the context it runs in. A position where a shell or an interpreter
// would run code the text does not show is refused as Unreadable, so a caller
// that fails closed on Analyze's error never allows what it cannot see.
package shellir

import (
	"fmt"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// MaxCommandBytes is the largest command text the reader accepts. Text that a
// carrier re-reads, such as the string of bash -c or eval, counts against the
// same limit.
const MaxCommandBytes = 64 * 1024

// MaxNestingDepth bounds compound commands, substitutions, wrappers, function
// calls and carrier re-parses. Deeper text is unreadable.
const MaxNestingDepth = 32

// Stdin source kinds recorded in Context.Stdin.
const (
	StdinNone       = ""
	StdinPipe       = "pipe"
	StdinFile       = "file"
	StdinHeredoc    = "heredoc"
	StdinHerestring = "herestring"
	StdinUnknown    = "unknown"
)

// Kind tells a command run apart from a script file that a shell, source or
// sed -f reads and that the caller must judge with this package again.
type Kind int

const (
	// KindCommand is a program the text runs. An assignment-only statement
	// has an empty Name and carries its redirections.
	KindCommand Kind = iota
	// KindScriptFile names a script that a shell, source or sed -f reads.
	KindScriptFile
)

// Unreadable reports that the text cannot be read. Reason names the position.
type Unreadable struct {
	Reason string
}

func (e *Unreadable) Error() string { return "shell command unreadable: " + e.Reason }

func unreadablef(format string, args ...any) error {
	return &Unreadable{Reason: fmt.Sprintf(format, args...)}
}

// Word is one word after evaluation. Known is false when the value depends on
// something the reader cannot see; Reason then names what it depends on.
type Word struct {
	Known  bool
	Value  string
	Reason string
}

// Dir is the working directory at a point in the text. Known is false when
// the directory cannot be proven.
type Dir struct {
	Path  string
	Known bool
	// Unset is a reading made with no directory at all (AnalyzeNoDir): the directory-dependent judgments that need a directory
	// (a relative stdin alias) are left to the readings that are given one. A directory
	// that becomes unknown inside the text (cd "$X") is not Unset: it is unknown and those judgments refuse.
	Unset bool
}

// Assign is a variable assignment with its value when that value is known.
type Assign struct {
	Name   string
	Value  Word
	Append bool
	Array  bool
}

// Redir is one redirection. Op is the operator text (">", ">>", "<", ">&",
// "&>", "<<", "<<-", "<<<" and the rest). For a here-document, Target is the
// body; for a here-string, Target is the word with its trailing newline.
type Redir struct {
	Op     string
	Fd     string
	Target Word
}

// Context describes where an Exec sits in the text.
type Context struct {
	Conditional bool
	Background  bool
	Coprocess   bool
	Subshell    bool
	Pipeline    bool
	Loop        bool
	FuncBody    bool
	CmdSubst    bool
	ProcSubst   bool
	// Stdin is the source of standard input: one of the Stdin constants.
	Stdin string
	// stdinFile is the file the last input redirection names when Stdin is StdinFile and the reader knows it; inTextPipe is
	// whether the command is the right side of a pipe in the text being read (a carried text starts again at false).
	stdinFile  string
	inTextPipe bool
	// Carrier names the construct that re-read this text, for example "bash -c".
	Carrier string
	// RuntimeCarrier keeps the outer run-time wrapper through nested shells, even when it has no structured Feed.
	RuntimeCarrier string
	Depth   int
	// Feed says where the operands of a program that find or xargs runs come from; nil outside them.
	Feed *Feed
	// pipeSrc is what the left side of the pipe the command reads prints.
	pipeSrc *pipeSource
}

// Inline is the program text an interpreter receives on its command line or
// standard input. Language is python, node, perl, ruby, awk or sed.
type Inline struct {
	Language string
	Source   Word
}

// Exec is one program execution, or one script file that the caller judges.
type Exec struct {
	Kind    Kind
	Program Word
	Name    string
	Args    []Word
	Assigns []Assign
	Redirs  []Redir
	Dir     Dir
	Ctx     Context
	Inline  *Inline
	// Script is the script path of a KindScriptFile record.
	Script Word
}

// Result is the list of Exec records in the order the text runs them.
type Result struct {
	Execs []Exec
}

// Analyze reads a command text run in the working directory cwd. It returns
// an *Unreadable error for any text it cannot prove.

func parseText(src string) (*syntax.File, error) {
	p := syntax.NewParser(syntax.Variant(syntax.LangBash))
	file, err := p.Parse(strings.NewReader(src), "")
	if err != nil {
		return nil, unreadablef("parse: %v", err)
	}
	return file, nil
}

// Analyze reads a command text run in the working directory cwd.
// state is what the walk knows at one point.
type state struct {
	dir    Dir
	vars   map[string]string
	funcs  map[string]*syntax.Stmt
	cdpath bool
	lookup func(string) (string, bool)
}

func newState(cwd string) *state {
	return &state{
		dir:   Dir{Path: cwd, Known: cwd != ""},
		vars:  map[string]string{},
		funcs: map[string]*syntax.Stmt{},
	}
}

func (s *state) clone() *state {
	c := &state{
		dir:    s.dir,
		vars:   make(map[string]string, len(s.vars)),
		funcs:  make(map[string]*syntax.Stmt, len(s.funcs)),
		cdpath: s.cdpath,
		lookup: s.lookup,
	}
	for k, v := range s.vars {
		c.vars[k] = v
	}
	for k, v := range s.funcs {
		c.funcs[k] = v
	}
	return c
}

func (s *state) replace(o *state) { *s = *o }

func (s *state) clearVars() { s.vars = map[string]string{} }

func (s *state) unsetVar(name string) { delete(s.vars, name) }

func (s *state) setVar(name string, v Word, appendValue bool) {
	if !v.Known {
		delete(s.vars, name)
		return
	}
	if appendValue {
		if old, ok := s.vars[name]; ok {
			s.vars[name] = old + v.Value
		} else {
			delete(s.vars, name)
		}
		return
	}
	s.vars[name] = v.Value
}

// joinStates is the state after one of several branches runs. The directory
// and each variable survive only where every branch agrees.
func joinStates(states ...*state) *state {
	first := states[0]
	out := &state{
		dir:   Dir{Path: first.dir.Path, Known: true, Unset: true},
		vars:  map[string]string{},
		funcs: map[string]*syntax.Stmt{},
	}
	out.lookup = first.lookup
	for _, s := range states {
		if !s.dir.Known || !first.dir.Known || s.dir.Path != first.dir.Path {
			out.dir.Known = false
		}
		if !s.dir.Unset {
			out.dir.Unset = false // a branch that changed the directory leaves it unknown, not unset
		}
		if s.cdpath {
			out.cdpath = true
		}
		for k, v := range s.funcs {
			out.funcs[k] = v
		}
	}
	for k, v := range first.vars {
		same := true
		for _, s := range states[1:] {
			if sv, ok := s.vars[k]; !ok || sv != v {
				same = false
				break
			}
		}
		if same {
			out.vars[k] = v
		}
	}
	return out
}

// unknownDir is the directory after something that may have changed it (a cd to a place the reader cannot name, pushd, popd,
// a sourced file): it is not known, and it is no longer a reading with no directory (Dir.Unset).
func unknownDir(d Dir) Dir { return Dir{Path: d.Path} }

// notProvenDir is the directory where the reader gives up proving a state without having seen the directory change (after a
// loop, past a case arm that falls through, before a loop body that changes something): it is not known, but a reading with
// no directory at all stays one. A cd inside the loop or the arm is a change of its own and has made the directory unknown.
func notProvenDir(d Dir) Dir { return Dir{Path: d.Path, Unset: d.Unset} }

// walker collects Exec records in run order.
type walker struct {
	out   []Exec
	calls []string
	// created is the set of files the records out[:createdUpTo] write (see createdByText); it grows as the walk appends records, so
	// the check is linear in the text.
	created      map[string]bool
	createdTrees map[string]bool // directories a copy fills: any file below one is created by the text
	createdUpTo  int
	// pipeOut is what the last pipeline the walk finished prints (see stageSource).
	pipeOut *pipeSource
}

func (w *walker) stmts(list []*syntax.Stmt, st *state, ctx Context) error {
	for _, s := range list {
		if err := w.stmt(s, st, ctx); err != nil {
			return err
		}
	}
	return nil
}

func isCompound(c syntax.Command) bool {
	switch c.(type) {
	case *syntax.CallExpr, *syntax.TestClause, *syntax.ArithmCmd, *syntax.LetClause, *syntax.DeclClause:
		return false
	}
	return true
}

func (w *walker) stmt(s *syntax.Stmt, st *state, ctx Context) error {
	if s == nil {
		return nil
	}
	if s.Background {
		ctx.Background = true
		st = st.clone()
	}
	if s.Coprocess {
		ctx.Coprocess = true
		st = st.clone()
	}
	if isCompound(s.Cmd) {
		ctx.Depth++
		if ctx.Depth > MaxNestingDepth {
			return unreadablef("nesting is deeper than %d", MaxNestingDepth)
		}
	}
	redirs, err := w.redirects(s.Redirs, st, ctx)
	if err != nil {
		return err
	}
	ctx.Stdin = stdinKind(redirs, ctx.Stdin)
	switch {
	case ctx.Stdin != StdinFile:
		ctx.stdinFile = ""
	case stdinKind(redirs, "") != "":
		// An input redirection of its own replaces what an outer one gave: when it names no file the reader can name (a
		// variable, a descriptor alias) the command reads a file unknown, not the file the outer redirection named.
		f, _ := lastStdinFile(redirs, st.dir)
		ctx.stdinFile = f
	}
	if isCompound(s.Cmd) && len(redirs) > 0 {
		// A redirection on a compound command (a block, a subshell, a loop) writes its file as a command does: it
		// is an empty program with these redirections, judged with the state before the body runs.
		w.out = append(w.out, Exec{Kind: KindCommand, Program: Word{Known: true}, Redirs: redirs, Dir: st.dir, Ctx: ctx})
	}
	switch c := s.Cmd.(type) {
	case *syntax.CallExpr:
		return w.call(c, redirs, st, ctx)
	case *syntax.BinaryCmd:
		return w.binary(c, st, ctx)
	case *syntax.Subshell:
		sctx := ctx
		sctx.Subshell = true
		return w.stmts(c.Stmts, st.clone(), sctx)
	case *syntax.Block:
		return w.stmts(c.Stmts, st, ctx)
	case *syntax.IfClause:
		return w.ifClause(c, st, ctx)
	case *syntax.WhileClause:
		keepsDir := prescanLoop(st, c.Cond, c.Do)
		before := st.dir
		lctx := loopContext(ctx)
		if err := w.stmts(c.Cond, st, lctx); err != nil {
			return err
		}
		err := w.stmts(c.Do, st, lctx)
		afterLoop(st, before, keepsDir)
		return err
	case *syntax.ForClause:
		return w.forClause(c, st, ctx)
	case *syntax.CaseClause:
		return w.caseClause(c, st, ctx)
	case *syntax.FuncDecl:
		return w.funcDecl(c, st, ctx)
	case *syntax.ArithmCmd:
		if err := w.substsIn(c, st, ctx); err != nil {
			return err
		}
		st.clearVars()
		return nil
	case *syntax.TestClause:
		return w.substsIn(c, st, ctx)
	case *syntax.LetClause:
		if err := w.substsIn(c, st, ctx); err != nil {
			return err
		}
		st.clearVars()
		return nil
	case *syntax.DeclClause:
		return w.decl(c, st, ctx)
	case *syntax.TimeClause:
		if c.Stmt == nil {
			return nil
		}
		return w.stmt(c.Stmt, st, ctx)
	case *syntax.CoprocClause:
		cctx := ctx
		cctx.Coprocess = true
		return w.stmt(c.Stmt, st.clone(), cctx)
	}
	return unreadablef("unsupported command %T", s.Cmd)
}

// afterLoop makes the state unknown once a loop has run: the body may have run zero or many times, so the directory and the
// variables after it are not those of any one iteration (a cd in a loop body is unknown afterwards). A loop whose condition and
// body cannot change the directory (keepsDir, from prescanLoop) leaves it where it was before the loop (before): every iteration
// starts and ends there, so the directory after zero, one or many iterations is that one.
func afterLoop(st *state, before Dir, keepsDir bool) {
	if !keepsDir || st.dir != before {
		st.dir = notProvenDir(st.dir)
	}
	st.clearVars()
}

func loopContext(ctx Context) Context {
	ctx.Loop = true
	return ctx
}

// prescanLoop makes the state conservative before a loop body runs, because a
// later iteration sees what an earlier one changed. It reports whether the
// statements cannot change the directory (see changesDir); only then does the
// directory stay known through the loop.
func prescanLoop(st *state, lists ...[]*syntax.Stmt) (keepsDir bool) {
	changes, dir := false, changesDir(st, lists...)
	for _, list := range lists {
		for _, s := range list {
			syntax.Walk(s, func(n syntax.Node) bool {
				switch c := n.(type) {
				case *syntax.CallExpr:
					if len(c.Assigns) > 0 || changesStateCall(c, st) {
						changes = true
					}
				case *syntax.DeclClause, *syntax.LetClause, *syntax.ArithmCmd, *syntax.ForClause:
					changes = true
				}
				return true
			})
		}
	}
	if dir {
		st.dir = notProvenDir(st.dir)
	}
	if changes {
		st.clearVars()
	}
	return !dir
}

// changesDir reports statements that may change the directory of the shell that runs them: a cd, pushd or popd; anything
// that runs text or a builtin the reader does not see at this point (eval, source, ., trap, builtin, command, exec, an alias, a
// zsh precommand modifier, a function call, a command whose name is not a plain word); or any mention of CDPATH, which changes
// where a later cd goes. It looks inside nested statements, command substitutions and subshells too (it does not tell a subshell
// from the shell itself, so it may say yes where the directory cannot change, never the other way).
func changesDir(st *state, lists ...[]*syntax.Stmt) bool {
	changes := false
	for _, list := range lists {
		for _, s := range list {
			syntax.Walk(s, func(n syntax.Node) bool {
				switch c := n.(type) {
				case *syntax.Lit:
					if strings.Contains(c.Value, "CDPATH") {
						changes = true
					}
				case *syntax.CallExpr:
					if len(c.Args) > 0 && changesDirCall(c.Args[0].Lit(), st) {
						changes = true
					}
				}
				return !changes
			})
		}
	}
	return changes
}

func changesDirCall(name string, st *state) bool {
	if name == "" || strings.ContainsAny(name, `\$'"`+"`") {
		return true // a name the parser does not hand over as one plain word: the reader cannot say which command it is
	}
	if _, ok := st.funcs[name]; ok {
		return true
	}
	switch name {
	case "cd", "pushd", "popd", "eval", "source", ".", "trap", "builtin", "command", "exec",
		"alias", "unalias", "shopt", "enable", "noglob", "nocorrect", "-":
		return true
	}
	return false
}

func changesStateCall(c *syntax.CallExpr, st *state) bool {
	if len(c.Args) == 0 {
		return false
	}
	name := c.Args[0].Lit()
	if _, ok := st.funcs[name]; ok {
		return true
	}
	switch name {
	case "cd", "pushd", "popd", "read", "mapfile", "readarray", "getopts", "printf", "unset",
		"let", "export", "declare", "typeset", "local", "readonly", "eval", "source", ".",
		"trap", "set", "shift", "builtin", "command", "exec":
		return true
	}
	return false
}

func (w *walker) binary(c *syntax.BinaryCmd, st *state, ctx Context) error {
	switch c.Op {
	case syntax.AndStmt, syntax.OrStmt:
		if err := w.stmt(c.X, st, ctx); err != nil {
			return err
		}
		right := st.clone()
		rctx := ctx
		rctx.Conditional = true
		if err := w.stmt(c.Y, right, rctx); err != nil {
			return err
		}
		st.replace(joinStates(st, right))
		return nil
	case syntax.Pipe, syntax.PipeAll:
		lctx := ctx
		lctx.Pipeline = true
		before := len(w.out)
		if err := w.stmt(c.X, st.clone(), lctx); err != nil {
			return err
		}
		rctx := ctx
		rctx.Pipeline = true
		rctx.Stdin = StdinPipe
		rctx.inTextPipe = true
		rctx.pipeSrc = w.stageSource(c.X, before, st)
		beforeY := len(w.out)
		if err := w.stmt(c.Y, st.clone(), rctx); err != nil {
			return err
		}
		w.pipeOut = w.stageSource(c.Y, beforeY, st)
		return nil
	}
	return unreadablef("unsupported binary operator %v", c.Op)
}

func (w *walker) ifClause(c *syntax.IfClause, st *state, ctx Context) error {
	if err := w.stmts(c.Cond, st, ctx); err != nil {
		return err
	}
	bctx := ctx
	bctx.Conditional = true
	thenSt := st.clone()
	if err := w.stmts(c.Then, thenSt, bctx); err != nil {
		return err
	}
	elseSt := st.clone()
	if c.Else != nil {
		if len(c.Else.Cond) == 0 {
			if err := w.stmts(c.Else.Then, elseSt, bctx); err != nil {
				return err
			}
		} else if err := w.ifClause(c.Else, elseSt, bctx); err != nil {
			return err
		}
	}
	st.replace(joinStates(thenSt, elseSt))
	return nil
}

func (w *walker) forClause(c *syntax.ForClause, st *state, ctx Context) error {
	var keepsDir bool
	switch loop := c.Loop.(type) {
	case *syntax.WordIter:
		for _, item := range loop.Items {
			if _, err := w.word(item, st, ctx); err != nil {
				return err
			}
		}
		keepsDir = prescanLoop(st, c.Do)
		st.unsetVar(loop.Name.Value)
	case *syntax.CStyleLoop:
		if err := w.substsIn(loop, st, ctx); err != nil {
			return err
		}
		st.clearVars()
		keepsDir = prescanLoop(st, c.Do)
	default:
		return unreadablef("unsupported loop %T", c.Loop)
	}
	before := st.dir
	err := w.stmts(c.Do, st, loopContext(ctx))
	afterLoop(st, before, keepsDir)
	return err
}

func (w *walker) caseClause(c *syntax.CaseClause, st *state, ctx Context) error {
	if _, err := w.word(c.Word, st, ctx); err != nil {
		return err
	}
	for _, item := range c.Items {
		if item.Op != syntax.Break {
			// Fall-through runs later arms after this one, so the arms share state. The directory stays known when no arm can
			// change it (changesDir).
			arms := make([][]*syntax.Stmt, 0, len(c.Items))
			for _, it := range c.Items {
				arms = append(arms, it.Stmts)
			}
			if changesDir(st, arms...) {
				st.dir = notProvenDir(st.dir)
			}
			st.clearVars()
			break
		}
	}
	actx := ctx
	actx.Conditional = true
	states := []*state{st.clone()}
	for _, item := range c.Items {
		for _, p := range item.Patterns {
			if _, err := w.word(p, st, ctx); err != nil {
				return err
			}
		}
		arm := st.clone()
		if err := w.stmts(item.Stmts, arm, actx); err != nil {
			return err
		}
		states = append(states, arm)
	}
	st.replace(joinStates(states...))
	return nil
}

func (w *walker) funcDecl(c *syntax.FuncDecl, st *state, ctx Context) error {
	name := c.Name.Value
	if modelledName(name) {
		return unreadablef("function %s would shadow a modelled program", name)
	}
	st.funcs[name] = c.Body
	fctx := ctx
	fctx.FuncBody = true
	return w.stmt(c.Body, st.clone(), fctx)
}

func (w *walker) callFunc(name string, body *syntax.Stmt, st *state, ctx Context) error {
	for _, c := range w.calls {
		if c == name {
			return unreadablef("function %s calls itself", name)
		}
	}
	ctx.Depth++
	if ctx.Depth > MaxNestingDepth {
		return unreadablef("nesting is deeper than %d", MaxNestingDepth)
	}
	ctx.FuncBody = true
	w.calls = append(w.calls, name)
	defer func() { w.calls = w.calls[:len(w.calls)-1] }()
	return w.stmt(body, st, ctx)
}

func (w *walker) decl(c *syntax.DeclClause, st *state, ctx Context) error {
	var assigns []Assign
	for _, a := range c.Args {
		if a.Name == nil {
			if a.Value == nil {
				continue
			}
			v, err := w.word(a.Value, st, ctx)
			if err != nil {
				return err
			}
			if !v.Known || strings.HasPrefix(v.Value, "-") && strings.Contains(v.Value, "n") {
				st.clearVars()
			}
			continue
		}
		asg := Assign{Name: a.Name.Value, Append: a.Append}
		switch {
		case a.Array != nil:
			asg.Value = Word{Reason: "array declaration"}
			asg.Array = true
		case a.Value != nil:
			v, err := w.word(a.Value, st, ctx)
			if err != nil {
				return err
			}
			asg.Value = v
		default:
			asg.Value = Word{Reason: "declared without a value"}
		}
		assigns = append(assigns, asg)
	}
	if err := checkAssigns(assigns, st); err != nil {
		return err
	}
	for _, a := range assigns {
		st.setVar(a.Name, a.Value, a.Append)
	}
	return nil
}

// stageSource is what a pipeline stage prints: the last stage of a pipeline the walk just finished prints what pipeOut says; any
// other stage is read by pipeProducer from the programs it showed since before.
func (w *walker) stageSource(x *syntax.Stmt, before int, st *state) *pipeSource {
	if b, ok := x.Cmd.(*syntax.BinaryCmd); ok && (b.Op == syntax.Pipe || b.Op == syntax.PipeAll) && w.pipeOut != nil {
		return w.pipeOut
	}
	return w.pipeProducer(x, before, st)
}

// pipeProducer is what the left side of a pipe prints. A compound command, a function and a chain the reader cannot trace are
// not read; a simple command is classified by the programs it showed outside its substitutions.
func (w *walker) pipeProducer(x *syntax.Stmt, before int, st *state) *pipeSource {
	call, ok := x.Cmd.(*syntax.CallExpr)
	if !ok {
		return &pipeSource{unknown: "a compound command feeds the pipe"}
	}
	if len(call.Args) > 0 {
		if _, isFunc := st.funcs[call.Args[0].Lit()]; isFunc {
			return &pipeSource{unknown: "a function feeds the pipe"}
		}
	}
	var execs []Exec
	for _, e := range w.out[before:] {
		if e.Ctx.CmdSubst || e.Ctx.ProcSubst || e.Kind == KindCommand && e.Name == "" {
			continue
		}
		execs = append(execs, e)
	}
	return producerSource(execs)
}

func (w *walker) call(c *syntax.CallExpr, redirs []Redir, st *state, ctx Context) error {
	assigns := make([]Assign, 0, len(c.Assigns))
	for _, a := range c.Assigns {
		if a.Name == nil {
			return unreadablef("assignment without a name")
		}
		asg := Assign{Name: a.Name.Value, Append: a.Append}
		switch {
		case a.Index != nil:
			asg.Value = Word{Reason: "indexed assignment"}
		case a.Array != nil:
			asg.Value = Word{Reason: "array assignment"}
			asg.Array = true
		case a.Value != nil:
			v, err := w.word(a.Value, st, ctx)
			if err != nil {
				return err
			}
			asg.Value = v
		default:
			asg.Value = Word{Known: true}
		}
		assigns = append(assigns, asg)
	}
	words := make([]Word, 0, len(c.Args))
	for _, arg := range c.Args {
		v, err := w.word(arg, st, ctx)
		if err != nil {
			return err
		}
		words = append(words, v)
	}
	if len(words) == 0 {
		if err := checkAssigns(assigns, st); err != nil {
			return err
		}
		// A command with no words is a program position that is empty, not an unknown one: a redirection alone
		// (">file") creates or truncates its file, and the record carries that write.
		w.out = append(w.out, Exec{Kind: KindCommand, Program: Word{Known: true}, Assigns: assigns, Redirs: redirs, Dir: st.dir, Ctx: ctx})
		for _, a := range assigns {
			st.setVar(a.Name, a.Value, a.Append)
		}
		return nil
	}
	return w.dispatch(words, assigns, redirs, st, ctx)
}

// checkAssigns refuses the assignments that change what a program means
// before it runs, and makes the directory unknown when CDPATH changes.
func checkAssigns(assigns []Assign, st *state) error {
	for _, a := range assigns {
		switch {
		case a.Name == "PATH" || a.Name == "path":
			return unreadablef("assignment to %s changes which program runs", a.Name)
		case isCodeEnvName(a.Name):
			return unreadablef("assignment to %s makes a program run code the text does not show", a.Name)
		case a.Name == "CDPATH":
			st.cdpath = true
			st.dir = unknownDir(st.dir)
		}
	}
	return nil
}

// stdinKind reports the standard input source after a statement's own
// redirections. The last redirection of standard input wins.
func stdinKind(redirs []Redir, def string) string {
	for _, r := range redirs {
		if r.Fd != "" && r.Fd != "0" {
			continue
		}
		switch r.Op {
		case "<", "<>":
			def = StdinFile
		case "<&":
			def = StdinUnknown
		case ">&", ">", ">>", ">|":
			if r.Fd == "0" {
				def = StdinUnknown // 0>&3 copies a descriptor onto standard input
			}
		case "<<", "<<-":
			def = StdinHeredoc
		case "<<<":
			def = StdinHerestring
		}
	}
	return def
}

// stdinProgram returns the text a program reads from standard input when that
// text is a here-document or here-string the reader can see.
func stdinProgram(redirs []Redir, stdin, name string) (string, error) {
	if stdin == StdinHeredoc || stdin == StdinHerestring {
		for i := len(redirs) - 1; i >= 0; i-- {
			r := redirs[i]
			if r.Fd != "" && r.Fd != "0" {
				continue
			}
			if r.Op == "<<" || r.Op == "<<-" || r.Op == "<<<" {
				if !r.Target.Known {
					return "", unreadablef("%s reads a here-document that is not known (%s)", name, r.Target.Reason)
				}
				return r.Target.Value, nil
			}
			break
		}
	}
	return "", unreadablef("%s reads its program from standard input, which the text does not show", name)
}

func knownValue(a Word, what string) (string, error) {
	if !a.Known {
		return "", unreadablef("%s is not known (%s)", what, a.Reason)
	}
	return a.Value, nil
}

func joinKnown(args []Word, what string) (string, error) {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		v, err := knownValue(a, what)
		if err != nil {
			return "", err
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, " "), nil
}

func (w *walker) dispatch(words []Word, assigns []Assign, redirs []Redir, st *state, ctx Context) error {
	prog := words[0]
	if !prog.Known {
		return unreadablef("program word is not known (%s)", prog.Reason)
	}
	if prog.Value == "" {
		return unreadablef("empty program word")
	}
	if strings.HasPrefix(prog.Value, "=") {
		return unreadablef("zsh =word program %q", prog.Value)
	}
	if strings.Contains(prog.Value, ":") && prog.Value != ":" {
		return unreadablef("a program word with a colon is not modelled: %q", prog.Value)
	}
	name := programName(prog.Value)
	if w.createdByText(prog.Value, st.dir) {
		return unreadablef("program %q is created by this text; the file it writes is what runs", prog.Value)
	}
	if err := checkAssigns(assigns, st); err != nil {
		return err
	}
	if reason := zshOnlyUse(name, words[1:]); reason != "" {
		return unreadablef("%s", reason)
	}
	if name == "git" {
		if err := checkGit(words[1:]); err != nil {
			return err
		}
	}
	if name == "npm" {
		if err := checkNpm(words[1:]); err != nil {
			return err
		}
	}
	if isOpaqueInterpreter(name) {
		if err := opaqueInterpreter(name, words[1:], redirs, st.dir, ctx); err != nil {
			return err
		}
	}
	var inline *Inline
	var script *Word
	if isInterpreter(name) {
		var err error
		inline, script, err = w.interpreterInline(name, words[1:], redirs, st.dir, ctx)
		if err != nil {
			return err
		}
	}
	w.out = append(w.out, Exec{
		Kind: KindCommand, Program: prog, Name: name, Args: words[1:],
		Assigns: assigns, Redirs: redirs, Dir: st.dir, Ctx: ctx, Inline: inline,
	})
	if script != nil {
		if err := w.scriptFile(name, *script, st, ctx); err != nil {
			return err
		}
	}
	if body, ok := st.funcs[name]; ok {
		return w.callFunc(name, body, st, ctx)
	}
	switch {
	case isShell(name):
		return w.shellCall(name, words[1:], redirs, st, ctx)
	case name == "eval":
		evalArgs := words[1:]
		if len(evalArgs) > 0 && evalArgs[0].Known && evalArgs[0].Value == "--" {
			evalArgs = evalArgs[1:]
		}
		text, err := joinKnown(evalArgs, "eval argument")
		if err != nil {
			return err
		}
		return w.carried(text, st, ctx, "eval")
	case name == "source" || name == ".":
		return w.sourceCall(words[1:], st, ctx)
	case name == "trap":
		return w.trapCall(words[1:], st, ctx)
	case name == "su":
		return w.suCall(words[1:], st, ctx)
	case name == "cd":
		if ctx.Feed.replacedIn(words[1:]) {
			// The wrapper puts a name the reader does not know in place of its string (find's {} is each path it finds).
			st.dir = unknownDir(st.dir)
			return nil
		}
		st.cd(words[1:])
		return nil
	case name == "pushd" || name == "popd":
		st.dir = unknownDir(st.dir)
		return nil
	case name == "unset":
		unsetWords(words[1:], st)
		return nil
	case isInterpreter(name):
		return nil
	case isWrapper(name):
		return w.wrapped(name, words[1:], assigns, redirs, st, ctx)
	case clobbersVars(name, words[1:]):
		st.clearVars()
	}
	return nil
}

func (s *state) cd(args []Word) {
	if s.cdpath || len(args) != 1 || !args[0].Known {
		s.dir = unknownDir(s.dir)
		return
	}
	target := args[0].Value
	if target == "" || strings.HasPrefix(target, "-") || strings.Contains(target, "..") {
		s.dir = unknownDir(s.dir)
		return
	}
	if path.IsAbs(target) {
		s.dir = Dir{Path: target, Known: true}
		return
	}
	if !s.dir.Known {
		return
	}
	s.dir = Dir{Path: path.Join(s.dir.Path, target), Known: true}
}

func unsetWords(args []Word, st *state) {
	for _, a := range args {
		if !a.Known {
			st.clearVars()
			return
		}
		if strings.HasPrefix(a.Value, "-") {
			continue
		}
		st.unsetVar(a.Value)
	}
}

func clobbersVars(name string, args []Word) bool {
	switch name {
	case "read", "mapfile", "readarray", "getopts", "let", "export", "declare", "typeset", "local", "readonly":
		return true
	case "printf":
		for _, a := range args {
			if !a.Known || a.Value == "-v" {
				return true
			}
		}
	}
	return false
}

func (w *walker) scriptFile(name string, script Word, st *state, ctx Context) error {
	if !script.Known {
		return unreadablef("%s reads a script file that is not known (%s)", name, script.Reason)
	}
	if !st.dir.Known {
		return unreadablef("script file %s is resolved from an unknown directory", script.Value)
	}
	if fdAliasPath(script.Value, st.dir) {
		// A script operand that names a descriptor reads what the shell gave that descriptor (the pipe, a here-document): there
		// is no file to read, so the program is unreadable whatever the spelling of the path.
		return unreadablef("%s reads its program from %s, a file-descriptor alias", name, script.Value)
	}
	w.out = append(w.out, Exec{
		Kind: KindScriptFile, Program: Word{Known: true, Value: name}, Name: name,
		Script: script, Dir: st.dir, Ctx: ctx,
	})
	return nil
}

// carried parses text that a carrier runs (bash -c, eval, trap and the rest)
// and walks it in the same layer.
func (w *walker) carried(text string, st *state, ctx Context, carrier string) error {
	if len(text) > MaxCommandBytes {
		return unreadablef("%s text is %d bytes; the limit is %d", carrier, len(text), MaxCommandBytes)
	}
	ctx.Depth++
	if ctx.Depth > MaxNestingDepth {
		return unreadablef("nesting is deeper than %d", MaxNestingDepth)
	}
	ctx.Carrier = carrier
	// The operands of a wrapper outside the text reach it through the shell's positional parameters, and through the text where the
	// wrapper replaces a string in it (find's {}).
	ctx.Feed = ctx.Feed.asCarried(text)
	// A shell that runs the text starts a new text: a pipe of the text around it is not a pipe inside it, so an input
	// redirection in it replaces the inherited input (zsh with MULTIOS joins a pipe and a file only within one pipeline).
	ctx.inTextPipe = false
	file, err := parseText(text)
	if err != nil {
		return err
	}
	return w.stmts(file.Stmts, st, ctx)
}

func (w *walker) sourceCall(args []Word, st *state, ctx Context) error {
	if len(args) == 0 {
		return unreadablef("source without a file")
	}
	if err := w.scriptFile("source", args[0], st, ctx); err != nil {
		return err
	}
	st.dir = unknownDir(st.dir)
	st.clearVars()
	return nil
}

func (w *walker) trapCall(args []Word, st *state, ctx Context) error {
	if len(args) > 0 && args[0].Known && args[0].Value == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return nil
	}
	first, err := knownValue(args[0], "trap operand")
	if err != nil {
		return err
	}
	switch first {
	case "-l", "-p", "-":
		return nil
	}
	if len(args) < 2 {
		return unreadablef("trap with one operand is not modelled")
	}
	cctx := ctx
	cctx.Conditional = true
	return w.carried(first, st.clone(), cctx, "trap")
}

func (w *walker) wrapped(name string, args []Word, assigns []Assign, redirs []Redir, st *state, ctx Context) error {
	ctx.Depth++
	if ctx.Depth > MaxNestingDepth {
		return unreadablef("nesting is deeper than %d", MaxNestingDepth)
	}
	if name == "busybox" && len(args) == 0 && ctx.Stdin != StdinNone && ctx.Stdin != StdinFile {
		// A bare busybox names no applet. Behind a pipe, a here-document or a here-string the reader cannot prove
		// what it runs, so it is refused; a bare busybox with nothing to read only prints its usage.
		return unreadablef("busybox without an applet has standard input from %s", ctx.Stdin)
	}
	u, err := unwrapCommand(name, args)
	if err != nil {
		return err
	}
	if err := checkAssigns(u.assigns, st); err != nil {
		return err
	}
	if name == "xargs" || name == "find" || name == "parallel" || name == "entr" {
		// The operands of these programs arrive at run time, so the inner program is marked.
		ctx.Carrier = name
		ctx.RuntimeCarrier = name
	}
	if u.recordName != "" {
		// The wrapper's own file operand is a write of its own (script transcript, strace -o FILE).
		w.out = append(w.out, Exec{Kind: KindCommand, Program: Word{Known: true, Value: u.recordName}, Name: u.recordName,
			Args: u.record, Redirs: redirs, Dir: st.dir, Ctx: ctx})
	}
	if u.isShell {
		// A shell string the wrapper runs is code the text shows: it is read by the same layer, as bash -c is.
		return w.carried(u.shell, st.clone(), ctx, u.shellCarrier)
	}
	if name == "xargs" {
		if err := xargsPrograms(&u, ctx, redirs); err != nil {
			return err
		}
	}
	inherited := append(append([]Assign{}, assigns...), u.assigns...)
	// An external program runs in a child process: it cannot change this shell's directory or variables. Its inner
	// program gets a copy of the state, and a shell builtin named behind an external program is not modelled.
	external := name != "command" && name != "builtin" && name != "exec"
	for i, inner := range u.inner {
		ictx := ctx
		if i < len(u.feeds) {
			ictx.Feed = u.feeds[i]
			if name == "find" {
				ictx.Feed.Outer = ctx.Feed
				ictx.Feed.Dir = st.dir
			}
		}
		if external {
			if len(inner) > 0 && shellStateBuiltin(inner[0]) {
				return unreadablef("a shell builtin named behind the external program %s", name)
			}
			if err := w.dispatch(inner, inherited, redirs, st.clone(), ictx); err != nil {
				return err
			}
			continue
		}
		if err := w.dispatch(inner, inherited, redirs, st, ictx); err != nil {
			return err
		}
	}
	return nil
}

// shellStateBuiltin is whether a word names a shell builtin that changes the shell's own directory, variables, functions or
// options: these take no effect in a child process, so behind an external program they are not read.
func shellStateBuiltin(w Word) bool {
	if !w.Known {
		return false
	}
	switch w.Value {
	case "cd", "pushd", "popd", "export", "unset", "set", "shopt", "alias", "unalias", "hash", "trap", "source", ".",
		"eval", "read", "mapfile", "readarray", "getopts", "let", "declare", "typeset", "local", "readonly", "shift",
		"umask", "ulimit", "enable", "builtin", "command", "exec":
		return true
	}
	return false
}
