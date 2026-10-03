package reading

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

// JSONKind is a decoded JSON value's kind, as a message names it (quote.Kind).
func JSONKind(v any) string { return quote.Kind(v) }

// Show is a decoded value as a message names it: its JSON text.
func Show(v any) string { return pyjson.Dumps(v, pyjson.Options{}) }

// Text is a string value itself, and any other value as Show spells it.
func Text(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return Show(v)
}

// Spelling is a path's lexical form: repeated separators and "." components dropped and a
// trailing separator gone, ".." kept (folding it lexically would name another directory behind a
// symbolic link), so "/d/current/" and "/d//./current" are "/d/current".
func Spelling(p string) string {
	var kept []string
	for _, part := range strings.Split(p, "/") {
		if part != "" && part != "." {
			kept = append(kept, part)
		}
	}
	out := strings.Join(kept, "/")
	switch {
	case strings.HasPrefix(p, "/"):
		return "/" + out
	case out == "":
		return "."
	}
	return out
}
