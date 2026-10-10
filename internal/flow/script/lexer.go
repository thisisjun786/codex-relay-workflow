package script

import (
	"fmt"
	"strings"
)

type tokKind int

const (
	tokIdent tokKind = iota
	tokPunct
	tokString
	tokTemplate
	tokNumber
	tokRegex
)

// token is one lexical element of a script. Template literals are one token that follows the tokens of their
// substitutions, so a reader that walks the stream sees every expression of a template and braces stay balanced.
type token struct {
	kind       tokKind
	text       string // the source text of the token; a string token holds the literal with its quotes
	start, end int    // byte offsets, end exclusive
}

// value is the string a string token denotes (escape sequences are not supported in module specifiers).
func (t token) value() string { return t.text[1 : len(t.text)-1] }

// lexError is a lexical error with the byte offset it was found at.
type lexError struct {
	off int
	msg string
}

func (e *lexError) Error() string { return e.msg }

// regexPrecedingKeywords are the identifiers after which a slash starts a regular expression.
var regexPrecedingKeywords = map[string]bool{
	"return": true, "typeof": true, "instanceof": true, "in": true, "of": true, "new": true, "delete": true,
	"void": true, "throw": true, "case": true, "do": true, "else": true, "yield": true, "await": true,
}

// tokenize reads src into tokens. It tracks strings, template literals (with nested substitutions), comments and
// regular expression literals, which is all the header and body checks need; it does not parse. A string,
// template, comment or regular expression that does not end is an error.
func tokenize(src string) ([]token, error) {
	s := &scanner{src: src, closeRegex: map[int]bool{}}
	if err := s.run(0, 0, false); err != nil {
		return nil, err
	}
	return s.toks, nil
}

type scanner struct {
	src  string
	pos  int
	toks []token

	// parens and braces hold, for every bracket still open, whether it is a statement header ("if (", "while (", "for (",
	// "with (") or a block; closeRegex marks, by token index, the closing ) and } after which a slash starts a regular
	// expression (the end of a header or of a block) and not a division (the end of an operand).
	parens, braces []bool
	closeRegex     map[int]bool
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool { return isIdentStart(c) || c >= '0' && c <= '9' }

// statementHeaders are the keywords whose parenthesised header is followed by a statement, which may start with a
// regular expression.
var statementHeaders = map[string]bool{"if": true, "while": true, "for": true, "with": true}

// isProperty reports whether the identifier at index i is a property name (after a dot, which also covers ?. and the
// spread), so it is an operand whatever its spelling.
func (s *scanner) isProperty(i, from int) bool {
	return i-1 >= from && s.toks[i-1].kind == tokPunct && s.toks[i-1].text == "."
}

// allowsRegexAfter reports whether a slash that follows the token at index i starts a regular expression, which is
// the case where an operand is expected and not where one has just ended.
func (s *scanner) allowsRegexAfter(i, from int) bool {
	if i < from {
		return true
	}
	p := s.toks[i]
	switch p.kind {
	case tokIdent:
		return !s.isProperty(i, from) && regexPrecedingKeywords[p.text]
	case tokPunct:
		switch p.text {
		case ")", "}":
			return s.closeRegex[i]
		case "]":
			return false
		case "+", "-":
			// x++ / 2 and x-- / 2: a postfix operator ends an operand; a prefix or binary one does not.
			if i-1 >= from && s.toks[i-1].text == p.text && s.toks[i-1].end == p.start && s.toks[i-1].kind == tokPunct {
				return s.allowsRegexAfter(i-2, from)
			}
		}
		return true
	}
	return false
}

// prevAllowsRegex reports whether a slash at this point starts a regular expression.
func (s *scanner) prevAllowsRegex(from int) bool {
	return s.allowsRegexAfter(len(s.toks)-1, from)
}

// opensBlock reports whether a { at this point opens a block (a function or control-flow body, a class body) and not an
// object literal.
func (s *scanner) opensBlock(from int, inSubst bool) bool {
	n := len(s.toks)
	if n-1 < from {
		return !inSubst
	}
	p := s.toks[n-1]
	switch p.kind {
	case tokPunct:
		switch p.text {
		case ")", ";", "{", "}":
			return true
		case ">":
			return n-2 >= from && s.toks[n-2].text == "=" && s.toks[n-2].end == p.start
		}
		return false
	case tokIdent:
		if s.isProperty(n-1, from) {
			return false
		}
		if p.text == "do" || p.text == "else" {
			return true
		}
		return !regexPrecedingKeywords[p.text]
	}
	return true
}

// run scans tokens until the end of the source or, when inSubst is set, until the brace that closes the template
// substitution it was started for (consumed, not emitted).
func (s *scanner) run(from, startOff int, inSubst bool) error {
	base := len(s.braces)
	src := s.src
	for s.pos < len(src) {
		c := src[s.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			s.pos++
		case c == '/' && s.pos+1 < len(src) && src[s.pos+1] == '/':
			for s.pos < len(src) && src[s.pos] != '\n' && src[s.pos] != '\r' {
				s.pos++
			}
		case c == '/' && s.pos+1 < len(src) && src[s.pos+1] == '*':
			i := strings.Index(src[s.pos+2:], "*/")
			if i < 0 {
				return &lexError{s.pos, "unterminated comment"}
			}
			s.pos += i + 4
		case c == '"' || c == '\'':
			start := s.pos
			s.pos++
			for {
				if s.pos >= len(src) || src[s.pos] == '\n' {
					return &lexError{start, "unterminated string literal"}
				}
				if src[s.pos] == '\\' {
					s.pos += 2
					continue
				}
				if src[s.pos] == c {
					s.pos++
					break
				}
				s.pos++
			}
			s.toks = append(s.toks, token{tokString, src[start:s.pos], start, s.pos})
		case c == '`':
			if err := s.template(); err != nil {
				return err
			}
		case c == '/' && s.prevAllowsRegex(from):
			start := s.pos
			s.pos++
			inClass := false
			for {
				if s.pos >= len(src) || src[s.pos] == '\n' {
					return &lexError{start, "unterminated regular expression literal"}
				}
				ch := src[s.pos]
				if ch == '\\' {
					s.pos += 2
					continue
				}
				s.pos++
				if ch == '[' {
					inClass = true
				} else if ch == ']' {
					inClass = false
				} else if ch == '/' && !inClass {
					break
				}
			}
			for s.pos < len(src) && isIdentPart(src[s.pos]) {
				s.pos++
			}
			s.toks = append(s.toks, token{tokRegex, src[start:s.pos], start, s.pos})
		case isIdentStart(c) || c == '\\':
			start := s.pos
			for s.pos < len(src) && (isIdentPart(src[s.pos]) || src[s.pos] == '\\') {
				s.pos++
			}
			s.toks = append(s.toks, token{tokIdent, src[start:s.pos], start, s.pos})
		case c >= '0' && c <= '9' || c == '.' && s.pos+1 < len(src) && src[s.pos+1] >= '0' && src[s.pos+1] <= '9':
			start := s.pos
			hex := c == '0' && s.pos+1 < len(src) && (src[s.pos+1] == 'x' || src[s.pos+1] == 'X')
			for s.pos < len(src) {
				ch := src[s.pos]
				if isIdentPart(ch) || ch == '.' {
					s.pos++
					if !hex && (ch == 'e' || ch == 'E') && s.pos < len(src) && (src[s.pos] == '+' || src[s.pos] == '-') {
						s.pos++
					}
					continue
				}
				break
			}
			s.toks = append(s.toks, token{tokNumber, src[start:s.pos], start, s.pos})
		default:
			switch c {
			case '(':
				header := false
				if n := len(s.toks); n-1 >= from && s.toks[n-1].kind == tokIdent && statementHeaders[s.toks[n-1].text] && !s.isProperty(n-1, from) {
					header = true
				}
				s.parens = append(s.parens, header)
			case ')':
				if n := len(s.parens); n > 0 {
					s.closeRegex[len(s.toks)] = s.parens[n-1]
					s.parens = s.parens[:n-1]
				}
			case '{':
				s.braces = append(s.braces, s.opensBlock(from, inSubst))
			case '}':
				if inSubst && len(s.braces) == base {
					s.pos++
					return nil
				}
				if n := len(s.braces); n > 0 {
					s.closeRegex[len(s.toks)] = s.braces[n-1]
					s.braces = s.braces[:n-1]
				}
			}
			s.toks = append(s.toks, token{tokPunct, string(c), s.pos, s.pos + 1})
			s.pos++
		}
	}
	if inSubst {
		return &lexError{startOff, "unterminated template substitution"}
	}
	return nil
}

// template scans a template literal starting at the backtick under the cursor.
func (s *scanner) template() error {
	src := s.src
	start := s.pos
	s.pos++
	for {
		if s.pos >= len(src) {
			return &lexError{start, "unterminated template literal"}
		}
		switch {
		case src[s.pos] == '\\':
			s.pos += 2
		case src[s.pos] == '`':
			s.pos++
			s.toks = append(s.toks, token{tokTemplate, src[start:s.pos], start, s.pos})
			return nil
		case src[s.pos] == '$' && s.pos+1 < len(src) && src[s.pos+1] == '{':
			s.pos += 2
			if err := s.run(len(s.toks), s.pos, true); err != nil {
				return err
			}
		default:
			s.pos++
		}
	}
}

// lineOf returns the 1-based line of byte offset off.
func lineOf(src string, off int) int {
	if off > len(src) {
		off = len(src)
	}
	return 1 + strings.Count(src[:off], "\n")
}

// errAt formats a positioned load message.
func errAt(name, src string, off int, format string, args ...any) error {
	return fmt.Errorf("%s:%d: %s", name, lineOf(src, off), fmt.Sprintf(format, args...))
}
