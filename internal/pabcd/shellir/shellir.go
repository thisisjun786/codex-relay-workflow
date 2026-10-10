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
	// succeeds is an assumption about this statement's exit status, used only
	// for the directory passed to the right side of &&. It never covers a body.
	succeeds    bool
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
	// pipeProgram is the text a literal printf or echo writes into the pipe this command reads; pipeKnown says it is set.
	pipeProgram string
	pipeKnown   bool
	// Carrier names the construct that re-read this text, for example "bash -c".
	Carrier string
	// RuntimeCarrier keeps the outer run-time wrapper through nested shells, even when it has no structured Feed.
	RuntimeCarrier string
	Depth          int
	// Repeat is whether the text may run more than once or alongside the rest of its text: a carrier other than a shell's own
	// -c string runs its text again or later (xargs, find, watch, trap, eval, a shell reading stdin). A shell's -c string runs
	// once, where the text puts it.
	Repeat bool
	// Unsequenced is whether the command substitutions of one simple command run in an order the reader does not model: bash expands
	// the argument substitutions before the assignment ones, so with two or more of them, none is ordered against the others.
	Unsequenced bool
	// Feed says where the operands of a program that find or xargs runs come from; nil outside them.
	Feed *Feed
	// pipeSrc is what the left side of the pipe the command reads prints.
	pipeSrc *pipeSource
	// wrapRedirs says a wrapper around this command carries a redirection: the reader does not follow a pipe into a shell
	// behind a redirected wrapper (exec with a redirection), so the piped program stays unreadable there.
	wrapRedirs bool
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
	// Cdpath is whether CDPATH (or the zsh cdpath array) may be set where the program runs: the text assigned it, or names it. A
	// program or script file that is run there inherits it: a cd to a bare name in a script body searches its directories. A
	// reader of the body starts with it (AnalyzeScript).
	Cdpath bool
	// Line is the line, in the text the record was read from, of the statement that holds it (1-based; 0 when unknown).
	Line int
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
	dir Dir
	// proveCD retains failed-cd possibilities for consumers that require a
	// proven effective directory rather than the legacy literal-cd reading.
	proveCD bool
	vars    map[string]string
	funcs   map[string]*syntax.Stmt
	cdpath  bool
	lookup  func(string) (string, bool)
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
		dir:     s.dir,
		proveCD: s.proveCD,
		vars:    make(map[string]string, len(s.vars)),
		funcs:   make(map[string]*syntax.Stmt, len(s.funcs)),
		cdpath:  s.cdpath,
		lookup:  s.lookup,
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
	out.proveCD = first.proveCD
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
	// staleBeside are module runs that skipped a stale cache entry and run alongside records read after them (CRW-1178).
	staleBeside []staleWatch
}

func (w *walker) stmts(list []*syntax.Stmt, st *state, ctx Context) error {
	ctx.succeeds = false
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
	_, simple := s.Cmd.(*syntax.CallExpr)
	_, binary := s.Cmd.(*syntax.BinaryCmd)
	if (!simple && !binary) || s.Negated || s.Background || s.Coprocess || ctx.Pipeline {
		ctx.succeeds = false
	}
	// The records this statement adds that no inner statement already placed are on this statement's line.
	start, line := len(w.out), int(s.Pos().Line())
	defer func() {
		for i := start; i < len(w.out); i++ {
			if w.out[i].Line == 0 {
				w.out[i].Line = line
			}
		}
	}()
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
	// With two or more command substitutions in one simple command, none of them is ordered against the others (Context.Unsequenced).
	multiSubst := false
	if c, ok := s.Cmd.(*syntax.CallExpr); ok {
		multiSubst = cmdSubsts(c, s.Redirs) > 1
	}
	rctx := ctx
	rctx.Unsequenced = multiSubst
	redirs, err := w.redirects(s.Redirs, st, rctx)
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
		wctx := ctx
		wctx.Unsequenced = multiSubst
		return w.call(c, redirs, st, ctx, wctx)
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
	if s.Cmd == nil {
		return unreadablef("a redirection with no command is not modelled")
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
	dir := changesDir(st, lists...)
	vars := newEffectScan(st, false)
	changes := false
	for _, list := range lists {
		for _, s := range list {
			if vars.stateChanges(s) {
				changes = true
			}
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

// changesDir checks only directory changes, following the same functions and wrappers as the state prescan. CDPATH and
// commands that may replace the shell's builtins remain conservative; harmless function and wrapper calls keep the directory.
func changesDir(st *state, lists ...[]*syntax.Stmt) bool {
	e := newEffectScan(st, true)
	for _, list := range lists {
		for _, s := range list {
			if e.stateChanges(s) {
				return true
			}
		}
	}
	return false
}

// effectScan judges whether a statement can change the directory (directoryOnly) or the variables a later command reads. A call
// to a function counts when the function's body does, and a wrapper (command, builtin, exec) counts when the program it runs
// does. The verdict for each function is kept, so a function that many calls reach is judged once and the work stays bounded by
// the size of the text. A verdict of no change is exact. A verdict of change may be conservative: a call back into a function
// being judged, or a chain of calls deeper than MaxNestingDepth, counts as a change.
type effectScan struct {
	st            *state
	directoryOnly bool
	verdicts      map[string]bool
	active        []string
}

func newEffectScan(st *state, directoryOnly bool) *effectScan {
	return &effectScan{st: st, directoryOnly: directoryOnly, verdicts: map[string]bool{}}
}

// stateChanges reports whether a node can change the state the scan judges. Changes are only ever set to true, so a later
// node in the same tree cannot undo an earlier one.
func (e *effectScan) stateChanges(n syntax.Node) bool {
	changes := false
	syntax.Walk(n, func(n syntax.Node) bool {
		switch c := n.(type) {
		case *syntax.Lit:
			if e.directoryOnly && strings.Contains(c.Value, "CDPATH") {
				changes = true
			}
		case *syntax.CallExpr:
			if !e.directoryOnly && len(c.Assigns) > 0 || e.callChanges(c) {
				changes = true
			}
		case *syntax.DeclClause, *syntax.LetClause, *syntax.ArithmCmd, *syntax.ForClause:
			if !e.directoryOnly {
				changes = true
			}
		case *syntax.BinaryArithm:
			// an assignment inside an arithmetic expansion, $((n=1)), sets the variable on its left
			if !e.directoryOnly && assignsArithm(c.Op) {
				changes = true
			}
		case *syntax.UnaryArithm:
			if !e.directoryOnly && (c.Op == syntax.Inc || c.Op == syntax.Dec) {
				changes = true
			}
		case *syntax.ParamExp:
			// a default assignment, ${n:=x} or ${n=x}, sets the variable it names when the test holds
			if !e.directoryOnly && c.Exp != nil && (c.Exp.Op == syntax.AssignUnset || c.Exp.Op == syntax.AssignUnsetOrNull) {
				changes = true
			}
		}
		return !changes
	})
	return changes
}

// assignsArithm reports whether an arithmetic operator assigns to its left operand.
func assignsArithm(op syntax.BinAritOperator) bool {
	switch op {
	case syntax.Assgn, syntax.AddAssgn, syntax.SubAssgn, syntax.MulAssgn, syntax.QuoAssgn, syntax.RemAssgn,
		syntax.AndAssgn, syntax.OrAssgn, syntax.XorAssgn, syntax.ShlAssgn, syntax.ShrAssgn,
		syntax.AndBoolAssgn, syntax.OrBoolAssgn, syntax.XorBoolAssgn, syntax.PowAssgn:
		return true
	}
	return false
}

func (e *effectScan) callChanges(c *syntax.CallExpr) bool {
	words := make([]Word, len(c.Args))
	for i, a := range c.Args {
		v := a.Lit()
		words[i] = Word{Known: v != "", Value: v}
	}
	return e.wordsChange(words)
}

// wordsChange reports whether a command with these words can change the state. A program the text does not show counts.
func (e *effectScan) wordsChange(words []Word) bool {
	if len(words) == 0 {
		return false
	}
	if !words[0].Known {
		return true
	}
	name := words[0].Value
	if e.directoryOnly && strings.ContainsAny(name, `\$'"`+"`") {
		return true
	}
	if body, ok := e.st.funcs[name]; ok {
		return e.function(name, body)
	}
	switch name {
	case "command", "builtin", "exec":
		u, err := unwrapCommand(name, words[1:])
		if err != nil {
			return true
		}
		for _, inner := range u.inner {
			if e.wordsChange(inner) {
				return true
			}
		}
		return false
	case "cd", "chdir", "pushd", "popd", "eval", "source", ".", "trap",
		"alias", "unalias", "shopt", "enable", "noglob", "nocorrect", "-":
		return true
	case "read", "mapfile", "readarray", "getopts", "printf", "unset",
		"let", "export", "declare", "typeset", "local", "readonly", "set", "shift":
		return !e.directoryOnly
	}
	return false
}

// function judges the body of a function once for this scan. A call back into a function being judged is a change, as is a
// chain deeper than MaxNestingDepth; both only keep the state unknown.
func (e *effectScan) function(name string, body *syntax.Stmt) bool {
	if v, ok := e.verdicts[name]; ok {
		return v
	}
	if len(e.active) >= MaxNestingDepth {
		return true
	}
	for _, f := range e.active {
		if f == name {
			return true
		}
	}
	e.active = append(e.active, name)
	changes := e.stateChanges(body)
	e.active = e.active[:len(e.active)-1]
	e.verdicts[name] = changes
	return changes
}

func (w *walker) binary(c *syntax.BinaryCmd, st *state, ctx Context) error {
	switch c.Op {
	case syntax.AndStmt, syntax.OrStmt:
		before := st.dir
		lctx := ctx
		lctx.succeeds = st.proveCD && c.Op == syntax.AndStmt
		if err := w.stmt(c.X, st, lctx); err != nil {
			return err
		}
		right := st.clone()
		rctx := ctx
		rctx.Conditional = true
		rctx.succeeds = ctx.succeeds && c.Op == syntax.AndStmt
		if err := w.stmt(c.Y, right, rctx); err != nil {
			return err
		}
		if ctx.succeeds && c.Op == syntax.AndStmt {
			st.replace(right)
		} else {
			st.replace(joinStates(st, right))
			// The left side can fail and skip the right side. A directory
			// established only on success cannot escape the && list.
			if st.proveCD && c.Op == syntax.AndStmt && st.dir != before {
				st.dir = unknownDir(st.dir)
			}
		}
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
		rctx.pipeProgram, rctx.pipeKnown = "", false
		if text, ok := pipeProducer(c.X); ok && simpleCallStmt(c.Y) {
			rctx.pipeProgram, rctx.pipeKnown = text, true
		}
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
		if err := w.substsIn(loop, st, loopContext(ctx)); err != nil {
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
	ctx.succeeds = false
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
			// A word that names a variable by a value built at run time (export "${n}PATH=/x") may name CDPATH.
			if mentionsCdpath([]Word{v}) {
				st.cdpath = true
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

// cmdSubsts counts the command substitutions a simple command expands in its assignments, its words and its redirections. A process
// substitution is not counted: its body is unordered already.
func cmdSubsts(c *syntax.CallExpr, redirs []*syntax.Redirect) int {
	n := 0
	visit := func(m syntax.Node) bool {
		switch m.(type) {
		case *syntax.CmdSubst:
			n++
			return false
		case *syntax.ProcSubst:
			return false
		}
		return true
	}
	for _, a := range c.Assigns {
		if a.Value != nil {
			syntax.Walk(a.Value, visit)
		}
		if a.Index != nil {
			syntax.Walk(a.Index, visit)
		}
		if a.Array != nil {
			syntax.Walk(a.Array, visit)
		}
	}
	for _, x := range c.Args {
		syntax.Walk(x, visit)
	}
	for _, r := range redirs {
		if r.Word != nil {
			syntax.Walk(r.Word, visit)
		}
		if r.Hdoc != nil {
			syntax.Walk(r.Hdoc, visit)
		}
	}
	return n
}

func (w *walker) call(c *syntax.CallExpr, redirs []Redir, st *state, ctx, wctx Context) error {
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
			v, err := w.word(a.Value, st, wctx)
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
		v, err := w.word(arg, st, wctx)
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

// isCdpathName is whether a variable name is the directory search path of cd: CDPATH of bash, and zsh's cdpath array, which
// zsh ties to CDPATH and searches the same way (a lower-case assignment of bash is a plain variable; it is read as CDPATH here).
func isCdpathName(name string) bool { return strings.EqualFold(name, "CDPATH") }

// mentionsCdpath is whether the words of a builtin that assigns variables (read, printf -v, export, declare, mapfile and the
// rest) may name CDPATH: one is not known, or is the name, with or without a value.
func mentionsCdpath(args []Word) bool {
	for _, a := range args {
		if !a.Known {
			return true
		}
		name, _, _ := strings.Cut(a.Value, "=")
		if isCdpathName(name) {
			return true
		}
	}
	return false
}

// checkAssigns refuses the assignments that change what a program means
// before it runs, and records that CDPATH was assigned: from then on a cd to a bare name may land in a CDPATH directory.
func checkAssigns(assigns []Assign, st *state) error {
	for _, a := range assigns {
		switch {
		case a.Name == "PATH" || a.Name == "path":
			return unreadablef("assignment to %s changes which program runs", a.Name)
		case isCodeEnvName(a.Name):
			return unreadablef("assignment to %s makes a program run code the text does not show", a.Name)
		case isCdpathName(a.Name):
			st.cdpath = true
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
func stdinProgram(redirs []Redir, stdin, name string, ctx Context) (string, error) {
	if stdin == StdinPipe && ctx.pipeKnown && !hasStdinRedirect(redirs) && !ctx.wrapRedirs {
		return ctx.pipeProgram, nil
	}
	if stdin == StdinUnknown && !ctx.wrapRedirs {
		if fd, idx, ok := stdinCopySource(redirs); ok {
			if ctx.inTextPipe {
				// A copy onto standard input on the right of a pipe: zsh with MULTIOS reads the pipe too, so the copy is not the program.
				return "", unreadablef("%s reads descriptor %s copied onto a pipe's standard input", name, fd)
			}
			return fdBody(redirs, fd, idx, 0)
		}
	}
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
	if isPythonName(name) {
		if handled, err := w.pythonModule(prog, words[1:], assigns, redirs, st, ctx); handled {
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
		Assigns: assigns, Redirs: redirs, Dir: st.dir, Ctx: ctx, Inline: inline, Cdpath: st.cdpath,
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
	case name == "cd" || name == "chdir":
		if ctx.Feed.replacedIn(words[1:]) {
			// The wrapper puts a name the reader does not know in place of its string (find's {} is each path it finds).
			st.dir = unknownDir(st.dir)
			return nil
		}
		before := st.dir
		st.cd(words[1:])
		if st.proveCD && !ctx.succeeds && st.dir != before {
			// A failed cd keeps the previous directory. Without a success
			// condition both possibilities remain, even for a literal target.
			st.dir = unknownDir(st.dir)
		}
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
		if mentionsCdpath(words[1:]) {
			st.cdpath = true
		}
	}
	return nil
}

func (s *state) cd(args []Word) {
	if len(args) != 1 || !args[0].Known {
		s.dir = unknownDir(s.dir)
		return
	}
	target := args[0].Value
	if target == "" || strings.HasPrefix(target, "-") || strings.Contains(target, "..") {
		s.dir = unknownDir(s.dir)
		return
	}
	// With CDPATH assigned a bare name is searched in its directories first, so it names a directory the text does not show. A
	// target that begins with / or ./ is never searched there (bash and zsh): it resolves from the directory as without CDPATH.
	if s.cdpath && !path.IsAbs(target) && !strings.HasPrefix(target, "./") {
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
	if st.dir.Unset && !path.IsAbs(script.Value) {
		return nil
	} // directory-aware readings judge this file
	if !st.dir.Known && !path.IsAbs(script.Value) {
		return unreadablef("script file %s is resolved from an unknown directory", script.Value)
	}
	if fdAliasPath(script.Value, st.dir) {
		// A script operand that names a descriptor reads what the shell gave that descriptor (the pipe, a here-document): there
		// is no file to read, so the program is unreadable whatever the spelling of the path.
		return unreadablef("%s reads its program from %s, a file-descriptor alias", name, script.Value)
	}
	w.out = append(w.out, Exec{
		Kind: KindScriptFile, Program: Word{Known: true, Value: name}, Name: name,
		Script: script, Dir: st.dir, Ctx: ctx, Cdpath: st.cdpath,
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
	if !isOnceCarrier(carrier) {
		ctx.Repeat = true
	}
	// The operands of a wrapper outside the text reach it through the shell's positional parameters, and through the text where the
	// wrapper replaces a string in it (find's {}).
	ctx.Feed = ctx.Feed.asCarried(text)
	// A shell that runs the text starts a new text: a pipe of the text around it is not a pipe inside it, so an input
	// redirection in it replaces the inherited input (zsh with MULTIOS joins a pipe and a file only within one pipeline).
	ctx.inTextPipe = false
	ctx.pipeKnown = false
	file, err := parseText(text)
	if err != nil {
		return err
	}
	if textNamesCdpath(text) {
		st.cdpath = true
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
	for _, r := range redirs {
		if r.Fd == "" || r.Fd == "0" || r.Fd == "1" {
			ctx.wrapRedirs = true // a redirection of standard input or output; a descriptor 2 copy leaves the pipe alone
		}
	}
	if name == "busybox" && len(args) == 0 && ctx.Stdin != StdinNone && ctx.Stdin != StdinFile {
		// A bare busybox names no applet. Behind a pipe, a here-document or a here-string the reader cannot prove
		// what it runs, so it is refused; a bare busybox with nothing to read only prints its usage.
		return unreadablef("busybox without an applet has standard input from %s", ctx.Stdin)
	}
	if name == "parallel" && ctx.Feed != nil {
		// An operand feed (xargs, find) appends values to the ::: sources of parallel, and the reader does not see them.
		return unreadablef("parallel receives operands from %s, which add to its ::: sources", ctx.Feed.Wrapper)
	}
	u, err := unwrapCommand(name, args)
	if err != nil {
		return err
	}
	if err := checkAssigns(u.assigns, st); err != nil {
		return err
	}
	if name == "xargs" || name == "find" || name == "entr" {
		// The operands of these programs arrive at run time, so the inner program is marked.
		ctx.Carrier = name
		ctx.RuntimeCarrier = name
		ctx.Repeat = true
	}
	if name == "watch" {
		// watch runs its command again at every interval, with or without -x: the text may run after the rest of the text.
		ctx.Repeat = true
	}
	if name == "parallel" {
		// parallel carries shell text now, but its moves still receive operands at run time.
		ctx.RuntimeCarrier = name
	}
	if u.recordName != "" {
		// The wrapper's own file operand is a write of its own (script transcript, strace -o FILE).
		w.out = append(w.out, Exec{Kind: KindCommand, Program: Word{Known: true, Value: u.recordName}, Name: u.recordName,
			Args: u.record, Redirs: redirs, Dir: st.dir, Ctx: ctx})
	}
	if u.isShell {
		// A shell string the wrapper runs is code the text shows: it is read by the same layer, as bash -c is. Each job of a
		// parallel text is a shell of its own that starts where this text starts, so each line is read from a copy of the state.
		if len(u.shellLines) > 0 {
			for _, line := range u.shellLines {
				if err := w.carried(line, st.clone(), ctx, u.shellCarrier); err != nil {
					return err
				}
			}
			return nil
		}
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
			// env -C and --chdir move the program's own directory, not the shell's: the copy takes each operand in turn.
			child := st.clone()
			childRedirs := redirs
			if len(u.chdirs) > 0 {
				// The shell opens a redirection before env changes directory, so its file is placed from the command's own directory.
				childRedirs = redirsFromOuter(redirs, st.dir)
			}
			for _, d := range u.chdirs {
				if ictx.Feed.replacedIn([]Word{d}) {
					// find or xargs replaces the string with a path only the run can name: the directory is unknown, as cd leaves it.
					child.dir = unknownDir(child.dir)
					continue
				}
				child.cd([]Word{d})
			}
			if err := w.dispatch(inner, inherited, childRedirs, child, ictx); err != nil {
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

// redirsFromOuter places the file operands of redirections in the directory the outer command runs in. The shell opens them before
// env changes directory, so the program's own directory does not move them. Here-documents, here-strings and descriptor copies are
// not files and stay as they are; a relative file the outer directory cannot place is unknown.
func redirsFromOuter(redirs []Redir, outer Dir) []Redir {
	out := make([]Redir, len(redirs))
	copy(out, redirs)
	for i, r := range redirs {
		switch r.Op {
		case "<<", "<<-", "<<<":
			continue
		case ">&", "<&":
			if r.Target.Known && isDescriptorDup(r.Target.Value) {
				continue
			}
		}
		if !r.Target.Known || r.Target.Value == "" || path.IsAbs(r.Target.Value) {
			continue
		}
		if !outer.Known {
			out[i].Target = Word{Reason: "a relative file in a directory the reader cannot place"}
			continue
		}
		out[i].Target = Word{Known: true, Value: path.Join(outer.Path, r.Target.Value)}
	}
	return out
}

// shellStateBuiltin is whether a word names a shell builtin that changes the shell's own directory, variables, functions or
// options: these take no effect in a child process, so behind an external program they are not read.
func shellStateBuiltin(w Word) bool {
	if !w.Known {
		return false
	}
	switch w.Value {
	case "cd", "chdir", "pushd", "popd", "export", "unset", "set", "shopt", "alias", "unalias", "hash", "trap", "source", ".",
		"eval", "read", "mapfile", "readarray", "getopts", "let", "declare", "typeset", "local", "readonly", "shift",
		"umask", "ulimit", "enable", "builtin", "command", "exec":
		return true
	}
	return false
}
