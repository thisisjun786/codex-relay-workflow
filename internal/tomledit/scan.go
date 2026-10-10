package tomledit

import (
	"errors"
	"slices"
	"strings"
)

// The scanner below finds where each statement of a document lies, so an edit can touch only the bytes it means to. It reads
// the statements TOML has (a [table] or [[array]] header, a key = value line, a comment, a blank line) with the strings,
// arrays and inline tables of a value read whole, so a header-like or key-like line inside a multi-line string or a multi-line
// array is never taken for a statement. It is run only on a document the decoder accepted, and answers an error for anything
// it does not expect rather than guessing; the decoded document stays the authority on what a key holds.

type stmtKind int

const (
	kvStmt stmtKind = iota
	tableStmt
	arrayTableStmt
)

type stmt struct {
	kind stmtKind
	// path is the full key path of a key = value statement (the section's header path, then the key as written), or the
	// path a header names.
	path []string
	// rel is the key path as written on a key = value line.
	rel []string
	// indent is the blank space before the statement.
	indent string
	// lineStart is the offset of the line's first byte; end is past the statement's line ending, or the end of the input.
	lineStart, end int
	// eol is the line ending that ended the statement, "" at the end of the input.
	eol string
	// valStart and valEnd delimit the value of a key = value statement, without blanks or a comment.
	valStart, valEnd int
	// multiline marks a value crw does not rewrite: a multi-line string, an array or an inline table.
	multiline bool
}

type document struct {
	stmts []stmt
	// last holds, per section (the index of its header statement, -1 for the root), the index into items of the last
	// non-blank line of that section: a statement or a comment.
	last  map[int]item
	crlf  bool
	input string
}

// item is a non-blank line: where it ends, the line ending that ended it, and the indent of the last key line before it in
// its section.
type item struct {
	end    int
	eol    string
	indent string
}

var errScan = errors.New("config.toml has a form crw cannot locate safely")

func isWS(c byte) bool { return c == ' ' || c == '\t' }

func skipWS(s string, i int) int {
	for i < len(s) && isWS(s[i]) {
		i++
	}
	return i
}

// lineEnd answers the end of the line that holds i (past its line ending) and that ending.
func lineEnd(s string, i int) (int, string) {
	n := strings.IndexByte(s[i:], '\n')
	if n < 0 {
		return len(s), ""
	}
	end := i + n
	if end > 0 && s[end-1] == '\r' {
		return end + 1, "\r\n"
	}
	return end + 1, "\n"
}

func atEOL(s string, i int) bool {
	return i >= len(s) || s[i] == '\n' || s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n'
}

func isBare(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// parseKey reads a dotted key at i and answers its segments and the offset after it.
func parseKey(s string, i int) ([]string, int, error) {
	var segs []string
	for {
		i = skipWS(s, i)
		if i >= len(s) {
			return nil, 0, errScan
		}
		switch s[i] {
		case '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				return nil, 0, errScan
			}
			v, err := DecodeValue(s[i : j+1])
			name, ok := v.(string)
			if err != nil || !ok {
				return nil, 0, errScan
			}
			segs, i = append(segs, name), j+1
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, 0, errScan
			}
			segs, i = append(segs, s[i+1:i+1+j]), i+2+j
		default:
			j := i
			for j < len(s) && isBare(s[j]) {
				j++
			}
			if j == i {
				return nil, 0, errScan
			}
			segs, i = append(segs, s[i:j]), j
		}
		i = skipWS(s, i)
		if i < len(s) && s[i] == '.' {
			i++
			continue
		}
		return segs, i, nil
	}
}

// skipString answers the offset past the string that starts at i (any of the four kinds) and whether it was multi-line.
func skipString(s string, i int) (int, bool, error) {
	q := s[i]
	if strings.HasPrefix(s[i:], strings.Repeat(string(q), 3)) {
		j := i + 3
		for j < len(s) {
			if q == '"' && s[j] == '\\' {
				j += 2
				continue
			}
			if s[j] == q {
				run := 1
				for j+run < len(s) && s[j+run] == q {
					run++
				}
				if run >= 3 {
					return j + run, true, nil
				}
				j += run
				continue
			}
			j++
		}
		return 0, false, errScan
	}
	j := i + 1
	for j < len(s) && s[j] != q {
		if s[j] == '\n' {
			return 0, false, errScan
		}
		if q == '"' && s[j] == '\\' {
			j++
		}
		j++
	}
	if j >= len(s) {
		return 0, false, errScan
	}
	return j + 1, false, nil
}

// parseValue answers the end of the value that starts at i, and whether crw must not rewrite it in place.
func parseValue(s string, i int) (int, bool, error) {
	if i >= len(s) {
		return 0, false, errScan
	}
	switch s[i] {
	case '"', '\'':
		return skipString(s, i)
	case '[', '{':
		depth := 0
		for j := i; j < len(s); {
			switch c := s[j]; {
			case c == '"' || c == '\'':
				end, _, err := skipString(s, j)
				if err != nil {
					return 0, false, err
				}
				j = end
				continue
			case c == '#':
				j, _ = lineEnd(s, j)
				continue
			case c == '[' || c == '{':
				depth++
			case c == ']' || c == '}':
				depth--
				if depth == 0 {
					return j + 1, true, nil
				}
			}
			j++
		}
		return 0, false, errScan
	}
	j := i
	for j < len(s) && s[j] != '#' && !atEOL(s, j) {
		j++
	}
	for j > i && isWS(s[j-1]) {
		j--
	}
	if j == i {
		return 0, false, errScan
	}
	return j, false, nil
}

// afterValue reads the blanks, an optional comment and the line ending after a statement.
func afterValue(s string, i int) (int, string, error) {
	i = skipWS(s, i)
	if i < len(s) && s[i] == '#' {
		end, eol := lineEnd(s, i)
		return end, eol, nil
	}
	if !atEOL(s, i) {
		return 0, "", errScan
	}
	if i >= len(s) {
		return len(s), "", nil
	}
	end, eol := lineEnd(s, i)
	return end, eol, nil
}

func scan(s string) (*document, error) {
	d := &document{last: map[int]item{}, input: s}
	crlf, lf := strings.Count(s, "\r\n"), strings.Count(s, "\n")
	d.crlf = crlf > lf-crlf
	section, cur := -1, []string(nil)
	indents := map[int]string{}
	for pos := 0; pos < len(s); {
		lineStart := pos
		p := skipWS(s, pos)
		if p >= len(s) {
			break
		}
		if atEOL(s, p) {
			pos, _ = lineEnd(s, p)
			continue
		}
		if s[p] == '#' {
			end, eol := lineEnd(s, p)
			d.last[section] = item{end, eol, indents[section]}
			pos = end
			continue
		}
		if s[p] == '[' {
			kind, q := tableStmt, p+1
			if q < len(s) && s[q] == '[' {
				kind, q = arrayTableStmt, q+1
			}
			path, q, err := parseKey(s, q)
			if err != nil {
				return nil, err
			}
			closing := "]"
			if kind == arrayTableStmt {
				closing = "]]"
			}
			if !strings.HasPrefix(s[q:], closing) {
				return nil, errScan
			}
			end, eol, err := afterValue(s, q+len(closing))
			if err != nil {
				return nil, err
			}
			d.stmts = append(d.stmts, stmt{kind: kind, path: path, indent: s[lineStart:p], lineStart: lineStart, end: end, eol: eol})
			section, cur = len(d.stmts)-1, path
			d.last[section] = item{end, eol, ""}
			pos = end
			continue
		}
		rel, q, err := parseKey(s, p)
		if err != nil || q >= len(s) || s[q] != '=' {
			return nil, errScan
		}
		vs := skipWS(s, q+1)
		ve, multiline, err := parseValue(s, vs)
		if err != nil {
			return nil, err
		}
		end, eol, err := afterValue(s, ve)
		if err != nil {
			return nil, err
		}
		full := append(slices.Clone(cur), rel...)
		d.stmts = append(d.stmts, stmt{kind: kvStmt, path: full, rel: rel, indent: s[lineStart:p], lineStart: lineStart, end: end, eol: eol, valStart: vs, valEnd: ve, multiline: multiline})
		indents[section] = s[lineStart:p]
		d.last[section] = item{end, eol, indents[section]}
		pos = end
	}
	return d, nil
}

// keysAt answers the key = value statements whose full path is path.
func (d *document) keysAt(path []string) []int {
	var out []int
	for i, st := range d.stmts {
		if st.kind == kvStmt && slices.Equal(st.path, path) {
			out = append(out, i)
		}
	}
	return out
}

func (d *document) newline() string {
	if d.crlf {
		return "\r\n"
	}
	return "\n"
}

// formatKey writes one key segment bare when it can be, quoted otherwise.
func formatKey(name string) string {
	bare := name != ""
	for i := 0; i < len(name); i++ {
		bare = bare && isBare(name[i])
	}
	if bare {
		return name
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range name {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func formatPath(path []string) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = formatKey(p)
	}
	return strings.Join(parts, ".")
}

// insertAfter puts line after the item at, giving the line the item's line ending, or adding one before it when the item
// ends the input without one (the input then still ends without a line ending).
func (d *document) insertAfter(content string, at item, line string) string {
	if at.eol == "" {
		return content[:at.end] + d.newline() + line + content[at.end:]
	}
	return content[:at.end] + line + at.eol + content[at.end:]
}

// insert adds path = raw to a document that does not define path. It is only called after Get answered Absent.
func (d *document) insert(content string, path []string, raw string) string {
	table, key := path[:len(path)-1], path[len(path)-1]
	section, found := -1, len(table) == 0
	for i, st := range d.stmts {
		if st.kind == tableStmt && slices.Equal(st.path, table) {
			section, found = i, true
		}
	}
	if found {
		at, ok := d.last[section]
		if !ok { // the root without a line: the key goes first
			return formatKey(key) + " = " + raw + d.newline() + content
		}
		return d.insertAfter(content, at, at.indent+formatKey(key)+" = "+raw)
	}
	// A table defined by dotted keys gets the new key next to the last of them, written relative to their section.
	for i := len(d.stmts) - 1; i >= 0; i-- {
		st := d.stmts[i]
		if st.kind != kvStmt || len(st.path) <= len(table) || !slices.Equal(st.path[:len(table)], table) {
			continue
		}
		sectionPath := st.path[:len(st.path)-len(st.rel)]
		if len(sectionPath) > len(table) {
			continue
		}
		rel := append(slices.Clone(table[len(sectionPath):]), key)
		return d.insertAfter(content, item{st.end, st.eol, st.indent}, st.indent+formatPath(rel)+" = "+raw)
	}
	nl := d.newline()
	out := content
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += nl
	}
	if trimmed := strings.TrimRight(out, "\r\n"); trimmed != "" && strings.Count(out[len(trimmed):], "\n") < 2 {
		out += nl
	}
	return out + "[" + formatPath(table) + "]" + nl + formatKey(key) + " = " + raw + nl
}
