// Package pyjson is CPython's json.loads as far as the port reproduces it: the decoding of the
// bytes it is given (DecodeBytes), and the JSONDecodeError, integer-limit and recursion
// refusals its C scanner raises, with their messages and positions (Error, HookedError). It
// imports nothing of the relay or the bridge, so both parse Python's documents alike.
package pyjson

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Error returns json.loads's JSONDecodeError text for a document it refuses, or "" when
// Python would accept it. It walks the document the way CPython's scanner does, so the message
// and its "line L column C (char N)" position match (N counts characters, not bytes).
func Error(doc string) string {
	message, _ := ErrorWithLimit(doc, 0)
	return message
}

// ErrorWithLimit also models the C JSON scanner's container recursion
// budget. A zero limit leaves the caller's existing unbounded syntax check intact.
// Recursion errors are host failures rather than JSONDecodeError/ValueError.
func ErrorWithLimit(doc string, maxDepth int) (message string, recursion bool) {
	s := []rune(doc)
	p := &pyScan{s: s, maxDepth: maxDepth}
	if strings.HasPrefix(doc, "\ufeff") {
		return p.format("Unexpected UTF-8 BOM (decode using utf-8-sig)", 0), false
	}
	end, msg, at := p.value(p.ws(0))
	if p.recursionError != "" {
		return p.recursionError, true
	}
	if p.integerError != "" {
		return p.integerError, false
	}
	if msg != "" {
		return p.format(msg, at), false
	}
	end = p.ws(end)
	if end != len(s) {
		return p.format("Extra data", end), false
	}
	return "", false
}

// RawDecodePrefix is the text of the value json.JSONDecoder().raw_decode(doc) reads at the start
// of doc, with whatever follows it left unread, or false where raw_decode raises ValueError: a
// JSONDecodeError, or the integer-digit limit. As in Python, NaN, Infinity and -Infinity are
// values, and no whitespace is skipped before the value.
func RawDecodePrefix(doc string) (string, bool) {
	p := &pyScan{s: []rune(doc)}
	end, msg, _ := p.value(0)
	if msg != "" || p.integerError != "" {
		return "", false
	}
	return string(p.s[:end]), true
}

type pyScan struct {
	s                            []rune
	integerError, recursionError string
	depth, maxDepth              int
	// hookDepth > 0 models an object_pairs_hook: an object closing deeper than it refuses
	// with RecursionError, and with hookKeys a repeated key refuses it at its close.
	hookDepth int
	hookKeys  bool
}

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
	if p.s[i] == '{' || p.s[i] == '[' {
		p.depth++
		defer func() { p.depth-- }()
		if p.maxDepth > 0 && p.depth > p.maxDepth {
			kind := "object"
			if p.s[i] == '[' {
				kind = "array"
			}
			p.recursionError = "maximum recursion depth exceeded while decoding a JSON " + kind + " from a unicode string"
			return 0, p.recursionError, i
		}
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
		text := string(p.s[i:end])
		if !strings.ContainsAny(text, ".eE") {
			digits := len(strings.TrimPrefix(text, "-"))
			if digits > 4300 {
				p.integerError = fmt.Sprintf("Exceeds the limit (4300 digits) for integer string conversion: value has %d digits; use sys.set_int_max_str_digits() to increase the limit", digits)
				return 0, p.integerError, i
			}
		}
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
		return p.closed(i, false)
	}
	keys, repeated := map[string]bool{}, false
	for {
		if p.at(i) != '"' {
			return 0, "Expecting property name enclosed in double quotes", i
		}
		end, msg, at := p.str(i + 1)
		if msg != "" {
			return 0, msg, at
		}
		if p.hookKeys {
			var key string
			_ = json.Unmarshal([]byte(string(p.s[i:end])), &key)
			repeated = repeated || keys[key]
			keys[key] = true
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
			return p.closed(i, repeated)
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

// closed ends an object at its '}' (index i): with a hook modelled, calling it past hookDepth
// is the RecursionError, and a repeated key is the hook's own refusal.
func (p *pyScan) closed(i int, repeated bool) (int, string, int) {
	if p.hookDepth > 0 && p.depth > p.hookDepth {
		p.recursionError = "maximum recursion depth exceeded"
		return 0, p.recursionError, i
	}
	if repeated {
		return 0, "duplicate key", i
	}
	return i + 1, "", 0
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

// HookedRecursion is whether json.loads(raw, object_pairs_hook=hook) raises
// RecursionError before anything else refuses the document: the parse the execution policy
// file gets (codex_thread_bridge.execution.from_bytes, _no_duplicates). The C scanner enters
// one recursion level per container and refuses past limit; the hook every object calls on
// closing costs two more, so an object that closes deeper than limit-2 refuses too. A syntax
// error met first, or a duplicate key the hook refuses at an earlier close, wins instead, as
// it does in Python. doc is the decoded text; a caller with undecodable bytes never gets here.
func HookedRecursion(doc string, limit int) bool {
	_, _, recursion := HookedError(doc, limit)
	return recursion
}

// HookedError is json.loads(doc, object_pairs_hook=hook) for a hook that refuses a repeated
// key (the execution policy's _no_duplicates), with the recursion budget HookedRecursion
// models (none when limit is 0). It reports the first refusal in the scanner's order: the
// RecursionError, the hook's refusal of an object at its close (duplicate), or the
// JSONDecodeError or integer-limit text; "" and neither when the document is read whole.
func HookedError(doc string, limit int) (message string, duplicate, recursion bool) {
	s := []rune(doc)
	p := &pyScan{s: s, hookKeys: true}
	if limit > 0 {
		p.maxDepth, p.hookDepth = limit, limit-2
	}
	end, msg, at := p.value(p.ws(0))
	switch {
	case p.recursionError != "":
		return p.recursionError, false, true
	case p.integerError != "":
		return p.integerError, false, false
	case msg == "duplicate key":
		return "", true, false
	case msg != "":
		return p.format(msg, at), false, false
	}
	if end = p.ws(end); end != len(s) {
		return p.format("Extra data", end), false, false
	}
	return "", false, false
}
