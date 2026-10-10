package configguard

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The functions below edit one key line the way a byte-exact undo needs. SetTableKey and
// RestoreTableKey are the oracle's and normalise the line they write (`enabled=true` comes back as
// `enabled = true`) and the file's line endings; the switch must give a config.toml back to the byte,
// so it records the key's line verbatim and puts exactly that line back, touching no other byte
// (CRW-201).
//
// Unlike the oracle's grammar (bare table headers and bare keys only, toml-edit.ts:53 and :154) they
// identify a table and a key by the name TOML decodes from its spelling: "enabled", 'enabled',
// "en\u0061bled" and enabled are one key, and [plugins."a@b"], [ plugins . 'a@b' ] and
// ["plugins"."a@b"] are one table. A key the editor does not see would otherwise be written a second
// time, and a config.toml with the key twice is one Codex cannot read (CRW-201 evaluation d1, d3). A
// table that holds the key on more than one line, and a key whose value is not a bare true or false,
// is refused.

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

// tomlSimpleEscape is the character a one-letter escape of a basic string stands for.
func tomlSimpleEscape(c byte) (string, bool) {
	switch c {
	case 'b':
		return "\b", true
	case 't':
		return "\t", true
	case 'n':
		return "\n", true
	case 'f':
		return "\f", true
	case 'r':
		return "\r", true
	case 'e':
		return "\x1b", true
	case '"':
		return "\"", true
	case '\\':
		return "\\", true
	}
	return "", false
}

// tomlKeyPart decodes one part of a dotted key at the start of s: a bare key, a "basic" string with
// its escapes or a 'literal' string. rest is what follows the part.
func tomlKeyPart(s string) (part, rest string, ok bool) {
	if s == "" {
		return "", "", false
	}
	switch s[0] {
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 || strings.ContainsAny(s[1:1+end], "\n\r") {
			return "", "", false
		}
		return s[1 : 1+end], s[end+2:], true
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); {
			switch c := s[i]; {
			case c == '"':
				return b.String(), s[i+1:], true
			case c == '\n' || c == '\r':
				return "", "", false
			case c != '\\':
				b.WriteByte(c)
				i++
			default:
				if i+1 >= len(s) {
					return "", "", false
				}
				if r, found := tomlSimpleEscape(s[i+1]); found {
					b.WriteString(r)
					i += 2
					continue
				}
				width := map[byte]int{'x': 2, 'u': 4, 'U': 8}[s[i+1]]
				if width == 0 || i+2+width > len(s) {
					return "", "", false
				}
				code, err := strconv.ParseUint(s[i+2:i+2+width], 16, 32)
				if err != nil || !utf8.ValidRune(rune(code)) {
					return "", "", false
				}
				b.WriteRune(rune(code))
				i += 2 + width
			}
		}
		return "", "", false
	}
	n := 0
	for n < len(s) && (s[n] == '-' || s[n] == '_' || s[n] >= '0' && s[n] <= '9' || s[n] >= 'a' && s[n] <= 'z' || s[n] >= 'A' && s[n] <= 'Z') {
		n++
	}
	if n == 0 {
		return "", "", false
	}
	return s[:n], s[n:], true
}

// tomlKeyPath decodes the dotted key at the start of s, whitespace around its parts allowed, and
// returns what follows it (after the whitespace).
func tomlKeyPath(s string) (path []string, rest string, ok bool) {
	for {
		s = tomlTrimStart(s)
		part, after, ok := tomlKeyPart(s)
		if !ok {
			return nil, "", false
		}
		path = append(path, part)
		s = tomlTrimStart(after)
		if !strings.HasPrefix(s, ".") {
			return path, s, true
		}
		s = s[1:]
	}
}

// tomlHeaderPath is the decoded name of a "[table]" line, or false when the line is not a table header
// (an array-of-tables header, a key line, a malformed header). A trailing comment is allowed.
func tomlHeaderPath(line string) ([]string, bool) {
	body := tomlTrimStart(line)
	if !strings.HasPrefix(body, "[") || strings.HasPrefix(body, "[[") {
		return nil, false
	}
	path, rest, ok := tomlKeyPath(body[1:])
	if !ok || !strings.HasPrefix(rest, "]") {
		return nil, false
	}
	if tail := tomlTrimStart(rest[1:]); tail != "" && !strings.HasPrefix(tail, "#") {
		return nil, false
	}
	return path, true
}

// exactFindHeader is the index of the first line outside a string that is the header of table, the
// table being spelled as a header body (plugins."a@b"). Headers are compared by their decoded names.
func exactFindHeader(lines []string, inString []bool, table string) int {
	want, rest, ok := tomlKeyPath(table)
	if !ok || rest != "" {
		return -1
	}
	for i, line := range lines {
		if inString[i] {
			continue
		}
		if got, ok := tomlHeaderPath(line); ok && slices.Equal(got, want) {
			return i
		}
	}
	return -1
}

// exactFindKey is tomlFindKey that identifies the key by its decoded name, so it also matches the key
// in a quoted spelling, and refuses (TomlKeyUnsupported) a second line for the key and a value that
// is not a bare true or false. name is the key as the line spells it.
func exactFindKey(lines []string, inString []bool, headerIdx int, key string) (found KeyLine, name string, lookup TomlKeyLookup) {
	lookup = TomlKeyAbsent
	for i, end := tomlBodyStart(lines, headerIdx), tomlBodyEnd(lines, inString, headerIdx); i < end; i++ {
		if inString[i] {
			continue
		}
		line := lines[i]
		body := tomlTrimStart(line)
		path, rest, ok := tomlKeyPath(body)
		if !ok || !slices.Equal(path, []string{key}) || !strings.HasPrefix(rest, "=") {
			continue
		}
		if lookup != TomlKeyAbsent {
			return KeyLine{}, "", TomlKeyUnsupported
		}
		value, comment, ok := tomlSplitValueAndComment(rest[1:])
		if v := text.Trim(value); !ok || v != "true" && v != "false" {
			return KeyLine{}, "", TomlKeyUnsupported
		}
		spelled := body[:len(body)-len(rest)]
		found, name, lookup = KeyLine{Index: i, Indent: line[:len(line)-len(body)], Value: text.Trim(value), Comment: comment}, tomlTrimEnd(spelled), TomlKeyFound
	}
	return found, name, lookup
}

// PluginTableState is what ReadPluginTableState found for one plugin table.
type PluginTableState struct {
	// Present is whether the [plugins."<key>"] table exists, under any spelling TOML reads as that name.
	Present bool
	// Enabled is whether Codex runs the plugin: the table exists and its enabled key is not false.
	Enabled bool
	// Unsupported is whether the enabled key holds a form crw does not interpret; Enabled is then true,
	// so a state that cannot be read is never reported as the plugin being off.
	Unsupported bool
}

// ReadPluginTableState reads plugins."<pluginKey>" under TOML key identity (the same one the switch
// edits by), and writes nothing.
func ReadPluginTableState(content, pluginKey string) PluginTableState {
	st := ReadTableKeyLine(content, `plugins."`+pluginKey+`"`, "enabled")
	out := PluginTableState{Present: st.TablePresent, Unsupported: st.Unsupported}
	out.Enabled = st.TablePresent && (st.Unsupported || !st.Found || st.Value != "false")
	return out
}

// ReadTableKeyLine locates [table] key and returns its line verbatim.
func ReadTableKeyLine(content, table, key string) TomlKeyLineState {
	lines, exact := exactLines(content)
	inString := tomlInString(lines)
	headerIdx := exactFindHeader(lines, inString, table)
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
	headerIdx := exactFindHeader(lines, inString, table)
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
	headerIdx := exactFindHeader(lines, inString, table)
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
