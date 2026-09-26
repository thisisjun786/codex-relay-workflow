package store

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// PythonJSONError returns json.loads's JSONDecodeError text for a document it refuses, or "" when
// Python would accept it. It walks the document the way CPython's scanner does, so the message
// and its "line L column C (char N)" position match (N counts characters, not bytes).
func PythonJSONError(doc string) string {
	s := []rune(doc)
	p := &pyScan{s: s}
	end, msg, at := p.value(p.ws(0))
	if msg != "" {
		return p.format(msg, at)
	}
	end = p.ws(end)
	if end != len(s) {
		return p.format("Extra data", end)
	}
	return ""
}

type pyScan struct{ s []rune }

func (p *pyScan) format(msg string, pos int) string {
	line := 1
	last := -1
	for i := 0; i < pos && i < len(p.s); i++ {
		if p.s[i] == '\n' {
			line++
			last = i
		}
	}
	col := pos - last
	if last < 0 {
		col = pos + 1
	}
	return fmt.Sprintf("%s: line %d column %d (char %d)", msg, line, col, pos)
}

func isWS(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

func (p *pyScan) ws(i int) int {
	for i < len(p.s) && isWS(p.s[i]) {
		i++
	}
	return i
}

func (p *pyScan) at(i int) rune {
	if i < len(p.s) {
		return p.s[i]
	}
	return -1
}

func (p *pyScan) has(i int, word string) bool {
	w := []rune(word)
	if i+len(w) > len(p.s) {
		return false
	}
	return string(p.s[i:i+len(w)]) == word
}

// value is scan_once: (end, "", 0) or (0, message, position).
func (p *pyScan) value(i int) (int, string, int) {
	if i >= len(p.s) {
		return 0, "Expecting value", i
	}
	switch c := p.s[i]; {
	case c == '"':
		return p.str(i + 1)
	case c == '{':
		return p.object(i + 1)
	case c == '[':
		return p.array(i + 1)
	case c == 'n' && p.has(i, "null"):
		return i + 4, "", 0
	case c == 't' && p.has(i, "true"):
		return i + 4, "", 0
	case c == 'f' && p.has(i, "false"):
		return i + 5, "", 0
	case c == 'N' && p.has(i, "NaN"):
		return i + 3, "", 0
	case c == 'I' && p.has(i, "Infinity"):
		return i + 8, "", 0
	case c == '-' && p.has(i, "-Infinity"):
		return i + 9, "", 0
	}
	if end, ok := p.number(i); ok {
		return end, "", 0
	}
	return 0, "Expecting value", i
}

// number matches (-?(?:0|[1-9]\d*))(\.\d+)?([eE][-+]?\d+)? at i.
func (p *pyScan) number(i int) (int, bool) {
	digit := func(j int) bool { r := p.at(j); return r >= '0' && r <= '9' }
	j := i
	if p.at(j) == '-' {
		j++
	}
	switch {
	case p.at(j) == '0':
		j++
	case p.at(j) >= '1' && p.at(j) <= '9':
		for digit(j) {
			j++
		}
	default:
		return 0, false
	}
	if p.at(j) == '.' && digit(j+1) {
		j += 2
		for digit(j) {
			j++
		}
	}
	if r := p.at(j); r == 'e' || r == 'E' {
		k := j + 1
		if r := p.at(k); r == '+' || r == '-' {
			k++
		}
		if digit(k) {
			for digit(k) {
				k++
			}
			j = k
		}
	}
	return j, true
}

func (p *pyScan) str(i int) (int, string, int) {
	begin := i - 1
	for {
		if i >= len(p.s) {
			return 0, "Unterminated string starting at", begin
		}
		c := p.s[i]
		switch {
		case c == '"':
			return i + 1, "", 0
		case c < 0x20:
			return 0, "Invalid control character at", i
		case c == '\\':
			i++
			if i >= len(p.s) {
				return 0, "Unterminated string starting at", begin
			}
			e := p.s[i]
			if e == 'u' {
				if i+5 > len(p.s) || !isHex(p.s[i+1:i+5]) {
					return 0, "Invalid \\uXXXX escape", i
				}
				i += 5
				continue
			}
			if !strings.ContainsRune(`"\/bfnrt`, e) {
				return 0, "Invalid \\escape", i - 1
			}
		}
		i++
	}
}

func isHex(rs []rune) bool {
	for _, r := range rs {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func (p *pyScan) object(i int) (int, string, int) {
	i = p.ws(i)
	if p.at(i) == '}' {
		return i + 1, "", 0
	}
	for {
		if p.at(i) != '"' {
			return 0, "Expecting property name enclosed in double quotes", i
		}
		end, msg, at := p.str(i + 1)
		if msg != "" {
			return 0, msg, at
		}
		i = p.ws(end)
		if p.at(i) != ':' {
			return 0, "Expecting ':' delimiter", i
		}
		i = p.ws(i + 1)
		end, msg, at = p.value(i)
		if msg != "" {
			return 0, msg, at
		}
		i = p.ws(end)
		switch p.at(i) {
		case '}':
			return i + 1, "", 0
		case ',':
		default:
			return 0, "Expecting ',' delimiter", i
		}
		comma := i
		i = p.ws(i + 1)
		if p.at(i) == '}' {
			return 0, "Illegal trailing comma before end of object", comma
		}
	}
}

func (p *pyScan) array(i int) (int, string, int) {
	i = p.ws(i)
	if p.at(i) == ']' {
		return i + 1, "", 0
	}
	for {
		end, msg, at := p.value(i)
		if msg != "" {
			return 0, msg, at
		}
		i = p.ws(end)
		switch p.at(i) {
		case ']':
			return i + 1, "", 0
		case ',':
		default:
			return 0, "Expecting ',' delimiter", i
		}
		comma := i
		i = p.ws(i + 1)
		if p.at(i) == ']' {
			return 0, "Illegal trailing comma before end of array", comma
		}
	}
}

// ValidUTF8 guards the character positions above; Python reads the file as UTF-8 first.
func ValidUTF8(data []byte) bool { return utf8.Valid(data) }
