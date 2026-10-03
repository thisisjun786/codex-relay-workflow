package hook

import (
	"crypto/sha256"
	"fmt"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// FileEditShape is the file and whole-hunk fingerprint of edit-shape.ts:44-111.
// The dormant edit-shape hook and its ledger are not part of this port.
type FileEditShape struct {
	File string `json:"file"`
	Key  string `json:"key"`
}

// NormalizeEditLine performs the oracle's single tokenizer pass. Expanding the
// three quoted alternatives preserves its backreference and escape backtracking.
func NormalizeEditLine(line string) string {
	const noLine = `\n\r\x{2028}\x{2029}`
	quoted := func(q string) string { return q + `(?:\\[^` + noLine + `]|[^` + q + noLine + `])*?` + q }
	tokens := regexp.MustCompile(quoted(`"`) + "|" + quoted("'") + "|" + quoted("`") + `|\d[\w.]*|[A-Za-z_$][\w$]*`)
	line = tokens.ReplaceAllStringFunc(text.Trim(line), func(token string) string {
		switch token[0] {
		case '"', '\'', '`':
			return "S"
		}
		if token[0] >= '0' && token[0] <= '9' {
			return "N"
		}
		return "I"
	})
	var out strings.Builder
	space := false
	for _, r := range line {
		if text.Trim(string(r)) == "" {
			if !space {
				out.WriteByte(' ')
			}
			space = true
		} else {
			out.WriteRune(r)
			space = false
		}
	}
	return out.String()
}

// FileEditShapes skips deleted and unchanged sections and diff headers, just as
// the oracle does; changed lines keep their signs and order in the SHA-256 input.
func FileEditShapes(patch string) []FileEditShape {
	directive := regexp.MustCompile(`^\*\*\* (Add|Update|Delete) File: ([^\r\n\x{2028}\x{2029}]+)$`)
	out := []FileEditShape{}
	file, deleting := "", false
	changed := []string{}
	flush := func() {
		if file != "" && !deleting && len(changed) > 0 {
			out = append(out, FileEditShape{file, fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(changed, "\n"))))})
		}
		changed = nil
	}
	for _, raw := range text.SplitLines(patch) {
		if match := directive.FindStringSubmatch(raw); match != nil {
			flush()
			file, deleting = text.Trim(match[2]), match[1] == "Delete"
			continue
		}
		if strings.HasPrefix(raw, "*** ") {
			if strings.HasPrefix(raw, "*** End Patch") {
				flush()
				file = ""
			}
			continue
		}
		if file == "" || deleting || strings.HasPrefix(raw, "+++") || strings.HasPrefix(raw, "---") {
			continue
		}
		if strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "-") {
			changed = append(changed, raw[:1]+NormalizeEditLine(raw[1:]))
		}
	}
	flush()
	return out
}
