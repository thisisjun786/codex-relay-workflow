package hook

import (
	"slices"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ShellWriteDestinations ports shell-write-destinations.ts:25-264 at CXC
// v0.2.40 (3c1459ac). Legacy reports keep their order and duplicates; literal
// redirects missed by that lexer are appended conservatively. Verbs are deferred.
// This is a lexical detector, not a shell evaluator. At the string boundary,
// lone UTF-16 units become U+FFFD, as Node's UTF-8 encoding does.
func ShellWriteDestinations(command string) []string {
	return shellWriteDestinationsIn(command, 0)
}

// shellWriteDestinationsIn is ShellWriteDestinations with the here-document nesting depth carried, so a shell body read
// as program text stops at shellWriteHeredocMaxDepth instead of recursing without bound.
func shellWriteDestinationsIn(command string, depth int) []string {
	dests := []string{}
	for _, segment := range splitShellSegments(stripHeredocBodies(utf16.Encode([]rune(command)))) {
		dests = append(dests, shellStrings(redirectDestinations(segment))...)
		dests = append(dests, verbDestinations(shellString(segment))...)
	}
	for _, dest := range literalRedirectDestinations(command) {
		if !slices.Contains(dests, dest) {
			dests = append(dests, dest)
		}
	}
	return shellVerbAppendNew(dests, shellWriteHeredocDestinations(command, depth))
}

// shellWriteHeredoc is one here-document of a command: the words of the command line that owns the operator (so
// shellWriteHeredocKindOf can tell which interpreter, if any, reads the body), the delimiter word, whether that word
// was quoted (a quoted word makes the body literal; the outer shell expands an unquoted one), whether the operator was
// <<- (leading tabs are stripped, as the shell does), and the body text.
type shellWriteHeredoc struct {
	command []uint16
	delim   []uint16
	body    []uint16
	quoted  bool
	tabs    bool
}

// shellWriteHeredocs enumerates the here-documents of a command without changing the oracle's own stripHeredocBodies:
// it walks the same text with the same quote, operator and line helpers (skipQuoted, skipHeredoc, shellNewline) and
// records each body instead of deleting it. The command text is the words from the start of the command that owns the
// operator to the operator itself. One here-document per << operator is recorded, matching how stripHeredocBodies
// consumes them (a second operator on one line keeps its body in the text).
func shellWriteHeredocs(command []uint16) []shellWriteHeredoc {
	out := []shellWriteHeredoc{}
	start := 0
	for i := 0; i < len(command); {
		ch := command[i]
		if ch == '\'' || ch == '"' {
			i = skipQuoted(command, i)
			continue
		}
		if ch == '<' && shellAt(command, i+1) == '<' && shellAt(command, i+2) != '<' {
			tabs := shellAt(command, i+2) == '-'
			after := skipHeredoc(command, i)
			delim, quoted := shellWriteHeredocDelimiter(command, i)
			if len(delim) == 0 {
				i = after
				continue
			}
			eol := shellNewline(command, after)
			if eol == -1 {
				break
			}
			j, end := eol+1, eol+1
			for {
				nl := shellNewline(command, j)
				line := command[j:len(command)]
				if nl != -1 {
					line = command[j:nl]
				}
				if tabs {
					line = shellWriteHeredocTrimTabs(line)
				}
				if slices.Equal(line, delim) {
					end = j
					if nl == -1 {
						j = len(command)
					} else {
						j = nl + 1
					}
					break
				}
				if nl == -1 {
					end, j = len(command), len(command)
					break
				}
				j = nl + 1
			}
			out = append(out, shellWriteHeredoc{command: command[start:i], delim: delim, body: command[eol+1 : end], quoted: quoted, tabs: tabs})
			i, start = j, j
			continue
		}
		if ch == '\n' || ch == ';' || ch == '|' && shellAt(command, i-1) != '>' || ch == '&' && shellAt(command, i-1) != '>' && shellAt(command, i-1) != '<' && shellAt(command, i+1) != '>' {
			start = i + 1
		}
		i++
	}
	return out
}

// shellWriteHeredocDelimiter reads the delimiter word at a << operator, removing its quoting and reporting whether the
// word was quoted. It is the collector's reader; the oracle's heredocDelimiter keeps its own answer for the recorded
// corpus, where a backslash-escaped delimiter reads as absent.
func shellWriteHeredocDelimiter(s []uint16, i int) (delim []uint16, quoted bool) {
	i += 2
	if shellAt(s, i) == '-' {
		i++
	}
	for i < len(s) && shellSpace(s[i]) {
		i++
	}
	out := []uint16{}
	for i < len(s) {
		c := s[i]
		if shellSpace(c) || c == ';' || c == '|' || c == '&' || c == '(' || c == ')' || c == '<' || c == '>' {
			break
		}
		switch c {
		case '\'', '"':
			quoted = true
			q := c
			i++
			for i < len(s) && s[i] != q {
				out = append(out, s[i])
				i++
			}
			if shellAt(s, i) == q {
				i++
			}
		case '\\':
			quoted = true
			i++
			if i < len(s) {
				out = append(out, s[i])
				i++
			}
		default:
			out = append(out, c)
			i++
		}
	}
	return out, quoted
}

// shellWriteHeredocTrimTabs removes the leading tabs of one here-document body line, as <<- does before the terminator
// is matched and before the interpreter reads the line.
func shellWriteHeredocTrimTabs(line []uint16) []uint16 {
	i := 0
	for i < len(line) && line[i] == '\t' {
		i++
	}
	return line[i:]
}

// verbDestinations is the verb step: shellwrite_verbs.go reads the destinations of tee, sed -i, cp, mv, perl, ruby, python
// and node from the segment's words (shellTokenize below). It adds no memory-gate activation here.
func verbDestinations(segment string) []string { return shellVerbDestinations(segment) }

type shellToken struct {
	token []uint16
	next  int
}

func shellString(s []uint16) string { return string(utf16.Decode(s)) }

func shellStrings(ss [][]uint16) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = shellString(s)
	}
	return out
}

func shellAt(s []uint16, i int) uint16 {
	if i < 0 || i >= len(s) {
		return 0
	}
	return s[i]
}

func shellSpace(c uint16) bool { return text.Trim(string(rune(c))) == "" }

func shellWordUnit(c uint16) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_'
}

func shellNewline(s []uint16, from int) int {
	for i := from; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

// The helper deliberately retains exact-line delimiters and one body per
// operator line. Exported results compensate the security misses separately.
func stripHeredocBodies(command []uint16) []uint16 {
	out := []uint16{}
	for i := 0; i < len(command); {
		ch := command[i]
		if ch == '\'' || ch == '"' {
			next := skipQuoted(command, i)
			out = append(out, command[i:next]...)
			i = next
			continue
		}
		if ch == '<' && shellAt(command, i+1) == '<' && shellAt(command, i+2) != '<' {
			after := skipHeredoc(command, i)
			delim := heredocDelimiter(command, i)
			out = append(out, command[i:after]...)
			i = after
			if len(delim) == 0 {
				continue
			}
			eol := shellNewline(command, i)
			if eol == -1 {
				return append(out, command[i:]...)
			}
			out = append(out, command[i:eol]...)
			j := eol + 1
			for {
				nl := shellNewline(command, j)
				end := nl
				if end == -1 {
					end = len(command)
				}
				if slices.Equal(command[j:end], delim) {
					j = end
					if nl != -1 {
						j++
					}
					break
				}
				if nl == -1 {
					j = len(command)
					break
				}
				j = nl + 1
			}
			out = append(out, '\n')
			i = j
			continue
		}
		out = append(out, ch)
		i++
	}
	return out
}

func heredocDelimiter(s []uint16, i int) []uint16 {
	i += 2
	if shellAt(s, i) == '-' {
		i++
	}
	for i < len(s) && shellSpace(s[i]) {
		i++
	}
	if shellAt(s, i) == '\'' || shellAt(s, i) == '"' {
		q := s[i]
		i++
		start := i
		for i < len(s) && s[i] != q {
			i++
		}
		return s[start:i]
	}
	start := i
	for i < len(s) && shellWordUnit(s[i]) {
		i++
	}
	// JavaScript slice clamps offsets beyond the string, including an absent
	// delimiter when called directly by the recorded helper cases.
	return s[min(start, len(s)):min(i, len(s))]
}

func splitShellSegments(command []uint16) [][]uint16 {
	segments := [][]uint16{}
	cur := []uint16{}
	for i := 0; i < len(command); {
		ch := command[i]
		if ch == '\'' || ch == '"' {
			next := skipQuoted(command, i)
			cur = append(cur, command[i:next]...)
			i = next
			continue
		}
		if ch == '<' && shellAt(command, i+1) == '<' && shellAt(command, i+2) != '<' {
			next := skipHeredoc(command, i)
			cur = append(cur, command[i:next]...)
			i = next
			continue
		}
		if ch == '|' && i > 0 && command[i-1] == '>' {
			cur = append(cur, ch)
			i++
			continue
		}
		if ch == ';' || ch == '|' || ch == '&' && shellAt(command, i+1) == '&' {
			if text.Trim(shellString(cur)) != "" {
				segments = append(segments, cur)
			}
			cur = []uint16{}
			if ch == '&' || ch == '|' && shellAt(command, i+1) == '|' {
				i++
			}
			i++
			continue
		}
		cur = append(cur, ch)
		i++
	}
	if text.Trim(shellString(cur)) != "" {
		segments = append(segments, cur)
	}
	return segments
}

func skipQuoted(s []uint16, i int) int {
	q := shellAt(s, i)
	i++
	for i < len(s) {
		if q == '"' && s[i] == '\\' && i+1 < len(s) {
			i += 2
			continue
		}
		if s[i] == q {
			return i + 1
		}
		i++
	}
	return i
}

func skipHeredoc(s []uint16, i int) int {
	i += 2
	if shellAt(s, i) == '-' {
		i++
	}
	for i < len(s) && shellSpace(s[i]) {
		i++
	}
	if shellAt(s, i) == '\'' || shellAt(s, i) == '"' {
		q := s[i]
		i++
		for i < len(s) && s[i] != q {
			i++
		}
		if shellAt(s, i) == q {
			i++
		}
		return i
	}
	for i < len(s) && shellWordUnit(s[i]) {
		i++
	}
	return i
}

func readToken(s []uint16, i int) shellToken {
	for i < len(s) && shellSpace(s[i]) {
		i++
	}
	if i >= len(s) {
		return shellToken{[]uint16{}, i}
	}
	if s[i] == '\'' || s[i] == '"' {
		start := i + 1
		end := skipQuoted(s, i) - 1
		return shellToken{s[start:max(start, end)], end + 1}
	}
	start := i
	for i < len(s) && !shellSpace(s[i]) {
		i++
	}
	return shellToken{s[start:i], i}
}

func redirectDestinations(segment []uint16) [][]uint16 {
	dests := [][]uint16{}
	for i := 0; i < len(segment); {
		ch := segment[i]
		if ch == '\'' || ch == '"' {
			i = skipQuoted(segment, i)
			continue
		}
		if ch == '<' && shellAt(segment, i+1) == '<' {
			if shellAt(segment, i+2) == '<' {
				i = readToken(segment, i+3).next
			} else {
				i = skipHeredoc(segment, i)
			}
			continue
		}
		if ch != '>' {
			i++
			continue
		}
		k := i - 1
		fd := ""
		if shellAt(segment, k) == '&' {
			fd = "&"
			k--
		} else {
			for k >= 0 && segment[k] >= '0' && segment[k] <= '9' {
				k--
			}
			fd = shellString(segment[k+1 : i])
		}
		if before := shellAt(segment, k); before == '-' || before == '<' {
			i++
			continue
		}
		opEnd := i + 1
		if shellAt(segment, opEnd) == '>' || shellAt(segment, opEnd) == '|' {
			opEnd++
		}
		if shellAt(segment, opEnd) == '&' {
			i = readToken(segment, opEnd+1).next
			continue
		}
		dest := readToken(segment, opEnd)
		if fd != "2" && len(dest.token) != 0 && dest.token[0] != '&' {
			dests = append(dests, dest.token)
		}
		i = dest.next
	}
	return dests
}

func tokenizeUnits(segment []uint16) [][]uint16 {
	tokens := [][]uint16{}
	for i := 0; i < len(segment); {
		if segment[i] == '\'' || segment[i] == '"' {
			r := readToken(segment, i)
			tokens = append(tokens, r.token)
			i = r.next
			continue
		}
		if segment[i] == '<' && shellAt(segment, i+1) == '<' && shellAt(segment, i+2) != '<' {
			i = skipHeredoc(segment, i)
			continue
		}
		if shellSpace(segment[i]) {
			i++
			continue
		}
		r := readToken(segment, i)
		if len(r.token) != 0 {
			tokens = append(tokens, r.token)
		}
		if r.next == i {
			i++
		} else {
			i = r.next
		}
	}
	return tokens
}

func shellTokenize(segment string) []string {
	return shellStrings(tokenizeUnits(utf16.Encode([]rune(segment))))
}
