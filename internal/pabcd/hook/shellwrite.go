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

// shellWriteDestinationsIn is ShellWriteDestinations with the program nesting depth carried (the top command is 0, a
// here-document body is one deeper), so a body read as program text stops at shellWriteHeredocMaxDepth instead of
// recursing without bound.
func shellWriteDestinationsIn(command string, depth int) []string {
	dests := []string{}
	for _, segment := range splitShellSegments(stripHeredocBodies(utf16.Encode([]rune(command)))) {
		dests = append(dests, shellStrings(redirectDestinations(segment))...)
		dests = append(dests, verbDestinations(shellString(segment), depth)...)
	}
	for _, dest := range literalRedirectDestinations(command) {
		if !slices.Contains(dests, dest) {
			dests = append(dests, dest)
		}
	}
	return shellVerbAppendNew(dests, shellWriteHeredocDestinations(command, depth))
}

// shellWriteHeredocDecl is one << operator declared in a command header: its delimiter word, whether that word was
// quoted, and whether the operator was <<-.
type shellWriteHeredocDecl struct {
	delim  []uint16
	quoted bool
	tabs   bool
}

// shellWriteHeredoc is one here-document of a command: the whole header line that declared it (so the interpreter
// predicate can read the command's words with every operator and delimiter word skipped by the tokenizer), the
// delimiter word, whether that word was quoted (a quoted word makes the body literal; the outer shell expands an
// unquoted one), whether the operator was <<- (leading tabs are stripped, as the shell does), and the body text.
type shellWriteHeredoc struct {
	command []uint16
	delim   []uint16
	body    []uint16
	quoted  bool
	tabs    bool
}

// shellWriteHeredocs enumerates the here-documents of a command without changing the oracle's own stripHeredocBodies:
// it walks the same text with the same quote, operator and line helpers (skipQuoted, skipHeredoc, shellNewline) and
// records each body instead of deleting it. A header may declare several << operators, and the shell reads their bodies
// in operator order, so every declaration of the header is recorded (CRW-765 review). The header is the whole logical
// line, not only the words before the first operator, because a redirect may precede the command it feeds
// (<<'PY' python3) and because the tokenizer already skips the operator and delimiter words. A body's leading tabs are
// removed when the operator was <<-, before the interpreter reads the line.
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
			eol := shellNewline(command, i)
			if eol == -1 {
				eol = len(command)
			}
			header := command[start:eol]
			decls := shellWriteHeredocDecls(header)
			j := eol
			if j < len(command) {
				j++
			}
			for _, d := range decls {
				if len(d.delim) == 0 && !d.quoted {
					continue // no delimiter word at all (the oracle's absent-delimiter case)
				}
				body, next := shellWriteHeredocBody(command, j, d)
				out = append(out, shellWriteHeredoc{command: header, delim: d.delim, body: body, quoted: d.quoted, tabs: d.tabs})
				j = next
			}
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

// shellWriteHeredocDecls reads every << operator declared in one header line, in order, skipping quoted spans so a
// quoted << is not an operator.
func shellWriteHeredocDecls(header []uint16) []shellWriteHeredocDecl {
	out := []shellWriteHeredocDecl{}
	for i := 0; i < len(header); {
		ch := header[i]
		if ch == '\'' || ch == '"' {
			i = skipQuoted(header, i)
			continue
		}
		if ch == '<' && shellAt(header, i+1) == '<' && shellAt(header, i+2) != '<' {
			delim, quoted := shellWriteHeredocDelimiter(header, i)
			out = append(out, shellWriteHeredocDecl{delim: delim, quoted: quoted, tabs: shellAt(header, i+2) == '-'})
			i = skipHeredoc(header, i)
			continue
		}
		i++
	}
	return out
}

// shellWriteHeredocBody reads one here-document body from from up to its delimiter line, returning the body text and the
// offset after the terminator. When the operator was <<-, leading tabs are removed from every body line before the
// terminator is matched and before the interpreter reads the line.
func shellWriteHeredocBody(command []uint16, from int, d shellWriteHeredocDecl) (body []uint16, next int) {
	body = []uint16{}
	k := from
	for k <= len(command) {
		nl := shellNewline(command, k)
		line := command[k:]
		if nl != -1 {
			line = command[k:nl]
		}
		cmp := line
		if d.tabs {
			cmp = shellWriteHeredocTrimTabs(cmp)
		}
		if slices.Equal(cmp, d.delim) {
			if nl == -1 {
				return body, len(command)
			}
			return body, nl + 1
		}
		body = append(body, cmp...)
		body = append(body, '\n')
		if nl == -1 {
			return body, len(command)
		}
		k = nl + 1
	}
	return body, len(command)
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

// shellWriteHeredocHeaderWords is the words of the simple command that owns the here-document, read the way the shell
// reads it (CRW-765 correction 2): the header is cut at pipes and control operators, so only the command the
// here-document is attached to is judged, and every redirection (with its target word) is dropped wherever it stands,
// before or after the verb. A redirection is not a command word, so a redirect before the verb reads as the interpreter
// and a redirect after the operator leaves the command word alone; a here-string operator and its word are dropped the
// same way.
func shellWriteHeredocHeaderWords(header []uint16) []string {
	seg := shellWriteHeredocOwningSegment(header)
	out := []uint16{}
	for i := 0; i < len(seg); {
		ch := seg[i]
		if ch == '\'' || ch == '"' {
			next := skipQuoted(seg, i)
			out = append(out, seg[i:next]...)
			i = next
			continue
		}
		if ch == '<' && shellAt(seg, i+1) == '<' {
			if shellAt(seg, i+2) == '<' {
				i = readToken(seg, i+3).next // a here-string: the operator and its word
			} else {
				i = shellWriteHeredocSkipDelimiter(seg, i) // << / <<- and its delimiter word
			}
			out = append(out, ' ')
			continue
		}
		if ch == '<' || ch == '>' || ch == '&' && shellAt(seg, i+1) == '>' {
			out = shellWriteHeredocDropFd(out)
			i = shellWriteHeredocSkipRedirect(seg, i)
			out = append(out, ' ')
			continue
		}
		out = append(out, ch)
		i++
	}
	return shellTokenize(shellString(out))
}

// shellWriteHeredocOwningSegment is the part of a header between the last control operator before the here-document
// operator and the first one after it: the simple command the here-document is attached to.
func shellWriteHeredocOwningSegment(header []uint16) []uint16 {
	at := -1
	for i := 0; i < len(header); {
		ch := header[i]
		if ch == '\'' || ch == '"' {
			i = skipQuoted(header, i)
			continue
		}
		if ch == '<' && shellAt(header, i+1) == '<' && shellAt(header, i+2) != '<' {
			at = i
			break
		}
		i++
	}
	if at < 0 {
		return header
	}
	start, end := 0, len(header)
	for i := 0; i < at; {
		ch := header[i]
		if ch == '\'' || ch == '"' {
			i = skipQuoted(header, i)
			continue
		}
		if shellWriteHeredocSeparator(header, i) {
			start = i + 1
		}
		i++
	}
	for i := at; i < len(header); {
		ch := header[i]
		if ch == '\'' || ch == '"' {
			i = skipQuoted(header, i)
			continue
		}
		if shellWriteHeredocSeparator(header, i) {
			end = i
			break
		}
		i++
	}
	return header[start:end]
}

// shellWriteHeredocSeparator reports whether a byte is a shell control operator that ends a simple command: ;, | and &,
// with &> and &< kept as redirections and a >| (the clobber operator) left alone.
func shellWriteHeredocSeparator(s []uint16, i int) bool {
	switch shellAt(s, i) {
	case ';':
		return true
	case '|':
		return shellAt(s, i-1) != '>'
	case '&':
		// & is a control operator unless it is part of a redirection: &>f, &>>f, n>&m, n<&m.
		return shellAt(s, i-1) != '>' && shellAt(s, i-1) != '<' && shellAt(s, i+1) != '>' && shellAt(s, i+1) != '<'
	}
	return false
}

// shellWriteHeredocDropFd removes a file-descriptor digit word (the 2 of 2>) from the words read so far, when the digits
// stand as their own word; a digit inside a word (cmd2>) stays.
func shellWriteHeredocDropFd(out []uint16) []uint16 {
	k := len(out)
	for k > 0 && out[k-1] >= '0' && out[k-1] <= '9' {
		k--
	}
	if k == len(out) {
		return out
	}
	if k > 0 && !shellSpace(out[k-1]) {
		return out
	}
	return out[:k]
}

// shellWriteHeredocSkipRedirect is the offset after a redirection operator and its target: <, >, >>, <>, >|, &>, &>>,
// n>&m, n<&m, n>&- and the file target that follows.
func shellWriteHeredocSkipRedirect(s []uint16, i int) int {
	i++
	if shellAt(s, i) == '>' || shellAt(s, i) == '<' {
		i++ // >> or <>
	}
	if shellAt(s, i) == '&' {
		i++
		if shellAt(s, i) == '-' {
			return i + 1
		}
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		return i
	}
	for i < len(s) && shellSpace(s[i]) {
		i++
	}
	if i >= len(s) {
		return i
	}
	if s[i] == '\'' || s[i] == '"' {
		return skipQuoted(s, i)
	}
	for i < len(s) {
		c := s[i]
		if shellSpace(c) || c == ';' || c == '|' || c == '&' || c == '<' || c == '>' || c == '(' || c == ')' {
			break
		}
		i++
	}
	return i
}

// shellWriteHeredocSkipDelimiter is the offset after a << operator and its delimiter word, reading the word as the
// collector does (a quote pair, a backslash escape, or a run of word characters).
func shellWriteHeredocSkipDelimiter(s []uint16, i int) int {
	i += 2
	if shellAt(s, i) == '-' {
		i++
	}
	for i < len(s) && shellSpace(s[i]) {
		i++
	}
	for i < len(s) {
		c := s[i]
		if shellSpace(c) || c == ';' || c == '|' || c == '&' || c == '(' || c == ')' || c == '<' || c == '>' {
			break
		}
		switch c {
		case '\'', '"':
			q := c
			i++
			for i < len(s) && s[i] != q {
				i++
			}
			if shellAt(s, i) == q {
				i++
			}
		case '\\':
			i += 2
		default:
			i++
		}
	}
	return i
}

// verbDestinations is the verb step: shellwrite_verbs.go reads the destinations of tee, sed -i, cp, mv, perl, ruby, python
// and node from the segment's words (shellTokenize below). It adds no memory-gate activation here. depth is the
// here-document nesting the caller is inside.
func verbDestinations(segment string, depth int) []string {
	return shellVerbDestinations(segment, depth)
}

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
