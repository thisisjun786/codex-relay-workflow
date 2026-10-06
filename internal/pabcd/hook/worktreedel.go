package hook

import (
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// This file is the Go form of the deletion guard of CXC v0.2.40 pabcd-state/src/worktree-guard.ts (186-291, 347-447
// without the Windows branch at 404-416, 448-498 and 568-594): the PreToolUse hook that denies a shell command which
// deletes this session's own Codex-app-managed worktree (WORKTREE-GUARD-03). Detection is worktree.go. The guard reads
// text and runs nothing: it is a lexical early warning, and what it cannot see is listed in docs/port-cxc/known-defects.md.
//
// Parity first: the first walk over a command is the oracle's, byte for byte. Only POSIX removal verbs are ported (the
// rework decision leaves out the Windows ones: PS_REMOVE_VERBS, DIR_REMOVE_VERBS, PS_VALUE_PARAMS, parseWindowsRemoval).
// The extended walk is the one change, and it only adds denies (security class, port: fixed): it runs when the first walk
// allows, over a grammar that also cuts at a newline, a single ampersand and braces, scopes parentheses, joins a continued
// line, widens the lead set of the destructive hint, and counts the filesystem root as an ancestor of the cwd. It then judges
// every reading a second time over bash's own reading of quotes, backslashes and comments (CRW-611).

// GuardVerdict is the oracle's allow | deny with a reason; the zero value is allow.
type GuardVerdict struct {
	Deny   bool
	Reason string
}

// splitSegments cuts a shell command at &&, ||, ; and | outside quotes (no escapes are known), drops the empty pieces and
// trims the rest. The extended grammar also cuts at a single ampersand, a newline and a brace that stands alone as a word
// (a group, not .{a,b} or ${X}), keeps ( and ) as segments of their own so that the walk can scope a
// subshell.
func splitSegments(command string, extended bool) []string {
	var segments []string
	var cur []byte
	var quote byte
	edge := func(i int) bool {
		return i < 0 || i >= len(command) || strings.IndexByte(" \t\r\n;&|(){}", command[i]) >= 0
	}
	cut := func() {
		if s := text.Trim(string(cur)); s != "" {
			segments = append(segments, s)
		}
		cur = cur[:0]
	}
	for i := 0; i < len(command); i++ {
		ch := command[i]
		next := byte(0)
		if i+1 < len(command) {
			next = command[i+1]
		}
		switch {
		case quote != 0:
			cur = append(cur, ch)
			if ch == quote {
				quote = 0
			}
		case ch == '\'' || ch == '"':
			quote = ch
			cur = append(cur, ch)
		case ch == ';' || ch == '|' || (ch == '&' && next == '&'):
			cut()
			if ch == '&' || (ch == '|' && next == '|') {
				i++
			}
		case extended && (ch == '&' || ch == '\n' || (ch == '{' || ch == '}') && edge(i-1) && edge(i+1)):
			cut()
		case extended && (ch == '(' || ch == ')'):
			cut()
			segments = append(segments, string(ch))
		default:
			cur = append(cur, ch)
		}
	}
	cut()
	return segments
}

// tokenize is the oracle's minimal quote-aware tokenizer: whitespace (JavaScript's) separates, quotes group and vanish,
// an empty quoted pair is a token.
func tokenize(segment string) []string {
	var tokens []string
	var cur strings.Builder
	var quote rune
	has := false
	for _, ch := range segment {
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else {
				cur.WriteRune(ch)
			}
			has = true
		case ch == '\'' || ch == '"':
			quote, has = ch, true
		case text.Trim(string(ch)) == "":
			if has {
				tokens = append(tokens, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(ch)
			has = true
		}
	}
	if has {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// basename is what follows the last slash once every backslash is a slash.
func basename(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return p[strings.LastIndexByte(p, '/')+1:]
}

// isAssignment is /^[A-Za-z_][A-Za-z0-9_]*=/.
func isAssignment(token string) bool {
	name, _, found := strings.Cut(token, "=")
	if !found || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// stripPrefixes drops leading sudo, command and builtin, and env with the NAME=value words that follow it. A prefix is
// recognised by its basename; sudo's and env's own options are not understood (known defect).
func stripPrefixes(tokens []string) []string {
	for len(tokens) > 0 {
		switch basename(tokens[0]) {
		case "sudo", "command", "builtin":
			tokens = tokens[1:]
		case "env":
			tokens = tokens[1:]
			for len(tokens) > 0 && isAssignment(tokens[0]) {
				tokens = tokens[1:]
			}
		default:
			return tokens
		}
	}
	return tokens
}

// resolveFrom is path.resolve(base, target) for two segments: an empty target is the base, an absolute one stands alone, a
// relative base is made absolute from the process's working directory, and the result is cleaned.
func resolveFrom(base, target string) string {
	abs := func(p string) string {
		if a, err := filepath.Abs(p); err == nil {
			return a
		}
		return p
	}
	switch {
	case target == "":
		return abs(base)
	case filepath.IsAbs(target):
		return filepath.Clean(target)
	}
	return filepath.Join(abs(base), target)
}

// isProtectedTarget says whether deleting target (taken from segCwd) deletes the slot, the checkout, the directory the
// segment runs in, or an ancestor of that directory. The oracle compares against the segment's own directory, so a cd to
// an unrelated place moves what is protected along with it (known defect). Its ancestor test is cwd.startsWith(resolved
// + sep), which never holds for the root; the extended walk fixes that.
func isProtectedTarget(target, segCwd string, id WorktreeIdentity, extended bool) bool {
	resolved := canonicalize(resolveFrom(segCwd, target))
	cwd := canonicalize(segCwd)
	if resolved == id.SlotRoot && id.SlotRoot != "" || resolved == id.CheckoutRoot && id.CheckoutRoot != "" || resolved == cwd {
		return true
	}
	prefix := resolved + "/"
	if extended {
		prefix = strings.TrimSuffix(resolved, "/") + "/"
	}
	return strings.HasPrefix(cwd, prefix)
}

// evaluateSegment is the verdict of one segment, and false when it has none. rm and rmdir are the POSIX removals, git
// worktree remove the other; unlink is a file removal and never threatens a worktree.
func evaluateSegment(segment, cwd string, id WorktreeIdentity, extended, quoting bool) (GuardVerdict, bool) {
	tokens := stripPrefixes(worktreeDelQuoteTokens(segment, quoting))
	if len(tokens) == 0 {
		return GuardVerdict{}, false
	}
	deny := func(what string) (GuardVerdict, bool) {
		return GuardVerdict{Deny: true, Reason: denyReason(what, id)}, true
	}
	switch basename(tokens[0]) {
	case "rm":
		recursive, flagsDone := false, false
		var targets []string
		for _, tok := range tokens[1:] {
			switch {
			case !flagsDone && tok == "--":
				flagsDone = true
			case !flagsDone && strings.HasPrefix(tok, "--"):
				recursive = recursive || tok == "--recursive" // --force and the other long flags carry no targets
			case !flagsDone && strings.HasPrefix(tok, "-") && len(tok) > 1:
				recursive = recursive || strings.ContainsAny(tok, "rR")
			default:
				targets = append(targets, tok)
			}
		}
		if recursive { // plain file removal cannot delete the worktree
			for _, target := range targets {
				if isProtectedTarget(target, cwd, id, extended) {
					return deny("rm -r " + target)
				}
			}
		}
	case "rmdir":
		for _, tok := range tokens[1:] {
			if !strings.HasPrefix(tok, "-") && isProtectedTarget(tok, cwd, id, extended) {
				return deny("rmdir " + tok)
			}
		}
	case "git":
		segCwd := cwd
		var rest []string
		for args, i := tokens[1:], 0; i < len(args); i++ {
			switch {
			case args[i] == "-C" && i+1 < len(args):
				segCwd = resolveFrom(segCwd, args[i+1])
				i++
			case args[i] == "-c" && i+1 < len(args):
				i++
			default:
				rest = append(rest, args[i])
			}
		}
		if len(rest) > 1 && rest[0] == "worktree" && rest[1] == "remove" {
			target := ""
			for _, t := range rest[2:] {
				if !strings.HasPrefix(t, "-") {
					target = t
					break
				}
			}
			if target != "" && isProtectedTarget(target, segCwd, id, extended) {
				return deny("git worktree remove " + target)
			}
		}
	}
	return GuardVerdict{}, false
}

// destructiveHint is DESTRUCTIVE_HINT: a removal verb that opens a word, or git worktree remove. The oracle's /i is not
// unicode, so only ASCII folds: the pattern is lower case and the segment goes through foldASCII. The windows words stay,
// for the fallback. jsSpaceChars is JavaScript's \s; the pattern is built here at the call, never at package level. The
// extended walk also accepts a quote, a parenthesis, a brace or a backtick before the verb.
func destructiveHint(extended bool) *regexp.Regexp {
	lead := "/"
	if extended {
		lead = "/\"'(){}`"
	}
	return regexp.MustCompile("(^|[" + jsSpaceChars + lead + "])(rm|rmdir|rd|del|erase|ri|remove-item)\\b" +
		"|git[" + jsSpaceChars + "]+(-c[" + jsSpaceChars + "]+[^" + jsSpaceChars + "]+[" + jsSpaceChars + "]+)?worktree[" + jsSpaceChars + "]+remove")
}

// The five states in which bash reads a command (worktreeDelQuoteReader).
const (
	worktreeDelQuotePlain = iota
	worktreeDelQuoteSingle
	worktreeDelQuoteANSIC // $'...'
	worktreeDelQuoteDouble
	worktreeDelQuoteComment
)

// worktreeDelQuoteReader reads a command the way bash does, one byte or one escaped pair at a time, in five states: plain;
// single quotes, which keep everything; $'...', where a backslash escapes the next byte, so \' does not end it; double
// quotes, where a backslash pair stays together, so \" does not close them; and a comment, which a # that opens a word
// starts and its newline ends. A word opens at the start and after a blank, tab, newline, ; & | ( or ). Outside single
// quotes and comments a backslash makes the next byte literal (escapes, pair). prev is the last byte that decides the
// next one: a space at the start, the byte itself after a plain byte, a backslash after an escaped pair, a quote after it
// closes, a newline after a comment, and x for the second dollar of a pair ($$ is the process id, so only an odd run of
// dollars opens $'...'). Here-document bodies, substitutions, backtick bodies and parameter expansions are read as the same
// flat text: a quote nested in them is not tracked.
type worktreeDelQuoteReader struct {
	state int
	prev  byte
	// brace keeps a # from opening a comment while a ${...} parameter expansion is read: bash opens a comment only at the
	// start of a word in plain text, and a # inside the expansion is data (CRW-726, c13).
	brace bool
}

// escapes says whether the byte at i is a backslash that escapes the byte after it.
func (r *worktreeDelQuoteReader) escapes(command string, i int) bool {
	return command[i] == '\\' && i+1 < len(command) && r.state != worktreeDelQuoteSingle && r.state != worktreeDelQuoteComment
}

// pair records that a backslash and the byte it escapes were read.
func (r *worktreeDelQuoteReader) pair() {
	if r.state == worktreeDelQuotePlain {
		r.prev = '\\'
	}
}

// step reads a byte that is not an escaping backslash.
func (r *worktreeDelQuoteReader) step(c byte) {
	switch r.state {
	case worktreeDelQuotePlain:
		switch {
		case c == '\'':
			r.state = worktreeDelQuoteSingle
			if r.prev == '$' {
				r.state = worktreeDelQuoteANSIC
			}
		case c == '"':
			r.state = worktreeDelQuoteDouble
		case c == '#' && !r.brace && strings.IndexByte(" \t\n;&|()", r.prev) >= 0:
			r.state = worktreeDelQuoteComment
		case c == '$' && r.prev == '$':
			r.prev = 'x'
		default:
			r.prev = c
		}
	case worktreeDelQuoteSingle, worktreeDelQuoteANSIC:
		if c == '\'' {
			r.state, r.prev = worktreeDelQuotePlain, c
		}
	case worktreeDelQuoteDouble:
		if c == '"' {
			r.state, r.prev = worktreeDelQuotePlain, c
		}
	case worktreeDelQuoteComment:
		if c == '\n' {
			r.state, r.prev = worktreeDelQuotePlain, c
		}
	}
}

// worktreeDelQuoteSegments is splitSegments over bash's own reading of quotes, backslashes and comments. It cuts a command at ; | & and a newline in plain state,
// at a brace that stands alone as a word (a group; but a brace after a removal verb is an argument, as in rm -rf { x, and
// .{a,b} and ${X} are words) and at a parenthesis, and not at the ampersand of a redirection, which it keeps as a segment
// of its own so that the walk can scope a subshell. A backslash and the byte it escapes stay in the segment and never cut,
// quotes protect everything in them, and a comment is dropped up to its newline, which cuts. A backtick opens a command
// where it starts a pair, so a # right after it is a comment; the one after a closing backtick is not.
func worktreeDelQuoteSegments(command string) []string {
	var segments []string
	var cur []byte
	r := worktreeDelQuoteReader{prev: ' '}
	opened := false // the number of backticks read in plain state is odd
	edge := func(b byte) bool { return strings.IndexByte(" \t\r\n;&|(){}", b) >= 0 }
	cut := func() {
		if s := text.Trim(string(cur)); s != "" {
			segments = append(segments, s)
		}
		cur = cur[:0]
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if r.escapes(command, i) {
			cur = append(cur, c, command[i+1])
			i++
			r.pair()
			continue
		}
		if r.state == worktreeDelQuoteDouble && (c == '`' || c == '$' && i+1 < len(command) && command[i+1] == '(') {
			// an outer double quote keeps a substitution together: a quote, a separator or a parenthesis inside it is not this shell's
			skip := 0
			if c == '`' {
				if _, n, closed := worktreeDelSubstitutionBody(command[i+1:], true); closed {
					skip = 1 + n
				}
			} else if _, n, closed := worktreeDelSubstitutionBody(command[i+2:], false); closed {
				skip = 2 + n
			}
			if skip > 0 {
				cur = append(cur, command[i:i+skip]...)
				i += skip - 1
				r.prev = 'x'
				continue
			} // an unclosed body is read plainly below, so the ordinary grammar still cuts at a separator inside it
		}
		if r.state == worktreeDelQuoteComment && opened && c == '`' && worktreeDelQuoteBackslashes(command[:i])%2 == 0 {
			r.state, r.prev, opened = worktreeDelQuotePlain, '`', false // a comment in a backtick body ends at the closing backtick
			cur = append(cur, c)
			continue
		}
		state, prev := r.state, r.prev
		r.step(c)
		switch {
		case state == worktreeDelQuoteComment || r.state == worktreeDelQuoteComment:
			if c == '\n' {
				cut()
			}
		case state != worktreeDelQuotePlain || r.state != worktreeDelQuotePlain:
			cur = append(cur, c)
		case c == '&' && (prev == '>' || prev == '<' || i+1 < len(command) && command[i+1] == '>'):
			cur = append(cur, c) // part of a redirection (2>&1, >&2, &>f), not the end of a command
		case c == ';' || c == '|' || c == '&' || c == '\n' || (c == '{' || c == '}') && edge(prev) && (i+1 == len(command) || edge(command[i+1])) && !worktreeDelQuoteRemoves(cur):
			cut()
		case c == '(' || c == ')':
			cut()
			segments = append(segments, string(c))
		default:
			cur = append(cur, c)
			if c == '`' {
				if opened = !opened; opened {
					r.prev = ' '
				}
			}
		}
	}
	cut()
	return segments
}

// worktreeDelQuoteRemoves says whether the segment so far is a removal command (rm, rmdir or git, after prefixes such as sudo),
// where a standalone brace is an argument: a cut there would hide the targets from their verb. Elsewhere a brace may start a
// group (then, do, function f, time -p, coproc, a backtick) and cuts, as it always did.
func worktreeDelQuoteRemoves(cur []byte) bool {
	tokens := stripPrefixes(worktreeDelQuoteTokenize(string(cur)))
	if len(tokens) == 0 {
		return false
	}
	switch basename(tokens[0]) {
	case "rm", "rmdir", "git":
		return true
	}
	return false
}

// worktreeDelQuoteBackslashes is the length of the run of backslashes that ends s; an odd run escapes the byte after it.
func worktreeDelQuoteBackslashes(s string) int {
	n := 0
	for n < len(s) && s[len(s)-1-n] == '\\' {
		n++
	}
	return n
}

// worktreeDelQuoteTokens is the words of a segment: the oracle's tokenizer, or bash's quote removal when quoting.
func worktreeDelQuoteTokens(segment string, quoting bool) []string {
	if quoting {
		return worktreeDelQuoteTokenize(segment)
	}
	return tokenize(segment)
}

// worktreeDelQuoteTokenize is tokenize over bash's own reading: whitespace (JavaScript's, in plain state) separates, a < or > in
// plain state ends the word before it (rm -rf x>f), and quote
// removal is bash's. A quote opens and closes and vanishes, together with the dollar of a $'...' or $"..." (an empty quoted
// pair is a token); a backslash outside single quotes makes the next byte literal and vanishes (worktreeDelQuoteEscape).
// After a NUL that an escape in $'...' makes, the rest of that string is dropped as bash drops it.
func worktreeDelQuoteTokenize(segment string) []string {
	var tokens []string
	var cur []byte
	r := worktreeDelQuoteReader{prev: ' '}
	has, nul := false, false
	flush := func() {
		if has {
			tokens = append(tokens, string(cur))
			cur, has = cur[:0], false
		}
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if r.escapes(segment, i) {
			out, n := worktreeDelQuoteEscape(r.state, segment[i+1:])
			if r.state == worktreeDelQuoteANSIC && slices.Contains(out, 0) {
				nul = true
			}
			if !nul {
				cur = append(cur, out...)
			}
			i += n
			has = has || len(out) > 0
			r.pair()
			continue
		}
		state, prev := r.state, r.prev
		r.step(c)
		switch {
		case state == worktreeDelQuoteComment || r.state == worktreeDelQuoteComment:
		case state == worktreeDelQuotePlain && r.state != worktreeDelQuotePlain:
			if prev == '$' && len(cur) > 0 {
				cur = cur[:len(cur)-1]
			}
			has = true
		case state != worktreeDelQuotePlain && r.state == worktreeDelQuotePlain:
			nul = false
		case state != worktreeDelQuotePlain:
			if !nul {
				cur = append(cur, c)
			}
		default:
			if w := worktreeDelQuoteSpace(segment[i:]); w > 0 {
				flush()
				i += w - 1
				break
			}
			if (c == '<' || c == '>') && prev != '<' && prev != '>' {
				flush() // a redirection operator ends the word before it
			}
			cur = append(cur, c)
			has = true
			if c == '<' && string(cur) == "<<<" && worktreeDelHereStringTarget(segment, i) {
				flush() // a here-string written without a blank before its target: the operator is a word of its own
			}
		}
	}
	flush()
	return tokens
}

// worktreeDelHereStringTarget says whether the byte after the here-string operator whose third '<' stands at i begins the
// operator's target: a here-string written without a blank, `<<<'program'`, is the operator and its target, exactly as when
// a blank stands between them (worktreeDelQuoteTokenize flushes the operator and reads the target as the next word). A
// blank, another redirection byte, a command separator or the end of the text leaves the operator a word of its own.
func worktreeDelHereStringTarget(segment string, i int) bool {
	if i+1 >= len(segment) {
		return false
	}
	if worktreeDelQuoteSpace(segment[i+1:]) > 0 {
		return false
	}
	switch segment[i+1] {
	case '<', '>', ';', '|', '&', '(', ')':
		return false
	}
	return true
}

// worktreeDelQuoteSpace is the length of the JavaScript whitespace character that opens s, and 0 when s opens with another.
func worktreeDelQuoteSpace(s string) int {
	_, w := utf8.DecodeRuneInString(s)
	if text.Trim(s[:w]) == "" {
		return w
	}
	return 0
}

// worktreeDelQuoteEscape is what bash makes of a backslash and the text s after it, and how many bytes of s that took. Outside
// quotes the next character is literal, and a backslash-newline pair goes; in double quotes only " \ $ and the backtick lose
// the backslash (and the pair with a newline goes); in $'...' see worktreeDelQuoteDecode. Any other pair stays as written.
func worktreeDelQuoteEscape(state int, s string) ([]byte, int) {
	_, w := utf8.DecodeRuneInString(s)
	switch {
	case s[0] == '\n' && state != worktreeDelQuoteANSIC:
		return nil, 1
	case state == worktreeDelQuotePlain:
		return []byte(s[:w]), w
	case state == worktreeDelQuoteDouble:
		if strings.IndexByte("\"\\$`", s[0]) >= 0 {
			return []byte{s[0]}, 1
		}
	default:
		if out, n := worktreeDelQuoteDecode(s); n > 0 {
			return out, n
		}
	}
	return []byte("\\" + s[:w]), w
}

// worktreeDelQuoteDecode decodes the escape that opens s inside $'...' (s follows the backslash) and returns the bytes it
// stands for and how many bytes of s it took; n is 0 for an escape that stays as written. The backslash goes before \ ' " and
// ?; the numeric escapes are octal (up to three digits, the value wraps at 256), \x (two hex digits), \u (four) and \U (eight);
// \cX is the control character of the byte X (X and 0x1f, 0x7f for ?, so NUL for @, a blank, a backtick or a byte such as
// 0xE0), never the closing quote; \c\ takes the byte after the backslash as plain text, except another backslash, which it takes
// with it, as bash's ansicstr reads them. The letters a b f n r t v e E are the control characters (a tab or a newline in a
// program string separates words and commands). An escape without digits stays as written, so does a code point that is no character; a \U value with the
// high bit set (\U80000000 and above) vanishes, as in bash.
func worktreeDelQuoteDecode(s string) (out []byte, n int) {
	switch c := s[0]; {
	case strings.IndexByte("\\'\"?", c) >= 0:
		return []byte{c}, 1
	case strings.IndexByte("abfnrtvEe", c) >= 0:
		return []byte{"\a\b\f\n\r\t\v\x1b\x1b"[strings.IndexByte("abfnrtvEe", c)]}, 1
	case c == 'c' && len(s) > 1 && s[1] != '\'':
		v, used := []byte{s[1] & 0x1f}, 2
		if s[1] == '?' {
			v[0] = 0x7f
		}
		if s[1] == '\\' && len(s) > 2 {
			used = 3
			if s[2] != '\\' {
				v = append(v, s[2]) // the next byte is plain: it neither starts a pair nor ends the string
			}
		}
		return v, used
	}
	base, max, skip := 8, 3, 0
	switch s[0] {
	case 'x':
		base, max, skip = 16, 2, 1
	case 'u':
		base, max, skip = 16, 4, 1
	case 'U':
		base, max, skip = 16, 8, 1
	}
	k := 0
	for skip+k < len(s) && k < max {
		if d := strings.IndexByte("0123456789abcdef", s[skip+k]|0x20); d < 0 || d >= base {
			break
		}
		k++
	}
	if k == 0 {
		return nil, 0
	}
	v, _ := strconv.ParseUint(s[skip:skip+k], base, 32)
	if s[0] == 'u' || s[0] == 'U' {
		switch {
		case v >= 1<<31:
			return []byte{}, skip + k // bash drops a code point with the high bit set
		case !utf8.ValidRune(rune(v)):
			return nil, 0
		}
		return utf8.AppendRune(nil, rune(v)), skip + k
	}
	return []byte{byte(v)}, skip + k
}

// worktreeDelJoinContinuations is what bash does with a backslash-newline pair before it reads a word: it removes the pair
// where a line continues and keeps it where it does not, in one pass over the bytes through worktreeDelQuoteReader. Outside
// quotes a backslash escapes the next byte (both stay) and only backslash-newline goes; single quotes keep everything; $'...'
// keeps everything and a backslash there escapes the next byte, so \' does not end it; double quotes drop only the pair and keep
// any other backslash pair, so \" does not close them; a # that opens a word starts a comment, which keeps everything up to its
// newline. A removed pair leaves prev alone. Here-document bodies, substitutions, backtick bodies and parameter expansions are
// read as the same flat text (a quote nested in them is not tracked): worktreeDelReadings adds the plain removal as a second
// reading for the commands that hold a here-document operator or a backtick.
func worktreeDelJoinContinuations(command string) string {
	if !strings.Contains(command, "\\\n") {
		return command
	}
	var out strings.Builder
	out.Grow(len(command))
	r := worktreeDelQuoteReader{prev: ' '}
	for i := 0; i < len(command); i++ {
		c := command[i]
		escape := r.escapes(command, i)
		switch {
		case escape && command[i+1] == '\n' && r.state != worktreeDelQuoteANSIC:
			i++ // a continued line: both bytes go, prev stays
		case escape: // the backslash and the byte it escapes stay
			out.WriteString(command[i : i+2])
			i++
			r.pair()
		default:
			out.WriteByte(c)
			r.step(c)
		}
	}
	return out.String()
}

// worktreeDelReadings is the texts the extended walk judges for a command: the scan reading and, for a command whose plain
// removal of every backslash-newline holds a here-document operator or a backtick, also that plain removal. The scan reads
// a here-document body as shell text and a backtick body as part of the surrounding text, where bash reads the first as
// data and removes a pair in the second even inside single quotes, so for such a command either reading can be the one
// bash runs, and the walk denies when either denies: nothing the plain removal denies is allowed, and the scan still
// catches what the plain removal misses (a pair after an escaped backslash, or in a comment).
func worktreeDelReadings(command string) []string {
	plain := strings.ReplaceAll(command, "\\\n", "")
	if plain == command {
		return []string{command}
	}
	scan := worktreeDelJoinContinuations(command)
	if scan != plain && (strings.Contains(plain, "<<") || strings.Contains(plain, "`")) {
		return []string{scan, plain}
	}
	return []string{scan}
}

// The shell names, and the words that may stand before one without making it text (worktreeDelQuoteProgram).
const (
	worktreeDelQuoteShells   = " sh bash dash zsh ksh mksh ash csh tcsh fish su "
	worktreeDelQuoteWrappers = " sudo env nohup xargs command builtin exec time timeout nice setsid stdbuf ionice "
)

// worktreeDelQuoteProgram is the program strings that the words hand to a shell to be read again. The shell name or eval must
// start a command: only wrappers (sudo, env, nohup, xargs and the like), options, assignments, numbers and the argument of an
// option may stand before it, so that echo sh -c '...' is text. After a shell name every later word that holds a blank or a
// separator is a candidate, whichever options and redirections stand before it (-c, -co posix, +c, --rcfile f, -n +o noexec,
// &>f, >'a b'): the shell's own reading of its options picks one of them, and reading one that was an argument can only deny too
// much. After eval the program is its operands joined by blanks, as given and after the options up to --. The inner shell reads
// a program as a command of its own, so the walk judges it as one: this shell's single quotes keep a backslash-newline pair that
// the inner shell removes (sh -c 'r<backslash><newline>m -rf ...' runs rm). atDepth also returns a program that holds no
// blank, which the walk judges only to deny it at the reading depth limit: a program string still unread there is denied
// whether or not it holds a blank (CRW-670).
// A program that can name the operands the shell hands it ($@, $*, BASH_ARGV and the like) is read a second time as those
// operands joined by blanks, which is the command line the shell builds from them (CRW-726, c14(c)).
func worktreeDelQuoteProgram(words []string, atDepth bool) []string {
	for i, word := range words {
		name := basename(word)
		var programs []string
		if name == "eval" {
			for _, program := range []string{strings.Join(worktreeDelQuoteEvalArgs(words[i+1:]), " "), strings.Join(words[i+1:], " ")} {
				if strings.TrimSpace(program) != "" {
					programs = append(programs, program)
				}
			}
		}
		if strings.Contains(worktreeDelQuoteShells, " "+name+" ") {
			operands := words[i+1:]
			end := worktreeDelShellProgramEnd(name, operands)
			for _, operand := range operands[:end] { // the words from there on are data: $0, $1...
				candidates := []string{operand}
				if _, value, attached := strings.Cut(operand, "="); attached && strings.HasPrefix(operand, "--") { // --command='...'
					candidates = append(candidates, value)
				} else if (name == "su" || name == "fish") && len(operand) > 2 && operand[0] == '-' && operand[1] != '-' { // -cPROGRAM, -lcPROGRAM
					// getopt gives the rest of a cluster to its first option that takes an argument (c, g, G, s or w for su)
					if k := strings.IndexAny(operand[1:], "cgGsw") + 1; k > 0 && operand[k] == 'c' {
						candidates = append(candidates, operand[k+1:])
					}
				}
				for _, candidate := range candidates {
					if atDepth || strings.ContainsAny(candidate, " \t\r\n;&|()") { // a blank-less program is read only at the depth limit
						programs = append(programs, candidate)
					}
				}
			}
			if end == len(operands) { // the program is uncertain, so it may run the operands as a command line of its own
				if joined := worktreeDelShellOperandsJoin(name, operands); joined != "" {
					programs = append(programs, joined)
				}
			}
		}
		if len(programs) > 0 {
			return programs
		}
		number := word != "" && word[0] >= '0' && word[0] <= '9' // 5, 0.5, 5s
		if !(strings.Contains(worktreeDelQuoteWrappers, " "+name+" ") || strings.HasPrefix(word, "-") || isAssignment(word) || number ||
			i > 0 && strings.HasPrefix(words[i-1], "-")) {
			break
		}
	}
	return nil
}

// worktreeDelQuoteProgramPosition says whether a segment's words hand a program to a shell for sure: the -c program a
// listed shell's own option parse takes, an eval operand, or a source or . operand. At the reading-depth limit only
// these count, because a word the walk merely guessed at - an option, or an operand its over-reading fallback kept - is
// no program position, and refusing on one denies a command the guard could read (CRW-726, c14(d)).
func worktreeDelQuoteProgramPosition(words []string) bool {
	for i, word := range words {
		name := basename(word)
		switch name {
		case "eval":
			return len(worktreeDelUnreadableEvalIndices(words[i+1:])) > 0
		case "source", ".":
			return worktreeDelUnreadableSourceIndex(words[i+1:]) >= 0
		case "su":
			return worktreeDelSuProgram(worktreeDelQuoteDropRedirects(words[i+1:])) >= 0
		case "sh", "bash", "dash", "ash", "zsh", "ksh", "mksh":
			return worktreeDelFlagShellProgram(worktreeDelQuoteDropRedirects(words[i+1:])) >= 0
		}
		number := word != "" && word[0] >= '0' && word[0] <= '9'
		if !(strings.Contains(worktreeDelQuoteWrappers, " "+name+" ") || strings.HasPrefix(word, "-") || isAssignment(word) || number ||
			i > 0 && strings.HasPrefix(words[i-1], "-")) {
			break
		}
	}
	return false
}

// worktreeDelShellOperandsJoin is the command line a shell builds from the operands it hands its program as $1, $2 and so
// on: the words after the program's own $0, joined by blanks. A program that names its operands ($@, $*, BASH_ARGV) runs
// that line, so the walk reads it as a program too (CRW-726, c14(c)). It is empty when the shell's own option parse did
// not place a program or no operand stands after its $0.
func worktreeDelShellOperandsJoin(name string, operands []string) string {
	program := -1
	switch name {
	case "su":
		program = worktreeDelSuProgram(operands)
	case "sh", "bash", "dash", "ash", "zsh":
		program = worktreeDelFlagShellProgram(operands)
	}
	if program < 0 || program+2 > len(operands) {
		return ""
	}
	return strings.TrimSpace(strings.Join(operands[program+2:], " "))
}

// worktreeDelRedirectWord says whether a word starts like a redirection that the outer shell takes out of the arguments (>f, 2>f,
// &>f, {v}>f, <<<, a lone operator whose target is the next word). The tokenizer has lost the quotes by then, so a quoted '>' looks
// the same: the walk then reads every operand as a program, as before, rather than guess which words the shell passes on.
func worktreeDelRedirectWord(word string) bool {
	rest := strings.TrimLeft(word, "0123456789&")
	if strings.HasPrefix(rest, "{") {
		if end := strings.IndexByte(rest, '}'); end > 0 {
			rest = rest[end+1:]
		}
	}
	return strings.HasPrefix(rest, "<") || strings.HasPrefix(rest, ">")
}

// worktreeDelOptionWord says whether a word may be an option: it starts with - or +.
func worktreeDelOptionWord(word string) bool {
	return word != "" && (word[0] == '-' || word[0] == '+')
}

// worktreeDelCertainProgram says whether a shell program is a plain command that cannot reach the operands the shell hands it as
// $0, $1 and so on. A program that holds a substitution, a separator, a pipe, a redirection or a parenthesis, or that names the
// arguments (a dollar sign, a backtick, or ARGV, ARGC, argv or the BASH_AR* variables), is not certain, so the walk reads every
// operand after it as a program. A name check alone would miss an obfuscation (a grep pattern that spells BASH_ARGV around a
// wildcard), so any construct that could run a second command makes the program uncertain too.
func worktreeDelCertainProgram(word string) bool {
	if strings.ContainsAny(word, "$\x60") || strings.Contains(word, "<(") || strings.Contains(word, ">(") {
		return false // the program names a value or runs a second command of its own
	}
	for _, field := range strings.Fields(strings.ToLower(word)) {
		if strings.HasPrefix(field, "bash_ar") || field == "argv" || field == "argc" {
			return false // the program can name its own operands, which the shell hands it as $0, $1 and so on
		}
	}
	return true
}

// worktreeDelShellProgramEnd is how many of a shell's operands the walk reads as programs: the words from there on are $0, $1
// and so on, which the shell does not read (bash -c 'echo OK' 'rm -rf x' runs echo). It is every operand, the reading that
// CRW-611 left, unless the program is certain, and it holds back wherever the option parse could be wrong or the words after the
// program could matter: a redirection word anywhere (a quoted > looks like one) and a program that is a bare option or can name
// its operands (a dollar sign, BASH_ARGV, argv: bash -c 'eval "$0"' 'rm -rf x' runs rm). For sh, bash, dash and ash the operands
// after the program are the inner shell's $0, $1 and so on whatever they look like, so a later option-like word is not a program
// (bash -c 'echo OK' -c 'rm -rf x' runs echo, CRW-670); su, whose last -c is its program, and every other shell keep the reading
// of every later option-like word, because the walk does not tell those shells apart. The program is certain only when it is a
// plain command that cannot reach its operands (worktreeDelCertainProgram).
func worktreeDelShellProgramEnd(name string, operands []string) int {
	program := -1
	switch name {
	case "su":
		program = worktreeDelSuProgram(operands)
	case "sh", "bash", "dash", "ash", "zsh":
		program = worktreeDelFlagShellProgram(operands)
	}
	if program < 0 {
		return len(operands)
	}
	for _, other := range operands {
		if worktreeDelRedirectWord(other) {
			return len(operands)
		}
	}
	word := operands[program]
	if !worktreeDelCertainProgram(word) ||
		worktreeDelOptionWord(word) && !strings.ContainsAny(word, " \t\r\n;&|()") {
		return len(operands)
	}
	if !worktreeDelShellDataOperands(name) {
		for _, later := range operands[program+1:] {
			if worktreeDelOptionWord(later) {
				return len(operands)
			}
		}
	}
	return program + 1
}

// worktreeDelShellDataOperands says whether a shell reads the operands after its -c program as data, its $0, $1 and so on,
// whatever they look like: sh, bash, dash and ash do, so a later option-like word is not a program for them. su, whose last
// -c is its program, and every other shell do not, so the walk keeps reading every later option-like word for them.
func worktreeDelShellDataOperands(name string) bool {
	switch name {
	case "sh", "bash", "dash", "ash":
		return true
	}
	return false
}

// worktreeDelSuProgram is the index of the operand that holds su's -c program, -1 when it finds none. su reads its options with
// getopt_long (util-linux 2.41.3): they may stand after the user; -c, -s, -g, -G and -w take the rest of their cluster or the next
// word, whatever it looks like; --command and --session-command take theirs attached with = or in the next word and may be
// abbreviated; the last -c wins (the walk reads every -c: an earlier one is before the index).
func worktreeDelSuProgram(operands []string) int {
	program := -1
	for i := 0; i < len(operands); i++ {
		word := operands[i]
		switch {
		case word == "--":
			return program
		case strings.HasPrefix(word, "--"):
			if long, _, attached := strings.Cut(word[2:], "="); long != "" && (strings.HasPrefix("command", long) || strings.HasPrefix("session-command", long)) {
				if !attached {
					i++
				}
				if i < len(operands) {
					program = i
				}
			}
		case len(word) > 1 && word[0] == '-':
			for k := 1; k < len(word); k++ {
				if !strings.ContainsRune("cgGsw", rune(word[k])) {
					continue
				}
				if k+1 == len(word) {
					i++
				}
				if word[k] == 'c' && i < len(operands) {
					program = i
				}
				break
			}
		}
	}
	return program
}

// worktreeDelBashLongs are the long options of bash (bash --help): one outside this list is not a certainty.
const worktreeDelBashLongs = " --debug --debugger --dump-po-strings --dump-strings --help --init-file --login --noediting --noprofile --norc --posix --pretty-print --rcfile --restricted --verbose --version "

// worktreeDelFlagShellProgram is the index of the operand that holds the -c program of sh, bash, dash, ash or zsh, -1 when none is
// certain. Their options end at the first operand or after --; every letter of a cluster is a flag, an o or O (-o posix,
// -O nullglob) takes the next word, and so do --rcfile and --init-file. The program is the first operand when -c came before
// it (bash -c -e 'cmd' runs cmd). A lone -, +c and a long option that bash does not have, which shells read in their own ways, are
// not a certainty.
func worktreeDelFlagShellProgram(operands []string) int {
	command := false
	for i := 0; i < len(operands); i++ {
		word := operands[i]
		switch {
		case word == "-":
			return -1
		case word == "--":
			if i++; command && i < len(operands) {
				return i
			}
			return -1
		case strings.HasPrefix(word, "--"):
			if !strings.Contains(worktreeDelBashLongs, " "+word+" ") {
				return -1
			}
			if word == "--rcfile" || word == "--init-file" {
				i++
			}
		case len(word) > 1 && (word[0] == '-' || word[0] == '+'):
			for _, letter := range word[1:] {
				switch {
				case letter == 'c' && word[0] == '-':
					command = true
				case letter == 'o' || letter == 'O':
					i++
				}
			}
		case command:
			return i
		default:
			return -1
		}
	}
	return -1
}

// worktreeDelQuoteEvalArgs is the operands eval joins into its program: what is left after the redirections and the leading
// words that start with a dash (eval takes -- and, in bash, no other option).
func worktreeDelQuoteEvalArgs(words []string) []string {
	words = worktreeDelQuoteDropRedirects(words)
	for len(words) > 0 && len(words[0]) > 1 && words[0][0] == '-' {
		words = words[1:]
	}
	return words
}

// worktreeDelQuoteDropRedirects is words without the redirections, which the outer shell takes out of the arguments: an
// operator with its target in one word, an operator alone with the next word, and a descriptor in front of an operator. A word
// that was quoted and starts like a redirection is dropped too; the program joined from all the words still holds it.
func worktreeDelQuoteDropRedirects(words []string) []string {
	out := make([]string, 0, len(words))
	for i := 0; i < len(words); i++ {
		word := words[i]
		switch {
		case strings.HasPrefix(word, "<") || strings.HasPrefix(word, ">"):
			if strings.Trim(word, "<>&|") == "" && i+1 < len(words) {
				i++
			}
		case word != "" && i+1 < len(words) && strings.Trim(word, "0123456789") == "" && (strings.HasPrefix(words[i+1], "<") || strings.HasPrefix(words[i+1], ">")):
		default:
			out = append(out, word)
		}
	}
	return out
}

// worktreeDelSubstitutionBody is the text inside the substitution whose opener the caller has read: the body of $(...),
// <(...) or >(...) (backtick false) or of a backtick pair (backtick true), and how many bytes it spans up to and including
// the closing ) or backtick. It tracks the quotes, backslashes and comments inside the way the reader does, so a ) inside a
// quote or a comment does not close the substitution and a nested ( counts. An unterminated substitution runs to the end.
// closed says whether the closing byte was found, so a caller can fall back to the ordinary reading when it was not.
func worktreeDelSubstitutionBody(rest string, backtick bool) (string, int, bool) {
	var out []byte
	depth := 1
	r := worktreeDelQuoteReader{prev: ' '}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if r.escapes(rest, i) {
			out = append(out, c, rest[i+1])
			i++
			r.pair()
			continue
		}
		if !backtick && c == '$' && i+1 < len(rest) && rest[i+1] == '{' && r.state == worktreeDelQuotePlain {
			end := worktreeDelBraceEnd(rest[i+2:]) // a parameter expansion: its own ) does not close the substitution
			out = append(out, rest[i:i+2+end]...)
			i += 1 + end
			r.prev = 'x' // the expansion is part of the word it stands in: a # right after it does not open a comment
			continue
		}
		if backtick {
			if c == '`' {
				// the first unescaped backtick ends the substitution whatever the state inside it: bash ends it there even in a comment
				// or an unterminated quote (echo "`# '`" and echo "`'`" both close at the backtick)
				return string(out), i + 1, true
			}
		} else {
			switch {
			case c == '(' && r.state == worktreeDelQuotePlain:
				depth++
			case c == ')' && r.state == worktreeDelQuotePlain:
				if depth--; depth == 0 {
					return string(out), i + 1, true
				}
			}
		}
		out = append(out, c)
		r.step(c)
	}
	return string(out), len(rest), false
}

// worktreeDelBraceEnd is the length of the text up to and including the } that closes a ${...} parameter expansion, whose
// body may hold nested braces, quotes and backslashes; an unterminated one runs to the end.
func worktreeDelBraceEnd(rest string) int {
	depth := 1
	r := worktreeDelQuoteReader{prev: ' ', brace: true}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if r.escapes(rest, i) {
			i++
			r.pair()
			continue
		}
		if r.state == worktreeDelQuotePlain {
			switch c {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
		r.step(c)
	}
	return len(rest)
}

// worktreeDelSubstitutions is the programs a segment runs before it runs: the body of every command substitution, $(...) and
// backtick, and of every process substitution, <(...) and >(...), that stands in plain text or inside double quotes, because
// the outer shell reads and runs each of them first (CRW-670). A substitution inside single quotes, inside $'...' or inside a
// comment is data, and so is a $, < or backtick that a backslash escapes. The rest of the segment from an opener is returned
// apart from the body: the body reader cannot model every construct bash allows inside a substitution (a case pattern's )
// closes it early, a nested substitution under an outer double quote confuses its quote state), and a body it cut short would
// otherwise hide the rest of the substitution. Reading past the body can only deny too much, never too little. The body is a
// program of its own, one depth deeper; the rest is the same text, not a nested program, and the caller judges it at the
// segment's own depth, which also keeps the walk bounded (CRW-670).
// worktreeDelSubstitution is one program a segment runs before it runs: the body of a command or process substitution,
// the rest of the segment after its opener (the same text, not a nested program), and the directory the substitution
// runs in, which is the cwd after every cd command that stands before its opener (CRW-726, c14(b)).
type worktreeDelSubstitution struct {
	body string
	tail string
	cwd  string
}

func worktreeDelSubstitutions(segment, cwd string) []worktreeDelSubstitution {
	var out []worktreeDelSubstitution
	r := worktreeDelQuoteReader{prev: ' '}
	segCwd := cwd
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if r.escapes(segment, i) {
			r.pair()
			i++
			continue
		}
		if r.state == worktreeDelQuoteSingle || r.state == worktreeDelQuoteANSIC || r.state == worktreeDelQuoteComment {
			r.step(c)
			continue
		}
		if c == '$' && i+1 < len(segment) && segment[i+1] == '{' {
			// a ${...} parameter expansion is read to its closing }, where a # is data and never opens a comment (CRW-726, c13)
			n := worktreeDelBraceEnd(segment[i+2:]) + 2
			i += n - 1
			r.prev = 'x'
			continue
		}
		if target, n := worktreeDelCdAt(segment, i, r.prev); n > 0 { // a cd moves the directory the later substitutions run in
			segCwd = resolveFrom(segCwd, target)
			i += n - 1
			r.prev = 'x'
			continue
		}
		var body string
		var n int
		var start int // the byte after the opener: the whole rest of the segment is judged with the body
		state := r.state
		switch {
		case c == '$' && i+1 < len(segment) && segment[i+1] == '(':
			start = i + 2
			body, n, _ = worktreeDelSubstitutionBody(segment[i+2:], false)
			n += 2
		case c == '`':
			start = i + 1
			body, n, _ = worktreeDelSubstitutionBody(segment[i+1:], true)
			n++
		case (c == '<' || c == '>') && i+1 < len(segment) && segment[i+1] == '(':
			start = i + 2
			body, n, _ = worktreeDelSubstitutionBody(segment[i+2:], false)
			n += 2
		default:
			r.step(c)
			continue
		}
		sub := worktreeDelSubstitution{cwd: segCwd}
		if strings.TrimSpace(body) != "" {
			sub.body = body
		}
		// The rest of the segment from the opener: the body reader cannot model every construct bash allows inside a
		// substitution (a case pattern's ) closes it early), so the rest is judged too. When the reader stopped at the
		// substitution's own closing byte the rest is the text after it, read in the quote state the opener stood in, so
		// text in single quotes after the substitution stays data (CRW-726, c14(a)); when it stopped early the rest is
		// still the substitution's program, which bash parses from its own start, and is read from plain state.
		if tail := strings.TrimSpace(segment[start:]); tail != strings.TrimSpace(body) {
			if state == worktreeDelQuoteDouble && (i+n >= len(segment) || segment[i+n] == '"') {
				tail = "\"" + tail
			}
			sub.tail = tail
		}
		if sub.body != "" || sub.tail != "" {
			out = append(out, sub)
		}
		i += n - 1
		r.prev = 'x' // the substitution is part of the word it stands in: a # after it does not open a comment
	}
	return out
}

// worktreeDelCdAt is the target of the cd command that opens at byte i of a text, and how many bytes of the command that
// took; n is 0 when no cd opens there. The caller passes the byte the reader read last, so that a cd is read only where a
// command starts, the way the walk's segment loop reads it.
func worktreeDelCdAt(text string, i int, prev byte) (string, int) {
	if strings.IndexByte(" \t\n;&|(", prev) < 0 {
		return "", 0
	}
	if !strings.HasPrefix(text[i:], "cd") {
		return "", 0
	}
	j := i + 2
	if j < len(text) && text[j] != ' ' && text[j] != '\t' {
		return "", 0
	}
	for j < len(text) && (text[j] == ' ' || text[j] == '\t') {
		j++
	}
	start := j
	for j < len(text) && strings.IndexByte(" \t\r\n;&|()<>", text[j]) < 0 {
		j++
	}
	if start == j {
		return "", 0
	}
	target := text[start:j]
	if strings.ContainsAny(target, "$\x60\"'\\<>(") { // a target the shell builds is no move this reader can follow
		return "", 0
	}
	return target, j - i
}

// worktreeDelQuoteDepth is how many program strings deep the walk follows a shell's program (sh -c 'sh -c ...'). A program
// that is still unread there is denied, whether or not it holds a blank: the walk cannot tell what it runs.
const worktreeDelQuoteDepth = 8

// worktreeDelWalkBudget is how many distinct texts one evaluation may judge. Past it the walk denies with a reason of its
// own, fail-closed like the depth limit, so a long command line with many substitutions cannot make the guard take longer
// than the hook timeout (CRW-670). The state holds it in a field, so a test can lower it.
const worktreeDelWalkBudget = 4096

// worktreeDelWalkKey identifies one text the walk judges. The same text can answer differently in another directory or at
// another depth, so both are part of the key.
type worktreeDelWalkKey struct {
	text  string
	cwd   string
	depth int
}

// worktreeDelWalkState is one evaluation's memory: the verdict of every text already judged, keyed by text, directory and
// depth, and the budget of distinct texts it may judge. worktreeDelEvaluate creates it and passes it down the walk; it is
// never shared between evaluations and no package-level variable holds it.
type worktreeDelWalkState struct {
	verdicts map[worktreeDelWalkKey]GuardVerdict
	budget   int
	judged   int // the texts this evaluation has judged, against budget
}

// newWorktreeDelWalkState is the state one evaluation starts with, at the default budget.
func newWorktreeDelWalkState() *worktreeDelWalkState {
	return &worktreeDelWalkState{verdicts: map[worktreeDelWalkKey]GuardVerdict{}, budget: worktreeDelWalkBudget}
}

// judge is the extended walk of one text, depth program strings down, with the state's memory and budget: a text already
// judged returns its verdict instead of being walked again, and a text that would push the state past its budget is denied.
func (s *worktreeDelWalkState) judge(command, cwd string, id WorktreeIdentity, depth int) GuardVerdict {
	key := worktreeDelWalkKey{text: command, cwd: cwd, depth: depth}
	if verdict, ok := s.verdicts[key]; ok {
		return verdict
	}
	s.judged++
	if s.judged > s.budget {
		return GuardVerdict{Deny: true, Reason: denyReason("a command too complex for the guard to read", id)}
	}
	verdict := s.judgeReadings(command, cwd, id, depth)
	s.verdicts[key] = verdict
	return verdict
}

// judgeReadings is the walk of one text: every reading over the old grammar and then over the quote-aware one, the first
// grammar first, so nothing it denied is allowed and each earlier deny keeps its reason. The substitutions of the text are
// then judged too: a text that segmentation cuts at a parenthesis (echo $(true) is three segments) still holds a
// substitution the outer shell runs, and the state counts it.
func (s *worktreeDelWalkState) judgeReadings(command, cwd string, id WorktreeIdentity, depth int) GuardVerdict {
	readings := worktreeDelReadings(command)
	for _, quoting := range []bool{false, true} {
		for _, reading := range readings {
			if verdict := s.walk(reading, cwd, id, true, quoting, depth); verdict.Deny {
				return verdict
			}
		}
	}
	for _, reading := range readings {
		for _, sub := range worktreeDelSubstitutions(reading, cwd) {
			if sub.body != "" { // a substitution body is a program of its own, one depth deeper
				if verdict := s.judge(sub.body, sub.cwd, id, depth+1); verdict.Deny {
					return verdict
				}
			}
			if sub.tail != "" { // the rest of the text after an opener is the same text, not a nested program
				if verdict := s.judge(sub.tail, sub.cwd, id, depth); verdict.Deny {
					return verdict
				}
			}
		}
	}
	return GuardVerdict{}
}

// worktreeDelWalk is the single-use form of walk's loop over one text in the grammar of the oracle (and of the extended
// walk before CRW-611), for a caller that does not carry a state (the tests).
func worktreeDelWalk(command, cwd string, id WorktreeIdentity, extended bool) GuardVerdict {
	return newWorktreeDelWalkState().walk(command, cwd, id, extended, false, 0)
}

// walk is the loop over one text: the segments in order, a cd moving the directory later segments run in, and the
// conservative fallback when a destructive verb was seen and the command mentions the worktree but no target resolved.
// quoting reads the text over bash's own quotes, backslashes and comments instead, and judges the program string that a shell
// word hands to -c and the program a substitution runs, before the cd branch, because the outer shell runs a substitution
// before it runs cd (depth says how many programs deep this text is). It is a method so that every text it judges shares the
// state's memory and budget.
func (s *worktreeDelWalkState) walk(command, cwd string, id WorktreeIdentity, extended, quoting bool, depth int) GuardVerdict {
	hint := destructiveHint(extended)
	segCwd, destructiveSeen := cwd, false
	var scopes []string // the directories a subshell restores, extended walk only
	segments := splitSegments(command, extended)
	if quoting {
		segments = worktreeDelQuoteSegments(command)
	}
	for _, segment := range segments {
		if extended && (segment == "(" || segment == ")") {
			if segment == "(" {
				scopes = append(scopes, segCwd)
			} else if n := len(scopes); n > 0 {
				segCwd, scopes = scopes[n-1], scopes[:n-1]
			}
			continue
		}
		tokens := worktreeDelQuoteTokens(segment, quoting)
		if quoting {
			programs := worktreeDelQuoteProgram(tokens, depth >= worktreeDelQuoteDepth)
			if depth >= worktreeDelQuoteDepth && worktreeDelQuoteProgramPosition(tokens) {
				return GuardVerdict{Deny: true, Reason: denyReason("a shell program nested past the reading depth", id)}
			}
			for _, program := range programs {
				if verdict := s.judge(program, segCwd, id, depth+1); verdict.Deny {
					return verdict
				}
			}
			for _, sub := range worktreeDelSubstitutions(segment, segCwd) { // the outer shell runs a substitution before it runs cd
				if sub.body != "" { // a substitution body is a program of its own, one depth deeper
					if verdict := s.judge(sub.body, sub.cwd, id, depth+1); verdict.Deny {
						return verdict
					}
				}
				if sub.tail != "" { // the rest of the segment after an opener is the same text, not a nested program
					if verdict := s.judge(sub.tail, sub.cwd, id, depth); verdict.Deny {
						return verdict
					}
				}
			}
		}
		if len(tokens) > 1 && tokens[0] == "cd" && tokens[1] != "" {
			segCwd = resolveFrom(segCwd, tokens[1])
			continue
		}
		if hint.MatchString(foldASCII(segment)) {
			destructiveSeen = true
		}
		if verdict, ok := evaluateSegment(segment, segCwd, id, extended, quoting); ok {
			return verdict
		}
	}
	mentions := func(s string) bool { return s != "" && strings.Contains(command, s) }
	if destructiveSeen && (mentions(id.SlotRoot) || mentions(id.WorktreesDir) || mentions(id.Slot)) {
		return GuardVerdict{Deny: true, Reason: denyReason("unresolvable target mentioning the managed worktree", id)}
	}
	return GuardVerdict{}
}

// worktreeDelQuoteWalk is the single-use form of the walk over one text, for a caller that does not carry a state (the
// tests).
func worktreeDelQuoteWalk(command, cwd string, id WorktreeIdentity, extended, quoting bool, depth int) GuardVerdict {
	return newWorktreeDelWalkState().walk(command, cwd, id, extended, quoting, depth)
}

// evaluateCommand allows anything outside a managed worktree. Inside one it walks the command as the oracle does and,
// when that allows, walks it again over the extended grammar, at the default budget.
func evaluateCommand(command, cwd string, id WorktreeIdentity) GuardVerdict {
	return worktreeDelEvaluate(command, cwd, id, worktreeDelWalkBudget)
}

// worktreeDelEvaluate is evaluateCommand with an explicit budget: it creates the one walk state of this evaluation and
// judges the command over the oracle's grammar first, then over the extended one, so every text either walk judges shares
// the state's memory and budget.
func worktreeDelEvaluate(command, cwd string, id WorktreeIdentity, budget int) GuardVerdict {
	if !id.Managed || text.Trim(command) == "" {
		return GuardVerdict{}
	}
	state := &worktreeDelWalkState{verdicts: map[worktreeDelWalkKey]GuardVerdict{}, budget: budget}
	if verdict := state.walk(command, cwd, id, false, false, 0); verdict.Deny {
		return verdict
	}
	if verdict := state.judge(command, cwd, id, 0); verdict.Deny {
		return verdict
	}
	// The walk read every program it could see; a program position, or a command name, that the outer shell builds at
	// run time is still a program the guard cannot read, so the second reading refuses it (CRW-726).
	return worktreeDelUnreadableGuard(command, cwd, id)
}

func denyReason(what string, id WorktreeIdentity) string {
	slotRoot := id.SlotRoot
	if slotRoot == "" {
		slotRoot = "unknown"
	}
	return strings.Join([]string{
		"[crw: WORKTREE-GUARD-03] blocked `" + what + "`: it deletes this session's",
		"own Codex-app-managed worktree (slot: " + slotRoot + "). This thread",
		"is bound to that worktree; deletion destroys uncommitted work.",
		"Remedies: finish and commit here; rename in place (git switch -c / branch -m);",
		"teardown of THIS session's worktree is done by the user (archive the thread in",
		"the app — snapshot preserved — or remove it from OUTSIDE this session).",
		"See $crw:crw-worktree-guardian.",
	}, " ")
}

// HandleWorktreeGuardPreTool is handleWorktreeGuardPreTool, the PreToolUse leg: the deny line for a Bash command that
// deletes the session's own worktree, else empty (it never fails the tool). A tool name other than Bash, a payload
// without a cwd or a command, and anything that is not one JSON object all pass. The guard sits above the subagent exit,
// so a subagent's turn is checked too.
func HandleWorktreeGuardPreTool(raw string, env host.LookupEnv) string {
	payload := parseRaw(raw)
	if payload["hook_event_name"] != "PreToolUse" {
		return ""
	}
	if tool, _ := payload["tool_name"].(string); tool != "" && tool != "Bash" {
		return ""
	}
	cwd, _ := payload["cwd"].(string)
	input, _ := payload["tool_input"].(map[string]any)
	command, _ := input["command"].(string)
	if cwd == "" || command == "" {
		return ""
	}
	verdict := evaluateCommand(command, cwd, detectManagedWorktree(cwd, env))
	if !verdict.Deny {
		return ""
	}
	return editAnswer("deny", verdict.Reason, verdict.Reason)
}

// --------------------------------------------------------------------------------------------------------------------
// CRW-726: a program position, or a command name, that the outer shell builds at run time.
//
// Both guards read shell text and run nothing. A position that hands a program to a shell - the -c program of a listed
// shell, the operands of eval, the first operand of source or ., the standard input of a shell that stands to the right
// of a single pipe or takes a here-string, or the here-document a shell reads - is readable only when the outer shell
// hands the program over as written. When the word that holds it carries an expansion the outer shell performs first,
// the program is not known until it runs, so the guard refuses the command (fail closed). A simple command whose name
// word is built the same way is judged as rm -r with its literal operands, or refused outright when the whole name is a
// command substitution. CXC v0.2.40 allows every such form: this is a security fix under the parity rule revision of
// 2026-10-03 (port: fixed), not a port of the oracle's behaviour.
//
// The reading runs after the walk, over a command the walk allowed, so a deny the walk already found for the same
// command keeps its own reason.
// --------------------------------------------------------------------------------------------------------------------

// worktreeDelUnreadableShells are the shells this reading finds a program position for: the -c program of each of them,
// and the program each of them reads from standard input. worktreeDelUnreadableHereShells are the ones that also read a
// here-document as a program (su reads its standard input through the shell it starts, so it is not among them).
const (
	worktreeDelUnreadableShells     = " sh bash dash ash zsh ksh su "
	worktreeDelUnreadableHereShells = " sh bash dash ash zsh ksh "
)

// worktreeDelUnreadableWord is one word of a command as the outer shell reads it: text is the word with the outer
// shell's quotes and escaping backslashes removed, raw is the word as written, outer says the word holds an expansion
// the outer shell performs before the command runs, and whole says the word is nothing but one command substitution
// (or one backtick pair), which the shell runs as a command line of its own.
type worktreeDelUnreadableWord struct {
	text  string
	raw   string
	outer bool
	whole bool
}

// worktreeDelUnreadableWords is worktreeDelQuoteTokenize over bash's own reading, keeping each word's written text and
// the two marks above. It walks the same bytes with the same reader and cuts at the same places as the walk's
// tokenizer, so the words and their order are the same.
func worktreeDelUnreadableWords(segment string) []worktreeDelUnreadableWord {
	var words []worktreeDelUnreadableWord
	var text, raw []byte
	r := worktreeDelQuoteReader{prev: ' '}
	has, nul := false, false
	flush := func() {
		if has {
			written := string(raw)
			words = append(words, worktreeDelUnreadableWord{
				text:  string(text),
				raw:   written,
				outer: worktreeDelUnreadableOuter(written),
				whole: worktreeDelUnreadableWholeSubstitution(written),
			})
			text, raw, has = text[:0], raw[:0], false
		}
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if r.escapes(segment, i) {
			out, n := worktreeDelQuoteEscape(r.state, segment[i+1:])
			if r.state == worktreeDelQuoteANSIC && slices.Contains(out, 0) {
				nul = true
			}
			raw = append(raw, segment[i:i+1+n]...)
			if !nul {
				text = append(text, out...)
			}
			i += n
			has = has || len(out) > 0
			r.pair()
			continue
		}
		if r.state == worktreeDelQuotePlain || r.state == worktreeDelQuoteDouble {
			if n, ok := worktreeDelUnreadableRegion(segment, i); ok {
				// the outer shell performs the expansion while it reads the word it stands in, so a blank inside it never
				// ends that word: $(printf 'a b'), ${x:-a b} and a backtick pair are one word
				raw = append(raw, segment[i:i+n]...)
				text = append(text, segment[i:i+n]...)
				i += n - 1
				has = true
				r.prev = 'x'
				continue
			}
		}
		state, prev := r.state, r.prev
		r.step(c)
		switch {
		case state == worktreeDelQuoteComment || r.state == worktreeDelQuoteComment:
			// a comment is dropped from both readings
		case state == worktreeDelQuotePlain && r.state != worktreeDelQuotePlain:
			if prev == '$' && len(text) > 0 {
				text = text[:len(text)-1] // the dollar of $'...' or $"..." is quoting, not part of the word
			}
			raw = append(raw, c)
			has = true
		case state != worktreeDelQuotePlain && r.state == worktreeDelQuotePlain:
			raw = append(raw, c)
			nul = false
		case state != worktreeDelQuotePlain:
			raw = append(raw, c)
			if !nul {
				text = append(text, c)
			}
		default:
			if w := worktreeDelQuoteSpace(segment[i:]); w > 0 {
				flush()
				i += w - 1
				break
			}
			if (c == '<' || c == '>') && prev != '<' && prev != '>' {
				flush() // a redirection operator ends the word before it
			}
			raw = append(raw, c)
			text = append(text, c)
			has = true
			if c == '<' && string(text) == "<<<" && worktreeDelHereStringTarget(segment, i) {
				flush() // a here-string written without a blank before its target
			}
		}
	}
	flush()
	return words
}

// worktreeDelUnreadableRegion is the length of the expansion that opens at byte i, when one does: a command substitution
// $(...), an arithmetic expansion $((...)), a parameter expansion ${...} or a backtick pair. It is 0 when no expansion
// opens there.
func worktreeDelUnreadableRegion(segment string, i int) (int, bool) {
	switch {
	case segment[i] == '$' && i+1 < len(segment) && segment[i+1] == '{':
		return worktreeDelBraceEnd(segment[i+2:]) + 2, true
	case segment[i] == '$' && i+1 < len(segment) && segment[i+1] == '(':
		_, n, _ := worktreeDelSubstitutionBody(segment[i+2:], false)
		return n + 2, true
	case segment[i] == 96:
		_, n, _ := worktreeDelSubstitutionBody(segment[i+1:], true)
		return n + 1, true
	}
	return 0, false
}

// worktreeDelUnreadablePlainTexts is the words of a word list as the inner shell reads them.
func worktreeDelUnreadablePlainTexts(words []worktreeDelUnreadableWord) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		out = append(out, word.text)
	}
	return out
}

// worktreeDelUnreadableOuter says whether a word holds, outside single quotes and $'...' and not escaped by a backslash,
// an expansion the outer shell performs before the command runs: a command substitution ($(...) or a backtick), a
// parameter expansion ($name, ${...}, $1, $@ and the like), an arithmetic expansion $((...)), or, outside any quotes, a
// process substitution <(...) or >(...).
func worktreeDelUnreadableOuter(raw string) bool {
	r := worktreeDelQuoteReader{prev: ' '}
	for i := 0; i < len(raw); i++ {
		if r.escapes(raw, i) {
			r.pair()
			i++
			continue
		}
		state := r.state
		r.step(raw[i])
		if worktreeDelUnreadableOuterAt(raw, i, state) {
			return true
		}
	}
	return false
}

// worktreeDelUnreadableOuterAt says whether the byte at i, read in the state state, opens an expansion the outer shell
// performs. Single quotes and $'...' keep their text literal and a backslash escapes the byte after it, so a caller never
// asks about a byte read in those states. A dollar and a backtick expand inside double quotes too, while <(...) and
// >(...) are performed in plain text only.
func worktreeDelUnreadableOuterAt(raw string, i, state int) bool {
	if state == worktreeDelQuoteSingle || state == worktreeDelQuoteANSIC || state == worktreeDelQuoteComment {
		return false
	}
	switch c := raw[i]; c {
	case 96: // a backtick
		return true
	case '<', '>':
		return state == worktreeDelQuotePlain && i+1 < len(raw) && raw[i+1] == '('
	case '$':
		if i+1 >= len(raw) {
			return false
		}
		switch n := raw[i+1]; {
		case n == '(' || n == '{':
			return true
		case n == 39 || n == '"':
			return false // $'...' and $"..." quote a literal, they expand nothing
		case n == '_' || n == '@' || n == '*' || n == '#' || n == '?' || n == '$' || n == '!' || n == '-':
			return true
		default:
			return n >= '0' && n <= '9' || n >= 'a' && n <= 'z' || n >= 'A' && n <= 'Z'
		}
	}
	return false
}

// worktreeDelUnreadableWholeSubstitution says whether a written word is nothing but one command substitution or one
// backtick pair, with at most one layer of double quotes around it, so the shell runs the substitution's output as a
// command line of its own. An arithmetic expansion is not one: the shell does not run its value as a command line.
func worktreeDelUnreadableWholeSubstitution(raw string) bool {
	word := strings.TrimSpace(raw)
	for len(word) > 1 && word[0] == '"' && word[len(word)-1] == '"' {
		word = word[1 : len(word)-1]
	}
	switch {
	case strings.HasPrefix(word, "$(("):
		return false
	case strings.HasPrefix(word, "$("):
		_, n, closed := worktreeDelSubstitutionBody(word[2:], false)
		return closed && 2+n == len(word)
	case strings.HasPrefix(word, "`"):
		_, n, closed := worktreeDelSubstitutionBody(word[1:], true)
		return closed && 1+n == len(word)
	}
	return false
}

// worktreeDelUnreadablePosition is one place a segment hands a program to a shell: text is the word the inner shell
// reads, raw is that word as written, and what names the position the way the refusal reads it.
type worktreeDelUnreadablePosition struct {
	text  string
	raw   string
	check string
	what  string
}

// worktreeDelUnreadablePositions is the program positions of a segment's words: the -c program of a listed shell, the
// operands of eval, and the first operand of source or .. It mirrors worktreeDelQuoteProgram's scan of the same words -
// the same wrappers, options, assignments and numbers before a shell name, the same shell option tables, the same
// --command= value and su -cPROGRAM suffix - so a word the walk reads as a program is a position here too. Unlike the
// walk it keeps every operand, whether or not it holds a blank, because a position is unreadable as soon as its word
// carries an expansion.
func worktreeDelUnreadablePositions(words []worktreeDelUnreadableWord) []worktreeDelUnreadablePosition {
	plain := worktreeDelUnreadablePlainTexts(words)
	for i, word := range plain {
		name := basename(word)
		var found []worktreeDelUnreadablePosition
		switch {
		case name == "eval":
			for _, j := range worktreeDelUnreadableEvalIndices(plain[i+1:]) {
				found = append(found, worktreeDelUnreadablePosition{text: words[i+1+j].text, raw: words[i+1+j].raw, check: words[i+1+j].raw, what: "an eval operand"})
			}
		case name == "source" || name == ".":
			if j := worktreeDelUnreadableSourceIndex(plain[i+1:]); j >= 0 {
				found = append(found, worktreeDelUnreadablePosition{text: words[i+1+j].text, raw: words[i+1+j].raw, check: words[i+1+j].raw, what: "a source operand"})
			}
		case strings.Contains(worktreeDelQuoteShells, " "+name+" "):
			operands := plain[i+1:]
			for k := range operands[:worktreeDelShellProgramEnd(name, operands)] {
				found = append(found, worktreeDelUnreadableShellParts(name, words[i+1+k])...)
			}
		}
		if len(found) > 0 {
			return found
		}
		number := word != "" && word[0] >= '0' && word[0] <= '9' // 5, 0.5, 5s
		if !(strings.Contains(worktreeDelQuoteWrappers, " "+name+" ") || strings.HasPrefix(word, "-") || isAssignment(word) || number ||
			i > 0 && strings.HasPrefix(words[i-1].text, "-")) {
			break
		}
	}
	return nil
}

// worktreeDelUnreadableShellParts is the candidate programs a shell's operand word can hand to it: the word itself and,
// for --command= and su -cPROGRAM, the suffix the shell reads. worktreeDelQuoteProgram reads the same candidates.
func worktreeDelUnreadableShellParts(name string, word worktreeDelUnreadableWord) []worktreeDelUnreadablePosition {
	what := "a " + name + " -c program"
	out := []worktreeDelUnreadablePosition{{text: word.text, raw: word.raw, check: word.raw, what: what}}
	if _, value, attached := strings.Cut(word.raw, "="); attached && strings.HasPrefix(word.raw, "--") {
		out = append(out, worktreeDelUnreadablePart(value, what))
	} else if (name == "su" || name == "fish") && len(word.raw) > 2 && word.raw[0] == '-' && word.raw[1] != '-' {
		// getopt gives the rest of a cluster to its first option that takes an argument (c, g, G, s or w for su)
		if k := strings.IndexAny(word.raw[1:], "cgGsw") + 1; k > 0 && k < len(word.raw) && word.raw[k] == 'c' {
			out = append(out, worktreeDelUnreadablePart(word.raw[k+1:], what))
		}
	}
	return out
}

// worktreeDelUnreadablePart is one piece of a written word as the inner shell reads it: the piece with the outer shell's
// quotes removed.
func worktreeDelUnreadablePart(part, what string) worktreeDelUnreadablePosition {
	plain := worktreeDelUnreadablePlainText(part)
	return worktreeDelUnreadablePosition{text: plain, raw: part, check: plain, what: what}
}

// worktreeDelUnreadablePlainText is a written piece of text with the outer shell's quotes and escaping backslashes
// removed, the way worktreeDelQuoteTokenize reads a segment's words; the words are joined because a piece of a word holds
// no unquoted blank.
func worktreeDelUnreadablePlainText(raw string) string {
	return strings.Join(worktreeDelQuoteTokenize(raw), "")
}

// worktreeDelUnreadableEvalIndices is the indices, in a word list's tail, of the operands eval joins into its program:
// worktreeDelQuoteEvalArgs keeps those operands in order after dropping the redirections and the leading dash words, so
// walking the two lists together maps them back without a second option parse.
func worktreeDelUnreadableEvalIndices(tail []string) []int {
	operands := worktreeDelQuoteEvalArgs(tail)
	out := make([]int, 0, len(operands))
	j := 0
	for _, operand := range operands {
		for j < len(tail) && tail[j] != operand {
			j++
		}
		if j == len(tail) {
			break
		}
		out = append(out, j)
		j++
	}
	return out
}

// worktreeDelUnreadableSourceIndex is the index, in a source or . command's tail, of the file the shell reads: its first
// operand, after -- and the leading dash words. -1 when there is none.
func worktreeDelUnreadableSourceIndex(tail []string) int {
	for i, word := range tail {
		switch {
		case word == "--":
			if i+1 < len(tail) {
				return i + 1
			}
			return -1
		case len(word) > 1 && word[0] == '-':
		default:
			return i
		}
	}
	return -1
}

// worktreeDelUnreadableCommandWord is the index, in a word list, of the word stripPrefixes keeps first: the word that
// names the command, after sudo, command, builtin and env with its assignments. -1 when every word is a prefix.
func worktreeDelUnreadableCommandWord(words []string) int {
	for i := 0; i < len(words); {
		if i+1 < len(words) && strings.Trim(words[i], "0123456789") == "" && worktreeDelRedirectWord(words[i+1]) {
			i += 2 // a descriptor and the redirection it belongs to stand before the command word
			continue
		}
		if words[i] == "(" || words[i] == "{" {
			i++ // a subshell or a group opener stands before the command word
			continue
		}
		if worktreeDelRedirectWord(words[i]) { // a redirection stands before the command word and is no part of it
			if strings.Trim(words[i], "0123456789&<>") == "" && i+1 < len(words) {
				i++ // a lone operator takes the next word as its target
			}
			i++
			continue
		}
		switch basename(words[i]) {
		case "sudo", "command", "builtin":
			i++
		case "env":
			i++
			for i < len(words) && isAssignment(words[i]) {
				i++
			}
		default:
			return i
		}
	}
	return -1
}

// worktreeDelUnreadableCut is one piece of a command as the shell cuts it, with the operator that ended the piece
// before it: "|", "||", "&&", ";" or "&" for a separator, a newline for a line break, and "" for the first piece
// and for a parenthesis. The reading needs the operator, because a shell that stands to the right of a single pipe
// reads its program from it.
type worktreeDelUnreadableCut struct {
	text string
	sep  string
}

// worktreeDelUnreadableCuts is worktreeDelQuoteSegments' cut of a command, keeping the operator that ended each piece.
func worktreeDelUnreadableCuts(command string) []worktreeDelUnreadableCut {
	var cuts []worktreeDelUnreadableCut
	var cur []byte
	r := worktreeDelQuoteReader{prev: ' '}
	opened := false // the number of backticks read in plain state is odd
	pending := ""
	edge := func(b byte) bool { return strings.IndexByte(" \t\r\n;&|(){}", b) >= 0 }
	cut := func(sep string) {
		if s := text.Trim(string(cur)); s != "" {
			cuts = append(cuts, worktreeDelUnreadableCut{text: s, sep: pending})
			pending = sep
		} else if sep != "" {
			pending = sep
		}
		cur = cur[:0]
	}
	for i := 0; i < len(command); i++ {
		c := command[i]
		if r.escapes(command, i) {
			cur = append(cur, c, command[i+1])
			i++
			r.pair()
			continue
		}
		if r.state == worktreeDelQuoteDouble && (c == '`' || c == '$' && i+1 < len(command) && command[i+1] == '(') {
			// an outer double quote keeps a substitution together: a quote, a separator or a parenthesis inside it is not this shell's
			skip := 0
			if c == '`' {
				if _, n, closed := worktreeDelSubstitutionBody(command[i+1:], true); closed {
					skip = 1 + n
				}
			} else if _, n, closed := worktreeDelSubstitutionBody(command[i+2:], false); closed {
				skip = 2 + n
			}
			if skip > 0 {
				cur = append(cur, command[i:i+skip]...)
				i += skip - 1
				r.prev = 'x'
				continue
			}
		}
		if r.state == worktreeDelQuoteComment && opened && c == '`' && worktreeDelQuoteBackslashes(command[:i])%2 == 0 {
			r.state, r.prev, opened = worktreeDelQuotePlain, '`', false
			cur = append(cur, c)
			continue
		}
		state, prev := r.state, r.prev
		r.step(c)
		switch {
		case state == worktreeDelQuoteComment || r.state == worktreeDelQuoteComment:
			if c == '\n' {
				cut("\n")
			}
		case state != worktreeDelQuotePlain || r.state != worktreeDelQuotePlain:
			cur = append(cur, c)
		case c == '&' && (prev == '>' || prev == '<' || i+1 < len(command) && command[i+1] == '>'):
			cur = append(cur, c) // part of a redirection (2>&1, >&2, &>f), not the end of a command
		case c == ';' || c == '|' || c == '&' || c == '\n' || (c == '{' || c == '}') && edge(prev) && (i+1 == len(command) || edge(command[i+1])) && !worktreeDelQuoteRemoves(cur):
			sep := string(c)
			if (c == '|' || c == '&') && i+1 < len(command) && command[i+1] == c {
				sep, i = string(c)+string(c), i+1
			}
			cut(sep)
		default:
			cur = append(cur, c)
			if c == '`' {
				if opened = !opened; opened {
					r.prev = ' '
				}
			}
		}
	}
	cut("")
	return cuts
}

// worktreeDelUnreadableStdinShell says whether a shell named name with these operands reads its program from standard
// input: it has no -c program and no script operand, or it was asked for standard input with -s. The option parse is
// the one worktreeDelFlagShellProgram and worktreeDelSuProgram use, so a word the walk reads as a -c program is one
// here too.
func worktreeDelUnreadableStdinShell(name string, operands []string) bool {
	args := worktreeDelQuoteDropRedirects(worktreeDelUnreadableHereArgs(operands))
	if name == "su" {
		return worktreeDelSuProgram(args) < 0
	}
	if worktreeDelFlagShellProgram(args) >= 0 {
		return false
	}
	for _, word := range args {
		if len(word) > 1 && word[0] == '-' && word[1] != '-' && strings.ContainsRune(word[1:], 's') {
			return true
		}
	}
	return worktreeDelUnreadableScriptOperand(args) < 0
}

// worktreeDelUnreadableHereArgs is operands without the here-document and here-string operators and the target words
// that go with them, which the outer shell takes out of the command's arguments before the shell sees them.
func worktreeDelUnreadableHereArgs(operands []string) []string {
	out := make([]string, 0, len(operands))
	for i := 0; i < len(operands); i++ {
		word := operands[i]
		if !strings.HasPrefix(word, "<<") {
			out = append(out, word)
			continue
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(word[2:], "-"), "<")
		if rest == "" {
			i++ // the operator is a word of its own: its target is the next word
		}
	}
	return out
}

// worktreeDelUnreadableScriptOperand is the index, in a shell's operands, of the first word the shell reads as its
// script file: worktreeDelFlagShellProgram's option parse continued past -c, where the program would stand. -1 when
// every operand is an option, the argument of one, or a lone - (which asks for standard input).
func worktreeDelUnreadableScriptOperand(operands []string) int {
	for i := 0; i < len(operands); i++ {
		word := operands[i]
		switch {
		case word == "-":
			return -1
		case word == "--":
			if i+1 < len(operands) {
				return i + 1
			}
			return -1
		case strings.HasPrefix(word, "--"):
			if !strings.Contains(worktreeDelBashLongs, " "+word+" ") {
				return -1
			}
			if word == "--rcfile" || word == "--init-file" {
				i++
			}
		case len(word) > 1 && (word[0] == '-' || word[0] == '+'):
			for _, letter := range word[1:] {
				if letter == 'o' || letter == 'O' {
					i++
				}
			}
		default:
			return i
		}
	}
	return -1
}

// worktreeDelUnreadableHereString is the index, in a shell's operands, of the word a here-string operator feeds it: the
// word after a <<< operand. -1 when the shell takes no here-string.
func worktreeDelUnreadableHereString(operands []string) int {
	for i, word := range operands {
		if word == "<<<" && i+1 < len(operands) {
			return i + 1
		}
	}
	return -1
}

// worktreeDelUnreadableHereOperator is one here-document operator of a command line.
type worktreeDelUnreadableHereOperator struct {
	word      string // the delimiter word as written
	delimiter string // the delimiter word with the outer shell's quotes removed
	literal   bool   // the delimiter word holds a quote or a backslash, so the outer shell does not expand the body
	stripTabs bool   // <<- strips the leading tabs of the body and of its delimiter line
}

// worktreeDelUnreadableHereOperators is every here-document operator of a command line, in the order they stand: <<WORD,
// <<-WORD and a descriptor before the operator (0<<WORD). A here-string (<<<) and a quoted << are not operators.
func worktreeDelUnreadableHereOperators(line string) []worktreeDelUnreadableHereOperator {
	var ops []worktreeDelUnreadableHereOperator
	r := worktreeDelQuoteReader{prev: ' '}
	for i := 0; i < len(line); i++ {
		c := line[i]
		if r.escapes(line, i) {
			r.pair()
			i++
			continue
		}
		state := r.state
		r.step(c)
		if state != worktreeDelQuotePlain || c != '<' || i+1 >= len(line) || line[i+1] != '<' {
			continue
		}
		if i+2 < len(line) && line[i+2] == '<' { // a here-string operator (<<<), not a here-document
			i += 2
			continue
		}
		start := i + 2
		op := worktreeDelUnreadableHereOperator{}
		if start < len(line) && line[start] == '-' {
			op.stripTabs, start = true, start+1
		}
		for start < len(line) && worktreeDelQuoteSpace(line[start:]) > 0 {
			start += worktreeDelQuoteSpace(line[start:])
		}
		end := start
		for end < len(line) {
			if worktreeDelQuoteSpace(line[end:]) > 0 || strings.IndexByte(";&|()<>", line[end]) >= 0 {
				break
			}
			end++
		}
		op.word = line[start:end]
		op.delimiter = worktreeDelUnreadablePlainText(op.word)
		op.literal = op.word != op.delimiter
		ops = append(ops, op)
		i = end - 1
	}
	return ops
}

// worktreeDelUnreadableHereBody is one here-document of a command: the operator that opened it, the body text up to the
// line that closes it, and whether that line was found.
type worktreeDelUnreadableHereBody struct {
	op     worktreeDelUnreadableHereOperator
	body   string
	closed bool
}

// worktreeDelUnreadableLine is the line that opens at i, its newline kept, and the index after it.
func worktreeDelUnreadableLine(text string, i int) (string, int) {
	if j := strings.IndexByte(text[i:], '\n'); j >= 0 {
		return text[i : i+j+1], i + j + 1
	}
	return text[i:], len(text)
}

// worktreeDelUnreadableHereBodies reads the here-document bodies that follow a command line, in the order the operators
// stand: each body runs from the next line to the line that holds its delimiter. A body whose delimiter line is missing
// runs to the end of the text and reports closed false.
func worktreeDelUnreadableHereBodies(text string, from int, ops []worktreeDelUnreadableHereOperator) ([]worktreeDelUnreadableHereBody, int) {
	bodies := make([]worktreeDelUnreadableHereBody, 0, len(ops))
	i := from
	for _, op := range ops {
		var body []byte
		closed := false
		for i < len(text) {
			line, after := worktreeDelUnreadableLine(text, i)
			compare := strings.TrimSuffix(line, "\n")
			if op.stripTabs {
				compare = strings.TrimLeft(compare, "\t")
			}
			if compare == op.delimiter {
				closed, i = true, after
				break
			}
			body = append(body, line...)
			i = after
		}
		bodies = append(bodies, worktreeDelUnreadableHereBody{op: op, body: string(body), closed: closed})
	}
	return bodies, i
}

// worktreeDelUnreadableHereExpansion says whether a here-document body with an unquoted delimiter holds an expansion the
// outer shell performs on it before the shell reads it. A quote in the body protects nothing there, because the outer
// shell expands the body first, so every byte is read in plain state and only a backslash escape hides one.
func worktreeDelUnreadableHereExpansion(body string) bool {
	for i := 0; i < len(body); i++ {
		if body[i] == '\\' {
			i++
			continue
		}
		if worktreeDelUnreadableOuterAt(body, i, worktreeDelQuotePlain) {
			return true
		}
	}
	return false
}

// worktreeDelUnreadableRefusal is what the reading found: command says the CRW-772 command-name rule refused the
// command rather than a program position, and what names the position the way the refusal reads it.
type worktreeDelUnreadableRefusal struct {
	command bool
	what    string
}

// worktreeDelUnreadableScanKey identifies one text the reading covers. The same text can answer differently in another
// directory or at another depth, so both are part of the key.
type worktreeDelUnreadableScanKey struct {
	text  string
	cwd   string
	depth int
	named bool
}

// worktreeDelUnreadableScan is one evaluation's reading of the programs a command hands to a shell: the texts it has
// already covered, and how many it may still cover. named also turns on the CRW-772 command-name rule, which needs the
// worktree identity and the directory each segment runs in. It is never shared between evaluations.
type worktreeDelUnreadableScan struct {
	seen     map[worktreeDelUnreadableScanKey]bool
	budget   int
	named    bool
	id       WorktreeIdentity
	extended bool
}

// worktreeDelUnreadableProgram reports the first program position of a command whose text the guard cannot read before
// the outer shell runs it (CRW-726): a shell's -c program, an eval operand, a source or . operand, a shell reading its
// program from a pipe or a here-string, or a shell reading a here-document. what names the position the way the refusal
// reads it. It is target-free, so the memory gate (CRW-727) reads the same answer, and it reads the command and every
// program the command hands to a shell, down to worktreeDelQuoteDepth levels.
func worktreeDelUnreadableProgram(command string) (string, bool) {
	scan := &worktreeDelUnreadableScan{seen: map[worktreeDelUnreadableScanKey]bool{}, budget: worktreeDelWalkBudget}
	refusal, ok := scan.read(command, "", 0, false)
	return refusal.what, ok && !refusal.command
}

// worktreeDelUnreadableGuard is CRW-726's second reading of a command the walk allowed: the walk read every program it
// could see, but a program position, or a command name, that the outer shell builds at run time is still a program the
// guard cannot read, and the guard refuses it (fail closed). It runs after the walk, so a deny the guard already found
// for the same command keeps its own reason.
func worktreeDelUnreadableGuard(command, cwd string, id WorktreeIdentity) GuardVerdict {
	scan := &worktreeDelUnreadableScan{seen: map[worktreeDelUnreadableScanKey]bool{}, budget: worktreeDelWalkBudget, named: true, id: id, extended: true}
	refusal, ok := scan.read(command, cwd, 0, true)
	if !ok {
		return GuardVerdict{}
	}
	if refusal.command {
		return GuardVerdict{Deny: true, Reason: denyReason("a command the guard cannot name before it runs: "+refusal.what, id)}
	}
	return GuardVerdict{Deny: true, Reason: denyReason("a program the guard cannot read before it runs: "+refusal.what, id)}
}

// read is the reading of one text: the program positions of each of its segments, the shells that read a program from a
// pipe or a here-string, the command names the outer shell builds, and the programs a substitution, a here-document or a
// program position hands to a shell one depth deeper. cwd follows the cd segments the way the walk's does, because a
// target is judged from the directory its segment runs in.
// named turns the command-name rule on for this text: it holds for a command line the guard reads, and not for the rest
// of a segment after a substitution opener, which is the same text rather than a command of its own.
func (s *worktreeDelUnreadableScan) read(text, cwd string, depth int, named bool) (worktreeDelUnreadableRefusal, bool) {
	if depth > worktreeDelQuoteDepth {
		return worktreeDelUnreadableRefusal{}, false
	}
	key := worktreeDelUnreadableScanKey{text: text, cwd: cwd, depth: depth, named: named}
	if s.seen[key] {
		return worktreeDelUnreadableRefusal{}, false
	}
	if s.budget--; s.budget < 0 {
		return worktreeDelUnreadableRefusal{what: "a command too complex for the guard to read"}, true
	}
	s.seen[key] = true
	// A here-document a listed shell reads as its program is judged first, so its refusal names the here-document even
	// when the body also holds a command substitution (CRW-726, c1(e)).
	if refusal, ok := s.heredocs(text, cwd, depth, named); ok {
		return refusal, true
	}
	for _, reading := range worktreeDelReadings(text) {
		segCwd := cwd
		for _, cut := range worktreeDelUnreadableCuts(reading) {
			if cut.text == "(" || cut.text == ")" {
				continue
			}
			words := worktreeDelUnreadableWords(cut.text)
			if len(words) == 0 {
				continue
			}
			plain := worktreeDelUnreadablePlainTexts(words)
			if what, ok := worktreeDelUnreadableStdin(cut, words, plain); ok {
				return worktreeDelUnreadableRefusal{what: what}, true
			}
			for _, position := range worktreeDelUnreadablePositions(words) {
				if worktreeDelUnreadableOuter(position.check) {
					return worktreeDelUnreadableRefusal{what: position.what}, true
				}
				if refusal, ok := s.read(position.text, segCwd, depth+1, named); ok {
					return refusal, true
				}
			}
			if named {
				if what, ok := worktreeDelNamedByExpansion(cut.text, segCwd, s.id, s.extended); ok {
					return worktreeDelUnreadableRefusal{command: true, what: what}, true
				}
			}
			if len(plain) > 1 && plain[0] == "cd" && plain[1] != "" {
				segCwd = resolveFrom(segCwd, plain[1])
			}
		}
		for _, sub := range worktreeDelSubstitutions(reading, cwd) {
			if sub.body != "" {
				if refusal, ok := s.read(sub.body, sub.cwd, depth+1, named); ok {
					return refusal, true
				}
			}
			if sub.tail != "" {
				if refusal, ok := s.read(sub.tail, sub.cwd, depth, false); ok {
					return refusal, true
				}
			}
		}
	}
	return worktreeDelUnreadableRefusal{}, false
}

// worktreeDelUnreadableStdin is the rule for a shell that reads its program from its standard input: it stands to the
// right of a single pipe, whose left side the guard can never read, or it takes a here-string, whose word is judged as a
// -c program is. The shell must have no -c program and no script operand.
func worktreeDelUnreadableStdin(cut worktreeDelUnreadableCut, words []worktreeDelUnreadableWord, plain []string) (string, bool) {
	i := worktreeDelUnreadableCommandWord(plain)
	if i < 0 {
		return "", false
	}
	name := basename(plain[i])
	if !strings.Contains(worktreeDelUnreadableShells, " "+name+" ") || !worktreeDelUnreadableStdinShell(name, plain[i+1:]) {
		return "", false
	}
	if cut.sep == "|" {
		return "a shell program read from a pipe", true
	}
	if j := worktreeDelUnreadableHereString(plain[i+1:]); j >= 0 && worktreeDelUnreadableOuter(words[i+1+j].raw) {
		return "a shell program read from a here-string", true
	}
	return "", false
}

// heredocs judges every here-document of a text that a listed shell reads as its program: with a delimiter word that
// holds a quote or a backslash the body is literal and is read as shell text, and with an unquoted delimiter the outer
// shell expands the body first, so a body holding an expansion is a program the guard cannot read. A body whose closing
// delimiter line is missing never ends, so it is refused too.
func (s *worktreeDelUnreadableScan) heredocs(text, cwd string, depth int, named bool) (worktreeDelUnreadableRefusal, bool) {
	for i := 0; i < len(text); {
		line, after := worktreeDelUnreadableLine(text, i)
		ops := worktreeDelUnreadableHereOperators(line)
		if len(ops) == 0 {
			i = after
			continue
		}
		bodies, next := worktreeDelUnreadableHereBodies(text, after, ops)
		if worktreeDelUnreadableLineShell(line) {
			for _, body := range bodies {
				if !body.closed || !body.op.literal && worktreeDelUnreadableHereExpansion(body.body) {
					return worktreeDelUnreadableRefusal{what: "a shell program read from a here-document"}, true
				}
				if refusal, ok := s.read(body.body, cwd, depth+1, named); ok {
					return refusal, true
				}
			}
		}
		i = next
	}
	return worktreeDelUnreadableRefusal{}, false
}

// worktreeDelUnreadableLineShell says whether a command line runs a listed shell that reads its program from standard
// input, so that the here-documents opened on that line are the program it reads.
func worktreeDelUnreadableLineShell(line string) bool {
	for _, cut := range worktreeDelUnreadableCuts(line) {
		if cut.text == "(" || cut.text == ")" {
			continue
		}
		plain := worktreeDelUnreadablePlainTexts(worktreeDelUnreadableWords(cut.text))
		i := worktreeDelUnreadableCommandWord(plain)
		if i < 0 {
			continue
		}
		name := basename(plain[i])
		if strings.Contains(worktreeDelUnreadableHereShells, " "+name+" ") && worktreeDelUnreadableStdinShell(name, plain[i+1:]) {
			return true
		}
	}
	return false
}

// worktreeDelNamedByExpansion is the refusal of a simple command whose name word the outer shell builds at run time
// (CRW-772). A name word that is wholly one command substitution or one backtick pair is a command line the shell
// runs, so it is refused whatever its operands; a name word that carries a parameter expansion is judged as rm -r with
// the command's literal operands, so it is refused only when one of them reaches the protected worktree. A literal
// name, an assignment-only command and a parameter-expansion name whose operands reach nothing protected are read as
// today.
func worktreeDelNamedByExpansion(segment, cwd string, id WorktreeIdentity, extended bool) (string, bool) {
	words := worktreeDelUnreadableWords(segment)
	if len(words) == 0 {
		return "", false
	}
	plain := worktreeDelUnreadablePlainTexts(words)
	i := worktreeDelUnreadableCommandWord(plain)
	if i < 0 || !words[i].outer || isAssignment(plain[i]) {
		return "", false
	}
	if words[i].whole {
		return "a command line built by a command substitution", true
	}
	flagsDone := false
	for _, operand := range plain[i+1:] {
		switch {
		case !flagsDone && operand == "--":
			flagsDone = true
		case !flagsDone && strings.HasPrefix(operand, "-") && len(operand) > 1:
		default:
			if isProtectedTarget(operand, cwd, id, extended) {
				return "a command named by an expansion", true
			}
		}
	}
	return "", false
}
