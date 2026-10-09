package configguard

import (
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The functions below edit one key line the way a byte-exact undo needs. SetTableKey and
// RestoreTableKey are the oracle's and normalise the line they write (`enabled=true` comes back as
// `enabled = true`) and the file's line endings; the switch must give a config.toml back to the byte,
// so it records the key's line verbatim and puts exactly that line back, touching no other byte
// (CRW-201).

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

// ReadTableKeyLine locates [table] key and returns its line verbatim.
func ReadTableKeyLine(content, table, key string) TomlKeyLineState {
	lines, exact := exactLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		return TomlKeyLineState{}
	}
	st := TomlKeyLineState{TablePresent: true}
	found, lookup := tomlFindKey(lines, inString, headerIdx, key)
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
// is inserted after the last non-blank line of the table with the file's dominant line ending. Every
// other byte of content is returned as it was. The returned state is the key as it was before.
func SetTableKeyExact(content, table, key string, value TomlScalar) (string, TomlKeyLineState, bool) {
	lines, exact := exactLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		return content, TomlKeyLineState{}, false
	}
	st := TomlKeyLineState{TablePresent: true}
	found, lookup := tomlFindKey(lines, inString, headerIdx, key)
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
		out[found.Index] = found.Indent + key + " = " + serialized + found.Comment + cr
		return strings.Join(out, "\n"), st, true
	}
	insertAfter := tomlLastNonBlank(lines, headerIdx+1, tomlBodyEnd(lines, inString, headerIdx))
	cr := ""
	// The line after the last one has no terminator of its own to share, so it takes none.
	if text.DominantEOL(content) == text.CRLF && insertAfter+1 < len(exact) {
		cr = "\r"
	}
	out := slices.Insert(slices.Clone(exact), insertAfter+1, key+" = "+serialized+cr)
	return strings.Join(out, "\n"), st, true
}

// RestoreTableKeyExact puts the key back to the state ReadTableKeyLine recorded: line verbatim in
// place of the key's current line, or, when the key was absent (priorLine nil), the line removed. The
// table header is left in place. It reports whether content changed.
func RestoreTableKeyExact(content, table, key string, priorLine *string) (string, bool) {
	lines, exact := exactLines(content)
	inString := tomlInString(lines)
	headerIdx := tomlFindHeader(lines, inString, table)
	if headerIdx == -1 {
		return content, false
	}
	found, lookup := tomlFindKey(lines, inString, headerIdx, key)
	if lookup != TomlKeyFound {
		return content, false
	}
	if priorLine == nil {
		return strings.Join(slices.Delete(slices.Clone(exact), found.Index, found.Index+1), "\n"), true
	}
	if exact[found.Index] == *priorLine {
		return content, false
	}
	out := slices.Clone(exact)
	out[found.Index] = *priorLine
	return strings.Join(out, "\n"), true
}
