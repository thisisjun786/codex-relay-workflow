package harness

import (
	"io"
	"strings"
	"unicode/utf8"
)

// MaxStdinBytes is the most a hook may send; more is refused, never cut short (cli.ts:76).
const MaxStdinBytes = 4 * 1024 * 1024

const oversizedReason = "[crw] hook input exceeded 4194304 bytes; refusing to bypass policy enforcement"

// ReadStdin reads a hook's input, at most MaxStdinBytes of it: one byte more is an overflow and the
// input is dropped, and a read that fails reads as empty input, whatever it had returned before
// (cli.ts readStdin). The input is text as Buffer.toString("utf8") makes it (utf8Text), so a limit on
// its byte length counts each U+FFFD as three bytes.
func ReadStdin(in io.Reader) (raw string, overflow bool) {
	b, err := io.ReadAll(io.LimitReader(in, MaxStdinBytes+1))
	if err != nil {
		return "", false
	}
	if len(b) > MaxStdinBytes {
		return "", true
	}
	return utf8Text(b), false
}

// utf8Text is Buffer.toString("utf8"): bytes that are not UTF-8 become U+FFFD, one for each maximal
// invalid subpart, as the WHATWG decoder defines it. A byte that ends a subpart early starts what follows.
func utf8Text(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var out strings.Builder
	var cp rune
	need, seen := 0, 0
	lower, upper := byte(0x80), byte(0xBF)
	for i := 0; i < len(b); i++ {
		c := b[i]
		if need == 0 {
			switch {
			case c < 0x80:
				out.WriteByte(c)
			case c >= 0xC2 && c <= 0xDF:
				need, cp = 1, rune(c&0x1F)
			case c >= 0xE0 && c <= 0xEF:
				need, cp = 2, rune(c&0x0F)
				if c == 0xE0 {
					lower = 0xA0
				} else if c == 0xED {
					upper = 0x9F
				}
			case c >= 0xF0 && c <= 0xF4:
				need, cp = 3, rune(c&0x07)
				if c == 0xF0 {
					lower = 0x90
				} else if c == 0xF4 {
					upper = 0x8F
				}
			default:
				out.WriteRune(utf8.RuneError)
			}
			continue
		}
		if c < lower || c > upper {
			need, seen, cp, lower, upper = 0, 0, 0, 0x80, 0xBF
			out.WriteRune(utf8.RuneError)
			i--
			continue
		}
		lower, upper = 0x80, 0xBF
		cp, seen = cp<<6|rune(c&0x3F), seen+1
		if seen == need {
			out.WriteRune(cp)
			need, seen, cp = 0, 0, 0
		}
	}
	if need != 0 {
		out.WriteRune(utf8.RuneError)
	}
	return out.String()
}

// OversizedHookOutput is what a leg of this slug answers to an input over MaxStdinBytes: a PreToolUse
// deny for any slug that starts with pre-tool-use, a block for stop and subagent-stop, and nothing for
// the rest (cli.ts:101-117). A slug that answers nothing leaves the process to exit 1, so the guard
// whose slug does not start with pre-tool-use (worktree-guard-pretool) is not a deny.
func OversizedHookOutput(slug string) string {
	switch {
	case strings.HasPrefix(slug, "pre-tool-use"):
		return `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"` + oversizedReason + `","additionalContext":"` + oversizedReason + `"}}` + "\n"
	case slug == "stop" || slug == "subagent-stop":
		return `{"decision":"block","reason":"` + oversizedReason + `"}` + "\n"
	}
	return ""
}
