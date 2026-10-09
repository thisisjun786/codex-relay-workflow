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

// AnalyzeNoDir reads a command text with no working directory at all, with no environment. It is the reading of the memory write
// gate that does not depend on a directory (the Python programs of the text, and whether the text is readable at all); the
// judgments that need a directory are made by the readings that have one (see Dir.Unset).
func AnalyzeNoDir(src string) (Result, error) {
	st := newState("")
	st.dir.Unset = true
	return analyze(src, st, nil)
}

func analyze(src string, st *state, lookup func(string) (string, bool)) (Result, error) {
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
	w := &walker{}
	if err := w.stmts(file.Stmts, st, Context{}); err != nil {
		return Result{}, err
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
