//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// pyDecoder is Python's json.loads: the same accepted language (including NaN and
// Infinity) and the same JSONDecodeError texts, so a check that echoes a decode failure reads
// as it did. Objects decode to map[string]any (last duplicate wins), numbers to json.Number.
type pyDecoder struct {
	text    []rune
	ordered bool     // objects decode to *pyDict, keeping Python's key order
	topKeys []string // member names of a top-level object in first-occurrence order
}

// pyJSONLoads is json.loads(text).
func pyJSONLoads(text string) (any, error) {
	d := &pyDecoder{text: []rune(text)}
	return d.document()
}

// pyJSONLoadsOrdered is json.loads(text) with objects as *pyDict.
func pyJSONLoadsOrdered(text string) (any, error) {
	d := &pyDecoder{text: []rune(text), ordered: true}
	return d.document()
}

// pyJSONLoadsKeys is json.loads(text) plus the top-level object's key order.
func pyJSONLoadsKeys(text string) (any, []string, error) {
	d := &pyDecoder{text: []rune(text)}
	value, err := d.document()
	return value, d.topKeys, err
}

func (d *pyDecoder) fail(message string, pos int) error {
	line, column := 1, pos+1
	for i := 0; i < pos; i++ {
		if d.text[i] == '\n' {
			line, column = line+1, pos-i
		}
	}
	return valueError{fmt.Sprintf("%s: line %d column %d (char %d)", message, line, column, pos)}
}

func (d *pyDecoder) skip(pos int) int {
	for pos < len(d.text) && strings.ContainsRune(" \t\n\r", d.text[pos]) {
		pos++
	}
	return pos
}

func (d *pyDecoder) document() (any, error) {
	if len(d.text) > 0 && d.text[0] == '\ufeff' {
		return nil, valueError{"Unexpected UTF-8 BOM (decode using utf-8-sig): line 1 column 1 (char 0)"}
	}
	pos := d.skip(0)
	value, end, err := d.value(pos, 0)
	if err != nil {
		return nil, err
	}
	end = d.skip(end)
	if end != len(d.text) {
		return nil, d.fail("Extra data", end)
	}
	return value, nil
}

func (d *pyDecoder) has(pos int, word string) bool {
	return strings.HasPrefix(string(d.text[pos:min(len(d.text), pos+len(word))]), word)
}

func (d *pyDecoder) value(pos, depth int) (any, int, error) {
	if pos >= len(d.text) {
		return nil, pos, d.fail("Expecting value", pos)
	}
	switch c := d.text[pos]; {
	case c == '"':
		return d.str(pos + 1)
	case c == '{':
		return d.object(pos+1, depth)
	case c == '[':
		return d.array(pos+1, depth)
	case c == 'n' && d.has(pos, "null"):
		return nil, pos + 4, nil
	case c == 't' && d.has(pos, "true"):
		return true, pos + 4, nil
	case c == 'f' && d.has(pos, "false"):
		return false, pos + 5, nil
	case c == 'N' && d.has(pos, "NaN"):
		return json.Number("NaN"), pos + 3, nil
	case c == 'I' && d.has(pos, "Infinity"):
		return json.Number("Infinity"), pos + 8, nil
	case c == '-' && d.has(pos, "-Infinity"):
		return json.Number("-Infinity"), pos + 9, nil
	}
	return d.number(pos)
}

func (d *pyDecoder) number(pos int) (any, int, error) {
	end := pos
	digits := func() int {
		start := end
		for end < len(d.text) && d.text[end] >= '0' && d.text[end] <= '9' {
			end++
		}
		return end - start
	}
	if end < len(d.text) && d.text[end] == '-' {
		end++
	}
	if end < len(d.text) && d.text[end] == '0' {
		end++
	} else if end < len(d.text) && d.text[end] >= '1' && d.text[end] <= '9' {
		digits()
	} else {
		return nil, pos, d.fail("Expecting value", pos)
	}
	if end+1 < len(d.text) && d.text[end] == '.' && d.text[end+1] >= '0' && d.text[end+1] <= '9' {
		end++
		digits()
	}
	if end < len(d.text) && (d.text[end] == 'e' || d.text[end] == 'E') {
		save := end
		end++
		if end < len(d.text) && (d.text[end] == '+' || d.text[end] == '-') {
			end++
		}
		if digits() == 0 {
			end = save
		}
	}
	return json.Number(string(d.text[pos:end])), end, nil
}

func (d *pyDecoder) str(pos int) (any, int, error) {
	begin := pos - 1
	var b strings.Builder
	for {
		if pos >= len(d.text) {
			return nil, pos, d.fail("Unterminated string starting at", begin)
		}
		c := d.text[pos]
		switch {
		case c == '"':
			return b.String(), pos + 1, nil
		case c < 0x20:
			return nil, pos, d.fail("Invalid control character at", pos)
		case c != '\\':
			b.WriteRune(c)
			pos++
			continue
		}
		pos++
		if pos >= len(d.text) {
			return nil, pos, d.fail("Unterminated string starting at", begin)
		}
		escapes := map[rune]string{'"': `"`, '\\': `\`, '/': "/", 'b': "\b", 'f': "\f", 'n': "\n", 'r': "\r", 't': "\t"}
		if text, ok := escapes[d.text[pos]]; ok {
			b.WriteString(text)
			pos++
			continue
		}
		if d.text[pos] != 'u' {
			return nil, pos, d.fail("Invalid \\escape", pos-1)
		}
		code, ok := d.hex4(pos + 1)
		if !ok {
			return nil, pos, d.fail("Invalid \\uXXXX escape", pos-1)
		}
		pos += 5
		if code >= 0xd800 && code <= 0xdbff && d.has(pos, `\u`) {
			if low, ok := d.hex4(pos + 2); ok && low >= 0xdc00 && low <= 0xdfff {
				b.WriteRune(utf16.DecodeRune(rune(code), rune(low)))
				pos += 6
				continue
			} else if !ok {
				return nil, pos, d.fail("Invalid \\uXXXX escape", pos)
			}
		}
		if code >= 0xd800 && code <= 0xdfff {
			b.WriteRune(utf8.RuneError) // a lone surrogate; Go strings cannot hold it
		} else {
			b.WriteRune(rune(code))
		}
	}
}

func (d *pyDecoder) hex4(pos int) (int, bool) {
	if pos+4 > len(d.text) {
		return 0, false
	}
	digits := string(d.text[pos : pos+4])
	if strings.ContainsAny(digits, "xX+-_ ") {
		return 0, false
	}
	code, err := strconv.ParseUint(digits, 16, 32)
	return int(code), err == nil
}

func (d *pyDecoder) object(pos, depth int) (any, int, error) {
	result := map[string]any{}
	var order []string
	done := func() any {
		if d.ordered {
			return &pyDict{keys: order, vals: result}
		}
		return result
	}
	pos = d.skip(pos)
	if pos < len(d.text) && d.text[pos] == '}' {
		return done(), pos + 1, nil
	}
	for {
		if pos >= len(d.text) || d.text[pos] != '"' {
			return nil, pos, d.fail("Expecting property name enclosed in double quotes", pos)
		}
		key, end, err := d.str(pos + 1)
		if err != nil {
			return nil, end, err
		}
		pos = d.skip(end)
		if pos >= len(d.text) || d.text[pos] != ':' {
			return nil, pos, d.fail("Expecting ':' delimiter", pos)
		}
		pos = d.skip(pos + 1)
		value, end, err := d.value(pos, depth+1)
		if err != nil {
			return nil, end, err
		}
		name := key.(string)
		if _, seen := result[name]; !seen {
			order = append(order, name)
			if depth == 0 {
				d.topKeys = append(d.topKeys, name)
			}
		}
		result[name] = value
		pos = d.skip(end)
		if pos < len(d.text) && d.text[pos] == '}' {
			return done(), pos + 1, nil
		}
		if pos >= len(d.text) || d.text[pos] != ',' {
			return nil, pos, d.fail("Expecting ',' delimiter", pos)
		}
		comma := pos
		pos = d.skip(pos + 1)
		if pos < len(d.text) && d.text[pos] == '}' {
			return nil, comma, d.fail("Illegal trailing comma before end of object", comma)
		}
	}
}

func (d *pyDecoder) array(pos, depth int) (any, int, error) {
	result := []any{}
	pos = d.skip(pos)
	if pos < len(d.text) && d.text[pos] == ']' {
		return result, pos + 1, nil
	}
	for {
		value, end, err := d.value(pos, depth+1)
		if err != nil {
			return nil, end, err
		}
		result = append(result, value)
		pos = d.skip(end)
		if pos < len(d.text) && d.text[pos] == ']' {
			return result, pos + 1, nil
		}
		if pos >= len(d.text) || d.text[pos] != ',' {
			return nil, pos, d.fail("Expecting ',' delimiter", pos)
		}
		comma := pos
		pos = d.skip(pos + 1)
		if pos < len(d.text) && d.text[pos] == ']' {
			return nil, comma, d.fail("Illegal trailing comma before end of array", comma)
		}
	}
}
