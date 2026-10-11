package shellir

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Analyze reads a command text run in the working directory cwd, with no environment: a variable the text
// names is unknown, and a home-relative path is unknown.
func Analyze(src, cwd string) (Result, error) {
	return AnalyzeEnv(src, cwd, nil)
}

// AnalyzeEnv reads a command text as AnalyzeEnv's caller's environment shows it: a variable the walk does not
// assign takes its value from lookup, and a leading ~ or ~/ expands to HOME when lookup knows HOME.
func AnalyzeEnv(src, cwd string, lookup func(string) (string, bool)) (Result, error) {
	return analyze(src, newState(cwd), lookup)
}

// AnalyzeEnvProvenDirectory retains the previous directory when a cd can fail
// and execution can continue. Only a success condition such as && establishes
// the requested directory for subsequent commands. The legacy AnalyzeEnv
// reading is kept for consumers whose existing policy assumes literal cd succeeds.
func AnalyzeEnvProvenDirectory(src, cwd string, lookup func(string) (string, bool)) (Result, error) {
	st := newState(cwd)
	st.proveCD = true
	return analyze(src, st, lookup)
}

// AnalyzeScript reads the text of a script file that a shell runs from a program record that had Cdpath: with CDPATH possibly set
// in the environment the script inherits, a cd to a bare name in it may land in a directory the text does not show.
func AnalyzeScript(src, cwd string, cdpath bool) (Result, error) {
	st := newState(cwd)
	st.cdpath = cdpath
	return analyze(src, st, nil)
}

// AnalyzeScriptEnv is AnalyzeScript with the environment the script inherits: a variable the walk does not assign takes its value from
// lookup. A consumer that knows only part of that environment answers the rest as unset.
func AnalyzeScriptEnv(src, cwd string, cdpath bool, lookup func(string) (string, bool)) (Result, error) {
	st := newState(cwd)
	st.cdpath = cdpath
	return analyze(src, st, lookup)
}

// AnalyzeScriptProvenDirectory is AnalyzeScript with the same failed-cd
// tracking as AnalyzeEnvProvenDirectory, for memory destinations in carried files.
func AnalyzeScriptProvenDirectory(src, cwd string, cdpath bool) (Result, error) {
	return AnalyzeScriptProven(src, cwd, ScriptOptions{Cdpath: cdpath})
}

// ScriptOptions is what a reader of a text with failed-cd tracking knows about the shell that runs it besides the text (CRW-1165).
type ScriptOptions struct {
	// Cdpath is Exec.Cdpath of the record that runs the text.
	Cdpath bool
	// ExitTainted is Exec.ExitTainted of the record that runs the text: the text that ran it did not leave exit and cd alone, so a
	// function or option it made may be inherited, and the text is not given the proof of `cd <literal> || exit <digits>`.
	ExitTainted bool
	// Startup answers the variables a shell reads at start (StartupEnvNames: BASH_ENV, ENV, ZDOTDIR, SHELLOPTS, BASHOPTS, BASH_FUNC_exit%%, BASH_FUNC_cd%%, CDPATH)
	// from the environment the command inherits, for a reader that is given no environment otherwise. nil: not known, nothing is held
	// against the proof for them (the other variables stay unknown either way).
	Startup func(string) (string, bool)
}

// AnalyzeScriptProven is AnalyzeScriptProvenDirectory with what is known about the shell around the text (ScriptOptions).
func AnalyzeScriptProven(src, cwd string, o ScriptOptions) (Result, error) {
	st := newState(cwd)
	st.cdpath = o.Cdpath
	st.proveCD = true
	st.exitTainted = o.ExitTainted
	st.startup = o.Startup
	return analyze(src, st, nil)
}

// textNamesCdpath is whether a text spells CDPATH (or zsh's cdpath) anywhere: an assignment, a read, a printf -v, a loop variable or
// a declaration may set it, and none of them is followed to its end, so the name in the text is enough.
func textNamesCdpath(src string) bool {
	return strings.Contains(strings.ToLower(src), "cdpath")
}

// AnalyzeNoDir reads a command text with no working directory at all, with no environment. It is the reading of the memory write
// gate that does not depend on a directory (the Python programs of the text, and whether the text is readable at all); the
// judgments that need a directory are made by the readings that have one (see Dir.Unset).
func AnalyzeNoDir(src string) (Result, error) {
	st := newState("")
	st.dir.Unset = true
	return analyze(src, st, nil)
}

func analyze(src string, st *state, lookup func(string) (string, bool)) (Result, error) {
	w := &walker{}
	fresh := st.clone()
	if len(src) > MaxCommandBytes {
		return Result{}, unreadablef("command is %d bytes; the limit is %d", len(src), MaxCommandBytes)
	}
	if continuationNearComment(src) {
		return Result{}, unreadablef("a line continuation next to a # is read differently by bash and by the parser")
	}
	file, err := parseText(src)
	if err != nil {
		return Result{}, err
	}
	if unquotedCarriageReturn(file, src) {
		return Result{}, unreadablef("a carriage return outside a word is a word break for the parser and an ordinary byte for bash")
	}
	st.lookup = lookup
	st.cdpath = st.cdpath || textNamesCdpath(src)
	w.prefixNamed = textNamesPycachePrefix(src)
	w.pythonPathNamed = strings.Contains(src, "PYTHONPATH")
	w.startupSet = textNamesStartupVar(src)
	startup := lookup
	if st.startup != nil {
		startup = st.startup
	}
	w.exitProof = st.proveCD && !st.exitTainted && !st.noExitProof && !w.startupSet && !textNamesShellTables(src) && plainForExitProof(file, startup)
	if err := w.stmts(file.Stmts, st, Context{top: true}); err != nil {
		return Result{}, err
	}
	if err := w.writersBesideModules(); err != nil {
		return Result{}, err
	}
	if w.prefixUnknown || w.startupSet {
		// A name the text does not spell may be BASH_ENV, ENV or another startup variable (printf -v "$N$M" with N=BASH_ and M=ENV), or a text names
		// one in a form that is not a plain assignment (${BASH_ENV:=x}, read BASH_ENV, a name built from known parts): the
		// scripts the text runs may start with an exit function from a file, so none of them is given the proof, wherever the
		// assignment stands and whether or not the text used the proof itself (CRW-1165).
		for i := range w.out {
			w.out[i].ExitTainted = true
		}
	}
	if w.exitUsed && (w.prefixUnknown || w.startupSet) {
		// The form was taken as proof, and the text assigns variables by names it does not spell (read "$N", printf -v "$N", export
		// "$N"): one of them may be a table that defines a function or an alias named exit (zsh functions, aliases), which the walk cannot
		// know. Read the text again without the proof (CRW-1165).
		fresh.noExitProof = true
		return analyze(src, fresh, lookup)
	}
	return Result{Execs: w.out}, nil
}

// value is a variable's value: one the walk assigns, else one the environment gives, else empty.
func (s *state) value(name string) string {
	if v, ok := s.vars[name]; ok {
		return v
	}
	if s.lookup != nil {
		if v, ok := s.lookup(name); ok {
			return v
		}
	}
	return ""
}

// known is whether a variable has a value the walk or the environment gives.
func (s *state) known(name string) bool {
	if _, ok := s.vars[name]; ok {
		return true
	}
	if s.lookup != nil {
		_, ok := s.lookup(name)
		return ok
	}
	return false
}

// tildeKnown is whether a leading tilde word is ~ or ~/... with HOME known.
func (s *state) tildeKnown(v string) bool {
	if v != "~" && !strings.HasPrefix(v, "~/") {
		return false
	}
	return s.known("HOME")
}

// programName is the program a word names: its lower-case base name, without a Windows extension.
func programName(v string) string {
	if i := strings.LastIndexByte(v, '/'); i >= 0 {
		v = v[i+1:]
	}
	v = strings.ToLower(v)
	for _, ext := range []string{".exe", ".cmd", ".bat"} {
		v = strings.TrimSuffix(v, ext)
	}
	return v
}

// unquotedCarriageReturn is whether a carriage return lies outside every word and comment. The parser ends a word at a
// lone CR where bash keeps it as an ordinary byte, so such a CR could split one destination into two. A CR inside a
// word (quoted or in a here-document body) or a comment is read the same by both.
func unquotedCarriageReturn(file *syntax.File, src string) bool {
	if !strings.Contains(src, "\r") {
		return false
	}
	type span struct{ from, to uint }
	var spans []span
	syntax.Walk(file, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.Word, *syntax.Comment:
			spans = append(spans, span{n.Pos().Offset(), n.End().Offset()})
		}
		return true
	})
	for i := 0; i < len(src); i++ {
		if src[i] != '\r' {
			continue
		}
		covered := false
		for _, s := range spans {
			if uint(i) >= s.from && uint(i) < s.to {
				covered = true
				break
			}
		}
		if !covered {
			return true
		}
	}
	return false
}
