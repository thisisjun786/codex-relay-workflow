package harness

import (
	"fmt"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// MaxContext is hook.ts' MAX_CTX, counted as JavaScript counts a string's length: in UTF-16 code units.
const MaxContext = 32000

// ContextOutput is the line a hook writes to hand the model context (hook.ts buildContextOutput,
// :587-597): CRLF and CR become LF, the text is trimmed as String.prototype.trim does, an empty one
// gives no output, and one over MaxContext units is cut at MaxContext-64 with trailing spaces, tabs
// and line breaks removed and "\n\n[truncated]" appended. The text is wrapped in the
// hookSpecificOutput envelope with a newline. The caller passes a normalised, valid UTF-8 string.
func ContextOutput(event, ctx string) string {
	norm := text.Trim(strings.ReplaceAll(strings.ReplaceAll(ctx, "\r\n", "\n"), "\r", "\n"))
	if norm == "" {
		return ""
	}
	body := jsString(norm)
	if units := utf16.Encode([]rune(norm)); len(units) > MaxContext {
		body = truncated(units[:MaxContext-64])
	}
	return `{"hookSpecificOutput":{"hookEventName":` + jsString(event) + `,"additionalContext":` + body + `}}` + "\n"
}

// truncated is the cut text followed by the marker, as JSON.stringify writes it. JavaScript cuts
// between UTF-16 units, so the cut can split an astral character and leave its high surrogate last;
// a Go string cannot hold that, so it is written as the escape JSON.stringify gives a lone
// surrogate (lower-case hex) and the trailing-space trim, which a regular expression anchored at the
// end of the text does not reach past it, is skipped.
func truncated(units []uint16) string {
	lone := ""
	if n := len(units); units[n-1] >= 0xD800 && units[n-1] < 0xDC00 {
		lone, units = fmt.Sprintf(`\u%04x`, units[n-1]), units[:n-1]
	}
	head := string(utf16.Decode(units))
	if lone == "" {
		head = strings.TrimRight(head, " \t\r\n")
	}
	quoted := jsString(head)
	return quoted[:len(quoted)-1] + lone + jsString("\n\n[truncated]")[1:]
}
