//go:build dev

package cxcfuzz

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// canonicalLongString is the length from which canonical writes a string by one pass over its bytes instead of
// the writer's pass over its characters. The recursion-limit answer of the pyjson oracle holds a two-megabyte
// traceback, and every canonical text of such an answer cost the writer more than twenty milliseconds.
const canonicalLongString = 4096

// canonicalDepthLimit is how deep canonicalLong follows containers before it leaves the value to the writer, which
// knows what to do with a container that holds itself.
const canonicalDepthLimit = 2000

// canonicalPlaceholder and its written form stand in for a long string while the writer writes the rest of the
// value. The placeholder is a string of control characters and letters the writer escapes as \u0000, so its written
// form is fixed; a value that already holds text of that form is detected (a placeholder must be found exactly once)
// and left to the writer.
const (
	canonicalPlaceholderLead  = `"\u0000crwbig`
	canonicalPlaceholderTrail = `\u0000"`
)

// canonicalLong is canonical for a value that holds a long ASCII string: those strings are set aside, the writer
// writes the value with a short placeholder where each stood, and each placeholder is then replaced by the string's
// written form. The text is the writer's own text, because the writer writes a string by that string alone, and a
// placeholder is replaced only when every one is found exactly once. ok is false when the value holds no such
// string or the text cannot be rebuilt this way, and the caller then asks the writer.
func canonicalLong(value any) (text string, ok bool) {
	var held []string
	swapped, _, ok := swapLongStrings(value, &held, 0)
	if !ok || len(held) == 0 {
		return "", false
	}
	return restoreLongStrings(pyjson.Dumps(swapped, pyjson.Options{SortKeys: true, Bytes: pyjson.SurrogateEscapes}), held)
}

// swapLongStrings returns value with every long ASCII string below it (in an object's member value, an array
// element or a map value, never a key) replaced by a placeholder, the strings appended to held in the order met.
// changed says whether anything was replaced: a container no long string stands in is returned as it is, and one
// that has is copied. ok is false at the depth limit.
func swapLongStrings(value any, held *[]string, depth int) (out any, changed, ok bool) {
	if depth > canonicalDepthLimit {
		return value, false, false
	}
	switch v := value.(type) {
	case string:
		if len(v) >= canonicalLongString && isASCII(v) {
			*held = append(*held, v)
			return "\x00crwbig" + strconv.Itoa(len(*held)-1) + "\x00", true, true
		}
	case pyjson.Object:
		var copied pyjson.Object
		for i, item := range v {
			inner, innerChanged, ok := swapLongStrings(item.Value, held, depth+1)
			if !ok {
				return value, false, false
			}
			if innerChanged {
				if copied == nil {
					copied = make(pyjson.Object, len(v))
					copy(copied, v)
				}
				copied[i].Value = inner
			}
		}
		if copied != nil {
			return copied, true, true
		}
	case []any:
		var copied []any
		for i, item := range v {
			inner, innerChanged, ok := swapLongStrings(item, held, depth+1)
			if !ok {
				return value, false, false
			}
			if innerChanged {
				if copied == nil {
					copied = make([]any, len(v))
					copy(copied, v)
				}
				copied[i] = inner
			}
		}
		if copied != nil {
			return copied, true, true
		}
	case map[string]any:
		var copied map[string]any
		for key, item := range v {
			inner, innerChanged, ok := swapLongStrings(item, held, depth+1)
			if !ok {
				return value, false, false
			}
			if innerChanged {
				if copied == nil {
					copied = make(map[string]any, len(v))
					for k, x := range v {
						copied[k] = x
					}
				}
				copied[key] = inner
			}
		}
		if copied != nil {
			return copied, true, true
		}
	}
	return value, false, true
}

// restoreLongStrings replaces each placeholder in the writer's text by the written form of the string it stood
// for. It reports false unless every held string is found exactly once and nothing else in the text has the
// placeholder's form.
func restoreLongStrings(text string, held []string) (string, bool) {
	size := len(text)
	for _, s := range held {
		size += len(s) + len(s)/8
	}
	var out strings.Builder
	out.Grow(size)
	seen := make([]bool, len(held))
	rest := text
	for {
		at := strings.Index(rest, canonicalPlaceholderLead)
		if at < 0 {
			break
		}
		digits := at + len(canonicalPlaceholderLead)
		end := digits
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end == digits || !strings.HasPrefix(rest[end:], canonicalPlaceholderTrail) {
			return "", false
		}
		n, err := strconv.Atoi(rest[digits:end])
		if err != nil || n >= len(held) || seen[n] {
			return "", false
		}
		seen[n] = true
		out.WriteString(rest[:at])
		writeASCIIString(&out, held[n])
		rest = rest[end+len(canonicalPlaceholderTrail):]
	}
	for _, found := range seen {
		if !found {
			return "", false
		}
	}
	out.WriteString(rest)
	return out.String(), true
}

// isASCII reports whether every byte of s is below 0x80.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// writeASCIIString writes an ASCII string as the pyjson writer writes it (json.dumps with ensure_ascii): the quote
// and the backslash with a backslash, the five short escapes, and every other control and DEL as \u00xx.
func writeASCIIString(out *strings.Builder, s string) {
	out.WriteByte('"')
	last := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c <= 0x7e && c != '"' && c != '\\' {
			continue
		}
		out.WriteString(s[last:i])
		last = i + 1
		switch c {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		default:
			fmt.Fprintf(out, `\u%04x`, c)
		}
	}
	out.WriteString(s[last:])
	out.WriteByte('"')
}
