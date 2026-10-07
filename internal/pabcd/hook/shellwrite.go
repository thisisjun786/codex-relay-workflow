package hook

import (
	"slices"
	"strings"
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
	at     int // the offset of the << operator in the header line
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
	joined  bool // the previous physical line ends with a backslash, so this header is a line continuation
	at      int  // the offset of the << operator in the header line (rule U1)
	bodyAt  int  // the offset the body begins at in the walked text (rule K5)
	bodyEnd int  // the offset after the body's terminator line (rule K5)
}

// shellWriteHeredocs enumerates the here-documents of a command without changing the oracle's own stripHeredocBodies:
// it walks the same text with the same quote, operator and line helpers (skipQuoted, skipHeredoc, shellNewline) and
// records each body instead of deleting it. A header may declare several << operators, and the shell reads their bodies
// in operator order, so every declaration of the header is recorded (CRW-765 review). The header is the whole physical
// line that holds the operator, not only the words before the first operator, because a redirect may precede the
// command it feeds (<<'PY' python3), a redirect or a pipe may follow it, and a second command may follow on the same
// line (CRW-765 corrections 2 and 4); the tokenizer already skips the operator and delimiter words. A body's leading
// tabs are removed when the operator was <<-, before the interpreter reads the line.
func shellWriteHeredocs(command []uint16) []shellWriteHeredoc {
	out := []shellWriteHeredoc{}
	for i := 0; i < len(command); {
		ch := command[i]
		if ch == '#' && shellWriteHeredocCommentStart(command, i) {
			// Rule K1: a word-initial # begins a comment, so the rest of the physical line holds no operator. The
			// command reader (shellWriteHeredocSegmentWords) already ends a command at a #; the collector must read
			// the same text, or a << written inside a comment would claim the next interpreter's body as its own.
			eol := shellNewline(command, i)
			if eol == -1 {
				break
			}
			i = eol + 1
			continue
		}
		if ch == '\\' {
			// A backslash in plain state escapes the character after it, so an escaped quote is a literal character
			// and opens no quoted span (CRW-765 correction 9). Reading `\'` as an opening quote let the span swallow
			// a real << operator, so no here-document was collected and the interpreter program it carried was never
			// read. The oracle's stripper keeps its own answer; this collector is the security reader.
			i += 2
			continue
		}
		if ch == '\'' || ch == '"' {
			i = skipQuoted(command, i)
			continue
		}
		if ch == '<' && shellAt(command, i+1) == '<' && shellAt(command, i+2) == '<' {
			// Rule K4: <<< is a here-string, not a here-document. The whole token is consumed, so its second and
			// third < are never read as a here-document operator (the CRW-726 collector reads it the same way).
			i += 3
			continue
		}
		if ch == '<' && shellAt(command, i+1) == '<' {
			eol := shellNewline(command, i)
			if eol == -1 {
				eol = len(command)
			}
			lineStart := shellWriteHeredocLineStart(command, i)
			// The physical line before this one ends with a backslash: the shell joins the two lines before it reads
			// them, so this header is a continuation and is not proven (CRW-765 correction 4, rule G1).
			joined := lineStart >= 2 && command[lineStart-2] == '\\'
			header := command[lineStart:eol]
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
				out = append(out, shellWriteHeredoc{command: header, delim: d.delim, body: body, quoted: d.quoted, tabs: d.tabs, joined: joined, at: d.at, bodyAt: j, bodyEnd: next})
				j = next
			}
			i = j
			continue
		}
		i++
	}
	return out
}

// shellWriteHeredocCommentStart reports whether the # at i begins a shell comment: it stands at the beginning of a word,
// which is the start of the text, a blank, a newline or a control operator (rule K1). A # inside a word, as in `a#b`, is
// part of that word and begins nothing.
func shellWriteHeredocCommentStart(s []uint16, i int) bool {
	if shellAt(s, i) != '#' {
		return false
	}
	if i == 0 {
		return true
	}
	switch prev := s[i-1]; {
	case shellWriteHeredocBlank(prev), prev == ';', prev == '&', prev == '|', prev == '(', prev == ')':
		// The boundary must be the shell's own: a blank or an operator the previous backslash escaped is part of the
		// word before the #, so the # is a literal character and begins no comment (CRW-765 correction 9).
		return !shellWriteHeredocEscaped(s, i-1)
	}
	return false
}

// shellWriteHeredocBlank reports whether a character is a blank the shell itself uses to separate words: a space, a tab
// or a newline. The repository's literal shell reader uses the same ASCII set; the oracle's JavaScript blank set (which
// strips U+00A0 and the other Unicode spaces) must not decide a comment boundary, because the shell does not split a
// word at a non-breaking space: `echo x #; python3 ...` keeps the # inside the argument and runs the command after
// the semicolon (CRW-765 correction 9).
func shellWriteHeredocBlank(c uint16) bool {
	return c == ' ' || c == '\t' || c == '\n'
}

// shellWriteHeredocEscaped reports whether the character at i is escaped by an odd run of backslashes immediately
// before it (CRW-765 correction 9). The shell reads `\ ` as a literal blank, so that blank is no word boundary and the
// # after it is part of the same word.
func shellWriteHeredocEscaped(s []uint16, i int) bool {
	n := 0
	for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
		n++
	}
	return n%2 == 1
}

// shellWriteHeredocBlankComments returns the text with every comment blanked to spaces, its newlines kept, so a check
// that reads the text as syntax does not read what the shell never parses. A word-initial # ends its physical line and
// nothing beyond it (CRW-765 correction 9, third pass, after the blind pre-merge evaluation of head 05dc1a743: a
// function example or a backslash written in a comment made an ordinary documentation command look like a here-document
// program the reader could not read).
func shellWriteHeredocBlankComments(s []uint16) []uint16 {
	out := slices.Clone(s)
	for i := 0; i < len(out); {
		c := out[i]
		if c == '\\' {
			i += 2 // a backslash escapes the character after it, so an escaped quote opens no quoted span
			continue
		}
		if c == '\'' || c == '"' {
			i = skipQuoted(out, i)
			continue
		}
		if c == '#' && shellWriteHeredocCommentStart(out, i) {
			eol := shellNewline(out, i)
			if eol == -1 {
				eol = len(out)
			}
			for k := i; k < eol; k++ {
				out[k] = ' '
			}
			i = eol
			continue
		}
		i++
	}
	return out
}

// shellWriteHeredocLineStart is the offset of the physical line that holds at: the byte after the last newline before it.
// The header a here-document is judged by is that physical line, so a backslash, a control operator or a second command
// beside the operator is seen by the header proof (CRW-765 correction 4, rule G1).
func shellWriteHeredocLineStart(s []uint16, at int) int {
	for i := at - 1; i >= 0; i-- {
		if s[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

// shellWriteHeredocDecls reads every << operator declared in one header line, in order, skipping quoted spans so a
// quoted << is not an operator.
func shellWriteHeredocDecls(header []uint16) []shellWriteHeredocDecl {
	out := []shellWriteHeredocDecl{}
	for i := 0; i < len(header); {
		ch := header[i]
		if ch == '#' && shellWriteHeredocCommentStart(header, i) {
			// Rule K1: the declaration reader ends the line at a word-initial # exactly as the command reader does, so
			// a << written inside a comment is no declaration and cannot claim the next interpreter's body.
			break
		}
		if ch == '\'' || ch == '"' {
			i = skipQuoted(header, i)
			continue
		}
		if ch == '\\' {
			// A backslash in plain state escapes the character after it, so an escaped quote opens no quoted span and
			// cannot hide the << that follows it (CRW-765 correction 9, the collector's reading).
			i += 2
			continue
		}
		if ch == '<' && shellAt(header, i+1) == '<' && shellAt(header, i+2) == '<' {
			i += 3 // rule K4: <<< is a here-string, and its second and third < open no declaration
			continue
		}
		if ch == '<' && shellAt(header, i+1) == '<' {
			delim, quoted := shellWriteHeredocDelimiter(header, i)
			out = append(out, shellWriteHeredocDecl{delim: delim, quoted: quoted, tabs: shellAt(header, i+2) == '-', at: i})
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
		// The shell joins a backslash-newline before it matches the delimiter, and it does so only when the delimiter
		// is unquoted (a quoted delimiter makes the body literal). Comparing physical lines only let a terminator
		// written as `E\<newline>OF` close the here-document in the shell while the collector absorbed the following
		// interpreter program into the data body (CRW-765 correction 9).
		line := []uint16{}
		end := k
		for {
			nl := shellNewline(command, end)
			part := command[end:]
			if nl != -1 {
				part = command[end:nl]
			}
			continued := nl != -1 && !d.quoted && shellWriteHeredocLineContinued(part)
			if continued {
				part = part[:len(part)-1]
			}
			line = append(line, part...)
			end = nl
			if !continued || nl == -1 {
				break
			}
			end = nl + 1
		}
		// The shell joins the raw physical lines first and strips leading tabs from the joined line afterwards, which
		// is what makes a `<<-` terminator written across a continuation reachable (CRW-765 correction 9).
		if d.tabs {
			line = shellWriteHeredocTrimTabs(line)
		}
		if slices.Equal(line, d.delim) {
			if end == -1 {
				return body, len(command)
			}
			return body, end + 1
		}
		body = append(body, line...)
		body = append(body, '\n')
		if end == -1 {
			return body, len(command)
		}
		k = end + 1
	}
	return body, len(command)
}

// shellWriteHeredocLineContinued reports whether a physical body line ends in an unescaped backslash, which the shell
// reads as a line continuation before it matches the delimiter (CRW-765 correction 9). An even count of trailing
// backslashes is a literal backslash and continues nothing.
func shellWriteHeredocLineContinued(part []uint16) bool {
	n := 0
	for i := len(part) - 1; i >= 0 && part[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
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
		case '$':
			// An ANSI-C quoted word ($'...') is the word the shell spells out after it decodes the escapes, and a
			// locale-translated word ($"...") is the double-quoted word with the dollar removed. Reading the raw text
			// left the dollar in the delimiter, so the terminator never matched and the document swallowed the
			// interpreter program that followed it (CRW-765 correction 9, third pass, after the blind pre-merge
			// evaluation of head 05dc1a743).
			if shellAt(s, i+1) == '\'' {
				quoted = true
				i += 2
				for i < len(s) && s[i] != '\'' {
					if s[i] == '\\' {
						dec, n := shellWriteHeredocAnsiC(s, i)
						out = append(out, dec...)
						i = n
						continue
					}
					out = append(out, s[i])
					i++
				}
				if shellAt(s, i) == '\'' {
					i++
				}
				continue
			}
			if shellAt(s, i+1) == '"' {
				quoted = true
				word, end := shellWriteHeredocDoubleQuotedWord(s, i+2)
				out = append(out, word...)
				i = end
				continue
			}
			out = append(out, c)
			i++
		case '\'':
			// A single-quoted span is literal: no escape applies inside it.
			quoted = true
			i++
			for i < len(s) && s[i] != '\'' {
				out = append(out, s[i])
				i++
			}
			if shellAt(s, i) == '\'' {
				i++
			}
		case '"':
			// The shell removes a backslash inside double quotes only before $, `, " and \ (and before a newline,
			// which it removes with the backslash). Any other backslash is a literal character of the word, so the
			// delimiter is the word the shell spells out, not the raw text (CRW-765 correction 9).
			quoted = true
			word, end := shellWriteHeredocDoubleQuotedWord(s, i+1)
			out = append(out, word...)
			i = end
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

// shellWriteHeredocDoubleQuoteEscape reports whether the shell removes a backslash that precedes c inside a
// double-quoted word: it lets a backslash escape only $, `, " and \ there, and a backslash-newline is a line
// continuation the shell removes with both characters (CRW-765 correction 9).
func shellWriteHeredocDoubleQuoteEscape(c uint16) bool {
	switch c {
	case '$', '\x60', '"', '\\', '\n':
		return true
	}
	return false
}

// shellWriteHeredocDoubleQuotedWord reads the body of a double-quoted word that begins at i (the character after the
// opening quote) and returns the word the shell spells out together with the offset after the closing quote. The shell
// removes a backslash only before $, a backtick, " and \ there, and it removes a backslash-newline with both
// characters; any other backslash is a literal character of the word (CRW-765 correction 9).
func shellWriteHeredocDoubleQuotedWord(s []uint16, i int) (word []uint16, next int) {
	word = []uint16{}
	for i < len(s) && s[i] != '"' {
		if s[i] == '\\' && i+1 < len(s) && shellWriteHeredocDoubleQuoteEscape(s[i+1]) {
			if s[i+1] != '\n' {
				word = append(word, s[i+1])
			}
			i += 2
			continue
		}
		word = append(word, s[i])
		i++
	}
	if shellAt(s, i) == '"' {
		i++
	}
	return word, i
}

// shellWriteHeredocAnsiC reads one backslash escape of an ANSI-C quoted word ($'...') at i, where s[i] is the
// backslash, and returns the character the shell produces with the offset after the escape. The shell decodes the
// escapes a C string literal accepts; the common ones a delimiter word may hold are decoded, and an escape this reader
// does not decode keeps the character after the backslash, as the shell does for an unknown escape.
func shellWriteHeredocAnsiC(s []uint16, i int) (out []uint16, next int) {
	if i+1 >= len(s) {
		return []uint16{'\\'}, i + 1
	}
	switch c := s[i+1]; c {
	case 'a':
		return []uint16{0x07}, i + 2
	case 'b':
		return []uint16{'\b'}, i + 2
	case 'e', 'E':
		return []uint16{0x1b}, i + 2
	case 'f':
		return []uint16{'\f'}, i + 2
	case 'n':
		return []uint16{'\n'}, i + 2
	case 'r':
		return []uint16{'\r'}, i + 2
	case 't':
		return []uint16{'\t'}, i + 2
	case 'v':
		return []uint16{'\v'}, i + 2
	case '\\', '\'', '"', '?':
		return []uint16{c}, i + 2
	case 'x':
		j, v := i+2, uint16(0)
		for j < len(s) && shellWriteHeredocHexDigit(s[j]) && j < i+2+2 {
			v = v*16 + shellWriteHeredocHexValue(s[j])
			j++
		}
		if j == i+2 {
			return []uint16{'\\', 'x'}, i + 2 // \x with no digit: the shell keeps the backslash and the x
		}
		return []uint16{v}, j
	case 'u', 'U':
		// The shell decodes a Unicode escape: \u takes one to four hex digits and \U takes one to eight. A word
		// that builds a delimiter out of one is a word the reader must read the same way, or the terminator it
		// expects is not the shell's and the following program is absorbed as data (CRW-765 correction 9).
		width := 4
		if c == 'U' {
			width = 8
		}
		j, v := i+2, uint32(0)
		for j < len(s) && shellWriteHeredocHexDigit(s[j]) && j < i+2+width {
			v = v*16 + uint32(shellWriteHeredocHexValue(s[j]))
			j++
		}
		if j == i+2 {
			return []uint16{'\\', c}, i + 2
		}
		return utf16.Encode([]rune{rune(v)}), j
	case 'c':
		// \cX is a control character: the shell takes X's uppercase value with the top bits cleared.
		if i+2 >= len(s) {
			return []uint16{'\\', 'c'}, i + 2
		}
		return []uint16{(s[i+2] &^ 0x20) ^ 0x40}, i + 3
	case '0', '1', '2', '3', '4', '5', '6', '7':
		j, v := i+1, uint16(0)
		for j < len(s) && s[j] >= '0' && s[j] <= '7' && j < i+1+3 {
			v = v*8 + (s[j] - '0')
			j++
		}
		return []uint16{v}, j
	}
	// An escape the shell does not decode keeps its backslash: the word is the two characters, not the second one.
	return []uint16{'\\', s[i+1]}, i + 2
}

// shellWriteHeredocHexDigit reports whether c is a hexadecimal digit.
func shellWriteHeredocHexDigit(c uint16) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// shellWriteHeredocHexValue is the value of a hexadecimal digit c.
func shellWriteHeredocHexValue(c uint16) uint16 {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	}
	return c - 'A' + 10
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

// shellWriteHeredocVerbExpanded reports whether the verb of the command a here-document is attached to still holds a
// shell expansion once its quotes and backslashes are removed (CRW-765 correction 5, rule R0). A verb the reader cannot
// spell out may run any program, an interpreter among them, so the here-document is denied. The scan stops at the first
// command word: a redirection, a here-document operator and its delimiter word are skipped, so `2>&1 python3 -` and
// `<<'PY' python3` both read `python3`.
func shellWriteHeredocVerbExpanded(line []uint16) bool {
	for i := 0; i < len(line); {
		if shellSpace(line[i]) {
			i++
			continue
		}
		kind, next := shellWriteHeredocOperator(line, i)
		if kind == shellWriteHeredocOpInvalid {
			return false // an operator shape the reader does not model is decided by the header proof instead
		}
		if kind != shellWriteHeredocOpNone {
			i = next
			continue
		}
		_, _, literal := shellWriteHeredocLiteralWord(line, i)
		return !literal
	}
	return false
}

// shellWriteHeredocNeverReadsStdin is the reader's data list (CRW-765 corrections 5 and 7, rules R1 and C1): the
// programs whose standard input is never executed, so a here-document they are attached to is data and its body is not
// read. The verb is compared by its last path element and case-insensitively, which shellVerbName has already applied.
func shellWriteHeredocNeverReadsStdin(verb string) bool {
	switch verb {
	case "cat", "tee", "head", "tail", "wc", "grep", "egrep", "fgrep", "sort", "uniq", "cut", "tr", "jq", "base64", "read":
		return true
	}
	return false
}

// shellWriteHeredocGitCommitStdin reports whether a git command takes its commit message from standard input (CRW-765
// correction 7, rule C1): `git commit -F -`, `--file=-`, `--file -` and their attached forms. The message is data, so a
// here-document that carries it is not read as a program; every other git invocation falls to rule C3.
func shellWriteHeredocGitCommitStdin(args []string) bool {
	if len(args) == 0 || shellVerbName(args[0]) != "commit" {
		return false
	}
	for i := 1; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-F" || a == "--file":
			// The next word names the message source; - is standard input.
			if i+1 < len(args) && args[i+1] == "-" {
				return true
			}
		case a == "-F-" || a == "--file=-":
			return true
		case strings.HasPrefix(a, "-F") && len(a) > 2:
			return true
		case strings.HasPrefix(a, "--file="):
			return true
		}
	}
	return false
}

// shellWriteHeredocSedReadsScript reports whether a sed command reads its script from a file (CRW-765 correction 7's sed
// exception, rewritten by correction 8, rule S8). Every option word is read after the CRW-783 normalisation - its
// quotes and backslashes removed - so `-\f` and `-n'f'` read as the bundles they are; an expansion left in a word
// denies, because the shell could build a -f out of it. A short bundle is read letter by letter, so any f denies; a long
// option is read the way GNU getopt reads it, by prefix, so a name before = that is a prefix of file (--f, --fi, --fil,
// --file) denies; nothing after -- is an option. With one of those the script is the here-document body, which the
// reader cannot read, so the gate refuses it; without one the here-document is input data.
func shellWriteHeredocSedReadsScript(args []string) bool {
	afterDoubleDash := false
	for _, a := range args {
		if afterDoubleDash {
			continue // nothing after -- is an option
		}
		if a == "--" {
			afterDoubleDash = true
			continue
		}
		n, expanded := shellWriteHeredocNormalizeWord(a)
		if !strings.HasPrefix(n, "-") || n == "-" {
			continue // not an option word: sed's script operand and its input files
		}
		if expanded {
			return true // an expansion the reader cannot read may build a -f
		}
		switch {
		case strings.HasPrefix(n, "--"):
			name := strings.TrimPrefix(n, "--")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name = name[:eq]
			}
			if name != "" && strings.HasPrefix("file", name) {
				return true
			}
		case len(n) > 1 && n[0] == '-':
			// A short bundle is read letter by letter, from the word with its case kept: -E is the no-argument
			// extended-regexp option while -e takes the script as its argument, and the normalisation above lowers
			// the letters, so it cannot tell them apart. The letters e, i and l take an argument, so the rest of the
			// word is that argument and holds no option: the f in -e's/foo/bar/' is the script, not a -f, and reading
			// it as one refused an ordinary filter (CRW-765 correction 9, after the blind pre-merge evaluation of
			// head a9ca76947).
			raw, _ := shellWriteHeredocStripQuotes(a)
			if len(raw) < 2 || raw[0] != '-' {
				continue
			}
			for _, letter := range raw[1:] {
				if letter == 'f' {
					return true
				}
				if letter == 'e' || letter == 'i' || letter == 'l' {
					break
				}
			}
		}
	}
	return false
}

// shellWriteHeredocStripQuotes removes a word's quote characters and backslashes the way the shell does before the
// program sees the word, and keeps its case: -\f reads as -f and -n'p' as -np, while -E stays -E. It is rule S8's
// reader for the letters of a short option bundle, which the lowercase normalisation cannot classify.
func shellWriteHeredocStripQuotes(word string) (stripped string, ok bool) {
	out := make([]byte, 0, len(word))
	var quote byte
	for i := 0; i < len(word); i++ {
		c := word[i]
		if quote == 0 {
			switch c {
			case '\'', '"':
				quote = c
				continue
			case '\\':
				if i+1 < len(word) && word[i+1] == '\n' {
					i++
					continue
				}
				i++
				if i >= len(word) {
					return "", false
				}
				out = append(out, word[i])
				continue
			}
			out = append(out, c)
			continue
		}
		if c == quote {
			quote = 0
			continue
		}
		if quote == '"' && c == '\\' {
			if i+1 < len(word) && shellWriteHeredocDoubleQuoteEscape(uint16(word[i+1])) {
				if word[i+1] != '\n' {
					out = append(out, word[i+1])
				}
				i++
				continue
			}
		}
		out = append(out, c)
	}
	if quote != 0 {
		return string(out), false
	}
	return string(out), true
}

// shellWriteHeredocNormalizeWord applies the CRW-783 normalisation to one word (rule S8): its quote characters and
// backslashes are removed, as the shell removes them before the program sees the word, so -\f reads as -f and -n'p' as
// -np. A backslash-newline is a line continuation and both characters go; the character a backslash escapes stays.
// expanded reports that an expansion (a dollar sign, a backtick, a substitution, a brace or a glob) is left, which the
// reader cannot resolve.
func shellWriteHeredocNormalizeWord(word string) (normalized string, expanded bool) {
	out := make([]byte, 0, len(word))
	for i := 0; i < len(word); i++ {
		switch c := word[i]; c {
		case '\\':
			if i+1 < len(word) && word[i+1] == '\n' {
				i++ // a backslash-newline is a line continuation: both characters go
			}
		case '\'', '"':
			// removed: quoting does not hide the option
		case '$', '`', '(', ')', '{', '}', '*', '?', '[', ']':
			expanded = true
			out = append(out, c)
		default:
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			out = append(out, c)
		}
	}
	return string(out), expanded
}

// shellWriteHeredocCuts splits one physical line into its commands at ;, &&, ||, & and |, outside quotes (CRW-765
// correction 8, rule U1). ok is false when the line cannot be split: an unbalanced quote, or a backslash anywhere on
// the line, which the shell would use to escape or continue before the reader sees the command (rule U3).
func shellWriteHeredocCuts(line []uint16) (cuts [][2]int, ok bool) {
	start := 0
	for i := 0; i < len(line); {
		c := line[i]
		if c == '#' && shellWriteHeredocCommentStart(line, i) {
			// A word-initial # begins a comment, so the rest of the physical line is inert and belongs to no command:
			// an apostrophe or a quote inside it is a literal character, not the unbalanced quote that made this
			// splitter refuse an ordinary data here-document (CRW-765 correction 9).
			break
		}
		if c == '\'' || c == '"' {
			next := skipQuoted(line, i)
			if next <= i || next > len(line) || line[next-1] != c {
				return nil, false // an unbalanced quote: the line cannot be split
			}
			i = next
			continue
		}
		if c == '\\' {
			return nil, false // a backslash escapes or continues: the line is not readable
		}
		if shellWriteHeredocSeparator(line, i) {
			j := i + 1
			if (c == '&' || c == '|') && shellAt(line, j) == c {
				j++ // && and || are one operator
			}
			cuts = append(cuts, [2]int{start, i})
			start, i = j, j
			continue
		}
		i++
	}
	return append(cuts, [2]int{start, len(line)}), true
}

// shellWriteHeredocOwningCommand is the command the here-document is attached to: the cut of the header line that holds
// the << operator (CRW-765 correction 8, rule U1). ok is false when the line cannot be split or holds no such cut, which
// makes the header undecidable (rule U3).
func shellWriteHeredocOwningCommand(h shellWriteHeredoc) ([]uint16, bool) {
	cuts, ok := shellWriteHeredocCuts(h.command)
	if !ok {
		return nil, false
	}
	for _, c := range cuts {
		if c[0] <= h.at && h.at < c[1] {
			return h.command[c[0]:c[1]], true
		}
	}
	return nil, false
}

// shellWriteHeredocLoopLine reports whether a header line is a while or until loop, which rule U2 judges by its
// condition and body rather than by one command.
func shellWriteHeredocLoopLine(line string) bool {
	head := strings.TrimLeft(line, " \t")
	for _, keyword := range []string{"while", "until"} {
		rest, ok := strings.CutPrefix(head, keyword)
		if !ok {
			continue
		}
		if rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '\n' || rest[0] == ';' {
			return true
		}
	}
	return false
}

// shellWriteHeredocLoopWord reports whether a word is one of the words a while or until loop's own syntax uses, which
// rule U2 skips before it reads the command the loop runs.
func shellWriteHeredocLoopWord(word string) bool {
	switch shellVerbName(word) {
	case "while", "until", "do", "done", "{", "}", "!", "time":
		return true
	}
	return false
}

// shellWriteHeredocLoopVerb is rule U2's list: the commands a while or until loop may run while it takes the
// here-document. It is rule C1's data verbs and read, plus the short list of programs that never read standard input as
// a program. Anything else makes the loop's here-document a program the reader cannot read.
func shellWriteHeredocLoopVerb(verb string) bool {
	switch verb {
	case "echo", "printf", "true", "false", ":", "test", "[", "break", "continue":
		return true
	}
	return shellWriteHeredocNeverReadsStdin(verb)
}

// shellWriteHeredocLoopSafe reports whether every command of a while or until loop that takes the here-document is one
// rule U2 allows, with every word readable (rule U2). The loop's own syntax words are skipped; any other reserved word,
// an unreadable command, a command substitution that could run a program on the here-document, or a verb off the list
// denies.
func shellWriteHeredocLoopSafe(line []uint16) bool {
	cuts, ok := shellWriteHeredocCuts(line)
	if !ok {
		return false
	}
	for _, c := range cuts {
		seg := line[c[0]:c[1]]
		words, ok := shellWriteHeredocSegmentWords(seg, false)
		if !ok {
			return false
		}
		for _, w := range words {
			if strings.ContainsAny(w, "`") || strings.Contains(w, "$(") {
				return false // a substitution runs a program, whose standard input is the here-document
			}
		}
		rest := words
		for len(rest) > 0 && (shellWriteHeredocLoopWord(rest[0]) || shellVerbAssignment(rest[0])) {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			continue // the loop's own syntax alone, such as `while` or `done`
		}
		if !shellWriteHeredocLoopVerb(shellVerbName(rest[0])) {
			return false
		}
	}
	return true
}

// shellWriteHeredocHeaderProven reports whether a here-document's header is a line the reader can take apart (CRW-765
// corrections 4 and 8, rules G1, G3 and U1). The line must not be a continuation of the line before it, it must hold no
// backslash (which the shell would use to escape or continue before the reader sees the command) and it must define no
// function in any form; the commands on it are separated at the control operators, which rule U1 then judges one by one.
func shellWriteHeredocHeaderProven(h shellWriteHeredoc) bool {
	if h.joined {
		return false
	}
	// A word-initial # makes the rest of the physical line inert, so a backslash or a function example written in a
	// comment is neither a continuation nor a definition. Reading the comment as syntax refused an ordinary
	// documentation command (CRW-765 correction 9, third pass, after the blind pre-merge evaluation of head
	// 05dc1a743).
	line := shellWriteHeredocBlankComments(h.command)
	for i := 0; i < len(line); {
		c := line[i]
		if c == '\\' {
			return false
		}
		if c == '\'' || c == '"' {
			i = skipQuoted(line, i)
			continue
		}
		i++
	}
	return !shellWriteHeredocDefinesFunction(line)
}

// shellWriteHeredocDefinesFunction reports whether a header line defines a function in any form (CRW-765 correction 4, rule G3): name(), function name and function name() all make the header unprovable.
func shellWriteHeredocDefinesFunction(header []uint16) bool {
	return len(shellWriteHeredocFunctionNames(header)) > 0
}

// shellWriteHeredocHiddenOperator reports whether the command holds a here-document operator the collector cannot reach: a << inside a $( ... ) or backtick command substitution that a double-quoted word encloses (CRW-765 correction 4, rule G4). The shell reads that operator as a here-document, so a command holding one is an unprovable header and fails closed when the command text names an interpreter.
func shellWriteHeredocHiddenOperator(command []uint16) bool {
	for i := 0; i < len(command); {
		if command[i] == '#' && shellWriteHeredocCommentStart(command, i) {
			// A word-initial # begins a comment, so the rest of its physical line is inert: a quoted command
			// substitution written inside it is documentation, not a here-document the shell would read (CRW-765
			// correction 9). The comment ends at its newline and nothing beyond it, so the scan continues on the next
			// line; ending the whole search here let a here-document on a later line pass unseen (correction 9, third
			// pass, after the blind pre-merge evaluation of head 05dc1a743).
			nl := shellNewline(command, i)
			if nl == -1 {
				return false
			}
			i = nl + 1
			continue
		}
		if command[i] == '\'' {
			i = skipQuoted(command, i)
			continue
		}
		if command[i] == '"' {
			end := skipQuoted(command, i)
			if shellWriteHeredocSubstitutionOperator(command[i:end]) {
				return true
			}
			i = end
			continue
		}
		i++
	}
	return false
}

// shellWriteHeredocSubstitutionOperator reports whether a double-quoted span holds a << inside one of its $( ... ) or backtick command substitutions. A backslash escapes the character after it inside double quotes, so an escaped character is not read as part of a substitution.
func shellWriteHeredocSubstitutionOperator(span []uint16) bool {
	depth, tick := 0, false
	for i := 0; i < len(span); i++ {
		c := span[i]
		if c == '\\' {
			i++
			continue
		}
		if c == '$' && shellAt(span, i+1) == '(' {
			depth++
			i++
			continue
		}
		if tick {
			if c == '\x60' {
				tick = false
			} else if c == '<' && shellAt(span, i+1) == '<' && shellAt(span, i+2) != '<' {
				return true
			}
			continue
		}
		if c == '\x60' {
			tick = true
			continue
		}
		if depth > 0 {
			switch {
			case c == '(':
				depth++
			case c == ')':
				depth--
			case c == '<' && shellAt(span, i+1) == '<' && shellAt(span, i+2) != '<':
				return true
			}
		}
	}
	return false
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
	return shellWriteHeredocSegmentWords(seg, true)
}

// shellWriteHeredocSegmentWords reads one command segment. ok is false when the segment holds a construct the reader
// cannot follow: an operator shape the closed set does not allow, a redirection target that is not literal, a word it
// cannot end, or - when requireLiteral is set - a word holding an expansion, a substitution, a brace, a glob or a comment
// marker. With requireLiteral unset the words are read for their text only, which rule U2's loop check needs.
func shellWriteHeredocSegmentWords(seg []uint16, requireLiteral bool) (words []string, ok bool) {
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
		if len(w) == 0 || n <= i || requireLiteral && !lit {
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
