package harness

import (
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

// contextHead backs off one unit if a UTF-16 cut would split an astral character.
func contextHead(units []uint16) string {
	if n := len(units); n > 0 && units[n-1] >= 0xD800 && units[n-1] < 0xDC00 {
		units = units[:n-1]
	}
	return strings.TrimRight(string(utf16.Decode(units)), " \t\r\n")
}

// truncated retains the UTF-16 limit and marker without emitting an unpaired surrogate.
func truncated(units []uint16) string {
	return jsString(contextHead(units) + "\n\n[truncated]")
}

// ContextSection lets callers retain identity, snapshot and gate instructions
// before spending the context budget on variable descriptions.
type ContextSection struct {
	Text     string
	Required bool
}

func ContextOutputSections(event string, sections []ContextSection) string {
	lines := make([]string, len(sections))
	required := 0
	for i, s := range sections {
		lines[i] = s.Text
		if s.Required {
			required += len(utf16.Encode([]rune(s.Text)))
		}
	}
	if len(utf16.Encode([]rune(strings.Join(lines, "\n\n")))) <= MaxContext {
		return ContextOutput(event, strings.Join(lines, "\n\n"))
	}
	budget := max(0, MaxContext-64-required-2*max(0, len(lines)-1))
	for i, s := range sections {
		if s.Required {
			continue
		}
		units := utf16.Encode([]rune(s.Text))
		if len(units) > budget {
			lines[i] = contextHead(units[:budget])
		}
		budget -= min(budget, len(units))
	}
	return ContextOutput(event, strings.Join(lines, "\n\n")+"\n\n[truncated]")
}
