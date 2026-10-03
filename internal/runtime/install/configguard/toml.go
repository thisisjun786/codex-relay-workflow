// Package configguard is the Go form of CXC v0.2.40's config-guard component: the editor for the few keys CRW sets in the host's
// config.toml. This file is config-guard/src/toml-edit.ts. Every function is a pure string-to-string transform over a line-based reading of
// TOML tables and keys; nothing here opens a file, so the grammar can be tested from strings and cannot reach a real config.toml by itself.
//
// The grammar is the oracle's, quirks included (docs/port-cxc/known-defects.md): it recognises only the bare [table] header and bare
// "key = value" lines, and refuses value forms it cannot rewrite. It differs from the oracle in one class on purpose: a line that is the
// inside of a multi-line string is never a table header, a table start or a key, because the oracle rewrote or deleted such a line as if it
// were the managed key, which loses text of another setting.
package configguard

import (
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// TomlScalar is the one value the editor writes. The oracle narrowed it to boolean on purpose: a generic writer would have to solve TOML
// quoting and comment splitting for value forms nothing needs yet (toml-edit.ts:20-25).
type TomlScalar = bool

// TomlEditAction says what an edit did, so a test can prove each branch ran.
type TomlEditAction string

// The six outcomes of an edit; the strings are the oracle's.
const (
	TomlUpdated           TomlEditAction = "updated"
	TomlInsertedIntoTable TomlEditAction = "inserted-into-table"
	TomlCreatedTable      TomlEditAction = "created-table"
	TomlRemoved           TomlEditAction = "removed"
	TomlNoop              TomlEditAction = "noop"
	TomlUnsupportedValue  TomlEditAction = "unsupported-value"
)

// TomlEditResult is the outcome of SetTableKey and RestoreTableKey.
type TomlEditResult struct {
	// Content is the full new content, identical to the input when nothing changed.
	Content string
	// PriorValue is the value found before the edit, verbatim minus any comment tail, or nil when the key was absent.
	PriorValue *string
	Changed    bool
	Action     TomlEditAction
}

// KeyLine is a "key = value" line found inside a table.
type KeyLine struct {
	// Index is the line's index within the file.
	Index int
	// Indent is the leading whitespace, kept on rewrite.
	Indent string
	// Value is the raw value text with the comment tail removed, trimmed.
	Value string
	// Comment is the comment tail with its leading spaces, or "" when there is none.
	Comment string
}

// TomlKeyLookup is what FindKeyLine found.
type TomlKeyLookup int

// The three answers of FindKeyLine: no such key, the key, or a key whose value form the editor refuses to rewrite.
const (
	TomlKeyAbsent TomlKeyLookup = iota
	TomlKeyFound
	TomlKeyUnsupported
)

// The two character classes JavaScript's \s and . mean in the oracle's patterns. RE2's \s is ASCII only (no vertical tab, no NBSP, no
// U+FEFF) and its . excludes only LF, so the port spells the classes out. They are compiled inside the functions below, as the oracle does
// (new RegExp per call): the package holds no initialised variable.
const (
	tomlSpace    = "[\\t\\n\\v\\f\\r \\x{a0}\\x{1680}\\x{2000}-\\x{200a}\\x{2028}\\x{2029}\\x{202f}\\x{205f}\\x{3000}\\x{feff}]"
	tomlNotEOL   = "[^\\n\\r\\x{2028}\\x{2029}]"
	tomlQuoteRun = 3 // the quotes that open or close a multi-line string
)

// tomlIsSpace is JavaScript's whitespace, defined once as text.Trim's.
func tomlIsSpace(r rune) bool { return text.Trim(string(r)) == "" }

func tomlTrimStart(s string) string { return strings.TrimLeftFunc(s, tomlIsSpace) }

func tomlTrimEnd(s string) string { return strings.TrimRightFunc(s, tomlIsSpace) }

// tomlRunLength is the length of the run of identical bytes that starts at line[i].
func tomlRunLength(line string, i int) int {
	n := 1
	for i+n < len(line) && line[i+n] == line[i] {
		n++
	}
	return n
}

// tomlInString reports, for each line, whether it STARTS inside a multi-line string. A scan over all the lines tracks the strings: in
// normal state a # ends the line, three quotes open a multi-line string, and a single quote opens a one-line string that ends at its
// closing quote or at the end of the line; inside a """ string a backslash skips the next byte, and inside either kind the first run of
// three or more quotes closes it, the whole run being consumed (one or two of them are content) and the line scanned on. Brackets are not
// tracked: an array that spans lines is still read line by line, as the oracle does.
func tomlInString(lines []string) []bool {
	mask := make([]bool, len(lines))
	var open byte // the quote of the multi-line string the scan is inside, or 0
	for n, line := range lines {
		mask[n] = open != 0
		for i := 0; i < len(line); {
			c := line[i]
			switch {
			case open != 0 && c == '\\' && open == '"':
				i += 2
			case open != 0 && c == open:
				run := tomlRunLength(line, i)
				i += run
				if run >= tomlQuoteRun {
					open = 0
				}
			case open != 0:
				i++
			case c == '#':
				i = len(line)
			case c != '"' && c != '\'':
				i++
			case tomlRunLength(line, i) >= tomlQuoteRun:
				open = c
				i += tomlQuoteRun
			default:
				i++ // a one-line string: skip to its closing quote, or to the end of the line
				for ; i < len(line) && line[i] != c; i++ {
					if c == '"' && line[i] == '\\' {
						i++
					}
				}
				i++
			}
		}
	}
	return mask
}

// tomlFindHeader is the index of the first [header] line outside a string, or -1. A trailing comment on the header is allowed. The pattern
// is ^\s*\[header\]\s*(?:#.*)?$ (toml-edit.ts:53); a header that cannot be a pattern (invalid UTF-8, which no JavaScript string holds)
// matches nothing.
func tomlFindHeader(lines []string, inString []bool, header string) int {
	re, err := regexp.Compile("^" + tomlSpace + "*\\[" + regexp.QuoteMeta(header) + "\\]" + tomlSpace + "*(?:#" + tomlNotEOL + "*)?$")
	if err != nil {
		return -1
	}
	for i, line := range lines {
		if !inString[i] && re.MatchString(line) {
			return i
		}
	}
	return -1
}

// tomlBodyStart is the index of the first line after headerIdx, kept within [0, len(lines)+1] without overflowing: a headerIdx below -1
// reads as -1 (the oracle's lines[i] is undefined there and matches nothing) and one at or past the last line leaves no body.
func tomlBodyStart(lines []string, headerIdx int) int { return min(max(headerIdx, -1), len(lines)) + 1 }

// tomlBodyEnd is the exclusive end of a table body that starts after headerIdx: the next table start (a line whose first non-space
// character is [) outside a string.
func tomlBodyEnd(lines []string, inString []bool, headerIdx int) int {
	for i := tomlBodyStart(lines, headerIdx); i < len(lines); i++ {
		if !inString[i] && strings.HasPrefix(tomlTrimStart(lines[i]), "[") {
			return i
		}
	}
	return len(lines)
}

// tomlCountTrailingSpaces counts the spaces and tabs at the end of s (not the other JavaScript whitespace).
func tomlCountTrailingSpaces(s string) int { return len(s) - len(strings.TrimRight(s, " \t")) }

// tomlSplitValueAndComment splits "= <value> # <comment>" conservatively. It refuses (ok is false) the value forms the editor will not
// rewrite: multi-line strings, arrays, inline tables, and a quote that does not close on the line. A # inside a quoted scalar is skipped
// by scanning past the closing quote first. Every byte compared is ASCII, so byte offsets agree with the oracle's UTF-16 ones.
func tomlSplitValueAndComment(raw string) (value, comment string, ok bool) {
	body := tomlTrimStart(raw)
	lead := raw[:len(raw)-len(body)]
	for _, prefix := range []string{"\"\"\"", "'''", "[", "{"} {
		if strings.HasPrefix(body, prefix) {
			return "", "", false
		}
	}
	if body != "" && (body[0] == '"' || body[0] == '\'') {
		quote, closed := body[0], -1
		for i := 1; i < len(body); { // basic strings honor backslash escapes, literal strings do not
			if quote == '"' && body[i] == '\\' {
				i += 2
				continue
			}
			if body[i] == quote {
				closed = i
				break
			}
			i++
		}
		if closed == -1 {
			return "", "", false
		}
		scalar, tail := body[:closed+1], body[closed+1:]
		hash := strings.IndexByte(tail, '#')
		if hash == -1 {
			return lead + scalar + tomlTrimEnd(tail), "", true
		}
		return lead + scalar, tail[hash-tomlCountTrailingSpaces(tail[:hash]):], true
	}
	hash := strings.IndexByte(body, '#') // a bare value: the first '#' starts the comment, with or without a space before it
	if hash == -1 {
		return lead + tomlTrimEnd(body), "", true
	}
	before := body[:hash]
	return lead + tomlTrimEnd(before), before[len(before)-tomlCountTrailingSpaces(before):] + body[hash:], true
}

// tomlFindKey locates "key = value" inside the table that starts at headerIdx, skipping lines inside a string. The pattern is
// ^(\s*)key\s*=(.*)$ (toml-edit.ts:154).
func tomlFindKey(lines []string, inString []bool, headerIdx int, key string) (KeyLine, TomlKeyLookup) {
	re, err := regexp.Compile("^(" + tomlSpace + "*)" + regexp.QuoteMeta(key) + tomlSpace + "*=(" + tomlNotEOL + "*)$")
	if err != nil {
		return KeyLine{}, TomlKeyAbsent
	}
	for i, end := tomlBodyStart(lines, headerIdx), tomlBodyEnd(lines, inString, headerIdx); i < end; i++ {
		if inString[i] {
			continue
		}
		m := re.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		value, comment, ok := tomlSplitValueAndComment(m[2])
		if !ok {
			return KeyLine{}, TomlKeyUnsupported
		}
		return KeyLine{Index: i, Indent: m[1], Value: text.Trim(value), Comment: comment}, TomlKeyFound
	}
	return KeyLine{}, TomlKeyAbsent
}

// tomlLastNonBlank is the last index in [from, to) of a line that is not blank, or from-1 when they all are. It is not masked: on valid
// TOML the last non-blank line of a body that ends in a string is the string's closing line, which is where the key belongs.
func tomlLastNonBlank(lines []string, from, to int) int {
	last := from - 1
	for i := from; i < to; i++ {
		if text.Trim(lines[i]) != "" {
			last = i
		}
	}
	return last
}

func tomlSerialize(value TomlScalar) string {
	if value {
		return "true"
	}
	return "false"
}

// FindTableHeader is the index of the [header] line, or -1. A trailing comment on the header is allowed.
func FindTableHeader(lines []string, header string) int {
	return tomlFindHeader(lines, tomlInString(lines), header)
}

// TomlTableBody is the body lines of the table [header], up to (not including) the next table header, joined by LF; false when the table
// is absent.
func TomlTableBody(content, header string) (string, bool) {
	lines := text.SplitLines(content)
	inString := tomlInString(lines)
	start := tomlFindHeader(lines, inString, header)
	if start == -1 {
		return "", false
	}
	return strings.Join(lines[start+1:tomlBodyEnd(lines, inString, start)], "\n"), true
}

// FindKeyLine locates "key = value" inside the table that starts at headerIdx. It answers TomlKeyAbsent when the key is not there and
// TomlKeyUnsupported when it is but carries a value form the editor refuses to rewrite.
func FindKeyLine(lines []string, headerIdx int, key string) (KeyLine, TomlKeyLookup) {
	return tomlFindKey(lines, tomlInString(lines), headerIdx, key)
}

// SetTableKey sets [table] key = value, preserving the dominant line ending, comment tails, neighboring tables and whether the text ended
// with a newline.
func SetTableKey(content, table, key string, value TomlScalar) TomlEditResult {
	eol := text.DominantEOL(content)
	lines := text.SplitLines(content)
	inString := tomlInString(lines)
	serialized := tomlSerialize(value)
	headerIdx := tomlFindHeader(lines, inString, table)

	if headerIdx == -1 {
		out := slices.Clone(lines)
		// Keep exactly one blank line before a table we append, and never leave the previous content glued to our header.
		for len(out) > 0 && text.Trim(out[len(out)-1]) == "" {
			out = out[:len(out)-1]
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, "["+table+"]", key+" = "+serialized, "")
		return TomlEditResult{Content: text.WithEOL(strings.Join(out, "\n"), eol), Changed: true, Action: TomlCreatedTable}
	}

	found, lookup := tomlFindKey(lines, inString, headerIdx, key)
	switch lookup {
	case TomlKeyUnsupported:
		return TomlEditResult{Content: content, Action: TomlUnsupportedValue}
	case TomlKeyFound:
		prior := found.Value
		if prior == serialized {
			return TomlEditResult{Content: content, PriorValue: &prior, Action: TomlNoop}
		}
		out := slices.Clone(lines)
		out[found.Index] = found.Indent + key + " = " + serialized + found.Comment
		return TomlEditResult{Content: text.WithEOL(strings.Join(out, "\n"), eol), PriorValue: &prior, Changed: true, Action: TomlUpdated}
	}

	insertAfter := tomlLastNonBlank(lines, headerIdx+1, tomlBodyEnd(lines, inString, headerIdx))
	out := slices.Insert(slices.Clone(lines), insertAfter+1, key+" = "+serialized)
	return TomlEditResult{Content: text.WithEOL(strings.Join(out, "\n"), eol), Changed: true, Action: TomlInsertedIntoTable}
}

// RestoreTableKey restores [table] key to priorValue, or removes the key line when priorValue is nil. The [table] header is always left in
// place, even when removing the last key: the table belongs to codex, "empty" is not decidable from this narrow view, and dropping a
// user's header and its comments would be a write outside the key the editor owns.
func RestoreTableKey(content, table, key string, priorValue *string) TomlEditResult {
	eol := text.DominantEOL(content)
	lines := text.SplitLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		return TomlEditResult{Content: content, Action: TomlNoop}
	}
	found, lookup := tomlFindKey(lines, inString, headerIdx, key)
	switch lookup {
	case TomlKeyUnsupported:
		return TomlEditResult{Content: content, Action: TomlUnsupportedValue}
	case TomlKeyAbsent:
		return TomlEditResult{Content: content, Action: TomlNoop}
	}
	prior := found.Value
	if priorValue == nil {
		out := slices.Delete(slices.Clone(lines), found.Index, found.Index+1)
		return TomlEditResult{Content: text.WithEOL(strings.Join(out, "\n"), eol), PriorValue: &prior, Changed: true, Action: TomlRemoved}
	}
	if prior == *priorValue {
		return TomlEditResult{Content: content, PriorValue: &prior, Action: TomlNoop}
	}
	out := slices.Clone(lines)
	out[found.Index] = found.Indent + key + " = " + *priorValue + found.Comment
	return TomlEditResult{Content: text.WithEOL(strings.Join(out, "\n"), eol), PriorValue: &prior, Changed: true, Action: TomlUpdated}
}

// ReadTableKey is the current raw value of [table] key, or false when it is absent. A value form the editor refuses to rewrite reads as
// absent too.
func ReadTableKey(content, table, key string) (string, bool) {
	lines := text.SplitLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		return "", false
	}
	found, lookup := tomlFindKey(lines, inString, headerIdx, key)
	return found.Value, lookup == TomlKeyFound
}
