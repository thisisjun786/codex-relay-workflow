package shellir

import (
	"strings"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/syntax"
)

// word evaluates one word. Substitutions inside it run in a copy of the state
// and are walked first, so their programs appear in the Exec list. A word that
// depends on anything the reader cannot see is returned with Known false.
func (w *walker) word(x *syntax.Word, st *state, ctx Context) (Word, error) {
	if x == nil {
		return Word{Known: true}, nil
	}
	if err := w.substsIn(x, st, ctx); err != nil {
		return Word{}, err
	}
	if reason := unknownWord(x, st); reason != "" {
		return Word{Reason: reason}, nil
	}
	cfg := &expand.Config{
		Env:       expand.FuncEnviron(func(name string) string { return st.value(name) }),
		ProcSubst: refuseProcSubst,
	}
	v, err := expand.Literal(cfg, unescapeUnquoted(x))
	if err != nil {
		return Word{Reason: err.Error()}, nil
	}
	// An unquoted expansion is split into fields at the blanks of its value and matched as a pattern: a value with a
	// blank or a glob character is several words, which expand.Literal does not split, so it is not one known word.
	if unquotedExpansion(x) && strings.ContainsAny(v, " \t\n*?[") {
		return Word{Reason: "an unquoted expansion splits into several words"}, nil
	}
	return Word{Known: true, Value: v}, nil
}

func refuseProcSubst(*syntax.ProcSubst) (string, error) {
	return "", unreadablef("process substitution is not evaluated")
}

// substsIn walks every command and process substitution under n. Each one runs
// in a copy of the state, so a directory change inside it does not leak out. An expansion that assigns a variable (an
// arithmetic assignment or increment, a default assignment) makes the variables unknown: the reader does not evaluate it.
func (w *walker) substsIn(n syntax.Node, st *state, ctx Context) error {
	var firstErr error
	syntax.Walk(n, func(m syntax.Node) bool {
		if firstErr != nil {
			return false
		}
		switch c := m.(type) {
		case *syntax.CmdSubst:
			sctx := ctx
			sctx.CmdSubst = true
			firstErr = w.substBody(c.Stmts, st, sctx)
			return false
		case *syntax.ProcSubst:
			sctx := ctx
			sctx.ProcSubst = true
			firstErr = w.substBody(c.Stmts, st, sctx)
			return false
		case *syntax.ArithmExp:
			w.arithmPrefix(st, c.X)
		case *syntax.ArithmCmd:
			w.arithmPrefix(st, c.X)
		case *syntax.LetClause:
			w.arithmPrefix(st, c.Exprs...)
		case *syntax.CStyleLoop:
			w.arithmPrefix(st, c.Init, c.Cond, c.Post)
		case *syntax.BinaryTest:
			// [[ a -eq b ]] evaluates both operands as arithmetic expressions
			if c.Op >= syntax.TsEql && c.Op <= syntax.TsGtr {
				w.arithmTestPrefix(st, c.X, false)
				w.arithmTestPrefix(st, c.Y, false)
			}
		case *syntax.UnaryTest:
			// [[ -v a[i] ]] evaluates the subscript
			if c.Op == syntax.TsVarSet {
				w.arithmTestPrefix(st, c.X, true)
			}
		case *syntax.BinaryArithm:
			// an assignment in an arithmetic expansion, $((n=1)), sets a variable the reader does not follow
			if assignsArithm(c.Op) {
				st.clearVars()
			}
		case *syntax.UnaryArithm:
			if c.Op == syntax.Inc || c.Op == syntax.Dec {
				st.clearVars()
			}
		case *syntax.ParamExp:
			// a subscript and a slice offset or length are arithmetic expressions
			w.arithmPrefix(st, c.Index)
			if c.Slice != nil {
				w.arithmPrefix(st, c.Slice.Offset, c.Slice.Length)
			}
			// a default assignment, ${n:=x}, sets the variable it names
			if c.Exp != nil && (c.Exp.Op == syntax.AssignUnset || c.Exp.Op == syntax.AssignUnsetOrNull) {
				st.clearVars()
				// ${!N:=x} assigns the variable whose name is N's value, built at run time and never spelled by the text: it may
				// be PYTHONPYCACHEPREFIX, exported under set -a (CRW-1178).
				if c.Excl || c.Param == nil || c.NestedParam != nil || !validName(c.Param.Value) {
					w.prefixUnknown = true
				}
			}
		}
		return true
	})
	return firstErr
}

// maxArithmDepth bounds how far arithmPrefix follows a variable whose value names another variable, and maxArithmNames how many
// names one check follows in all, so values that name each other many times over cannot make the check slow.
const (
	maxArithmDepth = 8
	maxArithmNames = 1024
)

// arithmPrefix records in w.prefixUnknown that evaluating an arithmetic expression may assign a variable whose name the text does
// not spell, which may be PYTHONPYCACHEPREFIX (CRW-1178). bash expands each operand to text, parses that text as arithmetic and
// evaluates a name in it by evaluating its value as an expression of its own, so a value built at run time
// (x=PYTHONPYCACHE; x+=PREFIX=7; $((x))) assigns a name the text never spells. An expression is clear when every name it reads holds
// a value the walk knows and that is clear in turn; a plain assignment to a name the text spells evaluates only its right side.
func (w *walker) arithmPrefix(st *state, exprs ...syntax.ArithmExpr) {
	for _, x := range exprs {
		if x == nil || w.prefixUnknown {
			continue
		}
		syntax.Walk(x, func(n syntax.Node) bool {
			if w.prefixUnknown {
				return false
			}
			switch c := n.(type) {
			case *syntax.BinaryArithm:
				if wd, ok := c.X.(*syntax.Word); ok && c.Op == syntax.Assgn && validName(wd.Lit()) {
					w.arithmPrefix(st, c.Y)
					return false
				}
			case *syntax.Word:
				if text, ok := arithmWordText(c, st); !ok || !arithmTextClear(text, st) {
					w.prefixUnknown = true
				}
				return false
			}
			return true
		})
	}
}

// arithmTestPrefix is arithmPrefix for an operand of [[ ]] that bash evaluates as arithmetic: the whole word, or with subscript only
// the subscript of a name[subscript] word (-v).
func (w *walker) arithmTestPrefix(st *state, x syntax.TestExpr, subscript bool) {
	wd, ok := x.(*syntax.Word)
	if !ok || w.prefixUnknown {
		return
	}
	text, ok := arithmWordText(wd, st)
	if ok && subscript {
		i, j := strings.IndexByte(text, '['), strings.LastIndexByte(text, ']')
		switch {
		case i < 0:
			return
		case j > i:
			text = text[i+1 : j]
		default:
			text = text[i+1:]
		}
	}
	if !ok || !arithmTextClear(text, st) {
		w.prefixUnknown = true
	}
}

// assignArithm is arithmPrefix for the subscripts of an assignment (a[i]=v, a=([i]=v)), which bash evaluates as arithmetic.
func (w *walker) assignArithm(a *syntax.Assign, st *state) {
	w.arithmPrefix(st, a.Index)
	if a.Array != nil {
		for _, e := range a.Array.Elems {
			w.arithmPrefix(st, e.Index)
		}
	}
}

// arithmWordText is the text an arithmetic operand expands to, when the walk knows it.
func arithmWordText(x *syntax.Word, st *state) (string, bool) {
	if lit := x.Lit(); lit != "" {
		return lit, true
	}
	if unknownWord(x, st) != "" {
		return "", false
	}
	cfg := &expand.Config{
		Env:       expand.FuncEnviron(func(name string) string { return st.value(name) }),
		ProcSubst: refuseProcSubst,
	}
	v, err := expand.Literal(cfg, unescapeUnquoted(x))
	return v, err == nil
}

// arithmTextClear is whether an arithmetic text assigns only names it spells: it does not spell PYTHONPYCACHEPREFIX, and every name
// in it holds a value the walk knows that is clear in turn (bash evaluates it as an expression). Letters within a number (0x1f,
// 16#ff) are digits, not names.
func arithmTextClear(text string, st *state) bool {
	budget := maxArithmNames
	return arithmTextClearIn(text, st, 0, &budget)
}

func arithmTextClearIn(text string, st *state, depth int, budget *int) bool {
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c >= '0' && c <= '9':
			for i < len(text) && (validName("a"+text[i:i+1]) || text[i] == '#' || text[i] == '@') {
				i++
			}
		case validName(text[i : i+1]):
			j := i
			for j < len(text) && validName("a"+text[j:j+1]) {
				j++
			}
			name := text[i:j]
			i = j
			*budget--
			if textNamesPycachePrefix(name) || depth >= maxArithmDepth || *budget < 0 {
				return false
			}
			v, ok := st.vars[name]
			if !ok || !arithmTextClearIn(v, st, depth+1, budget) {
				return false
			}
		default:
			i++
		}
	}
	return true
}

func (w *walker) substBody(list []*syntax.Stmt, st *state, ctx Context) error {
	ctx.Depth++
	if ctx.Depth > MaxNestingDepth {
		return unreadablef("nesting is deeper than %d", MaxNestingDepth)
	}
	return w.stmts(list, st.clone(), ctx)
}

// unknownWord names the first part of x whose value the reader cannot prove.
func unknownWord(x *syntax.Word, st *state) string {
	// bash brace-expands an unquoted word before it runs it, and mvdan keeps the braces as literal text: a word with an
	// unescaped brace expansion names a different word at run time, so the reader cannot know it.
	if hasBraceExpansionWord(x) {
		return "brace expansion"
	}
	for i, p := range x.Parts {
		if r := unknownPart(p, false, i == 0, st); r != "" {
			return r
		}
	}
	return ""
}

func unknownPart(p syntax.WordPart, quoted, first bool, st *state) string {
	switch x := p.(type) {
	case *syntax.Lit:
		if quoted {
			return ""
		}
		if first && strings.HasPrefix(x.Value, "~") && !st.tildeKnown(x.Value) {
			return "tilde expansion"
		}
		if strings.ContainsAny(x.Value, "*?[") && x.Value != "[" {
			return "pathname pattern"
		}
		return ""
	case *syntax.SglQuoted:
		if x.Dollar && strings.Contains(x.Value, "\\") {
			return "ANSI-C escape"
		}
		return ""
	case *syntax.DblQuoted:
		for _, q := range x.Parts {
			if r := unknownPart(q, true, false, st); r != "" {
				return r
			}
		}
		return ""
	case *syntax.ParamExp:
		if simpleKnownParam(x, st) {
			return ""
		}
		if x.Param != nil {
			return "variable " + x.Param.Value
		}
		return "variable"
	case *syntax.CmdSubst:
		return "command substitution"
	case *syntax.ArithmExp:
		return "arithmetic expansion"
	case *syntax.ProcSubst:
		return "process substitution"
	case *syntax.ExtGlob:
		return "extended glob"
	case *syntax.BraceExp:
		return "brace expansion"
	}
	return "unsupported word part"
}

// simpleKnownParam reports a plain variable reference, dollar-name or braced
// name, whose literal value the walk holds. Any operator, index or indirection
// leaves the value unknown.
func simpleKnownParam(x *syntax.ParamExp, st *state) bool {
	if x.Param == nil || x.Exp != nil || x.Slice != nil || x.Repl != nil || x.Index != nil ||
		x.NestedParam != nil || x.Length || x.Width || x.Excl || x.Names != 0 ||
		len(x.Modifiers) > 0 || x.Flags != nil {
		return false
	}
	return st.known(x.Param.Value)
}

func (w *walker) redirects(list []*syntax.Redirect, st *state, ctx Context) ([]Redir, error) {
	out := make([]Redir, 0, len(list))
	for _, r := range list {
		op := redirOp(r.Op)
		if op == "" {
			return nil, unreadablef("redirection operator %v is not modelled", r.Op)
		}
		rd := Redir{Op: op}
		if r.N != nil {
			rd.Fd = r.N.Value
		}
		var err error
		switch r.Op {
		case syntax.Hdoc, syntax.DashHdoc:
			rd.Target, err = w.heredoc(r, st, ctx)
			if r.Op == syntax.DashHdoc && err == nil && rd.Target.Known {
				rd.Target.Value = stripLeadingTabs(rd.Target.Value)
			}
		case syntax.WordHdoc:
			rd.Target, err = w.herestring(r.Word, st, ctx)
		default:
			rd.Target, err = w.word(r.Word, st, ctx)
		}
		if err != nil {
			return nil, err
		}
		if r.Op == syntax.DplOut && (rd.Fd == "" || rd.Fd == "1") && !(rd.Target.Known && isDescriptorDup(rd.Target.Value)) {
			// >&word with a word that is no descriptor is &>word: standard output and error go to the file. A word the reader
			// cannot evaluate may be a file as well, so it is a write to a destination not known.
			rd.Op, rd.Fd = "&>", ""
		}
		out = append(out, rd)
	}
	return out, nil
}

// isDescriptorDup is whether the word after >& names a descriptor to copy or move (2, 2-) or closes standard output (-).
func isDescriptorDup(v string) bool {
	v = strings.TrimSuffix(v, "-")
	if v == "" {
		return true // the word was "-": close
	}
	for _, c := range v {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func redirOp(op syntax.RedirOperator) string {
	switch op {
	case syntax.RdrOut:
		return ">"
	case syntax.AppOut:
		return ">>"
	case syntax.RdrIn:
		return "<"
	case syntax.RdrInOut:
		return "<>"
	case syntax.DplIn:
		return "<&"
	case syntax.DplOut:
		return ">&"
	case syntax.RdrClob:
		return ">|"
	case syntax.AppClob:
		return ">>|"
	case syntax.Hdoc:
		return "<<"
	case syntax.DashHdoc:
		return "<<-"
	case syntax.WordHdoc:
		return "<<<"
	case syntax.RdrAll:
		return "&>"
	case syntax.RdrAllClob:
		return "&>|"
	case syntax.AppAll:
		return "&>>"
	case syntax.AppAllClob:
		return "&>>|"
	}
	return ""
}

// heredoc returns the body of a here-document. A quoted delimiter keeps the
// body as written. An unquoted delimiter is literal only when the body holds no
// expansion; then only the backslash escapes remain to be removed.
func (w *walker) heredoc(r *syntax.Redirect, st *state, ctx Context) (Word, error) {
	if r.Hdoc == nil {
		return Word{Known: true}, nil
	}
	if err := w.substsIn(r.Hdoc, st, ctx); err != nil {
		return Word{}, err
	}
	if delimiterQuoted(r.Word) {
		lit, ok := literalText(r.Hdoc)
		if !ok {
			return Word{Reason: "quoted here-document holds an expansion"}, nil
		}
		return Word{Known: true, Value: lit}, nil
	}
	if hasExpansion(r.Hdoc) {
		return Word{Reason: "unquoted here-document holds an expansion"}, nil
	}
	cfg := &expand.Config{Env: expand.FuncEnviron(func(string) string { return "" }), ProcSubst: refuseProcSubst}
	v, err := expand.Document(cfg, r.Hdoc)
	if err != nil {
		return Word{Reason: err.Error()}, nil
	}
	return Word{Known: true, Value: v}, nil
}

// herestring evaluates the word and appends the newline that bash adds.
func (w *walker) herestring(x *syntax.Word, st *state, ctx Context) (Word, error) {
	v, err := w.word(x, st, ctx)
	if err != nil || !v.Known {
		return v, err
	}
	v.Value += "\n"
	return v, nil
}

func delimiterQuoted(x *syntax.Word) bool {
	if x == nil {
		return false
	}
	for _, p := range x.Parts {
		lit, ok := p.(*syntax.Lit)
		if !ok || strings.Contains(lit.Value, "\\") {
			return true
		}
	}
	return false
}

func literalText(x *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, p := range x.Parts {
		lit, ok := p.(*syntax.Lit)
		if !ok {
			return "", false
		}
		b.WriteString(lit.Value)
	}
	return b.String(), true
}

func hasExpansion(x *syntax.Word) bool {
	found := false
	syntax.Walk(x, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ProcSubst:
			found = true
		}
		return !found
	})
	return found
}

// unquotedExpansion is whether the word has a parameter, command or arithmetic expansion outside every quote.
func unquotedExpansion(x *syntax.Word) bool {
	for _, p := range x.Parts {
		switch p.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp:
			return true
		}
	}
	return false
}

// hasBraceExpansionWord decides the whole word, not each literal run: a quoted part, an expansion or a command substitution
// is one opaque placeholder, so a brace group split by quotes ({rm,"-rf"}, {"a",b}, {$x,b}) is seen as the group bash
// expands. Quoted text is never a brace or a separator. Bash confirms the shapes: printf '%s' {rm,"-rf"} prints three words.
func hasBraceExpansionWord(x *syntax.Word) bool {
	var b []byte
	for _, p := range x.Parts {
		if lit, ok := p.(*syntax.Lit); ok {
			b = append(b, lit.Value...)
			continue
		}
		b = append(b, 'P')
	}
	return hasBraceExpansion(string(b))
}

// hasBraceExpansion reports an unescaped { that a comma or a .. sequence and a closing unescaped } enclose: the forms bash
// expands ({a,b}, {1..3}, {,}, nested groups). A backslash escapes the next character, so \{a,b\} is no expansion, and a
// lone {} is none.
func hasBraceExpansion(raw string) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '\\' {
			i++
			continue
		}
		if raw[i] != '{' {
			continue
		}
		sep := false
		for j := i + 1; j < len(raw); j++ {
			switch c := raw[j]; {
			case c == '\\':
				j++
			case c == ',' || (c == '.' && j+1 < len(raw) && raw[j+1] == '.'):
				sep = true
			case c == '}':
				if sep {
					return true
				}
				j = len(raw)
			}
		}
	}
	return false
}
