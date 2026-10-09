package configguard

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The functions below edit one key line the way a byte-exact undo needs. SetTableKey and
// RestoreTableKey are the oracle's and normalise the line they write (`enabled=true` comes back as
// `enabled = true`) and the file's line endings; the switch must give a config.toml back to the byte,
// so it records the key's line verbatim and puts exactly that line back, touching no other byte
// (CRW-201).
//
// Unlike the oracle's grammar they also know the key spelled as a quoted key ("enabled" or
// 'enabled'), which TOML reads as the same key: a key the editor does not see would otherwise be
// written a second time, and a config.toml with the key twice is one Codex cannot read. A table that
// holds the key on more than one line is refused.

// TomlKeyLineState is what ReadTableKeyLine found.
type TomlKeyLineState struct {
	// TablePresent is whether the [table] header exists.
	TablePresent bool
	// Found is whether the key line exists in the table.
	Found bool
	// Unsupported is whether the key holds a value form the editor refuses to rewrite.
	Unsupported bool
	// Line is the key's line verbatim, including a trailing CR of a CRLF line and excluding the LF; "" unless Found.
	Line string
	// Value is the raw value text, comment tail removed and trimmed; "" unless Found.
	Value string
}

func exactLines(content string) (read, exact []string) {
	return text.SplitLines(content), text.SplitLinesByteExact(content)
}

// exactFindKey is tomlFindKey that also matches the key in either quoted spelling and refuses
// (TomlKeyUnsupported) a second line for the key. name is the key as the line spells it.
func exactFindKey(lines []string, inString []bool, headerIdx int, key string) (found KeyLine, name string, lookup TomlKeyLookup) {
	q := regexp.QuoteMeta(key)
	re, err := regexp.Compile("^(" + tomlSpace + "*)(" + q + `|"` + q + `"|'` + q + `')` + tomlSpace + "*=(" + tomlNotEOL + "*)$")
	if err != nil {
		return KeyLine{}, "", TomlKeyAbsent
	}
	lookup = TomlKeyAbsent
	for i, end := tomlBodyStart(lines, headerIdx), tomlBodyEnd(lines, inString, headerIdx); i < end; i++ {
		if inString[i] {
			continue
		}
		m := re.FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if lookup != TomlKeyAbsent {
			return KeyLine{}, "", TomlKeyUnsupported
		}
		value, comment, ok := tomlSplitValueAndComment(m[3])
		if !ok {
			return KeyLine{}, "", TomlKeyUnsupported
		}
		found, name, lookup = KeyLine{Index: i, Indent: m[1], Value: text.Trim(value), Comment: comment}, m[2], TomlKeyFound
	}
	return found, name, lookup
}

// ReadTableKeyLine locates [table] key and returns its line verbatim.
func ReadTableKeyLine(content, table, key string) TomlKeyLineState {
	lines, exact := exactLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		return TomlKeyLineState{}
	}
	st := TomlKeyLineState{TablePresent: true}
	found, _, lookup := exactFindKey(lines, inString, headerIdx, key)
	switch lookup {
	case TomlKeyUnsupported:
		st.Unsupported = true
	case TomlKeyFound:
		st.Found, st.Line, st.Value = true, exact[found.Index], found.Value
	}
	return st
}

// SetTableKeyExact sets an existing [table] to key = value. It never creates the table. A key line
// is rewritten as indent + key + " = " + value + comment and keeps its own line ending; a missing key
// is inserted after the last non-blank line of the table with the file's dominant line ending. A
// quoted key keeps its spelling. Every
// other byte of content is returned as it was. The returned state is the key as it was before.
func SetTableKeyExact(content, table, key string, value TomlScalar) (string, TomlKeyLineState, bool) {
	lines, exact := exactLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		return content, TomlKeyLineState{}, false
	}
	st := TomlKeyLineState{TablePresent: true}
	found, name, lookup := exactFindKey(lines, inString, headerIdx, key)
	serialized := tomlSerialize(value)
	switch lookup {
	case TomlKeyUnsupported:
		st.Unsupported = true
		return content, st, false
	case TomlKeyFound:
		st.Found, st.Line, st.Value = true, exact[found.Index], found.Value
		if found.Value == serialized {
			return content, st, false
		}
		out := slices.Clone(exact)
		cr := ""
		if strings.HasSuffix(exact[found.Index], "\r") {
			cr = "\r"
		}
		out[found.Index] = found.Indent + name + " = " + serialized + found.Comment + cr
		return strings.Join(out, "\n"), st, true
	}
	return exactInsertLine(content, lines, exact, inString, headerIdx, key+" = "+serialized), st, true
}

// exactInsertLine inserts line (without its line ending) after the last non-blank line of the table
// that starts at headerIdx, with the file's dominant line ending.
func exactInsertLine(content string, lines, exact []string, inString []bool, headerIdx int, line string) string {
	insertAfter := tomlLastNonBlank(lines, headerIdx+1, tomlBodyEnd(lines, inString, headerIdx))
	cr := ""
	// The line after the last one has no terminator of its own to share, so it takes none.
	if text.DominantEOL(content) == text.CRLF && insertAfter+1 < len(exact) {
		cr = "\r"
	}
	return strings.Join(slices.Insert(slices.Clone(exact), insertAfter+1, line+cr), "\n")
}

// ErrRestoreTableKey is the cause of a restore RestoreTableKeyExact cannot do safely.
var ErrRestoreTableKey = errors.New("the recorded line cannot be put back")

// RestoreTableKeyExact puts the key back to the state ReadTableKeyLine recorded: line verbatim in
// place of the key's current line, or, when the key was absent (priorLine nil), the line removed. A
// key line deleted since is inserted again at the end of its table. The table header is left in
// place. It reports whether content changed, and refuses (ErrRestoreTableKey, content unchanged) a
// key it cannot rewrite or a recorded line whose table is gone; a key that is already as recorded is
// no change and no error.
func RestoreTableKeyExact(content, table, key string, priorLine *string) (string, bool, error) {
	lines, exact := exactLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		if priorLine == nil {
			return content, false, nil
		}
		return content, false, fmt.Errorf("%w: [%s] is no longer in config.toml", ErrRestoreTableKey, table)
	}
	found, _, lookup := exactFindKey(lines, inString, headerIdx, key)
	switch {
	case lookup == TomlKeyUnsupported:
		return content, false, fmt.Errorf("%w: %s.%s currently holds a value crw will not rewrite", ErrRestoreTableKey, table, key)
	case lookup == TomlKeyAbsent && priorLine == nil:
		return content, false, nil
	case lookup == TomlKeyAbsent:
		return exactInsertLine(content, lines, exact, inString, headerIdx, strings.TrimSuffix(*priorLine, "\r")), true, nil
	case priorLine == nil:
		return strings.Join(slices.Delete(slices.Clone(exact), found.Index, found.Index+1), "\n"), true, nil
	case exact[found.Index] == *priorLine:
		return content, false, nil
	}
	out := slices.Clone(exact)
	out[found.Index] = *priorLine
	return strings.Join(out, "\n"), true, nil
}
