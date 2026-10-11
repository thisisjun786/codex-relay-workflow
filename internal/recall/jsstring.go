package recall

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// isJSSpace is the white space of JavaScript's \s: text.Trim strips exactly that set, so a lone character is space iff it trims away.
func isJSSpace(r rune) bool { return text.Trim(string(r)) == "" }
