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
// in a copy of the state, so a directory change inside it does not leak out.
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
		}
		return true
	})
	return firstErr
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
		if strings.ContainsAny(x.Value, "*?[") {
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
		out = append(out, rd)
	}
	return out, nil
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
