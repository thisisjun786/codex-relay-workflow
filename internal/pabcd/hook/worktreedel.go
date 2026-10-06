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
		case c == '#' && strings.IndexByte(" \t\n;&|()", r.prev) >= 0:
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
			for _, operand := range operands[:worktreeDelShellProgramEnd(name, operands)] { // the words from there on are data: $0, $1...
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

// worktreeDelShellProgramEnd is how many of a shell's operands the walk reads as programs: the words from there on are $0, $1
// and so on, which the shell does not read (bash -c 'echo OK' 'rm -rf x' runs echo). It is every operand, the reading that
// CRW-611 left, unless the program is certain, and it holds back wherever the option parse could be wrong or the words after the
// program could matter: a redirection word anywhere (a quoted > looks like one) and a program that is a bare option or can name
// its operands (a dollar sign, BASH_ARGV, argv: bash -c 'eval "$0"' 'rm -rf x' runs rm). For sh, bash, dash and ash the operands
// after the program are the inner shell's $0, $1 and so on whatever they look like, so a later option-like word is not a program
// (bash -c 'echo OK' -c 'rm -rf x' runs echo, CRW-670); su, whose last -c is its program, and every other shell keep the reading
// of every later option-like word, because the walk does not tell those shells apart.
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
	if strings.Contains(word, "$") || strings.Contains(word, "BASH_ARG") || strings.Contains(word, "argv") ||
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
func worktreeDelSubstitutionBody(rest string, backtick bool) (string, int) {
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
		if backtick {
			if c == '`' && r.state == worktreeDelQuotePlain {
				return string(out), i + 1
			}
		} else {
			switch {
			case c == '(' && r.state == worktreeDelQuotePlain:
				depth++
			case c == ')' && r.state == worktreeDelQuotePlain:
				if depth--; depth == 0 {
					return string(out), i + 1
				}
			}
		}
		out = append(out, c)
		r.step(c)
	}
	return string(out), len(rest)
}

// worktreeDelSubstitutions is the programs a segment runs before it runs: the body of every command substitution, $(...) and
// backtick, and of every process substitution, <(...) and >(...), that stands in plain text or inside double quotes, because
// the outer shell reads and runs each of them first (CRW-670). A substitution inside single quotes, inside $'...' or inside a
// comment is data, and so is a $, < or backtick that a backslash escapes.
func worktreeDelSubstitutions(segment string) []string {
	var out []string
	r := worktreeDelQuoteReader{prev: ' '}
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
		var body string
		var n int
		switch {
		case c == '$' && i+1 < len(segment) && segment[i+1] == '(':
			body, n = worktreeDelSubstitutionBody(segment[i+2:], false)
			n += 2
		case c == '`':
			body, n = worktreeDelSubstitutionBody(segment[i+1:], true)
			n++
		case (c == '<' || c == '>') && i+1 < len(segment) && segment[i+1] == '(':
			body, n = worktreeDelSubstitutionBody(segment[i+2:], false)
			n += 2
		default:
			r.step(c)
			continue
		}
		if strings.TrimSpace(body) != "" {
			out = append(out, body)
		}
		i += n - 1
	}
	return out
}

// walk judges a command: the oracle's walk reads it as it is, the extended walk reads each of its readings, where the
// shell has removed the backslash-newline pairs it removes, and denies when any reading denies. It judges every reading twice,
// over the grammar the extended walk had and over bash's own quoting, comments and backslashes (worktreeDelQuoteSegments), the
// first grammar first, so nothing it denied is allowed and each earlier deny keeps its reason. A here-document body is shell
// text to both, and each reads a quote in it its own way.
func walk(command, cwd string, id WorktreeIdentity, extended bool) GuardVerdict {
	if !extended {
		return worktreeDelWalk(command, cwd, id, false)
	}
	return worktreeDelQuoteJudge(command, cwd, id, 0)
}

// worktreeDelQuoteDepth is how many program strings deep the walk follows a shell's program (sh -c 'sh -c ...'). A program
// that is still unread there is denied, whether or not it holds a blank: the walk cannot tell what it runs.
const worktreeDelQuoteDepth = 8

// worktreeDelQuoteJudge is the extended walk of one text, depth program strings down: every reading over the old grammar and
// then over the quote-aware one.
func worktreeDelQuoteJudge(command, cwd string, id WorktreeIdentity, depth int) GuardVerdict {
	readings := worktreeDelReadings(command)
	for _, quoting := range []bool{false, true} {
		for _, reading := range readings {
			if verdict := worktreeDelQuoteWalk(reading, cwd, id, true, quoting, depth); verdict.Deny {
				return verdict
			}
		}
	}
	return GuardVerdict{}
}

// worktreeDelWalk is walk's loop over one text in the grammar of the oracle (and of the extended walk before CRW-611).
func worktreeDelWalk(command, cwd string, id WorktreeIdentity, extended bool) GuardVerdict {
	return worktreeDelQuoteWalk(command, cwd, id, extended, false, 0)
}

// worktreeDelQuoteWalk is the loop over one text: the segments in order, a cd moving the directory later segments run in, and
// the conservative fallback when a destructive verb was seen and the command mentions the worktree but no target resolved.
// quoting reads the text over bash's own quotes, backslashes and comments instead, and judges the program string that a shell
// word hands to -c and the program a substitution runs (depth says how many programs deep this text is).
func worktreeDelQuoteWalk(command, cwd string, id WorktreeIdentity, extended, quoting bool, depth int) GuardVerdict {
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
		if len(tokens) > 1 && tokens[0] == "cd" && tokens[1] != "" {
			segCwd = resolveFrom(segCwd, tokens[1])
			continue
		}
		if quoting {
			programs := worktreeDelQuoteProgram(tokens, depth >= worktreeDelQuoteDepth)
			if depth >= worktreeDelQuoteDepth && len(programs) > 0 {
				return GuardVerdict{Deny: true, Reason: denyReason("a shell program nested past the reading depth", id)}
			}
			for _, program := range programs {
				if verdict := worktreeDelQuoteJudge(program, segCwd, id, depth+1); verdict.Deny {
					return verdict
				}
			}
			for _, program := range worktreeDelSubstitutions(segment) {
				if verdict := worktreeDelQuoteJudge(program, segCwd, id, depth+1); verdict.Deny {
					return verdict
				}
			}
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

// evaluateCommand allows anything outside a managed worktree. Inside one it walks the command as the oracle does and,
// when that allows, walks it again over the extended grammar.
func evaluateCommand(command, cwd string, id WorktreeIdentity) GuardVerdict {
	if !id.Managed || text.Trim(command) == "" {
		return GuardVerdict{}
	}
	if verdict := walk(command, cwd, id, false); verdict.Deny {
		return verdict
	}
	return walk(command, cwd, id, true)
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
