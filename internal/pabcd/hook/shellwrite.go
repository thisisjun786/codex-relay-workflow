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

// shellWriteHeredocInterpreterName reports whether a word's last path element is one of the modeled interpreters
// (CRW-765 correction 3): python, python3, pythonX.Y, node, nodejs, sh, bash, dash, ash, zsh, ksh and mksh, also as
// the last element of a path. A here-document attached to a command whose verb is one of these may be that
// interpreter's program.
func shellWriteHeredocInterpreterName(word string) bool {
	name := shellVerbName(word)
	switch name {
	case "python", "python3", "node", "nodejs", "sh", "bash", "dash", "ash", "zsh", "ksh", "mksh":
		return true
	}
	return shellVerbVersioned(name)
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

// The closed rule (CRW-765 correction 3) classifies one here-document by proving its owning simple command, so a
// header shape the parser does not model fails closed instead of being added shape by shape. shellWriteHeredocSimpleCommand
// proves the owning command: it is a simple command made only of literal words, here-document operators (no file
// descriptor or fd 0) with their delimiter words, and the fixed redirections with a literal target word.
const (
	shellWriteHeredocOpNone     = iota
	shellWriteHeredocOpHeredoc  // << or <<-, with its delimiter word already consumed
	shellWriteHeredocOpRedirect // [n]>, [n]>>, [n]< or &>, with a target word to read
	shellWriteHeredocOpDup      // [n]>&m or [n]<&m, which names a descriptor and takes no target word
	shellWriteHeredocOpInvalid  // an operator shape the closed set does not allow, so the command is not proven
)

// shellWriteHeredocSimpleCommand reads the owning simple command strictly. ok is true only when the segment is made
// only of literal words, here-document operators (no file descriptor or fd 0) with their delimiter words, and the fixed
// redirections with a literal target word. A word holding an expansion, a substitution, a brace, a glob, a comment
// marker or a here-string is not literal, so the command is not proven and its here-document fails closed. words is the
// literal words in order, the verb first.
func shellWriteHeredocSimpleCommand(seg []uint16) (words []string, ok bool) {
	words = []string{}
	for i := 0; i < len(seg); {
		c := seg[i]
		if shellSpace(c) {
			i++
			continue
		}
		if c == '#' {
			return words, len(words) > 0 // a # at the start of a word begins a comment to the end of the line
		}
		switch kind, next := shellWriteHeredocOperator(seg, i); kind {
		case shellWriteHeredocOpNone:
		case shellWriteHeredocOpInvalid:
			return words, false
		case shellWriteHeredocOpRedirect:
			if _, n, lit := shellWriteHeredocLiteralWord(seg, next); lit {
				i = n
			} else {
				return words, false
			}
			continue
		default:
			i = next
			continue
		}
		w, n, lit := shellWriteHeredocLiteralWord(seg, i)
		if !lit || len(w) == 0 {
			return words, false
		}
		words = append(words, shellString(w))
		i = n
	}
	return words, len(words) > 0
}

// shellWriteHeredocOperator reads the operator at i with an optional file-descriptor number attached (no space). For a
// here-document it also consumes the delimiter word, whose quoting it ignores. It returns shellWriteHeredocOpNone when
// the bytes are no operator at all, and shellWriteHeredocOpInvalid when they are an operator shape the closed set does
// not allow (a here-string, a descriptor other than 0 on a here-document, >|, <>, &>>), so the command is not proven.
func shellWriteHeredocOperator(s []uint16, i int) (kind int, next int) {
	// &> is the only operator that begins with &.
	if shellAt(s, i) == '&' && shellAt(s, i+1) == '>' {
		if shellAt(s, i+2) == '>' {
			return shellWriteHeredocOpInvalid, i // &>> is not in the set
		}
		return shellWriteHeredocOpRedirect, i + 2
	}
	j := i
	for j < len(s) && s[j] >= '0' && s[j] <= '9' {
		j++
	}
	if j >= len(s) || (s[j] != '<' && s[j] != '>') {
		return shellWriteHeredocOpNone, i
	}
	fd := shellString(s[i:j])
	if s[j] == '<' {
		switch {
		case shellAt(s, j+1) == '<' && shellAt(s, j+2) == '<':
			return shellWriteHeredocOpInvalid, i // <<< is a here-string, not in the set
		case shellAt(s, j+1) == '<':
			if fd != "" && fd != "0" {
				return shellWriteHeredocOpInvalid, i // a file descriptor other than 0
			}
			k := j + 2
			if shellAt(s, k) == '-' {
				k++
			}
			return shellWriteHeredocOpHeredoc, shellWriteHeredocDelimiterEnd(s, k)
		case shellAt(s, j+1) == '&':
			k := j + 2
			for k < len(s) && s[k] >= '0' && s[k] <= '9' {
				k++
			}
			if k == j+2 {
				return shellWriteHeredocOpInvalid, i // <& with no descriptor
			}
			return shellWriteHeredocOpDup, k
		case shellAt(s, j+1) == '>':
			return shellWriteHeredocOpInvalid, i // <> is not in the set
		default:
			return shellWriteHeredocOpRedirect, j + 1 // [n]<
		}
	}
	switch {
	case shellAt(s, j+1) == '&':
		k := j + 2
		for k < len(s) && s[k] >= '0' && s[k] <= '9' {
			k++
		}
		if k == j+2 {
			return shellWriteHeredocOpInvalid, i // >& with no descriptor
		}
		return shellWriteHeredocOpDup, k
	case shellAt(s, j+1) == '>':
		if shellAt(s, j+2) == '&' {
			return shellWriteHeredocOpInvalid, i // >>& is not in the set
		}
		return shellWriteHeredocOpRedirect, j + 2 // [n]>>
	case shellAt(s, j+1) == '|':
		return shellWriteHeredocOpInvalid, i // >| is not in the set
	default:
		return shellWriteHeredocOpRedirect, j + 1 // [n]>
	}
}

// shellWriteHeredocLiteralWord reads one word and reports whether it is literal: an unquoted word without an expansion,
// a substitution, a brace, a glob or a comment marker; a single-quoted word; or a double-quoted word without an
// expansion, a backtick or a backslash. It returns the word's unquoted text and the offset after it.
func shellWriteHeredocLiteralWord(s []uint16, i int) (word []uint16, next int, literal bool) {
	for i < len(s) && shellSpace(s[i]) {
		i++
	}
	out := []uint16{}
	literal = true
	var quote uint16
	for i < len(s) {
		c := s[i]
		if quote == 0 {
			if shellSpace(c) || c == '<' || c == '>' || c == ';' || c == '|' || c == '&' || c == '(' || c == ')' {
				break
			}
			switch c {
			case '\'', '"':
				quote = c
				i++
				continue
			case '$', '\x60', '{', '}', '#', '*', '?', '[':
				literal = false
			}
			out = append(out, c)
			i++
			continue
		}
		if c == quote {
			quote = 0
			i++
			continue
		}
		if quote == '"' && (c == '$' || c == '\x60' || c == '\\') {
			literal = false
		}
		out = append(out, c)
		i++
	}
	if quote != 0 {
		literal = false
	}
	if len(out) == 0 {
		return nil, i, false
	}
	return out, i, literal
}

// shellWriteHeredocDelimiterEnd is the offset after a here-document delimiter word that begins at i.
func shellWriteHeredocDelimiterEnd(s []uint16, i int) int {
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
