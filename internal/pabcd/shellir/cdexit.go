package shellir

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// CRW-1165: the one form that proves a cd succeeded for the rest of a text. A failed cd keeps the previous directory, so the reader
// (proveCD) treats the directory after a cd as unproven unless a success condition (&&) establishes it. `cd <literal path> || exit
// <literal digits>`, as a statement at the top of the text, is the other proof: a cd that fails ends the shell, so every later statement
// of the text runs in the target. The form is exact and the text around it must leave the builtins alone, because the proof stands on
// `exit` ending the shell: any function, trap, alias, eval, source, enable, set or option change anywhere in the text, a command name
// the reader cannot spell, or (where the environment is known) a startup file, an exported function named exit or cd, or a CDPATH withdraws it.
// The first attempt at this (13 bypasses found by verification, each a way a failed cd still reaches the next statement) tried to
// prove more forms; none of them is proven here.

// exitProofBanned are the command names whose presence anywhere in the text may change what exit or cd does, or run text the
// walk does not see (the text is judged as a whole, before and after the form).
var exitProofBanned = map[string]bool{
	"trap": true, "enable": true, "source": true, ".": true, "eval": true, "alias": true, "unalias": true, "shopt": true,
	"builtin": true, "command": true, "unset": true, "set": true, "exec": true, "hash": true, "declare": true, "typeset": true,
	"local": true, "nameref": true, "setopt": true, "unsetopt": true, "emulate": true, "autoload": true, "functions": true,
	"fc": true, "history": true, "bind": true, "compgen": true, "complete": true,
	// zsh: disable removes a builtin (`disable exit` makes the exit of the form a command that is not found, and the text goes on), the
	// others remove a hash entry or a function, load or unload a module that holds builtins, or schedule a command (CRW-1165).
	"disable": true, "unhash": true, "unfunction": true, "zmodload": true, "zcompile": true, "sched": true,
}

// reShellTables are the names of the tables with which zsh (and bash, BASH_ALIASES) defines functions and aliases, or sets options, by
// assignment: `functions[exit]=:` defines a function named exit with no command that declares one, and `aliases[exit]=true` an alias
// (a later line of the text expands it); `options[...]` sets a shell option as setopt does. The name anywhere in the text withdraws the
// proof, in a command, a string or a comment alike (CRW-1165).
var reShellTables = regexp.MustCompile(`\b(functions|aliases|galiases|saliases|options|BASH_ALIASES|dis_functions|dis_aliases|dis_galiases|dis_saliases|dis_builtins)\b`)

// textNamesShellTables is whether a text spells one of the tables of reShellTables.
func textNamesShellTables(src string) bool { return reShellTables.MatchString(src) }

// plainForExitProof is whether the whole text leaves exit and cd alone (see above). lookup is the environment the text runs in, nil when
// the reader is given none (a variable is then unknown, and the startup variables cannot be checked). A function named exit is
// refused by the reader itself (modelledName), so the walk of the text never gets here with one; FuncDecl stays in the check for the
// readings that do not walk a body.
func plainForExitProof(file *syntax.File, lookup func(string) (string, bool)) bool {
	if lookup != nil {
		for _, name := range StartupEnvNames {
			if v, ok := lookup(name); ok && v != "" {
				return false
			}
		}
	}
	plain := true
	syntax.Walk(file, func(n syntax.Node) bool {
		if !plain {
			return false
		}
		switch x := n.(type) {
		case *syntax.FuncDecl, *syntax.CoprocClause:
			plain = false
		case *syntax.DeclClause:
			if x.Variant != nil && (x.Variant.Value == "declare" || x.Variant.Value == "typeset" || x.Variant.Value == "local") {
				plain = false
			}
			// export -f and readonly -f name a function: one the text or its environment defined reaches the shells the text runs.
			for _, a := range x.Args {
				if a.Naked && a.Value != nil && declFunctionFlag(a.Value) {
					plain = false
				}
			}
		case *syntax.CallExpr:
			if len(x.Args) == 0 {
				return true
			}
			name, ok := commandWordText(x.Args[0])
			if !ok || exitProofBanned[name] {
				plain = false
			}
		}
		return plain
	})
	return plain
}

// StartupEnvNames are the variables of the environment a text inherits that a shell reads before it runs a script, and that therefore may
// define a function named exit or cd, or set an option that changes them: BASH_ENV and ENV name a startup file (bash and sh), ZDOTDIR the
// directory of the .zshenv that zsh reads, SHELLOPTS and BASHOPTS set options, BASH_FUNC_exit%% and BASH_FUNC_cd%% are exported
// functions, and CDPATH changes where a bare cd lands. A non-empty value of one of them withdraws the proof of `cd <literal> || exit
// <digits>` (CRW-1165); an empty one is unset for the shell.
var StartupEnvNames = []string{"BASH_ENV", "ENV", "ZDOTDIR", "SHELLOPTS", "BASHOPTS", "BASH_FUNC_exit%%", "BASH_FUNC_cd%%", "CDPATH"}

// reStartupName is a name of a variable that a shell reads at start, as a text spells it (BASH_FUNC_ is the prefix of every exported function).
var reStartupName = regexp.MustCompile(`\b(BASH_ENV|ENV|ZDOTDIR|SHELLOPTS|BASHOPTS)\b|BASH_FUNC_`)

// textNamesStartupVar is whether a text holds the name of a startup variable (reStartupName). A text can set such a variable in more
// forms than a plain assignment (${BASH_ENV:=x}, for BASH_ENV in, read BASH_ENV, an arithmetic assignment, getopts), and a name built from
// known parts (N=BASH_; N+=ENV; printf -v "$N") is a word the builtins that assign see: so the name in the text, or in a word a builtin
// that assigns variables is given, is enough to withdraw the proof (CRW-1165).
func textNamesStartupVar(s string) bool { return reStartupName.MatchString(s) }

// noteAssignedNames records what the words of a builtin that assigns variables (export, declare, read, printf -v, ...) may name that the
// text does not spell: a startup variable, which withdraws the proof for the scripts the text runs, or PYTHONPATH, which -P keeps
// (CRW-1165, CRW-1187).
func (w *walker) noteAssignedNames(words []Word) {
	for _, a := range words {
		if !a.Known {
			continue
		}
		if textNamesStartupVar(a.Value) {
			w.startupSet = true
		}
		if strings.Contains(a.Value, "PYTHONPATH") {
			w.pythonPathNamed = true
		}
	}
}

// declFunctionFlag is whether a word of a declaration is an option word that holds f (-f, -fx, -nf).
func declFunctionFlag(w *syntax.Word) bool {
	v, ok := commandWordText(w)
	if !ok {
		return true
	}
	return strings.HasPrefix(v, "-") && strings.Contains(v, "f")
}

// commandWordText is the command a word names after the shell removed its quotes and backslashes, and false when the word holds an
// expansion, a pattern or a brace list (it then names a command the reader cannot spell).
func commandWordText(w *syntax.Word) (string, bool) {
	var b strings.Builder
	var add func(parts []syntax.WordPart) bool
	add = func(parts []syntax.WordPart) bool {
		for _, p := range parts {
			switch x := p.(type) {
			case *syntax.Lit:
				b.WriteString(strings.ReplaceAll(x.Value, "\\", ""))
			case *syntax.SglQuoted:
				if x.Dollar {
					return false
				}
				b.WriteString(x.Value)
			case *syntax.DblQuoted:
				if x.Dollar || !add(x.Parts) {
					return false
				}
			default:
				return false
			}
		}
		return true
	}
	if !add(w.Parts) {
		return "", false
	}
	s := b.String()
	if s == "" || strings.ContainsAny(s, "*?[]{}~") {
		return "", false
	}
	return s, true
}

// literalPlainWord is the text of a word that is the same text to the shell whatever the environment: unquoted or quoted literal
// characters only, with no backslash, expansion, pattern, brace, tilde or quote left in it.
func literalPlainWord(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for _, p := range w.Parts {
		switch x := p.(type) {
		case *syntax.Lit:
			b.WriteString(x.Value)
		case *syntax.SglQuoted:
			if x.Dollar {
				return "", false
			}
			b.WriteString(x.Value)
		case *syntax.DblQuoted:
			if x.Dollar {
				return "", false
			}
			for _, q := range x.Parts {
				l, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(l.Value)
			}
		default:
			return "", false
		}
	}
	s := b.String()
	if strings.ContainsAny(s, "*?[]{}$`\\~!\"'\x00\n") {
		return "", false
	}
	return s, true
}

// bareStmt is a simple command statement with nothing around it: no redirection, negation, background or coprocess.
func bareStmt(s *syntax.Stmt) (*syntax.CallExpr, bool) {
	if s == nil || s.Negated || s.Background || s.Coprocess || len(s.Redirs) > 0 {
		return nil, false
	}
	c, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(c.Assigns) > 0 {
		return nil, false
	}
	return c, true
}

// cdExitForm is whether c is exactly `cd <literal path> || exit <literal digits>`.
func cdExitForm(c *syntax.BinaryCmd) bool {
	if c.Op != syntax.OrStmt {
		return false
	}
	cd, ok := bareStmt(c.X)
	if !ok || len(cd.Args) != 2 || !plainLit(cd.Args[0], "cd") {
		return false
	}
	// An operand that starts with - is an option or an entry of the directory stack (cd -, zsh cd -N), one with + is a stack entry in
	// zsh (cd +0 is the directory it is in, and succeeds), and one with = the path of a command (zsh's =name): none is the literal
	// directory (CRW-1165).
	target, ok := literalPlainWord(cd.Args[1])
	if !ok || target == "" || strings.ContainsAny(target[:1], "-+=") {
		return false
	}
	ex, ok := bareStmt(c.Y)
	if !ok || len(ex.Args) != 2 || !plainLit(ex.Args[0], "exit") {
		return false
	}
	status, ok := literalPlainWord(ex.Args[1])
	if !ok || len(status) == 0 || len(status) > 3 {
		return false
	}
	for _, r := range status {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// plainLit is whether a word is the single unquoted literal want.
func plainLit(w *syntax.Word, want string) bool {
	if len(w.Parts) != 1 {
		return false
	}
	l, ok := w.Parts[0].(*syntax.Lit)
	return ok && l.Value == want
}
