package shellir

import "strings"

// Analyze reads a command text run in the working directory cwd, with no environment: a variable the text
// names is unknown, and a home-relative path is unknown.
func Analyze(src, cwd string) (Result, error) {
	return AnalyzeEnv(src, cwd, nil)
}

// AnalyzeEnv reads a command text as AnalyzeEnv's caller's environment shows it: a variable the walk does not
// assign takes its value from lookup, and a leading ~ or ~/ expands to HOME when lookup knows HOME.
func AnalyzeEnv(src, cwd string, lookup func(string) (string, bool)) (Result, error) {
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
	st := newState(cwd)
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
