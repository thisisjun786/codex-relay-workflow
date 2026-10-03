// Agent-thread TOML scanners from CXC v0.2.40 (3c1459ac),
// agent-thread-permissions.ts:58-88,117-315. Positions are UTF-8 byte offsets
// for Go slicing rather than JavaScript UTF-16 positions. Callers provide
// decoded UTF-8 text; config reads and permission decisions live elsewhere.
package hook

import (
	"errors"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

type tomlKind string

const (
	tomlTable  tomlKind = "table"
	tomlArray  tomlKind = "array"
	tomlScalar tomlKind = "scalar"
)

type tomlEntry struct {
	kind     tomlKind
	children map[string]*tomlEntry
	declared bool
	latest   *tomlEntry
}

type tomlKeyPathResult struct {
	parts []string
	end   int
}

func newTomlTable() *tomlEntry {
	return &tomlEntry{kind: tomlTable, children: make(map[string]*tomlEntry)}
}

// The error corresponds to a standalone BigInt syntax failure. The value
// scanner calls this only after accepting the token's scalar grammar.
func finiteTomlNumber(token string) (bool, error) {
	if !regexp.MustCompile(`^[+-]?(?:\d|\.)`).MatchString(token) ||
		regexp.MustCompile(`^[+-]?(?:inf|nan)$`).MatchString(token) ||
		regexp.MustCompile(`^\d{4}-|^\d\d:`).MatchString(token) {
		return true, nil
	}
	clean := strings.ReplaceAll(token, "_", "")
	radix := regexp.MustCompile(`(?i)^0[xob]`).MatchString(clean)
	if radix || !strings.ContainsAny(clean, ".eE") {
		negative := strings.HasPrefix(clean, "-")
		digits := clean
		if strings.HasPrefix(digits, "-") || strings.HasPrefix(digits, "+") {
			digits = digits[1:]
		}
		digits = text.Trim(digits)
		base := 10
		if len(digits) >= 2 && digits[0] == '0' {
			switch digits[1] {
			case 'x', 'X':
				base = 16
			case 'o', 'O':
				base = 8
			case 'b', 'B':
				base = 2
			}
			if base != 10 {
				digits = digits[2:]
			}
		}
		if strings.HasPrefix(digits, "-") || strings.HasPrefix(digits, "+") {
			return false, errors.New("invalid TOML integer")
		}
		value, ok := new(big.Int).SetString(digits, base)
		if !ok {
			return false, errors.New("invalid TOML integer")
		}
		limit := new(big.Int).Lsh(big.NewInt(1), 63)
		if !negative {
			limit.Sub(limit, big.NewInt(1))
		}
		return value.Cmp(limit) <= 0, nil
	}
	clean = text.Trim(clean)
	// Number() refuses signed radix syntax, unlike Go's hex float parser.
	unsigned := strings.TrimLeft(clean, "+-")
	if strings.HasPrefix(strings.ToLower(unsigned), "0x") {
		return false, nil
	}
	value, err := strconv.ParseFloat(clean, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return false, nil
	}
	return !math.IsInf(value, 0) && !math.IsNaN(value), nil
}

func validTomlDateTime(token string) bool {
	date := regexp.MustCompile(`^(\d{4})-(\d\d)-(\d\d)`).FindStringSubmatch(token)
	if date != nil {
		year, _ := strconv.Atoi(date[1])
		month, _ := strconv.Atoi(date[2])
		day, _ := strconv.Atoi(date[3])
		if month < 1 || month > 12 {
			return false
		}
		if year < 100 {
			year += 1900
		} // Date.UTC's two-digit-year behavior.
		if day < 1 || day > time.Date(year, time.Month(month+1), 0, 0, 0, 0, 0, time.UTC).Day() {
			return false
		}
	}
	clock := regexp.MustCompile(`(?:^|[Tt ])(\d\d):(\d\d):(\d\d)(?:\.\d+)?(?:[Zz]|[+-](\d\d):(\d\d))?$`).FindStringSubmatch(token)
	if clock != nil {
		for i, limit := range []int{23, 59, 60, 23, 59} {
			value, _ := strconv.Atoi(clock[i+1])
			if value > limit {
				return false
			}
		}
	}
	return true
}

func tomlKeyPath(source string, start int) *tomlKeyPathResult {
	index := start
	if index < 0 || index > len(source) {
		return nil
	}
	spaces := func() {
		for index < len(source) && (source[index] == ' ' || source[index] == '\t') {
			index++
		}
	}
	spaces()
	parts := make([]string, 0)
	for index < len(source) {
		var part string
		quote := source[index]
		if quote == '"' || quote == '\'' {
			var ok bool
			part, index, ok = tomlQuotedKey(source, index)
			if !ok {
				return nil
			}
		} else {
			begin := index
			for index < len(source) && tomlBareKeyByte(source[index]) {
				index++
			}
			if index == begin {
				return nil
			}
			part = source[begin:index]
		}
		parts = append(parts, part)
		spaces()
		if index >= len(source) || source[index] != '.' {
			break
		}
		index++
		spaces()
		if index >= len(source) {
			return nil
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return &tomlKeyPathResult{parts: parts, end: index}
}

func tomlBareKeyByte(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_' || b == '-'
}

func tomlQuotedKey(source string, index int) (string, int, bool) {
	quote := source[index]
	index++
	var part strings.Builder
	for index < len(source) {
		char := source[index]
		index++
		if char == quote {
			return part.String(), index, true
		}
		if char <= 0x1f || char == 0x7f {
			return "", index, false
		}
		if quote == '"' && char == '\\' {
			if index >= len(source) {
				return "", index, false
			}
			escape := source[index]
			index++
			if decoded, ok := tomlSimpleEscape(escape); ok {
				part.WriteByte(decoded)
			} else if escape == 'u' || escape == 'U' {
				decoded, next, ok := tomlUnicodeEscape(source, index, escape)
				if !ok {
					return "", index, false
				}
				part.WriteRune(decoded)
				index = next
			} else {
				return "", index, false
			}
		} else {
			part.WriteByte(char)
		}
	}
	return "", index, false
}

func tomlSimpleEscape(escape byte) (byte, bool) {
	switch escape {
	case 'b':
		return '\b', true
	case 't':
		return '\t', true
	case 'n':
		return '\n', true
	case 'f':
		return '\f', true
	case 'r':
		return '\r', true
	case '"', '\\':
		return escape, true
	}
	return 0, false
}

func tomlUnicodeEscape(source string, index int, escape byte) (rune, int, bool) {
	length := 4
	if escape == 'U' {
		length = 8
	}
	if len(source)-index < length {
		return 0, index, false
	}
	hex := source[index : index+length]
	for _, b := range []byte(hex) {
		if !(b >= '0' && b <= '9' || b >= 'a' && b <= 'f' || b >= 'A' && b <= 'F') {
			return 0, index, false
		}
	}
	code, _ := strconv.ParseUint(hex, 16, 32)
	if code > 0x10ffff || code >= 0xd800 && code <= 0xdfff {
		return 0, index, false
	}
	return rune(code), index + length, true
}
func tomlAssignKey(scope *tomlEntry, parts []string) bool {
	current := scope
	for index, name := range parts {
		children := current.children
		if children == nil {
			return false
		}
		existing := children[name]
		if index == len(parts)-1 {
			if existing != nil {
				return false
			}
			children[name] = &tomlEntry{kind: tomlScalar}
			return true
		}
		if existing == nil {
			existing = newTomlTable()
			children[name] = existing
		} else if existing.kind != tomlTable {
			return false
		}
		current = existing
	}
	return false
}

func tomlEnterTable(root *tomlEntry, parts []string, array bool) *tomlEntry {
	current := root
	for index, name := range parts {
		children := current.children
		if children == nil {
			return nil
		}
		entry := children[name]
		last := index == len(parts)-1
		if last && array {
			if entry == nil {
				entry = &tomlEntry{kind: tomlArray}
				children[name] = entry
			}
			if entry.kind != tomlArray {
				return nil
			}
			entry.latest = newTomlTable()
			return entry.latest
		}
		if entry == nil {
			entry = newTomlTable()
			children[name] = entry
		}
		switch entry.kind {
		case tomlArray:
			if entry.latest == nil {
				return nil
			}
			current = entry.latest
		case tomlTable:
			if last {
				if entry.declared {
					return nil
				}
				entry.declared = true
			}
			current = entry
		default:
			return nil
		}
	}
	return current
}

type tomlValueScanner struct {
	source                      string
	index                       int
	scalar, bare, inlineStrings *regexp.Regexp
}

const tomlDigits = `[0-9](?:_?[0-9])*`
const tomlScalarPattern = `^(?:true|false|[+-]?(?:0|[1-9](?:_?[0-9])*)(?:\.` + tomlDigits + `)?(?:[eE][+-]?` + tomlDigits + `)?|[+-]?(?:inf|nan)|\d{4}-\d\d-\d\d(?:[Tt ]\d\d:\d\d:\d\d(?:\.\d+)?(?:[Zz]|[+-]\d\d:\d\d)?)?|\d\d:\d\d:\d\d(?:\.\d+)?|0[xX][0-9a-fA-F](?:_?[0-9a-fA-F])*|0[oO][0-7](?:_?[0-7])*|0[bB][01](?:_?[01])*)$`

func validTomlValue(source string) bool {
	s := tomlValueScanner{
		source:        source,
		scalar:        regexp.MustCompile(tomlScalarPattern),
		bare:          regexp.MustCompile(`^[^` + lintSpace[1:len(lintSpace)-1] + `,\]\}#]+`),
		inlineStrings: regexp.MustCompile(`"""[\s\S]*?"""|'''[\s\S]*?'''|"(?:[^"\\\n]|\\` + lintDot + `)*"|'[^'\n]*'`),
	}
	if !s.value(0) {
		return false
	}
	s.skip()
	return s.index == len(source)
}

func (s *tomlValueScanner) skip() {
	for s.index < len(s.source) {
		r, size := utf8.DecodeRuneInString(s.source[s.index:])
		if text.Trim(string(r)) == "" {
			s.index += size
			continue
		}
		if s.source[s.index] == '#' {
			end := strings.IndexByte(s.source[s.index:], '\n')
			if end < 0 {
				s.index = len(s.source)
			} else {
				s.index += end
			}
			continue
		}
		break
	}
}

func (s *tomlValueScanner) quoted() bool {
	quote := s.source[s.index]
	mark := string(quote)
	triple := strings.HasPrefix(s.source[s.index:], strings.Repeat(mark, 3))
	if triple {
		mark = strings.Repeat(mark, 3)
	}
	s.index += len(mark)
	for s.index < len(s.source) {
		if strings.HasPrefix(s.source[s.index:], mark) {
			s.index += len(mark)
			return true
		}
		char := s.source[s.index]
		if !triple && (char == '\r' || char == '\n') {
			return false
		}
		if char <= 0x08 || char == 0x0b || char >= 0x0e && char <= 0x1f || char == 0x7f {
			return false
		}
		if quote == '"' && char == '\\' {
			s.index++
			if s.index >= len(s.source) {
				return false
			}
			escape := s.source[s.index]
			if _, ok := tomlSimpleEscape(escape); ok {
				s.index++
				continue
			}
			if escape == 'u' || escape == 'U' {
				_, next, ok := tomlUnicodeEscape(s.source, s.index+1, escape)
				if !ok {
					return false
				}
				s.index = next
				continue
			}
			if triple {
				continuation := regexp.MustCompile(`^[ \t]*(?:\r?\n)`).FindString(s.source[s.index:])
				if continuation != "" {
					s.index += len(continuation)
					continue
				}
			}
			return false
		}
		s.index++
	}
	return false
}

func (s *tomlValueScanner) singleLineInline(start int) bool {
	body := s.inlineStrings.ReplaceAllString(s.source[start:s.index], "")
	return !strings.ContainsAny(body, "\r\n")
}

func (s *tomlValueScanner) value(depth int) bool {
	if depth > 64 {
		return false
	}
	s.skip()
	if s.index >= len(s.source) {
		return false
	}
	opener := s.source[s.index]
	if opener == '"' || opener == '\'' {
		return s.quoted()
	}
	if opener == '[' || opener == '{' {
		return s.compound(depth, opener)
	}
	bare := s.bare.FindString(s.source[s.index:])
	if bare == "" || !s.scalar.MatchString(bare) || !validTomlDateTime(bare) {
		return false
	}
	finite, err := finiteTomlNumber(bare)
	if err != nil || !finite {
		return false
	}
	s.index += len(bare)
	return true
}

func (s *tomlValueScanner) compound(depth int, opener byte) bool {
	start := s.index
	s.index++
	closer := byte(']')
	var inline *tomlEntry
	if opener == '{' {
		closer = '}'
		inline = newTomlTable()
	}
	closeValue := func() bool {
		s.index++
		return opener == '[' || s.singleLineInline(start)
	}
	s.skip()
	if s.index < len(s.source) && s.source[s.index] == closer {
		return closeValue()
	}
	for s.index < len(s.source) {
		if inline != nil {
			parsed := tomlKeyPath(s.source, s.index)
			if parsed == nil || !tomlAssignKey(inline, parsed.parts) {
				return false
			}
			s.index = parsed.end
			s.skip()
			if s.index >= len(s.source) || s.source[s.index] != '=' {
				return false
			}
			s.index++
		}
		if !s.value(depth + 1) {
			return false
		}
		s.skip()
		if s.index < len(s.source) && s.source[s.index] == closer {
			return closeValue()
		}
		if s.index >= len(s.source) || s.source[s.index] != ',' {
			return false
		}
		s.index++
		s.skip()
		if opener == '[' && s.index < len(s.source) && s.source[s.index] == closer {
			s.index++
			return true
		}
	}
	return false
}
