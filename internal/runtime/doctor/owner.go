package doctor

import (
	"strings"
	"unicode"
)

// tokenBreaks are the characters a shell line or a joined string is cut at to find the words in
// it that name CRW: white space, quotes, and the operators and separators of the shell grammar.
const tokenBreaks = "'\"`;&|()<>=,"

// ownsMalformed settles whether a registration of hooks.json or config.toml that is not what its
// reader takes could be CRW's, by reading what it holds the way a well-formed one is read: it sets
// s.ours when anything it names or runs is CRW's. It is called inside shared (which clears s.ours
// before and trims what is not CRW's after) and never when ScanOptions.Foreign keeps everything.
//
// j is the judge of the entry's own context, built by the caller as for a well-formed entry (the
// server's cwd, and its env's HOME, CODEX_HOME and PATH; a hook's expansions), so a malformed
// entry cannot be read less than a well-formed one. name is the server's name (CRW registers the
// bridge under one), and unit is the entry as decoded: a map key is matched by spelling only,
// never read as a path, and every string value is read as a shell line (quotes, sh -c, env, exec
// and wrapper scripts followed) and as one literal program (an absolute path, a bare command
// on PATH, a relative path against cwd, also with a leading ~ or $HOME made, since a path may
// hold a space), besides being searched for the names of CRW's programs and the destination.
// Non-string values name nothing. A reading that fails reads as CRW's: the safe side.
func (s *scan) ownsMalformed(j argvJudge, name string, unit any) {
	defer func() {
		if recover() != nil {
			s.ours = true
		}
	}()
	report := j.report
	j.report = func(word string, e Executable) {
		if strings.Contains(e.Detail, readingFailed) {
			s.ours = true
		}
		report(word, e)
	}
	s.ours = s.ours || s.crwWord(name)
	s.walkMalformed(unit, func(text string) {
		if text == "" {
			return
		}
		s.ours = s.ours || s.crwWord(text)
		for _, word := range strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(tokenBreaks, r) }) {
			s.ours = s.ours || s.crwWord(word)
		}
		j.program(text)
		j.argv([]shellWord{literal(text)}, false)
		if expanded := expandedPrefix(j.c.Expand, text); expanded != "" {
			j.argv([]shellWord{literal(expanded)}, false)
		}
	})
}

// walkMalformed visits every string value of a decoded entry, JSON or TOML, and matches each map
// key by spelling.
func (s *scan) walkMalformed(v any, visit func(text string)) {
	switch value := v.(type) {
	case string:
		visit(value)
	case Object:
		for _, field := range value {
			s.ours = s.ours || s.crwWord(field.Key)
			s.walkMalformed(field.Value, visit)
		}
	case map[string]any:
		for key, item := range value {
			s.ours = s.ours || s.crwWord(key)
			s.walkMalformed(item, visit)
		}
	case []map[string]any:
		for _, item := range value {
			s.walkMalformed(item, visit)
		}
	case []any:
		for _, item := range value {
			s.walkMalformed(item, visit)
		}
	}
}

// expandedPrefix is text with a leading ~, $NAME or ${NAME} made from x's variables, as a shell
// makes it, or "" when text starts with none of them.
func expandedPrefix(x Expander, text string) string {
	if home := x.Vars["HOME"]; home != "" && (text == "~" || strings.HasPrefix(text, "~/")) {
		return home + text[1:]
	}
	for name, value := range x.Vars {
		if value == "" {
			continue
		}
		for _, spelling := range []string{"$" + name, "${" + name + "}"} {
			if text == spelling || strings.HasPrefix(text, spelling+"/") {
				return value + text[len(spelling):]
			}
		}
	}
	return ""
}
