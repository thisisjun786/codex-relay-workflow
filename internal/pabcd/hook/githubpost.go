package hook

// CRW-783: CRW's own protection, with no CXC oracle. The PreToolUse guard over the shell tools stops a
// GitHub posting command whose text sits inside the command or holds an outer-shell expansion, and
// scans the text bound for GitHub (an inline title, or the file of --body-file, -F body=@F or --input)
// for secrets. The deny reason names the rule and the place, never the value.
//
// Four rules: inline-github-body (a body may never sit in the command), expansion-in-github-text (text
// bound for GitHub holding an outer-shell expansion outside single quotes), secret-in-github-text (a
// secret pattern in that text) and unreadable-github-post (a post the guard cannot read, denied because
// the guard fails closed). A command unrelated to GitHub behaves as it did.

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const (
	githubPostWhereCommand = "command"
	githubPostRuleInline   = "inline-github-body"
	githubPostRuleExpand   = "expansion-in-github-text"
	githubPostRuleSecret   = "secret-in-github-text"
	githubPostRuleUnread   = "unreadable-github-post"

	// githubPostDepth bounds how deep a shell -c or eval program is read, so the work stays linear.
	githubPostDepth = 8

	// githubPostMaxFileBytes is the most the guard reads of a file a post names (1 MiB).
	githubPostMaxFileBytes = 1024 * 1024

	// GitHubPostMaxStdinBytes is this row's own stdin bound (1 MiB), the bound a PreToolUse gate of this
	// runtime reads its payload within. The row reads its input itself; over the bound the guard refuses
	// rather than passing an input it could not judge.
	GitHubPostMaxStdinBytes = 1024 * 1024
)

// GitHubPostAnswer is the component row's whole input policy: the guard's answer for the payload, or the
// deny envelope when the payload is over the bound. A read that fails is read as empty input, as the
// other component ingresses read it.
func GitHubPostAnswer(in io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(in, GitHubPostMaxStdinBytes+1))
	if err != nil {
		return ""
	}
	if len(b) > GitHubPostMaxStdinBytes {
		return githubPostDeny(githubPostRuleUnread, githubPostWhereCommand)
	}
	return HandleGitHubPostGuard(string(b))
}

// HandleGitHubPostGuard is the guard's PreToolUse leg: the deny envelope for a GitHub post these rules
// stop, else empty. A call that is not one PreToolUse object, or a tool that is not one of the shell
// tools, passes, as does a command unrelated to GitHub.
func HandleGitHubPostGuard(raw string) string {
	p := editObject(raw)
	if p["hook_event_name"] != "PreToolUse" {
		return ""
	}
	tool, _ := p["tool_name"].(string)
	if !memoryGateShellTool(tool) {
		return ""
	}
	input, _ := p["tool_input"].(map[string]any)
	if input == nil {
		return ""
	}
	cwd, _ := p["cwd"].(string)
	s := githubPostScan{cwd: cwd, depth: githubPostDepth}
	words, command, argv := githubPostInput(input)
	if argv {
		site, denied := s.command(words, "")
		if !denied {
			return ""
		}
		return githubPostDeny(site.rule, site.place)
	}
	if text.Trim(command) == "" {
		return ""
	}
	site, denied := s.text(command)
	if !denied {
		return ""
	}
	return githubPostDeny(site.rule, site.place)
}

// githubPostDeny is the deny envelope: the rule and the place, then one way forward. The text, the
// matched pattern and its context stay out of it, because this reason reaches a public thread.
func githubPostDeny(rule, place string) string {
	reason := "GitHub post blocked (" + rule + ") at " + place +
		": write the text to a file, check it, and pass it with --body-file, -F body=@file or --input"
	return editAnswer("deny", reason, reason)
}

// githubPostSite is why and where a GitHub post is denied: the rule and the place the reason names.
type githubPostSite struct{ rule, place string }

// githubPostScan is one payload's judgement: its working directory, the quoted heredoc body of the
// command being read, and how deep a program may still be read.
type githubPostScan struct {
	cwd, body string
	hasBody   bool
	depth     int
}

// githubPostWord is one word of a simple command: text is the word with its quoting removed, raw the
// word as written, so a caller can still ask what the outer shell would expand.
type githubPostWord struct{ text, raw string }

// githubPostInput is tool_input's command or cmd: an argv array becomes words that are already split, so
// no shell expands them and a word has no written form for the expansion rule to read; a string is shell
// text. The array is looked for first, as the host that sends one sends no text beside it.
func githubPostInput(input map[string]any) ([]githubPostWord, string, bool) {
	for _, key := range [...]string{"command", "cmd"} {
		items, ok := input[key].([]any)
		if !ok {
			continue
		}
		words := make([]githubPostWord, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, "", false
			}
			words = append(words, githubPostWord{text: s})
		}
		return words, "", true
	}
	for _, key := range [...]string{"command", "cmd"} {
		if s, ok := input[key].(string); ok {
			return nil, s, false
		}
	}
	return nil, "", false
}

// text reads one shell text: every simple command it holds. Text the guard cannot split, or a program it
// cannot read that mentions a GitHub post, is denied as unreadable.
func (s *githubPostScan) text(command string) (githubPostSite, bool) {
	stripped, balanced := githubPostStripped(command)
	if !balanced {
		if githubPostMentions(command) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		return githubPostSite{}, false
	}
	// The body is read from the text as written: the stripped form has already dropped it.
	if body, ok := githubPostHeredocBody(command); ok {
		s.body, s.hasBody = body, true
	}
	for _, segment := range githubPostSegments(stripped) {
		if site, denied := s.command(githubPostWords(segment), segment); denied {
			return site, true
		}
	}
	return githubPostSite{}, false
}

// command judges one simple command: a gh posting command, a shell -c or eval program read one level
// deeper, or nothing.
func (s *githubPostScan) command(words []githubPostWord, segment string) (githubPostSite, bool) {
	spelled := make([]string, len(words))
	for i, w := range words {
		spelled[i] = w.text
	}
	rest := shellVerbStripPrefixes(spelled)
	head := len(spelled) - len(rest)
	if len(rest) == 0 {
		return githubPostSite{}, false
	}
	if shellVerbName(rest[0]) == "gh" {
		return s.gh(words[head+1:])
	}
	if s.depth <= 0 {
		return githubPostSite{}, false
	}
	program, ok := githubPostProgram(shellVerbName(rest[0]), rest)
	if !ok {
		return githubPostSite{}, false
	}
	// A program holding a command substitution cannot be read at all, so a text that mentions gh with
	// pr, issue or api is denied rather than passed unjudged; anything else is read one level down.
	if githubPostExpands(program, false, true) {
		if githubPostMentions(segment) {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		return githubPostSite{}, false
	}
	s.depth--
	return s.text(program)
}

// githubPostProgram is the command string a shell -c or eval would run, when this command starts one.
func githubPostProgram(verb string, rest []string) (string, bool) {
	if shellVerbIsShell(verb) {
		return shellVerbShellScript(rest[1:])
	}
	if verb == "eval" {
		return strings.Join(rest[1:], " "), true
	}
	return "", false
}

// gh judges a gh command: pr create|edit|comment|review, issue create|comment|edit, or api with a body
// or title field. gh pr view|list|checks|diff and gh api without such a field are not targets. The group
// word is looked for rather than taken as the first argument, because gh takes global flags before it
// (gh --repo o/r pr create).
func (s *githubPostScan) gh(args []githubPostWord) (githubPostSite, bool) {
	group := -1
	for i, a := range args {
		if a.text == "pr" || a.text == "issue" || a.text == "api" {
			group = i
			break
		}
	}
	if group < 0 {
		return githubPostSite{}, false
	}
	if args[group].text == "api" {
		return s.api(args[group+1:])
	}
	if group+1 >= len(args) {
		return githubPostSite{}, false
	}
	sub := args[group+1].text
	if args[group].text == "pr" {
		if sub != "create" && sub != "edit" && sub != "comment" && sub != "review" {
			return githubPostSite{}, false
		}
	} else if sub != "create" && sub != "comment" && sub != "edit" {
		return githubPostSite{}, false
	}
	return s.flags(args[group+2:])
}

// flags judges a gh pr or gh issue command: -b and --body carry the body inside the command,
// --body-file and -F name a file, and -t and --title may stay inside the command unless they hold an
// outer-shell expansion or a secret.
func (s *githubPostScan) flags(args []githubPostWord) (githubPostSite, bool) {
	for i := 0; i < len(args); i++ {
		w := args[i]
		switch {
		case w.text == "-b" || w.text == "--body":
			return s.inline(githubPostNext(args, i), true)
		case strings.HasPrefix(w.text, "--body="):
			return s.inline(githubPostValue(w, "--body="), true)
		case w.text == "-t" || w.text == "--title":
			if site, denied := s.inline(githubPostNext(args, i), false); denied {
				return site, true
			}
		case strings.HasPrefix(w.text, "--title="):
			if site, denied := s.inline(githubPostValue(w, "--title="), false); denied {
				return site, true
			}
		case w.text == "--body-file" || w.text == "-F":
			return s.file(githubPostNext(args, i).text)
		case strings.HasPrefix(w.text, "--body-file="):
			return s.file(strings.TrimPrefix(w.text, "--body-file="))
		}
	}
	return githubPostSite{}, false
}

// api judges gh api, which is a target only through a body or title field: -f and --raw-field carry
// text, -F and --field carry text or @file, and --input names a file.
func (s *githubPostScan) api(args []githubPostWord) (githubPostSite, bool) {
	for i := 0; i < len(args); i++ {
		w := args[i]
		if value, ok := strings.CutPrefix(w.text, "--input="); ok {
			return s.file(value)
		}
		switch w.text {
		case "--input":
			return s.file(githubPostNext(args, i).text)
		case "-f", "--raw-field", "-F", "--field":
			field := githubPostNext(args, i)
			key, value, ok := strings.Cut(field.text, "=")
			switch {
			case !ok:
			case key == "body" && strings.HasPrefix(value, "@"):
				return s.file(strings.TrimPrefix(value, "@"))
			case key == "body":
				return s.inline(field, true)
			case key == "title":
				if site, denied := s.inline(field, false); denied {
					return site, true
				}
			}
		}
	}
	return githubPostSite{}, false
}

// githubPostNext is the word after the option at i, or an empty word when the option ends the command.
func githubPostNext(args []githubPostWord, i int) githubPostWord {
	if i+1 < len(args) {
		return args[i+1]
	}
	return githubPostWord{}
}

// githubPostValue is the value of a --flag=value word: its text is what follows the equals sign and its
// written form stays the whole word, so the expansion rule still reads the value's own quoting.
func githubPostValue(w githubPostWord, prefix string) githubPostWord {
	return githubPostWord{text: strings.TrimPrefix(w.text, prefix), raw: w.raw}
}

// inline judges text bound for GitHub that sits inside the command: an outer-shell expansion is denied
// first, then an inline body, then a secret pattern in a title that may stay inline.
func (s *githubPostScan) inline(w githubPostWord, body bool) (githubPostSite, bool) {
	if githubPostExpands(w.raw, true, false) {
		return githubPostSite{githubPostRuleExpand, githubPostWhereCommand}, true
	}
	if body {
		return githubPostSite{githubPostRuleInline, githubPostWhereCommand}, true
	}
	if _, found := githubPostSecretLine(w.text); found {
		return githubPostSite{githubPostRuleSecret, githubPostWhereCommand}, true
	}
	return githubPostSite{}, false
}

// file judges the text a post takes from a file: the file must be readable (a regular file of at most
// 1 MiB, relative to the payload's working directory) and hold no secret pattern. A name of "-" is
// standard input, which the guard reads from a quoted heredoc of the same command and denies otherwise,
// because what would be posted is then out of its sight.
func (s *githubPostScan) file(name string) (githubPostSite, bool) {
	if name == "" {
		return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
	}
	if name == "-" {
		if !s.hasBody {
			return githubPostSite{githubPostRuleUnread, githubPostWhereCommand}, true
		}
		if line, found := githubPostSecretLine(s.body); found {
			return githubPostSite{githubPostRuleSecret, "-:" + strconv.Itoa(line)}, true
		}
		return githubPostSite{}, false
	}
	content, ok := githubPostReadFile(name, s.cwd)
	if !ok {
		return githubPostSite{githubPostRuleUnread, name}, true
	}
	if line, found := githubPostSecretLine(content); found {
		return githubPostSite{githubPostRuleSecret, name + ":" + strconv.Itoa(line)}, true
	}
	return githubPostSite{}, false
}

// githubPostReadFile reads the text a post names: a regular file of at most githubPostMaxFileBytes.
func githubPostReadFile(name, cwd string) (string, bool) {
	path := name
	if !filepath.IsAbs(path) {
		base := cwd
		if base == "" {
			base, _ = os.Getwd()
		}
		path = filepath.Join(base, path)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()
	st, err := file.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	b, err := io.ReadAll(io.LimitReader(file, githubPostMaxFileBytes+1))
	if err != nil || len(b) > githubPostMaxFileBytes {
		return "", false
	}
	return string(b), true
}

// githubPostSegments is the simple commands of one shell text: the shellwrite reader's split of pipes, ;
// and && within a line, and a newline ends a command too. Heredoc bodies are already gone, so their text
// is never read as one.
func githubPostSegments(command string) []string {
	out := []string{}
	rs, cur := []rune(command), strings.Builder{}
	flush := func() {
		for _, segment := range splitShellSegments(utf16.Encode([]rune(cur.String()))) {
			if s := shellString(segment); text.Trim(s) != "" {
				out = append(out, s)
			}
		}
		cur.Reset()
	}
	for i := 0; i < len(rs); i++ {
		switch c := rs[i]; {
		case c == '\'' || c == '"':
			end, _ := githubPostQuotedEnd(rs, i)
			cur.WriteString(string(rs[i:end]))
			i = end - 1
		case c == '\n':
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// githubPostStripped is the shell text with its heredoc bodies replaced, as the shellwrite reader reads
// it, and whether every quote of it closes: text the guard cannot split is judged by the fail-closed
// rule, because the split it would make is not the shell's.
func githubPostStripped(command string) (string, bool) {
	stripped := shellString(stripHeredocBodies(utf16.Encode([]rune(command))))
	rs := []rune(stripped)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '\'', '"':
			end, closed := githubPostQuotedEnd(rs, i)
			if !closed {
				return stripped, false
			}
			i = end - 1
		}
	}
	return stripped, true
}

// githubPostQuotedEnd is the index just past the quoted region that starts at i, and whether the quote
// closes at all.
func githubPostQuotedEnd(rs []rune, i int) (int, bool) {
	quote := rs[i]
	for i++; i < len(rs); i++ {
		if quote == '"' && rs[i] == '\\' {
			i++
			continue
		}
		if rs[i] == quote {
			return i + 1, true
		}
	}
	return len(rs), false
}

// githubPostWords splits one simple command into its words: whitespace separates words outside quotes, a
// quoted region is part of its word, and a backslash escapes the next character outside single quotes.
func githubPostWords(segment string) []githubPostWord {
	rs, out := []rune(segment), []githubPostWord{}
	raw, spelled, mode, started := strings.Builder{}, strings.Builder{}, rune(0), false
	flush := func() {
		if started {
			out = append(out, githubPostWord{spelled.String(), raw.String()})
			raw, spelled, started = strings.Builder{}, strings.Builder{}, false
		}
	}
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		switch {
		case mode == '\'':
			raw.WriteRune(c)
			if c == '\'' {
				mode = 0
			} else {
				spelled.WriteRune(c)
			}
		case mode == '"':
			switch {
			case c == '\\' && i+1 < len(rs) && githubPostEscapable(rs[i+1]):
				raw.WriteString(string(rs[i : i+2]))
				spelled.WriteRune(rs[i+1])
				i++
			case c == '"':
				raw.WriteRune(c)
				mode = 0
			default:
				raw.WriteRune(c)
				spelled.WriteRune(c)
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flush()
		case c == '\'' || c == '"':
			started, mode = true, c
			raw.WriteRune(c)
		case c == '\\' && i+1 < len(rs):
			started = true
			raw.WriteString(string(rs[i : i+2]))
			spelled.WriteRune(rs[i+1])
			i++
		default:
			started = true
			raw.WriteRune(c)
			spelled.WriteRune(c)
		}
	}
	flush()
	return out
}

// githubPostEscapable is the character a backslash escapes inside double quotes, where the shell keeps
// every other backslash as itself.
func githubPostEscapable(c rune) bool {
	return c == '"' || c == '\\' || c == '$' || c == 0x60 || c == '\n'
}

// githubPostExpands is whether the text as written holds an outer-shell expansion the shell acts on
// before gh sees the text: a backtick, a command substitution, a brace or name expansion, an arithmetic
// expansion or a process substitution, and with newline true a newline too. A single-quoted region is
// literal to the shell and holds none of them. With substitution true only the two the guard cannot read
// at all count, a backtick and a command substitution.
func githubPostExpands(raw string, newline, substitution bool) bool {
	rs, mode := []rune(raw), rune(0)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if mode == '\'' {
			if c == '\'' {
				mode = 0
			}
			continue
		}
		if c == '\\' {
			if mode != '"' || i+1 < len(rs) && githubPostEscapable(rs[i+1]) {
				i++
				continue
			}
		}
		if c == 0x60 {
			return true
		}
		if c == '$' && i+1 < len(rs) {
			next := rs[i+1]
			if next == '(' {
				return true
			}
			if !substitution && (next == '{' || next == '_' || next >= 'a' && next <= 'z' || next >= 'A' && next <= 'Z') {
				return true
			}
		}
		if c == '"' {
			if mode == '"' {
				mode = 0
			} else {
				mode = '"'
			}
			continue
		}
		if c == '<' && !substitution && i+1 < len(rs) && rs[i+1] == '(' {
			return true
		}
		if c == '\n' && newline {
			return true
		}
	}
	return false
}

// githubPostMentions is whether the text names gh with pr, issue or api: the words the fail-closed rule
// looks for in a program the guard cannot read. The text is read as written, so a name inside a quoted
// program still counts.
func githubPostMentions(text string) bool {
	return githubPostWordPresent(text, "gh") && (githubPostWordPresent(text, "pr") ||
		githubPostWordPresent(text, "issue") || githubPostWordPresent(text, "api"))
}

// githubPostWordPresent is whether the text holds the word with no word character beside it.
func githubPostWordPresent(text, word string) bool {
	for i := 0; i+len(word) <= len(text); i++ {
		if text[i:i+len(word)] != word {
			continue
		}
		if i > 0 && githubPostWordByte(text[i-1]) || i+len(word) < len(text) && githubPostWordByte(text[i+len(word)]) {
			continue
		}
		return true
	}
	return false
}

// githubPostWordByte is a byte that continues a shell word.
func githubPostWordByte(c byte) bool {
	return c == '_' || c == '-' || c == '.' || c == '/' ||
		c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

// githubPostHeredocBody is the body of the first heredoc of the text whose delimiter is quoted, the form
// the shell does not expand, and whether there is one. A heredoc whose delimiter is not quoted is not
// read: the shell expands its body, so the guard cannot know what would be posted.
func githubPostHeredocBody(command string) (string, bool) {
	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '\\':
			i++
		case '\'', '"':
			end, _ := githubPostQuotedEnd(rs, i)
			i = end - 1
		case '<':
			if i+1 < len(rs) && rs[i+1] == '<' && (i+2 >= len(rs) || rs[i+2] != '<') {
				if body, ok := githubPostHeredocAt(rs, i); ok {
					return body, true
				}
			}
		}
	}
	return "", false
}

// githubPostHeredocAt reads the heredoc introduced by the << at index i when its delimiter is quoted, and
// returns its body: the lines after the operator's own line, up to the line that is the delimiter.
func githubPostHeredocAt(rs []rune, i int) (string, bool) {
	j := i + 2
	if j < len(rs) && rs[j] == '-' {
		j++
	}
	for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
		j++
	}
	if j >= len(rs) || rs[j] != '\'' && rs[j] != '"' {
		return "", false
	}
	quote, start := rs[j], j+1
	for j = start; j < len(rs) && rs[j] != quote; j++ {
	}
	if j >= len(rs) || j == start {
		return "", false
	}
	delim := string(rs[start:j])
	for j < len(rs) && rs[j] != '\n' {
		j++
	}
	body := strings.Builder{}
	for j++; j <= len(rs); {
		end := j
		for end < len(rs) && rs[end] != '\n' {
			end++
		}
		line := string(rs[j:end])
		if strings.TrimLeft(line, "\t") == delim {
			return body.String(), true
		}
		body.WriteString(line)
		body.WriteString("\n")
		if end >= len(rs) {
			return body.String(), true
		}
		j = end + 1
	}
	return "", false
}
